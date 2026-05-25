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
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.manager import SandboxManager
from opensandbox.pool_types import (
    AcquirePolicy,
    AsyncPoolConfig,
    AsyncPoolStateStore,
    IdleEntry,
    PoolCreationSpec,
    PoolLifecycleState,
    PoolSnapshot,
    PoolState,
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
        sandbox_manager_factory: Callable[
            [ConnectionConfig], Awaitable[SandboxManager]
        ] = SandboxManager.create,
        sandbox_factory: type[Sandbox] = Sandbox,
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
            raise PoolNotRunningException(f"Cannot acquire when pool state is {state.value}")
        await self._begin_operation()
        try:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                state = self._lifecycle_state
                raise PoolNotRunningException(
                    f"Cannot acquire when pool state is {state.value}"
                )
            pool_name = self._config.pool_name
            sandbox_id = await self._state_store.try_take_idle(pool_name)
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
                    return sandbox
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
                        "release_all_idle: failed to kill sandbox: pool_name=%s sandbox_id=%s error=%s",
                        pool_name,
                        sandbox_id,
                        exc,
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
                "Async pool graceful shutdown timed out waiting in-flight operations: pool_name=%s in_flight=%s timeout_ms=%s",
                self._config.pool_name,
                self._in_flight,
                int(self._config.drain_timeout.total_seconds() * 1000),
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
                await run_async_reconcile_tick(
                    config=self._config.with_max_idle(await self._resolve_max_idle()),
                    state_store=self._state_store,
                    create_one=self._create_one_sandbox,
                    on_discard_sandbox=self._kill_sandbox_best_effort,
                    reconcile_state=self._reconcile_state,
                )
            except Exception as exc:
                logger.error(
                    "Async pool reconcile tick failed unexpectedly: pool_name=%s",
                    self._config.pool_name,
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
            sandbox = await self._build_sandbox_from_spec()
            try:
                if self._config.warmup_sandbox_preparer is not None:
                    await self._config.warmup_sandbox_preparer(sandbox)
                if self._lifecycle_state != PoolLifecycleState.RUNNING:
                    try:
                        await sandbox.kill()
                    except Exception:
                        pass
                    return None
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

    async def _build_sandbox_from_spec(self) -> Sandbox:
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
        return sandbox

    async def _resolve_max_idle(self) -> int:
        shared = await self._state_store.get_max_idle(self._config.pool_name)
        return self._current_max_idle if shared is None else shared

    async def _create_sandbox_manager(self) -> SandboxManager:
        return await self._sandbox_manager_factory(self._connection_for_pool_resource())

    def _connection_for_pool_resource(self) -> ConnectionConfig:
        if self._connection_config.transport is not None and not self._connection_config._owns_transport:
            return self._connection_config
        config = self._connection_config.model_copy(update={"transport": None})
        config._owns_transport = True
        return config

    async def _kill_sandbox_best_effort(self, sandbox_id: str) -> None:
        try:
            if self._sandbox_manager is not None:
                await self._sandbox_manager.kill_sandbox(sandbox_id)
        except Exception as exc:
            logger.warning(
                "Async pool sandbox cleanup failed: pool_name=%s sandbox_id=%s error=%s",
                self._config.pool_name,
                sandbox_id,
                exc,
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
                "Async pool primary lock release failed: pool_name=%s owner_id=%s error=%s",
                self._config.pool_name,
                self._config.owner_id,
                exc,
            )

    async def _close_provider(self) -> None:
        if self._sandbox_manager is not None:
            await self._sandbox_manager.close()
            self._sandbox_manager = None

    def _warn_if_primary_lock_ttl_may_expire_during_warmup(self) -> None:
        if self._config.primary_lock_ttl > self._config.warmup_ready_timeout:
            return
        logger.warning(
            "Async pool primary lock TTL may expire during warmup: pool_name=%s primary_lock_ttl_ms=%s warmup_ready_timeout_ms=%s",
            self._config.pool_name,
            int(self._config.primary_lock_ttl.total_seconds() * 1000),
            int(self._config.warmup_ready_timeout.total_seconds() * 1000),
        )


AsyncSandboxPool = SandboxPoolAsync

__all__ = [
    "AsyncSandboxPool",
    "InMemoryAsyncPoolStateStore",
    "SandboxPoolAsync",
]
