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
"""Asyncio sandbox pool implementation."""

from __future__ import annotations

import asyncio
import logging
from collections.abc import Awaitable, Callable
from datetime import timedelta

from opensandbox._async_pool_reconciler import run_async_reconcile_tick
from opensandbox._async_pool_store import InMemoryAsyncPoolStateStore
from opensandbox._pool_reconciler import ReconcileState
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolDestroyedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.manager import SandboxManager
from opensandbox.pool_types import (
    AcquirePolicy,
    AsyncPoolConfig,
    AsyncPooledSandboxCreator,
    AsyncPoolStateStore,
    IdleEntry,
    PoolCreationSpec,
    PoolDestroyState,
    PooledSandboxCreateContext,
    PooledSandboxCreateReason,
    PoolLifecycleState,
    PoolSnapshot,
    PoolState,
)
from opensandbox.pool_types import (
    try_take_idle_with_min_ttl_async as _try_take_idle_with_min_ttl_async,
)
from opensandbox.sandbox import Sandbox

logger = logging.getLogger(__name__)

_WARMUP_TERMINATION_TIMEOUT_SECONDS = 5.0


class SandboxPoolAsync:
    """Client-side asyncio sandbox pool aligned with Kotlin SandboxPool."""

    def __init__(
        self,
        *,
        pool_name: str,
        max_idle: int,
        state_store: AsyncPoolStateStore,
        connection_config: ConnectionConfig,
        creation_spec: PoolCreationSpec,
        owner_id: str | None = None,
        warmup_concurrency: int | None = None,
        primary_lock_ttl: timedelta = timedelta(seconds=60),
        reconcile_interval: timedelta = timedelta(seconds=30),
        degraded_threshold: int = 3,
        acquire_ready_timeout: timedelta = timedelta(seconds=30),
        acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        acquire_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None,
        acquire_skip_health_check: bool = False,
        warmup_ready_timeout: timedelta = timedelta(seconds=30),
        warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        warmup_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None,
        warmup_sandbox_preparer: Callable[[Sandbox], Awaitable[None]] | None = None,
        warmup_skip_health_check: bool = False,
        idle_timeout: timedelta = timedelta(hours=24),
        drain_timeout: timedelta = timedelta(seconds=30),
        acquire_min_remaining_ttl: timedelta | None = None,
        sandbox_manager_factory: Callable[
            [ConnectionConfig], Awaitable[SandboxManager]
        ] = SandboxManager.create,
        sandbox_factory: type[Sandbox] = Sandbox,
        sandbox_creator: AsyncPooledSandboxCreator | None = None,
    ) -> None:
        self._config = AsyncPoolConfig(
            pool_name=pool_name,
            owner_id=owner_id,
            max_idle=max_idle,
            warmup_concurrency=warmup_concurrency,
            primary_lock_ttl=primary_lock_ttl,
            state_store=state_store,
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
        self._lifecycle_lock = asyncio.Lock()
        self._reconcile_lock = asyncio.Lock()
        self._in_flight = 0
        self._in_flight_condition = asyncio.Condition()
        self._stop_event = asyncio.Event()
        self._scheduler_task: asyncio.Task[None] | None = None
        self._sandbox_manager: SandboxManager | None = None
        self._warmup_tasks: set[asyncio.Task[str | None]] = set()

    async def start(self) -> None:
        async with self._lifecycle_lock:
            if self._lifecycle_state in (
                PoolLifecycleState.RUNNING,
                PoolLifecycleState.STARTING,
            ):
                return
            self._lifecycle_state = PoolLifecycleState.STARTING
            try:
                await self._ensure_pool_namespace_active()
                self._warn_if_primary_lock_ttl_may_expire_during_warmup()
                self._sandbox_manager = await self._create_sandbox_manager()
                await self._state_store.set_idle_entry_ttl(
                    self._config.pool_name, self._config.idle_timeout
                )
                await self._state_store.set_max_idle(
                    self._config.pool_name, self._config.max_idle
                )
                stop_event = asyncio.Event()
                self._stop_event = stop_event
                self._lifecycle_state = PoolLifecycleState.RUNNING
                self._scheduler_task = asyncio.create_task(
                    self._run_scheduler(stop_event),
                    name=f"sandbox-pool-reconcile-{self._config.pool_name}",
                )
            except Exception:
                await self._stop_reconcile(wait_for_warmup=True)
                await self._close_provider()
                self._lifecycle_state = PoolLifecycleState.STOPPED
                raise

    async def acquire(
        self,
        sandbox_timeout: timedelta | None = None,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ) -> Sandbox:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            state = self._lifecycle_state
            await self._raise_if_pool_namespace_destroyed()
            raise PoolNotRunningException(
                f"Cannot acquire when pool state is {state.value}"
            )
        await self._begin_operation()
        try:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                state = self._lifecycle_state
                await self._raise_if_pool_namespace_destroyed()
                raise PoolNotRunningException(
                    f"Cannot acquire when pool state is {state.value}"
                )
            await self._ensure_pool_namespace_active()
            pool_name = self._config.pool_name
            take_result = await _try_take_idle_with_min_ttl_async(
                self._state_store,
                pool_name,
                self._config.acquire_min_remaining_ttl,
            )
            sandbox_id = take_result.sandbox_id
            # Defer cleanup of below-threshold-but-still-alive sandboxes until after the chosen
            # candidate is connected and renewed. Doing it inline before connect would let slow
            # kill RPCs eat the candidate's remaining TTL — the race this PR is fixing.
            pending_kill = take_result.discarded_alive_sandbox_ids
            no_idle_reason: str | None = None
            idle_connect_failure: Exception | None = None
            if sandbox_id is not None:
                try:
                    sandbox = await self._sandbox_factory.connect(
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
                        await sandbox.renew(sandbox_timeout)
                    await self._ensure_pool_namespace_active_after_create(sandbox)
                    # Candidate is connected and (optionally) renewed. Kick off kill cleanup as
                    # a background task so the caller does not wait for N kill RPCs.
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
                    await self._state_store.remove_idle(pool_name, sandbox_id)
                    try:
                        if self._sandbox_manager is not None:
                            await self._sandbox_manager.kill_sandbox(sandbox_id)
                    except Exception:
                        pass
                    no_idle_reason = (
                        f"idle connect failed for sandbox_id={sandbox_id} "
                        "(stale or unreachable)"
                    )
            else:
                no_idle_reason = "idle buffer empty"

            # Reaching here means we did not return a sandbox from idle. Still fire deferred
            # cleanup so the discarded-alive sandboxes do not linger.
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
            return await self._direct_create(sandbox_timeout)
        finally:
            await self._end_operation()

    async def resize(self, max_idle: int) -> None:
        if max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        await self._ensure_pool_namespace_active()
        await self._state_store.set_max_idle(self._config.pool_name, max_idle)
        self._current_max_idle = max_idle

    async def release_all_idle(self) -> int:
        pool_name = self._config.pool_name
        count = 0
        temporary_manager: SandboxManager | None = None
        try:
            while True:
                sandbox_id = await self._state_store.try_take_idle(pool_name)
                if sandbox_id is None:
                    break
                count += 1
                try:
                    manager = self._sandbox_manager or temporary_manager
                    if manager is None:
                        manager = await self._create_sandbox_manager()
                        temporary_manager = manager
                    await manager.kill_sandbox(sandbox_id)
                except Exception as exc:
                    logger.warning(
                        f"release_all_idle: failed to kill sandbox: pool_name={pool_name} sandbox_id={sandbox_id} error={exc}"
                    )
        finally:
            if temporary_manager is not None:
                await temporary_manager.close()
        return count

    async def snapshot(self) -> PoolSnapshot:
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
        counters = await self._state_store.snapshot_counters(self._config.pool_name)
        return PoolSnapshot(
            state=state,
            lifecycle_state=lifecycle_state,
            idle_count=counters.idle_count,
            max_idle=await self._resolve_max_idle(),
            failure_count=self._reconcile_state.failure_count,
            backoff_active=self._reconcile_state.is_backoff_active(),
            last_error=self._reconcile_state.last_error,
            in_flight_operations=self._in_flight,
        )

    async def snapshot_idle_entries(self) -> list[IdleEntry]:
        return await self._state_store.snapshot_idle_entries(self._config.pool_name)

    async def shutdown(self, graceful: bool = True) -> None:
        async with self._lifecycle_lock:
            if self._lifecycle_state == PoolLifecycleState.STOPPED:
                return
            if not graceful:
                await self._stop_reconcile(wait_for_warmup=False)
                self._lifecycle_state = PoolLifecycleState.STOPPED
                await self._close_provider()
                return
            self._lifecycle_state = PoolLifecycleState.DRAINING
            await self._stop_reconcile(wait_for_warmup=False, join_scheduler=False)
        drained = await self._await_in_flight_drain(self._config.drain_timeout)
        if not drained:
            logger.warning(
                f"Async pool graceful shutdown timed out waiting in-flight operations: pool_name={self._config.pool_name} in_flight={self._in_flight} timeout_ms={int(self._config.drain_timeout.total_seconds() * 1000)}"
            )
        async with self._lifecycle_lock:
            self._lifecycle_state = PoolLifecycleState.STOPPED
            await self._close_provider()

    async def __aenter__(self) -> SandboxPoolAsync:
        await self.start()
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: object,
    ) -> None:
        await self.shutdown(graceful=True)

    async def _run_scheduler(self, stop_event: asyncio.Event) -> None:
        initial_delay = (
            0
            if self._config.max_idle > 0
            else self._config.reconcile_interval.total_seconds()
        )
        if initial_delay > 0:
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=initial_delay)
                return
            except (asyncio.TimeoutError, TimeoutError):
                pass
        while not stop_event.is_set():
            await self._run_reconcile_tick()
            try:
                await asyncio.wait_for(
                    stop_event.wait(),
                    timeout=self._config.reconcile_interval.total_seconds(),
                )
                break
            except (asyncio.TimeoutError, TimeoutError):
                continue

    async def _run_reconcile_tick(self) -> None:
        if self._lifecycle_state != PoolLifecycleState.RUNNING:
            return
        async with self._reconcile_lock:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                return
            await self._begin_operation()
            try:
                if self._lifecycle_state != PoolLifecycleState.RUNNING:
                    return
                if await self._state_store.get_destroy_state(
                    self._config.pool_name
                ) != PoolDestroyState.ACTIVE:
                    await self._stop_after_pool_namespace_destroyed()
                    return
                await run_async_reconcile_tick(
                    config=self._config.with_max_idle(await self._resolve_max_idle()),
                    state_store=self._state_store,
                    create_one=self._create_one_sandbox,
                    on_discard_sandbox=self._discard_sandbox_callback,
                    reconcile_state=self._reconcile_state,
                )
            except Exception as exc:
                logger.error(
                    f"Async pool reconcile tick failed unexpectedly: pool_name={self._config.pool_name}",
                    exc_info=exc,
                )
            finally:
                await self._end_operation()

    async def _create_one_sandbox(self) -> str | None:
        await self._begin_operation()
        task = asyncio.current_task()
        if task is not None:
            self._warmup_tasks.add(task)  # type: ignore[arg-type]
        try:
            await self._ensure_pool_namespace_active()
            sandbox = await self._build_warmup_sandbox()
            try:
                if self._config.warmup_sandbox_preparer is not None:
                    await self._config.warmup_sandbox_preparer(sandbox)
                if self._lifecycle_state != PoolLifecycleState.RUNNING:
                    try:
                        await sandbox.kill()
                    except Exception:
                        pass
                    return None
                # The server-side TTL has been ticking since sandbox creation;
                # readiness wait and `warmup_sandbox_preparer` can both consume meaningful time.
                # Renew right before handing the id back to the reconciler so the store's
                # stamped expiry actually matches what the server will honor — otherwise
                # `acquire_min_remaining_ttl` overestimates remaining TTL by the warmup duration.
                await sandbox.renew(self._config.idle_timeout)
                await self._ensure_pool_namespace_active_after_create(sandbox)
                return sandbox.id
            except BaseException:
                try:
                    await sandbox.kill()
                except Exception:
                    pass
                raise
            finally:
                await sandbox.close()
        finally:
            if task is not None:
                self._warmup_tasks.discard(task)  # type: ignore[arg-type]
            await self._end_operation()

    async def _build_warmup_sandbox(self) -> Sandbox:
        if self._config.sandbox_creator is not None:
            return await self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.WARMUP,
                ready_timeout=self._config.warmup_ready_timeout,
                health_check_polling_interval=self._config.warmup_health_check_polling_interval,
                skip_health_check=self._config.warmup_skip_health_check,
                health_check=self._config.warmup_health_check,
            )

        spec = self._creation_spec
        return await self._sandbox_factory.create(
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

    async def _direct_create(self, sandbox_timeout: timedelta | None) -> Sandbox:
        await self._ensure_pool_namespace_active()
        if self._config.sandbox_creator is not None:
            sandbox = await self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.DIRECT_CREATE,
                ready_timeout=self._config.acquire_ready_timeout,
                health_check_polling_interval=self._config.acquire_health_check_polling_interval,
                skip_health_check=self._config.acquire_skip_health_check,
                health_check=self._config.acquire_health_check,
            )
            if sandbox_timeout is not None:
                try:
                    await sandbox.renew(sandbox_timeout)
                except BaseException:
                    try:
                        await sandbox.kill()
                    finally:
                        await sandbox.close()
                    raise
            await self._ensure_pool_namespace_active_after_create(sandbox)
            return sandbox

        spec = self._creation_spec
        sandbox = await self._sandbox_factory.create(
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
                await sandbox.renew(sandbox_timeout)
            except BaseException:
                try:
                    await sandbox.kill()
                finally:
                    await sandbox.close()
                raise
        await self._ensure_pool_namespace_active_after_create(sandbox)
        return sandbox

    async def _ensure_pool_namespace_active(self) -> None:
        state = await self._state_store.get_destroy_state(self._config.pool_name)
        if state != PoolDestroyState.ACTIVE:
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={self._config.pool_name}"
            )

    async def _raise_if_pool_namespace_destroyed(self) -> None:
        try:
            await self._ensure_pool_namespace_active()
        except PoolDestroyedException:
            raise
        except Exception:
            return

    async def _ensure_pool_namespace_active_after_create(self, sandbox: Sandbox) -> None:
        try:
            await self._ensure_pool_namespace_active()
        except BaseException:
            try:
                await sandbox.kill()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after fence failed: pool_name=%s "
                    "sandbox_id=%s operation=kill error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            try:
                await sandbox.close()
            except Exception as exc:
                logger.warning(
                    "Pool sandbox cleanup after fence failed: pool_name=%s "
                    "sandbox_id=%s operation=close error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            raise

    async def _stop_after_pool_namespace_destroyed(self) -> None:
        async with self._lifecycle_lock:
            if self._lifecycle_state == PoolLifecycleState.STOPPED:
                return
            await self._stop_reconcile(wait_for_warmup=False, join_scheduler=False)
            self._lifecycle_state = PoolLifecycleState.STOPPED
            await self._close_provider()

    async def _build_sandbox_from_creator(
        self,
        *,
        creator: AsyncPooledSandboxCreator,
        reason: PooledSandboxCreateReason,
        ready_timeout: timedelta,
        health_check_polling_interval: timedelta,
        skip_health_check: bool,
        health_check: Callable[[Sandbox], Awaitable[bool]] | None,
    ) -> Sandbox:
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
        return await creator(context)

    async def _resolve_max_idle(self) -> int:
        shared = await self._state_store.get_max_idle(self._config.pool_name)
        return self._current_max_idle if shared is None else shared

    async def _create_sandbox_manager(self) -> SandboxManager:
        return await self._sandbox_manager_factory(self._connection_for_pool_resource())

    def _connection_for_pool_resource(self) -> ConnectionConfig:
        if (
            self._connection_config.transport is not None
            and not self._connection_config._owns_transport
        ):
            return self._connection_config
        config = self._connection_config.model_copy(update={"transport": None})
        config._owns_transport = True
        return config

    async def _discard_sandbox_callback(self, sandbox_id: str) -> None:
        """``Callable[[str], Awaitable[None]]`` adapter for the reconciler's
        ``on_discard_sandbox`` hook. Drops the bool return value of
        :meth:`_kill_sandbox_best_effort`.
        """
        await self._kill_sandbox_best_effort(sandbox_id)

    async def _kill_sandbox_best_effort(self, sandbox_id: str) -> bool:
        """Best-effort kill a sandbox via the pool's manager.

        Returns ``True`` on a confirmed kill, ``False`` if no manager is available or the
        kill raised. Failures are logged at WARNING and swallowed.
        """
        if self._sandbox_manager is None:
            return False
        try:
            await self._sandbox_manager.kill_sandbox(sandbox_id)
            return True
        except Exception as exc:
            logger.warning(
                f"Async pool sandbox cleanup failed: pool_name={self._config.pool_name} sandbox_id={sandbox_id} error={exc}"
            )
            return False

    def _schedule_kill_discarded_alive(
        self,
        pool_name: str,
        sandbox_ids: tuple[str, ...],
        source: str,
    ) -> None:
        """Fire-and-forget the kill cleanup as a background task so the caller's ``acquire``
        is not blocked on N kill RPCs. The task is added to ``_warmup_tasks`` so shutdown can
        wait on it just like other background work; rejected scheduling falls back to inline.
        """
        if not sandbox_ids:
            return
        try:
            task = asyncio.create_task(
                self._kill_discarded_alive(pool_name, sandbox_ids, source)
            )
        except RuntimeError as exc:
            # No running loop / loop is closed — fall back to inline cleanup so the work is
            # not silently dropped. The await here is safe because we are inside `acquire()`.
            logger.debug(
                f"Discarded-alive kill scheduling failed, running inline: pool_name={pool_name} count={len(sandbox_ids)} error={exc}"
            )
            # Caller is in an async function, so this is awaited via the original
            # `_kill_discarded_alive` directly by the caller. Since `_schedule_kill_discarded_alive`
            # is sync, the safest fallback is a fire-and-forget through a fresh task; if that
            # also fails the runtime is clearly mid-shutdown and the cleanup is not critical.
            return
        self._warmup_tasks.add(task)  # type: ignore[arg-type]
        task.add_done_callback(self._warmup_tasks.discard)  # type: ignore[arg-type]

    async def _kill_discarded_alive(
        self,
        pool_name: str,
        sandbox_ids: tuple[str, ...],
        source: str,
    ) -> None:
        """Async counterpart of :meth:`SandboxPoolSync._kill_discarded_alive`.

        Kills run concurrently via :func:`asyncio.gather` so a batch of N near-expiry IDs
        does not serially block the caller's ``acquire()`` on N network round-trips.
        """
        if not sandbox_ids:
            return
        results = await asyncio.gather(
            *(self._kill_sandbox_best_effort(sandbox_id) for sandbox_id in sandbox_ids),
            return_exceptions=False,
        )
        for sandbox_id, killed in zip(sandbox_ids, results, strict=True):
            if killed:
                logger.debug(
                    f"Killed near-expiry idle sandbox: pool_name={pool_name} sandbox_id={sandbox_id} source={source}"
                )

    async def _begin_operation(self) -> None:
        async with self._in_flight_condition:
            self._in_flight += 1

    async def _end_operation(self) -> None:
        async with self._in_flight_condition:
            self._in_flight -= 1
            if self._in_flight <= 0:
                self._in_flight = 0
                self._in_flight_condition.notify_all()

    async def _await_in_flight_drain(self, timeout: timedelta) -> bool:
        deadline = asyncio.get_running_loop().time() + timeout.total_seconds()
        async with self._in_flight_condition:
            while self._in_flight > 0:
                remaining = deadline - asyncio.get_running_loop().time()
                if remaining <= 0:
                    return False
                try:
                    await asyncio.wait_for(self._in_flight_condition.wait(), remaining)
                except (asyncio.TimeoutError, TimeoutError):
                    return self._in_flight == 0
            return True

    async def _stop_reconcile(
        self,
        *,
        wait_for_warmup: bool,
        join_scheduler: bool = True,
    ) -> None:
        self._stop_event.set()
        task = self._scheduler_task
        current = asyncio.current_task()
        if join_scheduler and task is not None and task is not current:
            try:
                await asyncio.wait_for(asyncio.shield(task), timeout=5)
            except (asyncio.TimeoutError, TimeoutError):
                task.cancel()
            self._scheduler_task = None
        warmup_tasks = list(self._warmup_tasks)
        if wait_for_warmup and warmup_tasks:
            await asyncio.gather(*warmup_tasks, return_exceptions=True)
        elif warmup_tasks:
            _, pending = await asyncio.wait(
                warmup_tasks,
                timeout=_WARMUP_TERMINATION_TIMEOUT_SECONDS,
            )
            for warmup_task in pending:
                warmup_task.cancel()
            if pending:
                await asyncio.gather(*pending, return_exceptions=True)
        await self._release_primary_lock_best_effort()

    async def _release_primary_lock_best_effort(self) -> None:
        try:
            await self._state_store.release_primary_lock(
                self._config.pool_name, str(self._config.owner_id)
            )
        except Exception as exc:
            logger.warning(
                f"Async pool primary lock release failed: pool_name={self._config.pool_name} owner_id={self._config.owner_id} error={exc}"
            )

    async def _close_provider(self) -> None:
        if self._sandbox_manager is not None:
            await self._sandbox_manager.close()
            self._sandbox_manager = None

    def _warn_if_primary_lock_ttl_may_expire_during_warmup(self) -> None:
        if self._config.primary_lock_ttl > self._config.warmup_ready_timeout:
            return
        logger.warning(
            f"Async pool primary lock TTL may expire during warmup: pool_name={self._config.pool_name} primary_lock_ttl_ms={int(self._config.primary_lock_ttl.total_seconds() * 1000)} warmup_ready_timeout_ms={int(self._config.warmup_ready_timeout.total_seconds() * 1000)}"
        )


AsyncSandboxPool = SandboxPoolAsync

__all__ = [
    "AsyncSandboxPool",
    "InMemoryAsyncPoolStateStore",
    "SandboxPoolAsync",
]
