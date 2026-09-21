#
# Copyright 2025 The OpenSandbox Authors
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

import asyncio
import logging
import math
import threading
import time
from collections.abc import Callable
from concurrent.futures import CancelledError, Future, ThreadPoolExecutor
from datetime import timedelta
from typing import TypeVar

from opensandbox._pool_reconciler import ReconcileState, run_reconcile_tick
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolDestroyedException,
    PoolEmptyException,
    PoolNotRunningException,
    PoolStateStoreUnavailableException,
    SandboxReadyTimeoutException,
)
from opensandbox.internal.pool_tracing import (
    WARMUP_COMMIT_SPAN,
    WARMUP_CREATE_SPAN,
    WARMUP_POST_PREPARE_CHECK_SPAN,
    WARMUP_PREPARE_SPAN,
    WARMUP_READINESS_CHECK_SPAN,
    WARMUP_RENEW_SPAN,
    PoolTracer,
    annotate_health_span,
)
from opensandbox.internal.readiness import is_readiness_auth_error
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
    effective_max_idle_attempts,
    policy_falls_through_to_direct_create,
)
from opensandbox.pool_types import (
    try_take_idle_with_min_ttl as _try_take_idle_with_min_ttl,
)
from opensandbox.sync.manager import SandboxManagerSync
from opensandbox.sync.sandbox import SandboxSync
from opensandbox.transport import RetryPolicy
from opensandbox.transport._sync_retry import RetrySyncTransport

logger = logging.getLogger(__name__)

