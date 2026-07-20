#
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""Synchronous sandbox pool implementation."""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from datetime import timedelta

from opensandbox._pool_reconciler import ReconcileState, run_reconcile_tick
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolDestroyedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.pool_types import (
    AcquirePolicy,
    IdleEntry,
    PoolConfig,
    PoolCreationSpec,
    PoolDestroyState,
    PooledSandboxCreateContext,
    PooledSandboxCreateReason,
    PooledSandboxCreator,
    PoolLifecycleState,
    PoolSnapshot,
    PoolState,
)
from opensandbox.pool_types import (
    try_take_idle_with_min_ttl as _try_take_idle_with_min_ttl,
)
from opensandbox.sync.manager import SandboxManagerSync
from opensandbox.sync.sandbox import SandboxSync

logger = logging.getLogger(__name__)

_WARMUP_TERMINATION_TIMEOUT_SECONDS = 5.0


class SandboxPoolSync:
    """Client-side synchronous sandbox pool aligned with Kotlin SandboxPool."""

    def __init__(
        self,
        *,
        pool_name: str,
        max_idle: int,
        state_store: object,
        connection_config: ConnectionConfigSync,
        creation_spec: PoolCreationSpec,
        owner_id: str | None = None,
        warmup_concurrency: int | None = None,
        primary_lock_ttl: timedelta = timedelta(seconds=60),
        reconcile_interval: timedelta = timedelta(seconds=30),
        degraded_threshold: int = 3,
        acquire_ready_timeout: timedelta = timedelta(seconds=30),
        acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        acquire_health_check: Callable[[SandboxSync], bool] | None = None,
        acquire_skip_health_check: bool = False,
        warmup_ready_timeout: timedelta = timedelta(seconds=30),
        warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        warmup_health_check: Callable[[SandboxSync], bool] | None = None,
        warmup_sandbox_preparer: Callable[[SandboxSync], None] | None = None,
        warmup_skip_health_check: bool = False,
        idle_timeout: timedelta = timedelta(hours=24),
        drain_timeout: timedelta = timedelta(seconds=30),
        acquire_min_remaining_ttl: timedelta | None = None,
        sandbox_manager_factory: Callable[
            [ConnectionConfigSync], SandboxManagerSync
        ] = SandboxManagerSync.create,
        sandbox_factory: type[SandboxSync] = SandboxSync,
        sandbox_creator: PooledSandboxCreator | None = None,
    ) -> None:
        self._config = PoolConfig(
            pool_name=pool_name,
            owner_id=owner_id,
            max_idle=max_idle,
            warmup_concurrency=warmup_concurrency,
            primary_lock_ttl=primary_lock_ttl,
            state_store=state_store,  # type: ignore[arg-type]
            connection_config=connection_config,
            creation_spec=creation_spec,
            reconcile_interval=reconcile_interval,
            degraded_threshold=degraded_threshold,
            acquire_ready_timeout=acquire_ready_timeout,
            acquire_health_check_polling_interval=acquire_health_check_polling_interval,
            acquire_health_check=acquire_health_check,
            acquire_skip_health_check=acquire_skip_health_check,
            warmup_ready_timeout=warmup_ready_timeout,
            warmup_health_check_polling_interval=warmup_health_check_polling_interval,
            warmup_health_check=warmup_health_check,
            warmup_sandbox_preparer=warmup_sandbox_preparer,
            warmup_skip_health_check=warmup_skip_health_check,
            idle_timeout=idle_timeout,
            drain_timeout=drain_timeout,
            acquire_min_remaining_ttl=acquire_min_remaining_ttl,
            sandbox_creator=sandbox_creator,
        )
        self._state_store = self._config.state_store
        self._connection_config = connection_config
        self._creation_spec = creation_spec
        self._sandbox_manager_factory = sandbox_manager_factory
        self._sandbox_factory = sandbox_factory
        self._reconcile_state = ReconcileState(degraded_threshold)
        self._current_max_idle = max_idle
        self._lifecycle_state = PoolLifecycleState.NOT_STARTED
        self._lifecycle_lock = threading.RLock()
        self._reconcile_lock = threading.Lock()
        self._in_flight = 0
        self._in_flight_condition = threading.Condition()
        self._stop_event = threading.Event()
        self._scheduler_thread: threading.Thread | None = None
        self._warmup_executor: ThreadPoolExecutor | None = None
        self._sandbox_manager: SandboxManagerSync | None = None

    def start(self) -> None:
        with self._lifecycle_lock:
            if self._lifecycle_state in (
                PoolLifecycleState.RUNNING,
                PoolLifecycleState.STARTING,
            ):
                return
            self._lifecycle_state = PoolLifecycleState.STARTING
            try:
                self._ensure_pool_namespace_active()
                self._warn_if_primary_lock_ttl_may_expire_during_warmup()
                self._sandbox_manager = self._create_sandbox_manager()
                self._state_store.set_idle_entry_ttl(
                    self._config.pool_name, self._config.idle_timeout
                )
                self._state_store.set_max_idle(
                    self._config.pool_name, self._config.max_idle
                )
                self._warmup_executor = ThreadPoolExecutor(
                    max_workers=max(1, int(self._config.warmup_concurrency or 1)),
                    thread_name_prefix=f"sandbox-pool-warmup-{self._config.pool_name}",
                )
                stop_event = threading.Event()
                self._stop_event = stop_event
                self._scheduler_thread = threading.Thread(
                    target=self._run_scheduler,
                    args=(stop_event,),
                    name=f"sandbox-pool-reconcile-{self._config.pool_name}",
                    daemon=True,
                )
                self._lifecycle_state = PoolLifecycleState.RUNNING
                self._scheduler_thread.start()
            except Exception:
                self._stop_reconcile(wait_for_warmup=True)
                self._close_provider()
                self._lifecycle_state = PoolLifecycleState.STOPPED
                raise

    def acquire(
        self,
        sandbox_timeout: timedelta | None = None,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ) -> SandboxSync:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            state = self._lifecycle_state
            self._raise_if_pool_namespace_destroyed()
            raise PoolNotRunningException(
                f"Cannot acquire when pool state is {state.value}"
            )
        self._begin_operation()
        try:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                state = self._lifecycle_state
                self._raise_if_pool_namespace_destroyed()
                raise PoolNotRunningException(
                    f"Cannot acquire when pool state is {state.value}"
                )
            self._ensure_pool_namespace_active()
            pool_name = self._config.pool_name
            take_result = _try_take_idle_with_min_ttl(
                self._state_store,
                pool_name,
                self._config.acquire_min_remaining_ttl,
            )
            sandbox_id = take_result.sandbox_id
            # Defer cleanup of below-threshold-but-still-alive sandboxes until after the chosen
            # candidate is connected and renewed. Doing it inline before connect would let slow
            # kill RPCs eat the candidate's remaining TTL — exactly the race this PR is fixing.
            pending_kill = take_result.discarded_alive_sandbox_ids
            no_idle_reason: str | None = None
            idle_connect_failure: Exception | None = None
            if sandbox_id is not None:
                try:
                    sandbox = self._sandbox_factory.connect(
                        sandbox_id,
                        connection_config=self._connection_for_pool_resource(),
                        health_check=self._config.acquire_health_check,
                        connect_timeout=self._config.acquire_ready_timeout,
                        health_check_polling_interval=(
                            self._config.acquire_health_check_polling_interval
                        ),
                        skip_health_check=self._config.acquire_skip_health_check,
                    )
                    if sandbox_timeout is not None:
                        sandbox.renew(sandbox_timeout)
                    self._ensure_pool_namespace_active_after_create(sandbox)
                    # Candidate is connected and (optionally) renewed. Now safe to clean up the
                    # discarded-alive sandboxes; offload to the warmup executor so the caller
                    # does not wait for N kill RPCs.
                    self._schedule_kill_discarded_alive(
                        pool_name, pending_kill, source="acquire"
                    )
                    return sandbox
                except PoolDestroyedException:
                    self._schedule_kill_discarded_alive(
                        pool_name, pending_kill, source="acquire"
                    )
                    raise
                except Exception as exc:
                    idle_connect_failure = exc
                    self._state_store.remove_idle(pool_name, sandbox_id)
                    try:
                        if self._sandbox_manager is not None:
                            self._sandbox_manager.kill_sandbox(sandbox_id)
                    except Exception:
                        pass
                    no_idle_reason = (
                        f"idle connect failed for sandbox_id={sandbox_id} "
                        "(stale or unreachable)"
                    )
            else:
                no_idle_reason = "idle buffer empty"

            # Reaching here means we did not return a sandbox from idle. Still kick off the
            # deferred cleanup so the discarded-alive sandboxes do not linger.
            self._schedule_kill_discarded_alive(
                pool_name, pending_kill, source="acquire"
            )
            reason = no_idle_reason or "idle buffer empty"
            if policy == AcquirePolicy.FAIL_FAST:
                if sandbox_id is not None:
                    raise PoolAcquireFailedException(
                        f"Cannot acquire: {reason}; policy is FAIL_FAST",
                        idle_connect_failure,
                    )
                raise PoolEmptyException(
                    f"Cannot acquire: {reason}; policy is FAIL_FAST"
                )
            return self._direct_create(sandbox_timeout)
        finally:
            self._end_operation()

    def resize(self, max_idle: int) -> None:
        if max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        self._ensure_pool_namespace_active()
        self._state_store.set_max_idle(self._config.pool_name, max_idle)
        self._current_max_idle = max_idle

    def release_all_idle(self) -> int:
        pool_name = self._config.pool_name
        count = 0
        temporary_manager: SandboxManagerSync | None = None
        try:
            while True:
                sandbox_id = self._state_store.try_take_idle(pool_name)
                if sandbox_id is None:
                    break
                count += 1
                try:
                    manager = self._sandbox_manager or temporary_manager
                    if manager is None:
                        manager = self._create_sandbox_manager()
                        temporary_manager = manager
                    manager.kill_sandbox(sandbox_id)
                except Exception as exc:
                    logger.warning(
                        f"release_all_idle: failed to kill sandbox: pool_name={pool_name} sandbox_id={sandbox_id} error={exc}"
                    )
        finally:
            if temporary_manager is not None:
                temporary_manager.close()
        return count

    def snapshot(self) -> PoolSnapshot:
        lifecycle_state = self._lifecycle_state
        if lifecycle_state in (
            PoolLifecycleState.NOT_STARTED,
            PoolLifecycleState.STOPPED,
        ):
            state = PoolState.STOPPED
        elif lifecycle_state == PoolLifecycleState.DRAINING:
            state = PoolState.DRAINING
        else:
            state = self._reconcile_state.state
        counters = self._state_store.snapshot_counters(self._config.pool_name)
        return PoolSnapshot(
            state=state,
            lifecycle_state=lifecycle_state,
            idle_count=counters.idle_count,
            max_idle=self._resolve_max_idle(),
            failure_count=self._reconcile_state.failure_count,
            backoff_active=self._reconcile_state.is_backoff_active(),
            last_error=self._reconcile_state.last_error,
            in_flight_operations=self._in_flight,
        )

    def snapshot_idle_entries(self) -> list[IdleEntry]:
        return self._state_store.snapshot_idle_entries(self._config.pool_name)

    def shutdown(self, graceful: bool = True) -> None:
        with self._lifecycle_lock:
            if self._lifecycle_state == PoolLifecycleState.STOPPED:
                return
            if not graceful:
                self._stop_reconcile(wait_for_warmup=False)
                self._lifecycle_state = PoolLifecycleState.STOPPED
                self._close_provider()
                return
            self._lifecycle_state = PoolLifecycleState.DRAINING
            self._stop_reconcile(wait_for_warmup=False)
        drained = self._await_in_flight_drain(self._config.drain_timeout)
        if not drained:
            logger.warning(
                f"Pool graceful shutdown timed out waiting in-flight operations: pool_name={self._config.pool_name} in_flight={self._in_flight} timeout_ms={int(self._config.drain_timeout.total_seconds() * 1000)}"
            )
        with self._lifecycle_lock:
            self._lifecycle_state = PoolLifecycleState.STOPPED
            self._close_provider()

    def _run_scheduler(self, stop_event: threading.Event) -> None:
        initial_delay = (
            0
            if self._config.max_idle > 0
            else self._config.reconcile_interval.total_seconds()
        )
        if initial_delay > 0 and stop_event.wait(initial_delay):
            return
        while not stop_event.is_set():
            self._run_reconcile_tick()
            if stop_event.wait(self._config.reconcile_interval.total_seconds()):
                break

    def _run_reconcile_tick(self) -> None:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            return
        executor = self._warmup_executor
        if executor is None:
            return
        with self._reconcile_lock:
            self._run_reconcile_tick_locked(executor)

    def _run_reconcile_tick_locked(self, executor: ThreadPoolExecutor) -> None:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            return
        self._begin_operation()
        try:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                return
            if self._state_store.get_destroy_state(self._config.pool_name) != PoolDestroyState.ACTIVE:
                self._stop_after_pool_namespace_destroyed()
                return
            run_reconcile_tick(
                config=self._config.with_max_idle(self._resolve_max_idle()),
                state_store=self._state_store,
                create_one=self._create_one_sandbox,
                on_discard_sandbox=self._discard_sandbox_callback,
                reconcile_state=self._reconcile_state,
                warmup_executor=executor,
            )
        except Exception as exc:
            logger.error(
                f"Pool reconcile tick failed unexpectedly: pool_name={self._config.pool_name}",
                exc_info=exc,
            )
        finally:
            self._end_operation()

    def _create_one_sandbox(self) -> str | None:
        self._begin_operation()
        try:
            self._ensure_pool_namespace_active()
            sandbox = self._build_warmup_sandbox()
            try:
                if self._config.warmup_sandbox_preparer is not None:
                    self._config.warmup_sandbox_preparer(sandbox)
                if self._lifecycle_state != PoolLifecycleState.RUNNING:
                    try:
                        sandbox.kill()
                    except Exception:
                        pass
                    return None
                # The server-side TTL has been ticking since sandbox creation;
                # readiness wait and `warmup_sandbox_preparer` can both consume meaningful time.
                # Renew right before handing the id back to the reconciler so the store's
                # stamped expiry actually matches what the server will honor — otherwise
                # `acquire_min_remaining_ttl` overestimates remaining TTL by the warmup duration.
                sandbox.renew(self._config.idle_timeout)
                self._ensure_pool_namespace_active_after_create(sandbox)
                return sandbox.id
            except Exception:
                try:
                    sandbox.kill()
                except Exception:
                    pass
                raise
            finally:
                sandbox.close()
        finally:
            self._end_operation()

    def _build_warmup_sandbox(self) -> SandboxSync:
        if self._config.sandbox_creator is not None:
            return self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.WARMUP,
                ready_timeout=self._config.warmup_ready_timeout,
                health_check_polling_interval=self._config.warmup_health_check_polling_interval,
                skip_health_check=self._config.warmup_skip_health_check,
                health_check=self._config.warmup_health_check,
            )

        spec = self._creation_spec
        return self._sandbox_factory.create(
            spec.image,
            timeout=self._config.idle_timeout,
            ready_timeout=self._config.warmup_ready_timeout,
            env=spec.env,
            metadata=spec.metadata,
            resource=spec.resource,
            network_policy=spec.network_policy,
            platform=spec.platform,
            extensions=spec.extensions,
            secure_access=spec.secure_access,
            entrypoint=spec.entrypoint,
            volumes=spec.volumes,
            connection_config=self._connection_for_pool_resource(),
            health_check=self._config.warmup_health_check,
            health_check_polling_interval=self._config.warmup_health_check_polling_interval,
            skip_health_check=self._config.warmup_skip_health_check,
        )

    def _direct_create(self, sandbox_timeout: timedelta | None) -> SandboxSync:
        self._ensure_pool_namespace_active()
        if self._config.sandbox_creator is not None:
            sandbox = self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.DIRECT_CREATE,
                ready_timeout=self._config.acquire_ready_timeout,
                health_check_polling_interval=self._config.acquire_health_check_polling_interval,
                skip_health_check=self._config.acquire_skip_health_check,
                health_check=self._config.acquire_health_check,
            )
            if sandbox_timeout is not None:
                try:
                    sandbox.renew(sandbox_timeout)
                except BaseException:
                    try:
                        sandbox.kill()
                    finally:
                        sandbox.close()
                    raise
            self._ensure_pool_namespace_active_after_create(sandbox)
            return sandbox

        spec = self._creation_spec
        sandbox = self._sandbox_factory.create(
            spec.image,
            timeout=self._config.idle_timeout,
            ready_timeout=self._config.acquire_ready_timeout,
            env=spec.env,
            metadata=spec.metadata,
            resource=spec.resource,
            network_policy=spec.network_policy,
            platform=spec.platform,
            extensions=spec.extensions,
            secure_access=spec.secure_access,
            entrypoint=spec.entrypoint,
            volumes=spec.volumes,
            connection_config=self._connection_for_pool_resource(),
            health_check=self._config.acquire_health_check,
            health_check_polling_interval=self._config.acquire_health_check_polling_interval,
            skip_health_check=self._config.acquire_skip_health_check,
        )
        if sandbox_timeout is not None:
            try:
                sandbox.renew(sandbox_timeout)
            except BaseException:
                try:
                    sandbox.kill()
                finally:
                    sandbox.close()
                raise
        self._ensure_pool_namespace_active_after_create(sandbox)
        return sandbox

    def _ensure_pool_namespace_active(self) -> None:
        state = self._state_store.get_destroy_state(self._config.pool_name)
        if state != PoolDestroyState.ACTIVE:
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={self._config.pool_name}"
            )

    def _raise_if_pool_namespace_destroyed(self) -> None:
        try:
            self._ensure_pool_namespace_active()
        except PoolDestroyedException:
            raise
        except Exception:
            return

    def _ensure_pool_namespace_active_after_create(self, sandbox: SandboxSync) -> None:
        try:
            self._ensure_pool_namespace_active()
        except BaseException:
            try:
                sandbox.kill()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after fence failed: pool_name=%s "
                    "sandbox_id=%s operation=kill error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            try:
                sandbox.close()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after fence failed: pool_name=%s "
                    "sandbox_id=%s operation=close error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            raise

    def _stop_after_pool_namespace_destroyed(self) -> None:
        with self._lifecycle_lock:
            if self._lifecycle_state == PoolLifecycleState.STOPPED:
                return
            self._stop_reconcile(wait_for_warmup=False, join_scheduler=False)
            self._lifecycle_state = PoolLifecycleState.STOPPED
            self._close_provider()

    def _build_sandbox_from_creator(
        self,
        *,
        creator: PooledSandboxCreator,
        reason: PooledSandboxCreateReason,
        ready_timeout: timedelta,
        health_check_polling_interval: timedelta,
        skip_health_check: bool,
        health_check: Callable[[SandboxSync], bool] | None,
    ) -> SandboxSync:
        context = PooledSandboxCreateContext(
            pool_name=self._config.pool_name,
            owner_id=str(self._config.owner_id),
            idle_timeout=self._config.idle_timeout,
            reason=reason,
            ready_timeout=ready_timeout,
            health_check_polling_interval=health_check_polling_interval,
            skip_health_check=skip_health_check,
            health_check=health_check,
            connection_config=self._connection_for_pool_resource(),
        )
        return creator(context)

    def _resolve_max_idle(self) -> int:
        shared = self._state_store.get_max_idle(self._config.pool_name)
        return self._current_max_idle if shared is None else shared

    def _create_sandbox_manager(self) -> SandboxManagerSync:
        return self._sandbox_manager_factory(self._connection_for_pool_resource())

    def _connection_for_pool_resource(self) -> ConnectionConfigSync:
        if (
            self._connection_config.transport is not None
            and not self._connection_config._owns_transport
        ):
            return self._connection_config
        config = self._connection_config.model_copy(update={"transport": None})
        config._owns_transport = True
        return config

    def _discard_sandbox_callback(self, sandbox_id: str) -> None:
        """``Callable[[str], None]`` adapter for the reconciler's ``on_discard_sandbox``
        hook. The reconciler does not care whether the kill succeeded — it only needs the
        sandbox to be removed from the pool's bookkeeping — so we drop the bool return
        value here.
        """
        self._kill_sandbox_best_effort(sandbox_id)

    def _kill_sandbox_best_effort(self, sandbox_id: str) -> bool:
        """Best-effort kill a sandbox via the pool's manager.

        Returns ``True`` on a confirmed kill, ``False`` if no manager is available or the
        kill raised. Failures are logged at WARNING and swallowed so the caller's primary
        outcome is unaffected.
        """
        if self._sandbox_manager is None:
            return False
        try:
            self._sandbox_manager.kill_sandbox(sandbox_id)
            return True
        except Exception as exc:
            logger.warning(
                f"Pool sandbox cleanup failed: pool_name={self._config.pool_name} sandbox_id={sandbox_id} error={exc}"
            )
            return False

    def _schedule_kill_discarded_alive(
        self,
        pool_name: str,
        sandbox_ids: tuple[str, ...],
        source: str,
    ) -> None:
        """Offload :meth:`_kill_discarded_alive` to the warmup executor so the caller does not
        block on the kill RPCs. Falls back to inline execution when no executor is available
        (e.g. mid-shutdown) — better to slow the caller than to drop the cleanup entirely.
        """
        if not sandbox_ids:
            return
        executor = self._warmup_executor
        if executor is None:
            self._kill_discarded_alive(pool_name, sandbox_ids, source)
            return
        try:
            executor.submit(self._kill_discarded_alive, pool_name, sandbox_ids, source)
        except Exception as exc:
            logger.debug(
                f"Discarded-alive kill submit rejected, running inline: pool_name={pool_name} count={len(sandbox_ids)} error={exc}"
            )
            self._kill_discarded_alive(pool_name, sandbox_ids, source)

    def _kill_discarded_alive(
        self,
        pool_name: str,
        sandbox_ids: tuple[str, ...],
        source: str,
    ) -> None:
        """Best-effort terminate sandboxes the store dropped because their remaining TTL
        fell below ``acquire_min_remaining_ttl``. Without this, alive-but-near-expiry
        sandboxes would linger past their pool membership until server-side TTL elapses.
        """
        if not sandbox_ids:
            return
        for sandbox_id in sandbox_ids:
            if self._kill_sandbox_best_effort(sandbox_id):
                logger.debug(
                    f"Killed near-expiry idle sandbox: pool_name={pool_name} sandbox_id={sandbox_id} source={source}"
                )

    def _begin_operation(self) -> None:
        with self._in_flight_condition:
            self._in_flight += 1

    def _end_operation(self) -> None:
        with self._in_flight_condition:
            self._in_flight -= 1
            if self._in_flight <= 0:
                self._in_flight = 0
                self._in_flight_condition.notify_all()

    def _await_in_flight_drain(self, timeout: timedelta) -> bool:
        deadline = time.monotonic() + timeout.total_seconds()
        with self._in_flight_condition:
            while self._in_flight > 0:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    return False
                self._in_flight_condition.wait(remaining)
            return True

    def _stop_reconcile(
        self,
        *,
        wait_for_warmup: bool,
        join_scheduler: bool = True,
    ) -> None:
        self._stop_event.set()
        thread = self._scheduler_thread
        if (
            join_scheduler
            and thread is not None
            and thread is not threading.current_thread()
        ):
            thread.join(timeout=5)
        if join_scheduler:
            self._scheduler_thread = None
        executor = self._warmup_executor
        if executor is not None:
            executor.shutdown(wait=False, cancel_futures=True)
            if wait_for_warmup:
                executor.shutdown(wait=True)
            else:
                self._await_executor_threads(
                    executor, _WARMUP_TERMINATION_TIMEOUT_SECONDS
                )
        self._warmup_executor = None
        self._release_primary_lock_best_effort()

    def _await_executor_threads(
        self, executor: ThreadPoolExecutor, timeout_seconds: float
    ) -> None:
        deadline = time.monotonic() + timeout_seconds
        threads = list(getattr(executor, "_threads", ()))
        for thread in threads:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return
            thread.join(timeout=remaining)

    def _release_primary_lock_best_effort(self) -> None:
        try:
            self._state_store.release_primary_lock(
                self._config.pool_name, str(self._config.owner_id)
            )
        except Exception as exc:
            logger.warning(
                f"Pool primary lock release failed: pool_name={self._config.pool_name} owner_id={self._config.owner_id} error={exc}"
            )

    def _close_provider(self) -> None:
        if self._sandbox_manager is not None:
            self._sandbox_manager.close()
            self._sandbox_manager = None

    def _warn_if_primary_lock_ttl_may_expire_during_warmup(self) -> None:
        if self._config.primary_lock_ttl > self._config.warmup_ready_timeout:
            return
        logger.warning(
            f"Pool primary lock TTL may expire during warmup: pool_name={self._config.pool_name} primary_lock_ttl_ms={int(self._config.primary_lock_ttl.total_seconds() * 1000)} warmup_ready_timeout_ms={int(self._config.warmup_ready_timeout.total_seconds() * 1000)}"
        )