_WARMUP_TERMINATION_TIMEOUT_SECONDS = 5.0
_RELEASE_ALL_IDLE_CONCURRENCY = 50
_RECONCILE_INTERVAL_SECONDS = 1.0
_CREATE_EXECUTOR_HEADROOM = 1.5
_T = TypeVar("_T")


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
        warmup_create_qps: int = 10,
        warmup_concurrency: int = 128,
        primary_lock_ttl: timedelta = timedelta(seconds=60),
        degraded_threshold: int = 3,
        acquire_ready_timeout: timedelta = timedelta(seconds=30),
        acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        acquire_health_check: Callable[[SandboxSync], bool] | None = None,
        acquire_skip_health_check: bool = False,
        warmup_ready_timeout: timedelta = timedelta(seconds=30),
        warmup_health_check_initial_delay: timedelta = timedelta(0),
        warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=500),
        warmup_health_check: Callable[[SandboxSync], bool] | None = None,
        warmup_sandbox_preparer: Callable[[SandboxSync], None] | None = None,
        warmup_post_prepare_health_check: Callable[[SandboxSync], bool] | None = None,
        warmup_post_prepare_health_check_timeout: timedelta = timedelta(seconds=30),
        warmup_skip_health_check: bool = False,
        idle_timeout: timedelta = timedelta(hours=24),
        drain_timeout: timedelta = timedelta(seconds=30),
        acquire_min_remaining_ttl: timedelta | None = None,
        max_acquire_retries: int = 3,
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
            warmup_create_qps=warmup_create_qps,
            warmup_concurrency=warmup_concurrency,
            primary_lock_ttl=primary_lock_ttl,
            state_store=state_store,  # type: ignore[arg-type]
            connection_config=connection_config,
            creation_spec=creation_spec,
            degraded_threshold=degraded_threshold,
            acquire_ready_timeout=acquire_ready_timeout,
            acquire_health_check_polling_interval=acquire_health_check_polling_interval,
            acquire_health_check=acquire_health_check,
            acquire_skip_health_check=acquire_skip_health_check,
            warmup_ready_timeout=warmup_ready_timeout,
            warmup_health_check_initial_delay=warmup_health_check_initial_delay,
            warmup_health_check_polling_interval=warmup_health_check_polling_interval,
            warmup_health_check=warmup_health_check,
            warmup_sandbox_preparer=warmup_sandbox_preparer,
            warmup_post_prepare_health_check=warmup_post_prepare_health_check,
            warmup_post_prepare_health_check_timeout=warmup_post_prepare_health_check_timeout,
            warmup_skip_health_check=warmup_skip_health_check,
            idle_timeout=idle_timeout,
            drain_timeout=drain_timeout,
            acquire_min_remaining_ttl=acquire_min_remaining_ttl,
            sandbox_creator=sandbox_creator,
            max_acquire_retries=max_acquire_retries,
        )
        self._state_store = self._config.state_store
        self._connection_config = connection_config
        self._creation_spec = creation_spec
        self._sandbox_manager_factory = sandbox_manager_factory
        self._sandbox_factory = sandbox_factory
        self._pool_tracer = PoolTracer(connection_config.enable_tracing)
        self._reconcile_state = ReconcileState(degraded_threshold)
        self._current_max_idle = max_idle
        self._lifecycle_state = PoolLifecycleState.NOT_STARTED
        self._lifecycle_lock = threading.RLock()
        self._reconcile_lock = threading.Lock()
        self._in_flight = 0
        self._in_flight_condition = threading.Condition()
        self._stop_event = threading.Event()
        self._scheduler_thread: threading.Thread | None = None
        self._heartbeat_thread: threading.Thread | None = None
        self._warmup_executor: ThreadPoolExecutor | None = None
        self._create_executor: ThreadPoolExecutor | None = None
        self._warmup_loop: asyncio.AbstractEventLoop | None = None
        self._warmup_loop_thread: threading.Thread | None = None
        self._warmup_futures: set[Future[None]] = set()
        self._next_warmup_token = 0
        self._warmup_tokens: set[int] = set()
        self._started_warmup_tokens: set[int] = set()
        self._sandbox_manager: SandboxManagerSync | None = None
        self._pool_connection_config: ConnectionConfigSync | None = None
        self._pool_transport_owner: ConnectionConfigSync | None = None
        self._run_generation = 0
        self._leader_epoch = 0
        self._primary_owned = False
        self._accept_warmup_commits = False
        self._warming_count = 0
        self._warming_lock = threading.Lock()

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
                self._open_pool_transport()
                self._sandbox_manager = self._create_sandbox_manager()
                self._state_store.set_idle_entry_ttl(
                    self._config.pool_name, self._config.idle_timeout
                )
                self._state_store.set_max_idle(
                    self._config.pool_name, self._config.max_idle
                )
                self._run_generation += 1
                self._primary_owned = False
                self._accept_warmup_commits = True
                self._create_executor = ThreadPoolExecutor(
                    max_workers=max(
                        1,
                        math.ceil(
                            self._config.warmup_create_qps * _CREATE_EXECUTOR_HEADROOM
                        ),
                    ),
                    thread_name_prefix=f"sandbox-pool-create-{self._config.pool_name}",
                )
                self._warmup_executor = ThreadPoolExecutor(
                    max_workers=self._config.warmup_concurrency,
                    thread_name_prefix=f"sandbox-pool-warmup-{self._config.pool_name}",
                )
                warmup_loop = asyncio.new_event_loop()
                self._warmup_loop = warmup_loop
                self._warmup_loop_thread = threading.Thread(
                    target=self._run_warmup_loop,
                    args=(warmup_loop,),
                    name=f"sandbox-pool-warmup-dispatch-{self._config.pool_name}",
                    daemon=True,
                )
                self._warmup_loop_thread.start()
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
                self._heartbeat_thread = threading.Thread(
                    target=self._run_heartbeat,
                    args=(stop_event, self._run_generation),
                    name=f"sandbox-pool-heartbeat-{self._config.pool_name}",
                    daemon=True,
                )
                self._heartbeat_thread.start()
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
        with self._lifecycle_lock:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                state = self._lifecycle_state
                self._raise_if_pool_namespace_destroyed()
                raise PoolNotRunningException(
                    f"Cannot acquire when pool state is {state.value}"
                )
            operation_generation = self._run_generation
        self._begin_operation()
        try:
            self._ensure_acquire_run_active(operation_generation)
            self._ensure_pool_namespace_active_for_acquire(policy)
            pool_name = self._config.pool_name
            max_attempts = effective_max_idle_attempts(
                policy, self._config.max_acquire_retries
            )

            # Accumulate discarded-alive sandbox ids across all take iterations so we schedule
            # a single deferred cleanup, instead of one kill batch per retry.
            pending_kill: list[str] = []
            last_sandbox_id: str | None = None
            last_idle_connect_failure: Exception | None = None
            attempted_any = False
            loop_exhausted = True
            attempt = 0
            while attempt < max_attempts:
                self._ensure_acquire_run_active(operation_generation)
                attempt += 1
                try:
                    take_result = _try_take_idle_with_min_ttl(
                        self._state_store,
                        pool_name,
                        self._config.acquire_min_remaining_ttl,
                    )
                except PoolStateStoreUnavailableException:
                    # State store outage. Per OSEP-0005, under policies that fall through to
                    # direct-create on empty idle we degrade to that fallback so the pool stays
                    # at least as available as raw SDK usage during store outages. Under
                    # non-fallthrough policies (FAIL_FAST / RETRY_NEXT_IDLE) we surface the
                    # outage as-is so callers can react.
                    if not policy_falls_through_to_direct_create(policy):
                        self._schedule_kill_discarded_alive(
                            pool_name, tuple(pending_kill), source="acquire"
                        )
                        raise
                    logger.warning(
                        "acquire: state store unavailable, falling through to direct create "
                        "per policy=%s",
                        policy.value,
                    )
                    loop_exhausted = False
                    break
                if take_result.discarded_alive_sandbox_ids:
                    pending_kill.extend(take_result.discarded_alive_sandbox_ids)
                sandbox_id = take_result.sandbox_id
                if sandbox_id is None:
                    # Idle buffer drained mid-loop (or was empty from the start). Stop retrying —
                    # another take round-trip is pure overhead.
                    loop_exhausted = False
                    break
                last_sandbox_id = sandbox_id
                attempted_any = True
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
                except AssertionError:
                    self._schedule_kill_discarded_alive(
                        pool_name, (*pending_kill, sandbox_id), source="acquire"
                    )
                    raise
                except PoolDestroyedException:
                    self._schedule_kill_discarded_alive(
                        pool_name, tuple(pending_kill), source="acquire"
                    )
                    raise
                except Exception as exc:
                    # Auth/permission verdicts (401/403) cannot be fixed by
                    # re-probing, so surface the original error instead of burning
                    # retries. The taken candidate still needs a disposition, though:
                    # try_take already removed it from the store, and leaving it alive
                    # would leak it untracked (the renew-failure path below documents
                    # the same trap). Kill this one candidate together with the
                    # already-condemned ones; warmup replaces the slot. A per-sandbox
                    # token failure (direct-mode X-EXECD-ACCESS-TOKEN after execd
                    # restart / snapshot resume) is also cleaned up this way.
                    if is_readiness_auth_error(exc):
                        self._schedule_kill_discarded_alive(
                            pool_name, (*pending_kill, sandbox_id), source="acquire"
                        )
                        raise
                    # Connect / readiness / health-check failure — the idle candidate itself
                    # is unusable. Remove it, fire-and-forget the remote kill on the warmup
                    # executor so a slow DELETE (up to the lifecycle client's request_timeout,
                    # 30s by default) does not block the next retry iteration.
                    last_idle_connect_failure = exc
                    self._state_store.remove_idle(pool_name, sandbox_id)
                    self._schedule_kill_discarded_alive(
                        pool_name, (sandbox_id,), source="acquire-stale"
                    )
                    try:
                        self._ensure_acquire_run_active(operation_generation)
                    except PoolNotRunningException as retired:
                        self._schedule_kill_discarded_alive(
                            pool_name, tuple(pending_kill), source="acquire"
                        )
                        raise retired from exc
                    self._ensure_pool_namespace_active()
                    continue
                # Connect + readiness succeeded. From here on the sandbox is a healthy,
                # borrowable idle: any failure below (renew rejection, namespace fenced) is
                # NOT a candidate-specific problem, so we must not treat it as "stale idle"
                # and burn another retry. Dispose the sandbox and surface the error.
                try:
                    if sandbox_timeout is not None:
                        sandbox.renew(sandbox_timeout)
                    self._ensure_pool_namespace_active_after_create(sandbox)
                    self._ensure_acquire_run_active(operation_generation)
                except PoolDestroyedException:
                    self._schedule_kill_discarded_alive(
                        pool_name, tuple(pending_kill), source="acquire"
                    )
                    raise
                except Exception as exc:
                    # Renew failed against a healthy sandbox. try_take_idle already popped this
                    # id out of the store; a bare close() would only release local HTTP resources
                    # and leave the remote sandbox alive-but-untracked until its server-side TTL
                    # expires. Kill it best-effort, then close local resources and re-raise.
                    logger.warning(
                        "Acquire renew failed after idle connect; killing remote sandbox and "
                        "not retrying (renew errors are not candidate-specific): "
                        "pool_name=%s sandbox_id=%s policy=%s error=%s",
                        pool_name,
                        sandbox_id,
                        policy.value,
                        exc,
                    )
                    try:
                        sandbox.kill()
                    except Exception as kill_exc:
                        logger.warning(
                            "Best-effort kill after renew failure failed: "
                            "pool_name=%s sandbox_id=%s error=%s",
                            pool_name,
                            sandbox_id,
                            kill_exc,
                        )
                    try:
                        sandbox.close()
                    except Exception as close_exc:
                        # Best-effort local resource release; original renew error must be the
                        # one that surfaces, so log at debug and continue with the raise below.
                        logger.debug(
                            "Best-effort close after renew failure failed: "
                            "pool_name=%s sandbox_id=%s error=%s",
                            pool_name,
                            sandbox_id,
                            close_exc,
                        )
                    self._schedule_kill_discarded_alive(
                        pool_name, tuple(pending_kill), source="acquire"
                    )
                    raise
                self._schedule_kill_discarded_alive(
                    pool_name, tuple(pending_kill), source="acquire"
                )
                return sandbox

            # Reached end of loop without a successful acquire. Fire deferred cleanup so the
            # discarded-alive sandboxes do not linger; both the raise and the direct-create
            # fallthrough benefit from async cleanup instead of a synchronous kill.
            self._schedule_kill_discarded_alive(
                pool_name, tuple(pending_kill), source="acquire"
            )

            if not attempted_any:
                reason = "idle buffer empty"
            elif loop_exhausted:
                reason = (
                    f"idle connect failed for {max_attempts} candidate(s); "
                    f"last sandbox_id={last_sandbox_id} (stale or unreachable)"
                )
            else:
                reason = (
                    f"idle connect failed for sandbox_id={last_sandbox_id}; "
                    f"idle buffer drained before reaching max_acquire_retries={max_attempts}"
                )
            if not policy_falls_through_to_direct_create(policy):
                if attempted_any:
                    raise PoolAcquireFailedException(
                        f"Cannot acquire: {reason}; policy is {policy.value}",
                        last_idle_connect_failure,
                    )
                raise PoolEmptyException(
                    f"Cannot acquire: {reason}; policy is {policy.value}"
                )
            self._ensure_acquire_run_active(operation_generation)
            try:
                sandbox = self._direct_create(sandbox_timeout, policy=policy)
            except (AssertionError, PoolDestroyedException):
                raise
            except Exception as exc:
                try:
                    self._ensure_acquire_run_active(operation_generation)
                except PoolNotRunningException as retired:
                    raise retired from exc
                raise
            try:
                self._ensure_acquire_run_active(operation_generation)
            except PoolNotRunningException:
                self._cleanup_uncommitted_warmup(sandbox)
                raise
            return sandbox
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

    def release_all_idle_parallel(
        self, max_workers: int = _RELEASE_ALL_IDLE_CONCURRENCY
    ) -> int:
        if max_workers <= 0:
            raise ValueError("max_workers must be positive")
        pool_name = self._config.pool_name
        sandbox_ids: list[str] = []
        drain_error: Exception | None = None
        temporary_manager: SandboxManagerSync | None = None
        try:
            while True:
                try:
                    sandbox_id = self._state_store.try_take_idle(pool_name)
                except Exception as exc:
                    drain_error = exc
                    break
                if sandbox_id is None:
                    break
                sandbox_ids.append(sandbox_id)

            if sandbox_ids:
                manager = self._sandbox_manager
                if manager is None:
                    try:
                        manager = self._create_sandbox_manager()
                        temporary_manager = manager
                    except Exception as exc:
                        logger.warning(
                            f"release_all_idle_parallel: failed to create sandbox manager; draining idle ids without remote kill: pool_name={pool_name} error={exc}"
                        )

                def kill(sandbox_id: str) -> None:
                    if manager is None:
                        return
                    try:
                        manager.kill_sandbox(sandbox_id)
                    except Exception as exc:
                        logger.warning(
                            f"release_all_idle_parallel: failed to kill sandbox: pool_name={pool_name} sandbox_id={sandbox_id} error={exc}"
                        )

                with ThreadPoolExecutor(
                    max_workers=min(max_workers, len(sandbox_ids)),
                    thread_name_prefix="sandbox-pool-release",
                ) as executor:
                    list(executor.map(kill, sandbox_ids))
        finally:
            if temporary_manager is not None:
                temporary_manager.close()
        if drain_error is not None:
            raise drain_error
        return len(sandbox_ids)

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
            if self._lifecycle_state in (
                PoolLifecycleState.NOT_STARTED,
                PoolLifecycleState.STOPPED,
            ):
                self._lifecycle_state = PoolLifecycleState.STOPPED
                return
            if not graceful:
                self._accept_warmup_commits = False
                self._lifecycle_state = PoolLifecycleState.DRAINING
            else:
                self._lifecycle_state = PoolLifecycleState.DRAINING
        if not graceful:
            self._stop_reconcile(wait_for_warmup=False)
            with self._lifecycle_lock:
                self._lifecycle_state = PoolLifecycleState.STOPPED
                self._close_provider()
            return
        drained = self._await_in_flight_drain(self._config.drain_timeout)
        if not drained:
            logger.warning(
                f"Pool graceful shutdown timed out waiting in-flight operations: pool_name={self._config.pool_name} in_flight={self._in_flight} timeout_ms={int(self._config.drain_timeout.total_seconds() * 1000)}"
            )
        with self._lifecycle_lock:
            self._accept_warmup_commits = False
        self._stop_reconcile(wait_for_warmup=False)
        with self._lifecycle_lock:
            self._lifecycle_state = PoolLifecycleState.STOPPED
            self._close_provider()

    def _run_scheduler(self, stop_event: threading.Event) -> None:
        initial_delay = 0 if self._config.max_idle > 0 else _RECONCILE_INTERVAL_SECONDS
        if initial_delay > 0 and stop_event.wait(initial_delay):
            return
        while not stop_event.is_set():
            self._run_reconcile_tick()
            if stop_event.wait(_RECONCILE_INTERVAL_SECONDS):
                break

    def _run_reconcile_tick(self) -> None:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            return
        with self._reconcile_lock:
            self._run_reconcile_tick_locked()

    def _run_reconcile_tick_locked(self) -> None:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            return
        self._begin_operation()
        try:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                return
            if (
                self._state_store.get_destroy_state(self._config.pool_name)
                != PoolDestroyState.ACTIVE
            ):
                self._stop_after_pool_namespace_destroyed()
                return
            with self._warming_lock:
                warming_count = self._warming_count
            primary_owned = run_reconcile_tick(
                config=self._config.with_max_idle(self._resolve_max_idle()),
                state_store=self._state_store,
                on_discard_sandbox=self._discard_sandbox_callback,
                warming_count=warming_count,
                submit_warmups=self._submit_warmups,
                on_primary_acquired=self._mark_primary_acquired,
            )
            if not primary_owned:
                self._mark_primary_lost()
        except Exception as exc:
            self._mark_primary_lost()
            logger.error(
                f"Pool reconcile tick failed unexpectedly: pool_name={self._config.pool_name}",
                exc_info=exc,
            )
        finally:
            self._end_operation()

    def _submit_warmups(self, count: int) -> None:
        loop = self._warmup_loop
        if loop is None:
            return
        generation = self._run_generation
        leader_epoch = self._leader_epoch
        for _ in range(count):
            self._begin_operation()
            with self._warming_lock:
                self._warming_count += 1
                self._next_warmup_token += 1
                token = self._next_warmup_token
                self._warmup_tokens.add(token)
            coroutine = self._run_warmup_async(
                generation, leader_epoch, token, time.time_ns()
            )
            try:
                future = asyncio.run_coroutine_threadsafe(coroutine, loop)
                self._warmup_futures.add(future)
                future.add_done_callback(
                    lambda done, current=token: self._warmup_done(done, current)
                )
            except BaseException as exc:
                # ``run_coroutine_threadsafe`` does not take ownership when the
                # dispatch loop has already stopped. Close the unsubmitted
                # coroutine explicitly so shutdown races do not leak it.
                coroutine.close()
                self._complete_warmup(token, exc)

    async def _run_warmup_async(
        self,
        generation: int,
        leader_epoch: int,
        token: int,
        submitted_ns: int,
    ) -> None:
        with self._warming_lock:
            self._started_warmup_tokens.add(token)
        create_executor = self._create_executor
        if create_executor is None:
            self._complete_warmup(
                token, RuntimeError("warmup create executor is stopped")
            )
            return
        sandbox: SandboxSync | None = None
        committed = False
        stage = "create"
        warmup_trace = self._pool_tracer.start_warmup(
            pool_name=self._config.pool_name,
            owner_id=str(self._config.owner_id),
            run_generation=generation,
            leader_epoch=leader_epoch,
            submitted_ns=submitted_ns,
            image=str(self._creation_spec.image),
        )
        try:
            loop = asyncio.get_running_loop()
            with warmup_trace.phase(WARMUP_CREATE_SPAN):
                # Keep the underlying create future independently observable. A
                # forced shutdown can retire the warmup loop before a slow control-
                # plane create returns; in that case the late sandbox still needs
                # to be terminated after this coroutine is gone.
                create_future = create_executor.submit(self._build_warmup_sandbox)
                try:
                    created_sandbox = await asyncio.shield(
                        asyncio.wrap_future(create_future, loop=loop)
                    )
                except asyncio.CancelledError:
                    create_future.add_done_callback(
                        self._cleanup_cancelled_warmup_create
                    )
                    raise
                sandbox = created_sandbox
            warmup_trace.set_sandbox_id(sandbox.id)
            await self._run_stage(self._ensure_pool_namespace_active)
            readiness_deadline = (
                loop.time() + self._config.warmup_ready_timeout.total_seconds()
            )
            if (
                not self._config.warmup_skip_health_check
                and self._config.warmup_health_check_initial_delay.total_seconds() > 0
            ):
                await asyncio.sleep(
                    min(
                        self._config.warmup_health_check_initial_delay.total_seconds(),
                        self._config.warmup_ready_timeout.total_seconds(),
                    )
                )
            if not self._config.warmup_skip_health_check:
                stage = "readiness"
                with warmup_trace.phase(WARMUP_READINESS_CHECK_SPAN) as health_span:
                    await self._wait_until_healthy_async(
                        sandbox,
                        self._config.warmup_health_check,
                        readiness_deadline,
                        "warmup readiness",
                        health_span,
                    )
            preparer = self._config.warmup_sandbox_preparer
            if preparer is not None:
                stage = "prepare"
                with warmup_trace.phase(WARMUP_PREPARE_SPAN):
                    await self._run_stage(lambda: preparer(sandbox))
            post_check = self._config.warmup_post_prepare_health_check
            if post_check is not None:
                stage = "post_prepare_readiness"
                with warmup_trace.phase(WARMUP_POST_PREPARE_CHECK_SPAN) as health_span:
                    await self._wait_until_healthy_async(
                        sandbox,
                        post_check,
                        loop.time()
                        + self._config.warmup_post_prepare_health_check_timeout.total_seconds(),
                        "post-prepare readiness",
                        health_span,
                    )
            stage = "renew"
            with warmup_trace.phase(WARMUP_RENEW_SPAN):
                await self._run_stage(lambda: sandbox.renew(self._config.idle_timeout))
            await self._run_stage(
                lambda: self._ensure_pool_namespace_active_after_create(sandbox)
            )
            stage = "commit"
            with warmup_trace.phase(WARMUP_COMMIT_SPAN):
                may_commit = await self._run_stage(
                    lambda: self._can_commit_warmup(generation, leader_epoch)
                )
                if not may_commit:
                    warmup_trace.end_dropped(stage, "leadership_lost")
                    return
                await self._run_stage(
                    lambda: self._state_store.put_idle(
                        self._config.pool_name, sandbox.id
                    )
                )
                if not self._can_commit_locally(generation, leader_epoch):
                    await self._run_stage(
                        lambda: self._state_store.remove_idle(
                            self._config.pool_name, sandbox.id
                        )
                    )
                    warmup_trace.end_dropped(stage, "run_retired")
                    return
            committed = True
            self._reconcile_state.record_success()
            warmup_trace.end_success()
        except asyncio.CancelledError:
            warmup_trace.end_cancelled(stage)
            raise
        except BaseException as exc:
            self._reconcile_state.record_failure(str(exc))
            warmup_trace.end_failure(stage, exc)
            logger.warning(
                f"Pool warmup failed: pool_name={self._config.pool_name} error={exc}"
            )
        finally:
            try:
                if sandbox is not None:
                    if committed:
                        await self._run_stage(sandbox.close)
                    else:
                        await self._run_stage(
                            lambda: self._cleanup_uncommitted_warmup(sandbox)
                        )
            finally:
                self._complete_warmup(token, None)

    async def _run_stage(
        self,
        action: Callable[[], _T],
        executor: ThreadPoolExecutor | None = None,
    ) -> _T:
        target = executor or self._warmup_executor
        if target is None:
            raise RuntimeError("warmup stage executor is stopped")
        future = asyncio.get_running_loop().run_in_executor(target, action)
        try:
            return await asyncio.shield(future)
        except asyncio.CancelledError:
            try:
                await future
            finally:
                raise

    async def _wait_until_healthy_async(
        self,
        sandbox: SandboxSync,
        health_check: Callable[[SandboxSync], bool] | None,
        deadline: float,
        stage: str,
        trace_span: object = None,
    ) -> None:
        last_error: Exception | None = None
        loop = asyncio.get_running_loop()
        attempt_count = 0
        false_count = 0
        exception_count = 0
        while True:
            try:
                attempt_count += 1
                healthy = await self._run_stage(
                    lambda: (
                        health_check(sandbox)
                        if health_check is not None
                        else sandbox.is_healthy()
                    )
                )
                if healthy:
                    annotate_health_span(
                        trace_span,  # type: ignore[arg-type]
                        attempt_count=attempt_count,
                        false_count=false_count,
                        exception_count=exception_count,
                    )
                    return
                false_count += 1
                last_error = None
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                exception_count += 1
                last_error = exc
            remaining = deadline - loop.time()
            if remaining <= 0:
                annotate_health_span(
                    trace_span,  # type: ignore[arg-type]
                    attempt_count=attempt_count,
                    false_count=false_count,
                    exception_count=exception_count,
                )
                raise SandboxReadyTimeoutException(
                    f"Pool {stage} timed out", cause=last_error
                )
            await asyncio.sleep(
                min(
                    remaining,
                    self._config.warmup_health_check_polling_interval.total_seconds(),
                )
            )

    @staticmethod
    def _run_warmup_loop(loop: asyncio.AbstractEventLoop) -> None:
        asyncio.set_event_loop(loop)
        loop.run_forever()

    def _warmup_done(self, future: Future[None], token: int) -> None:
        self._warmup_futures.discard(future)
        with self._warming_lock:
            started = token in self._started_warmup_tokens
        if not started:
            error: BaseException | None = (
                CancelledError() if future.cancelled() else future.exception()
            )
            self._complete_warmup(token, error)
        if future.cancelled():
            return
        error = future.exception()
        if error is not None:
            logger.warning(
                f"Pool warmup task failed: pool_name={self._config.pool_name} error={error}"
            )

    def _complete_warmup(self, token: int, error: BaseException | None) -> None:
        with self._warming_lock:
            if token not in self._warmup_tokens:
                return
            self._warmup_tokens.remove(token)
            self._started_warmup_tokens.discard(token)
            self._warming_count = max(0, self._warming_count - 1)
        if error is not None and not isinstance(error, CancelledError):
            self._reconcile_state.record_failure(str(error))
            logger.warning(
                f"Pool warmup create failed: pool_name={self._config.pool_name} error={error}"
            )
        self._end_operation()

    def _can_commit_warmup(self, generation: int, leader_epoch: int) -> bool:
        if not self._can_commit_locally(generation, leader_epoch):
            return False
        renewed = self._state_store.renew_primary_lock(
            self._config.pool_name,
            str(self._config.owner_id),
            self._config.primary_lock_ttl,
        )
        if not renewed:
            self._mark_primary_lost()
            return False
        return self._can_commit_locally(generation, leader_epoch)

    def _can_commit_locally(self, generation: int, leader_epoch: int) -> bool:
        with self._lifecycle_lock:
            return (
                generation == self._run_generation
                and leader_epoch == self._leader_epoch
                and self._primary_owned
                and self._accept_warmup_commits
                and self._lifecycle_state
                in (PoolLifecycleState.RUNNING, PoolLifecycleState.DRAINING)
            )

    def _mark_primary_acquired(self) -> None:
        with self._lifecycle_lock:
            if self._primary_owned:
                return
            self._primary_owned = True
            self._leader_epoch += 1

    def _mark_primary_lost(self) -> None:
        futures: tuple[Future[None], ...] = ()
        with self._lifecycle_lock:
            if not self._primary_owned:
                return
            self._primary_owned = False
            self._leader_epoch += 1
            futures = tuple(self._warmup_futures)
        for future in futures:
            future.cancel()

    def _run_heartbeat(self, stop_event: threading.Event, generation: int) -> None:
        interval = max(
            0.001,
            min(1.0, self._config.primary_lock_ttl.total_seconds() / 3),
        )
        while not stop_event.wait(interval):
            with self._lifecycle_lock:
                if generation != self._run_generation:
                    return
                should_renew = self._primary_owned and self._lifecycle_state in (
                    PoolLifecycleState.RUNNING,
                    PoolLifecycleState.DRAINING,
                )
            if not should_renew:
                continue
            try:
                renewed = self._state_store.renew_primary_lock(
                    self._config.pool_name,
                    str(self._config.owner_id),
                    self._config.primary_lock_ttl,
                )
                if not renewed:
                    self._mark_primary_lost()
            except Exception as exc:
                logger.warning(
                    f"Pool primary heartbeat failed: pool_name={self._config.pool_name} error={exc}"
                )

    def _cleanup_uncommitted_warmup(self, sandbox: SandboxSync) -> None:
        try:
            sandbox.kill()
        except Exception:
            pass
        finally:
            try:
                sandbox.close()
            except Exception:
                pass

    def _cleanup_cancelled_warmup_create(
        self, future: Future[SandboxSync]
    ) -> None:
        """Terminate a sandbox whose create completed after forced shutdown.

        This callback runs on the create worker, independently of the retired
        warmup event loop. It intentionally uses a fresh manager/transport because
        the pool-owned transport may already be closed by shutdown.
        """
        try:
            sandbox = future.result()
        except BaseException:
            return

        manager: SandboxManagerSync | None = None
        try:
            base = self._connection_config.model_copy(
                update={
                    "transport": None,
                    "retry_policy": RetryPolicy.disabled(),
                }
            )
            cleanup_config = base.with_transport_if_missing(
                max_connections=1,
                max_keepalive_connections=1,
            )
            manager = self._sandbox_manager_factory(cleanup_config)
            manager.kill_sandbox(sandbox.id)
        except Exception as exc:
            logger.warning(
                f"Pool late warmup cleanup failed: pool_name={self._config.pool_name} sandbox_id={sandbox.id} error={exc}"
            )
        finally:
            if manager is not None:
                try:
                    manager.close()
                except Exception:
                    pass
            try:
                sandbox.close()
            except Exception:
                pass

    def _build_warmup_sandbox(self) -> SandboxSync:
        if self._config.sandbox_creator is not None:
            return self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.WARMUP,
                ready_timeout=self._config.warmup_ready_timeout,
                health_check_polling_interval=self._config.warmup_health_check_polling_interval,
                skip_health_check=True,
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
            connection_config=self._connection_for_warmup_create(),
            health_check=self._config.warmup_health_check,
            health_check_polling_interval=self._config.warmup_health_check_polling_interval,
            skip_health_check=True,
        )

    def _direct_create(
        self,
        sandbox_timeout: timedelta | None,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ) -> SandboxSync:
        # policy-aware namespace check: if the state store is down and the policy is a
        # fallthrough one, treat destroy-state as unknown and proceed to direct-create
        # instead of surfacing the outage. See _ensure_pool_namespace_active_for_acquire
        # for the full rationale.
        self._ensure_pool_namespace_active_for_acquire(policy)
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
            self._ensure_pool_namespace_active_after_create(sandbox, policy=policy)
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
        self._ensure_pool_namespace_active_after_create(sandbox, policy=policy)
        return sandbox

    def _ensure_pool_namespace_active(self) -> None:
        state = self._state_store.get_destroy_state(self._config.pool_name)
        if state != PoolDestroyState.ACTIVE:
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={self._config.pool_name}"
            )

    def _ensure_acquire_run_active(self, generation: int) -> None:
        with self._lifecycle_lock:
            if (
                generation == self._run_generation
                and self._lifecycle_state == PoolLifecycleState.RUNNING
            ):
                return
            state = self._lifecycle_state
        self._raise_if_pool_namespace_destroyed()
        raise PoolNotRunningException(
            "Cannot acquire from a retired pool run: "
            f"pool_name={self._config.pool_name} state={state.value}"
        )

    def _ensure_pool_namespace_active_for_acquire(self, policy: AcquirePolicy) -> None:
        """Namespace-active check on the acquire path with graceful degradation.

        Same as :meth:`_ensure_pool_namespace_active`, but when the state store itself
        is unavailable (``PoolStateStoreUnavailableException``) and the effective
        ``policy`` falls through to direct-create on empty idle, we treat the destroy
        state as *unknown* and allow the acquire to proceed. This is the necessary
        counterpart to the state-store-outage fallthrough already implemented at the
        ``try_take_idle`` and ``_direct_create`` call sites (see OSEP-0005 error-code
        matrix): without it a full Redis outage would short-circuit acquire before the
        fallthrough branch could run, making ``RETRY_NEXT_IDLE_THEN_CREATE`` and
        ``DIRECT_CREATE`` less available than documented.

        Fail-closed behavior for non-fallthrough policies (``FAIL_FAST`` /
        ``RETRY_NEXT_IDLE``) is preserved: the outage is surfaced as-is so callers
        can react.
        """
        try:
            self._ensure_pool_namespace_active()
        except PoolStateStoreUnavailableException:
            if not policy_falls_through_to_direct_create(policy):
                raise
            logger.warning(
                "acquire: state store unavailable during namespace check, "
                "assuming ACTIVE and degrading to direct-create per policy=%s",
                policy.value,
            )

    def _raise_if_pool_namespace_destroyed(self) -> None:
        try:
            self._ensure_pool_namespace_active()
        except PoolDestroyedException:
            raise
        except Exception:
            return

    def _ensure_pool_namespace_active_after_create(
        self,
        sandbox: SandboxSync,
        policy: AcquirePolicy | None = None,
    ) -> None:
        """Post-create fence check.

        If the state store itself is unavailable we cannot tell whether the pool was
        destroyed, so under a fallthrough ``policy`` we assume ACTIVE and keep the
        freshly-created sandbox (mirrors the OSEP-0005 acquire-outage semantics).
        Non-fallthrough policies keep the original fail-closed behavior for backward
        compatibility.
        """
        try:
            self._ensure_pool_namespace_active()
        except PoolStateStoreUnavailableException:
            if policy is not None and policy_falls_through_to_direct_create(policy):
                logger.warning(
                    "acquire: state store unavailable during post-create fence check, "
                    "keeping sandbox and degrading per policy=%s sandbox_id=%s",
                    policy.value,
                    sandbox.id,
                )
                return
            try:
                sandbox.kill()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after store-outage fence failed: pool_name=%s "
                    "sandbox_id=%s operation=kill error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            try:
                sandbox.close()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after store-outage fence failed: pool_name=%s "
                    "sandbox_id=%s operation=close error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            raise
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
            self._accept_warmup_commits = False
            self._lifecycle_state = PoolLifecycleState.STOPPED
        self._stop_reconcile(wait_for_warmup=False, join_scheduler=False)
        with self._lifecycle_lock:
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
            connection_config=(
                self._connection_for_warmup_create()
                if reason == PooledSandboxCreateReason.WARMUP
                else self._connection_for_pool_resource()
            ),
        )
        return creator(context)

    def _resolve_max_idle(self) -> int:
        shared = self._state_store.get_max_idle(self._config.pool_name)
        return self._current_max_idle if shared is None else shared

    def _create_sandbox_manager(self) -> SandboxManagerSync:
        return self._sandbox_manager_factory(self._connection_for_pool_resource())

    def _connection_for_pool_resource(self) -> ConnectionConfigSync:
        shared = self._pool_connection_config
        if shared is None or self._pool_transport_owner is None:
            return shared or self._connection_config
        transport = shared.transport
        if (
            transport is None
            or not self._connection_config.retry_policy.wraps_transport()
        ):
            return shared
        wrapped = RetrySyncTransport(
            transport, self._connection_config.retry_policy, owns_inner=False
        )
        config = self._connection_config.model_copy(update={"transport": wrapped})
        config._owns_transport = True
        return config

    def _connection_for_warmup_create(self) -> ConnectionConfigSync:
        if self._pool_transport_owner is None:
            return self._pool_connection_config or self._connection_config
        # Staged-warmup sandboxes own short-lived transports. Keeping their
        # mutually incompatible endpoint origins out of the pool-wide shared
        # transport prevents lifecycle starvation and connection churn.
        config = self._connection_config.model_copy(
            update={
                "transport": None,
                "retry_policy": RetryPolicy.disabled(),
            }
        )
        config._owns_transport = True
        return config

    def _open_pool_transport(self) -> None:
        if self._connection_config.transport is not None:
            self._pool_connection_config = self._connection_config
            self._pool_transport_owner = None
            return
        size = max(1, self._config.warmup_concurrency)
        base = self._connection_config.model_copy(
            update={"retry_policy": RetryPolicy.disabled()}
        )
        owner = base.with_transport_if_missing(
            # warmup_concurrency maps to retained idle capacity, matching
            # Kotlin's ConnectionPool; it must not cap active acquire calls.
            max_connections=None,
            max_keepalive_connections=size,
            keepalive_expiry=300.0,
        )
        shared = self._connection_config.model_copy(
            update={
                "transport": owner.transport,
                "retry_policy": RetryPolicy.disabled(),
            }
        )
        shared._owns_transport = False
        self._pool_transport_owner = owner
        self._pool_connection_config = shared

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
        heartbeat = self._heartbeat_thread
        if heartbeat is not None and heartbeat is not threading.current_thread():
            heartbeat.join(timeout=5)
        self._heartbeat_thread = None
        warmup_futures = tuple(self._warmup_futures)
        if not wait_for_warmup:
            for future in warmup_futures:
                future.cancel()
        deadline = time.monotonic() + _WARMUP_TERMINATION_TIMEOUT_SECONDS
        # Cancelling the thread-safe Future marks that wrapper done before the
        # coroutine's shielded cleanup has finished. Keep the dispatch loop
        # alive until every warmup token reaches its terminal callback.
        while time.monotonic() < deadline:
            with self._warming_lock:
                if not self._warmup_tokens:
                    break
            time.sleep(0.01)
        for future in warmup_futures:
            try:
                future.result(timeout=max(0.0, deadline - time.monotonic()))
            except BaseException:
                pass
        loop = self._warmup_loop
        loop_thread = self._warmup_loop_thread
        if loop is not None:
            loop.call_soon_threadsafe(loop.stop)
        if loop_thread is not None and loop_thread is not threading.current_thread():
            loop_thread.join(timeout=5)
        if loop is not None and not loop.is_running():
            loop.close()
        self._warmup_loop = None
        self._warmup_loop_thread = None
        create_executor = self._create_executor
        if create_executor is not None:
            create_executor.shutdown(wait=False, cancel_futures=True)
            if wait_for_warmup:
                create_executor.shutdown(wait=True)
            else:
                self._await_executor_threads(
                    create_executor, _WARMUP_TERMINATION_TIMEOUT_SECONDS
                )
        self._create_executor = None
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
        self._mark_primary_lost()

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
        if self._pool_transport_owner is not None:
            self._pool_transport_owner.close_transport_if_owned()
        self._pool_transport_owner = None
        self._pool_connection_config = None

    def _warn_if_primary_lock_ttl_may_expire_during_warmup(self) -> None:
        if self._config.primary_lock_ttl > self._config.warmup_ready_timeout:
            return
        logger.warning(
            f"Pool primary lock TTL may expire during warmup: pool_name={self._config.pool_name} primary_lock_ttl_ms={int(self._config.primary_lock_ttl.total_seconds() * 1000)} warmup_ready_timeout_ms={int(self._config.warmup_ready_timeout.total_seconds() * 1000)}"
        )
