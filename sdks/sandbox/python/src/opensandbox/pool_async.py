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
"""Asyncio sandbox pool implementation."""

from __future__ import annotations

import asyncio
import logging
import math
import time
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
    effective_max_idle_attempts,
    policy_falls_through_to_direct_create,
)
from opensandbox.pool_types import (
    try_take_idle_with_min_ttl_async as _try_take_idle_with_min_ttl_async,
)
from opensandbox.sandbox import Sandbox
from opensandbox.transport import RetryPolicy
from opensandbox.transport._async_retry import RetryAsyncTransport

logger = logging.getLogger(__name__)

_WARMUP_TERMINATION_TIMEOUT_SECONDS = 5.0
_RELEASE_ALL_IDLE_CONCURRENCY = 50
_RECONCILE_INTERVAL_SECONDS = 1.0
_CREATE_EXECUTOR_HEADROOM = 1.5


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
        warmup_create_qps: int = 10,
        warmup_concurrency: int = 128,
        primary_lock_ttl: timedelta = timedelta(seconds=60),
        degraded_threshold: int = 3,
        acquire_ready_timeout: timedelta = timedelta(seconds=30),
        acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200),
        acquire_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None,
        acquire_skip_health_check: bool = False,
        warmup_ready_timeout: timedelta = timedelta(seconds=30),
        warmup_health_check_initial_delay: timedelta = timedelta(0),
        warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=500),
        warmup_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None,
        warmup_sandbox_preparer: Callable[[Sandbox], Awaitable[None]] | None = None,
        warmup_post_prepare_health_check: Callable[[Sandbox], Awaitable[bool]]
        | None = None,
        warmup_post_prepare_health_check_timeout: timedelta = timedelta(seconds=30),
        warmup_skip_health_check: bool = False,
        idle_timeout: timedelta = timedelta(hours=24),
        drain_timeout: timedelta = timedelta(seconds=30),
        acquire_min_remaining_ttl: timedelta | None = None,
        max_acquire_retries: int = 3,
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
            warmup_create_qps=warmup_create_qps,
            warmup_concurrency=warmup_concurrency,
            primary_lock_ttl=primary_lock_ttl,
            state_store=state_store,
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
        self._lifecycle_lock = asyncio.Lock()
        self._reconcile_lock = asyncio.Lock()
        self._in_flight = 0
        self._in_flight_condition = asyncio.Condition()
        self._stop_event = asyncio.Event()
        self._scheduler_task: asyncio.Task[None] | None = None
        self._heartbeat_task: asyncio.Task[None] | None = None
        self._sandbox_manager: SandboxManager | None = None
        self._pool_connection_config: ConnectionConfig | None = None
        self._pool_transport_owner: ConnectionConfig | None = None
        self._warmup_tasks: set[asyncio.Task[None]] = set()
        self._cleanup_tasks: set[asyncio.Task[None]] = set()
        self._run_generation = 0
        self._leader_epoch = 0
        self._primary_owned = False
        self._accept_warmup_commits = False
        self._post_create_semaphore = asyncio.Semaphore(warmup_concurrency)
        self._create_semaphore = asyncio.Semaphore(
            max(1, math.ceil(warmup_create_qps * _CREATE_EXECUTOR_HEADROOM))
        )

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
                self._open_pool_transport()
                self._sandbox_manager = await self._create_sandbox_manager()
                await self._state_store.set_idle_entry_ttl(
                    self._config.pool_name, self._config.idle_timeout
                )
                await self._state_store.set_max_idle(
                    self._config.pool_name, self._config.max_idle
                )
                self._run_generation += 1
                self._primary_owned = False
                self._accept_warmup_commits = True
                self._post_create_semaphore = asyncio.Semaphore(
                    self._config.warmup_concurrency
                )
                self._create_semaphore = asyncio.Semaphore(
                    max(
                        1,
                        math.ceil(
                            self._config.warmup_create_qps * _CREATE_EXECUTOR_HEADROOM
                        ),
                    )
                )
                stop_event = asyncio.Event()
                self._stop_event = stop_event
                self._lifecycle_state = PoolLifecycleState.RUNNING
                self._scheduler_task = asyncio.create_task(
                    self._run_scheduler(stop_event),
                    name=f"sandbox-pool-reconcile-{self._config.pool_name}",
                )
                self._heartbeat_task = asyncio.create_task(
                    self._run_heartbeat(stop_event, self._run_generation),
                    name=f"sandbox-pool-heartbeat-{self._config.pool_name}",
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
        async with self._lifecycle_lock:
            if self._lifecycle_state != PoolLifecycleState.RUNNING:
                state = self._lifecycle_state
                await self._raise_if_pool_namespace_destroyed()
                raise PoolNotRunningException(
                    f"Cannot acquire when pool state is {state.value}"
                )
            operation_generation = self._run_generation
        await self._begin_operation()
        try:
            await self._ensure_acquire_run_active(operation_generation)
            await self._ensure_pool_namespace_active_for_acquire(policy)
            pool_name = self._config.pool_name
            max_attempts = effective_max_idle_attempts(
                policy, self._config.max_acquire_retries
            )

            pending_kill: list[str] = []
            last_sandbox_id: str | None = None
            last_idle_connect_failure: Exception | None = None
            attempted_any = False
            loop_exhausted = True
            attempt = 0
            while attempt < max_attempts:
                await self._ensure_acquire_run_active(operation_generation)
                attempt += 1
                try:
                    take_result = await _try_take_idle_with_min_ttl_async(
                        self._state_store,
                        pool_name,
                        self._config.acquire_min_remaining_ttl,
                    )
                except PoolStateStoreUnavailableException:
                    # State store outage. Per OSEP-0005, under policies that fall through to
                    # direct-create on empty idle we degrade to that fallback so the pool stays
                    # at least as available as raw SDK usage during store outages.
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
                    loop_exhausted = False
                    break
                last_sandbox_id = sandbox_id
                attempted_any = True
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
                except (asyncio.CancelledError, AssertionError):
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
                    # is unusable. Remove it, fire-and-forget the remote kill so a slow DELETE
                    # (up to the lifecycle client's request_timeout, 30s by default) does not
                    # block the next retry iteration, then let the loop try the next candidate.
                    last_idle_connect_failure = exc
                    await self._state_store.remove_idle(pool_name, sandbox_id)
                    self._schedule_kill_discarded_alive(
                        pool_name, (sandbox_id,), source="acquire-stale"
                    )
                    try:
                        await self._ensure_acquire_run_active(operation_generation)
                    except PoolNotRunningException as retired:
                        self._schedule_kill_discarded_alive(
                            pool_name, tuple(pending_kill), source="acquire"
                        )
                        raise retired from exc
                    await self._ensure_pool_namespace_active()
                    continue
                # Connect + readiness succeeded. From here on the sandbox is a healthy,
                # borrowable idle: any failure below (renew rejection, namespace fenced) is
                # NOT a candidate-specific problem, so we must not treat it as "stale idle"
                # and burn another retry. Dispose the sandbox and surface the error.
                try:
                    return await self._finish_acquire(
                        sandbox, sandbox_timeout, operation_generation
                    )
                finally:
                    self._schedule_kill_discarded_alive(
                        pool_name, tuple(pending_kill), source="acquire"
                    )

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
            await self._ensure_acquire_run_active(operation_generation)
            try:
                sandbox = await self._direct_create(policy=policy)
            except (AssertionError, PoolDestroyedException):
                raise
            except Exception as exc:
                # shutdown() may close the pool-owned transport after its
                # graceful drain timeout while a direct create is still in
                # flight. Do not leak that transport-close error as if it were
                # an independent service failure once this run is retired.
                try:
                    await self._ensure_acquire_run_active(operation_generation)
                except PoolNotRunningException as retired:
                    raise retired from exc
                raise
            return await self._finish_acquire(
                sandbox, sandbox_timeout, operation_generation, policy=policy
            )
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

    async def release_all_idle_parallel(
        self, max_workers: int = _RELEASE_ALL_IDLE_CONCURRENCY
    ) -> int:
        if max_workers <= 0:
            raise ValueError("max_workers must be positive")

        cleanup_task = asyncio.create_task(self._release_all_idle_parallel(max_workers))
        cancellation: asyncio.CancelledError | None = None
        cleanup_failure: BaseException | None = None
        while not cleanup_task.done():
            try:
                await asyncio.shield(cleanup_task)
            except asyncio.CancelledError as exc:
                cancellation = cancellation or exc
            except BaseException as exc:
                cleanup_failure = exc

        if cancellation is not None:
            if cleanup_failure is None and cleanup_task.done():
                try:
                    cleanup_failure = cleanup_task.exception()
                except asyncio.CancelledError:
                    pass
            if cleanup_failure is not None:
                raise cancellation from cleanup_failure
            raise cancellation
        if cleanup_failure is not None:
            raise cleanup_failure
        return cleanup_task.result()

    async def _release_all_idle_parallel(self, max_workers: int) -> int:
        pool_name = self._config.pool_name
        sandbox_ids: list[str] = []
        drain_error: Exception | None = None
        temporary_manager: SandboxManager | None = None
        try:
            while True:
                try:
                    sandbox_id = await self._state_store.try_take_idle(pool_name)
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
                        manager = await self._create_sandbox_manager()
                        temporary_manager = manager
                    except Exception as exc:
                        logger.warning(
                            f"release_all_idle_parallel: failed to create sandbox manager; draining idle ids without remote kill: pool_name={pool_name} error={exc}"
                        )

                semaphore = asyncio.Semaphore(max_workers)

                async def kill(sandbox_id: str) -> None:
                    if manager is None:
                        return
                    async with semaphore:
                        try:
                            await manager.kill_sandbox(sandbox_id)
                        except Exception as exc:
                            logger.warning(
                                f"release_all_idle_parallel: failed to kill sandbox: pool_name={pool_name} sandbox_id={sandbox_id} error={exc}"
                            )

                await asyncio.gather(*(kill(sandbox_id) for sandbox_id in sandbox_ids))
        finally:
            if temporary_manager is not None:
                await temporary_manager.close()
        if drain_error is not None:
            raise drain_error
        return len(sandbox_ids)

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
            if self._lifecycle_state in (
                PoolLifecycleState.NOT_STARTED,
                PoolLifecycleState.STOPPED,
            ):
                self._lifecycle_state = PoolLifecycleState.STOPPED
                return
            if not graceful:
                self._accept_warmup_commits = False
                await self._stop_reconcile(wait_for_warmup=False)
                self._lifecycle_state = PoolLifecycleState.STOPPED
                await self._close_provider()
                return
            self._lifecycle_state = PoolLifecycleState.DRAINING
            await self._stop_scheduler_only()
        drained = await self._await_in_flight_drain(self._config.drain_timeout)
        if not drained:
            logger.warning(
                f"Async pool graceful shutdown timed out waiting in-flight operations: pool_name={self._config.pool_name} in_flight={self._in_flight} timeout_ms={int(self._config.drain_timeout.total_seconds() * 1000)}"
            )
        async with self._lifecycle_lock:
            self._accept_warmup_commits = False
            await self._stop_reconcile(wait_for_warmup=False)
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
        initial_delay = 0 if self._config.max_idle > 0 else _RECONCILE_INTERVAL_SECONDS
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
                    timeout=_RECONCILE_INTERVAL_SECONDS,
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
                if (
                    await self._state_store.get_destroy_state(self._config.pool_name)
                    != PoolDestroyState.ACTIVE
                ):
                    await self._stop_after_pool_namespace_destroyed()
                    return
                primary_owned = await run_async_reconcile_tick(
                    config=self._config.with_max_idle(await self._resolve_max_idle()),
                    state_store=self._state_store,
                    on_discard_sandbox=self._discard_sandbox_callback,
                    warming_count=len(self._warmup_tasks),
                    submit_warmups=self._submit_warmups,
                    on_primary_acquired=self._mark_primary_acquired,
                )
                if not primary_owned:
                    self._mark_primary_lost()
            except Exception as exc:
                self._mark_primary_lost()
                logger.error(
                    f"Async pool reconcile tick failed unexpectedly: pool_name={self._config.pool_name}",
                    exc_info=exc,
                )
            finally:
                await self._end_operation()

    def _submit_warmups(self, count: int) -> None:
        generation = self._run_generation
        leader_epoch = self._leader_epoch
        for _ in range(count):
            task = asyncio.create_task(
                self._run_warmup(generation, leader_epoch, time.time_ns()),
                name=f"sandbox-pool-warmup-{self._config.pool_name}",
            )
            self._warmup_tasks.add(task)
            task.add_done_callback(self._warmup_done)

    def _warmup_done(self, task: asyncio.Task[None]) -> None:
        self._warmup_tasks.discard(task)
        if task.cancelled():
            return
        error = task.exception()
        if error is not None:
            logger.warning(
                f"Async pool warmup failed: pool_name={self._config.pool_name} error={error}"
            )

    async def _run_warmup(
        self, generation: int, leader_epoch: int, submitted_ns: int
    ) -> None:
        await self._begin_operation()
        sandbox: Sandbox | None = None
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
            await self._ensure_pool_namespace_active()
            async with self._create_semaphore:
                with warmup_trace.phase(WARMUP_CREATE_SPAN):
                    sandbox = await self._build_warmup_sandbox()
            warmup_trace.set_sandbox_id(sandbox.id)

            readiness_deadline = (
                asyncio.get_running_loop().time()
                + self._config.warmup_ready_timeout.total_seconds()
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
                    await self._wait_until_healthy(
                        sandbox,
                        self._config.warmup_health_check,
                        readiness_deadline,
                        "warmup readiness",
                        health_span,
                    )
            if self._config.warmup_sandbox_preparer is not None:
                stage = "prepare"
                with warmup_trace.phase(WARMUP_PREPARE_SPAN):
                    async with self._post_create_semaphore:
                        await self._config.warmup_sandbox_preparer(sandbox)
            if self._config.warmup_post_prepare_health_check is not None:
                stage = "post_prepare_readiness"
                with warmup_trace.phase(WARMUP_POST_PREPARE_CHECK_SPAN) as health_span:
                    await self._wait_until_healthy(
                        sandbox,
                        self._config.warmup_post_prepare_health_check,
                        asyncio.get_running_loop().time()
                        + self._config.warmup_post_prepare_health_check_timeout.total_seconds(),
                        "post-prepare readiness",
                        health_span,
                    )
            async with self._post_create_semaphore:
                stage = "renew"
                with warmup_trace.phase(WARMUP_RENEW_SPAN):
                    await sandbox.renew(self._config.idle_timeout)
                await self._ensure_pool_namespace_active_after_create(sandbox)
                stage = "commit"
                with warmup_trace.phase(WARMUP_COMMIT_SPAN):
                    if not await self._can_commit_warmup(generation, leader_epoch):
                        warmup_trace.end_dropped(stage, "leadership_lost")
                        return
                    await self._state_store.put_idle(self._config.pool_name, sandbox.id)
                    if not self._can_commit_locally(generation, leader_epoch):
                        await self._state_store.remove_idle(
                            self._config.pool_name, sandbox.id
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
            raise
        finally:
            if sandbox is not None:
                try:
                    if not committed:
                        await self._cleanup_uncommitted_warmup(sandbox)
                    else:
                        await sandbox.close()
                finally:
                    await self._end_operation()
            else:
                await self._end_operation()

    async def _wait_until_healthy(
        self,
        sandbox: Sandbox,
        health_check: Callable[[Sandbox], Awaitable[bool]] | None,
        deadline: float,
        stage: str,
        trace_span: object = None,
    ) -> None:
        last_error: Exception | None = None
        attempt_count = 0
        false_count = 0
        exception_count = 0
        while True:
            try:
                attempt_count += 1
                async with self._post_create_semaphore:
                    healthy = (
                        await health_check(sandbox)
                        if health_check is not None
                        else await sandbox.is_healthy()
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
            remaining = deadline - asyncio.get_running_loop().time()
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

    async def _can_commit_warmup(self, generation: int, leader_epoch: int) -> bool:
        if not self._can_commit_locally(generation, leader_epoch):
            return False
        renewed = await self._state_store.renew_primary_lock(
            self._config.pool_name,
            str(self._config.owner_id),
            self._config.primary_lock_ttl,
        )
        if not renewed:
            self._mark_primary_lost()
            return False
        return self._can_commit_locally(generation, leader_epoch)

    def _can_commit_locally(self, generation: int, leader_epoch: int) -> bool:
        return (
            generation == self._run_generation
            and leader_epoch == self._leader_epoch
            and self._primary_owned
            and self._accept_warmup_commits
            and self._lifecycle_state
            in (PoolLifecycleState.RUNNING, PoolLifecycleState.DRAINING)
        )

    def _mark_primary_acquired(self) -> None:
        if self._primary_owned:
            return
        self._primary_owned = True
        self._leader_epoch += 1

    def _mark_primary_lost(self) -> None:
        if not self._primary_owned:
            return
        self._primary_owned = False
        self._leader_epoch += 1
        current = asyncio.current_task()
        for task in tuple(self._warmup_tasks):
            if task is not current:
                task.cancel()

    async def _run_heartbeat(self, stop_event: asyncio.Event, generation: int) -> None:
        interval = max(
            0.001,
            min(1.0, self._config.primary_lock_ttl.total_seconds() / 3),
        )
        while not stop_event.is_set() and generation == self._run_generation:
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=interval)
                return
            except (asyncio.TimeoutError, TimeoutError):
                pass
            if not self._primary_owned:
                continue
            if self._lifecycle_state not in (
                PoolLifecycleState.RUNNING,
                PoolLifecycleState.DRAINING,
            ):
                continue
            try:
                renewed = await self._state_store.renew_primary_lock(
                    self._config.pool_name,
                    str(self._config.owner_id),
                    self._config.primary_lock_ttl,
                )
                if not renewed:
                    self._mark_primary_lost()
            except Exception as exc:
                logger.warning(
                    f"Async pool primary heartbeat failed: pool_name={self._config.pool_name} error={exc}"
                )

    async def _cleanup_uncommitted_warmup(self, sandbox: Sandbox) -> None:
        async def cleanup() -> None:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        task = asyncio.create_task(cleanup())
        cancellation: asyncio.CancelledError | None = None
        while not task.done():
            try:
                await asyncio.shield(task)
            except asyncio.CancelledError as exc:
                cancellation = cancellation or exc
        await task
        if cancellation is not None:
            raise cancellation

    async def _build_warmup_sandbox(self) -> Sandbox:
        if self._config.sandbox_creator is not None:
            return await self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.WARMUP,
                ready_timeout=self._config.warmup_ready_timeout,
                health_check_polling_interval=self._config.warmup_health_check_polling_interval,
                skip_health_check=True,
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
            connection_config=self._connection_for_warmup_create(),
            health_check=self._config.warmup_health_check,
            health_check_polling_interval=self._config.warmup_health_check_polling_interval,
            skip_health_check=True,
        )

    async def _direct_create(
        self,
        policy: AcquirePolicy = AcquirePolicy.DIRECT_CREATE,
    ) -> Sandbox:
        # policy-aware namespace check: if the state store is down and the policy is a
        # fallthrough one, treat destroy-state as unknown and proceed to direct-create
        # instead of surfacing the outage. See _ensure_pool_namespace_active_for_acquire
        # for the full rationale.
        await self._ensure_pool_namespace_active_for_acquire(policy)
        if self._config.sandbox_creator is not None:
            sandbox = await self._build_sandbox_from_creator(
                creator=self._config.sandbox_creator,
                reason=PooledSandboxCreateReason.DIRECT_CREATE,
                ready_timeout=self._config.acquire_ready_timeout,
                health_check_polling_interval=self._config.acquire_health_check_polling_interval,
                skip_health_check=self._config.acquire_skip_health_check,
                health_check=self._config.acquire_health_check,
            )
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
        return sandbox

    async def _finish_acquire(
        self,
        sandbox: Sandbox,
        sandbox_timeout: timedelta | None,
        operation_generation: int,
        *,
        policy: AcquirePolicy | None = None,
    ) -> Sandbox:
        # Ownership transfers to the caller only after renew and both fences
        # succeed. Until then cancellation must dispose of the sandbox too.
        try:
            if sandbox_timeout is not None:
                await sandbox.renew(sandbox_timeout)
            await self._ensure_pool_namespace_active_after_create(sandbox, policy)
            await self._ensure_acquire_run_active(operation_generation)
        except BaseException:
            try:
                # Reuse the shielded cleanup so repeated cancellation cannot
                # interrupt the remote kill or leave the local client open.
                await self._cleanup_uncommitted_warmup(sandbox)
            except Exception as exc:
                logger.warning(
                    "Acquire cleanup failed: pool_name=%s sandbox_id=%s error=%s",
                    self._config.pool_name,
                    sandbox.id,
                    exc,
                )
            raise
        return sandbox

    async def _ensure_pool_namespace_active(self) -> None:
        state = await self._state_store.get_destroy_state(self._config.pool_name)
        if state != PoolDestroyState.ACTIVE:
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={self._config.pool_name}"
            )

    async def _ensure_acquire_run_active(self, generation: int) -> None:
        if (
            generation == self._run_generation
            and self._lifecycle_state == PoolLifecycleState.RUNNING
        ):
            return
        state = self._lifecycle_state
        await self._raise_if_pool_namespace_destroyed()
        raise PoolNotRunningException(
            "Cannot acquire from a retired pool run: "
            f"pool_name={self._config.pool_name} state={state.value}"
        )

    async def _ensure_pool_namespace_active_for_acquire(
        self, policy: AcquirePolicy
    ) -> None:
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
            await self._ensure_pool_namespace_active()
        except PoolStateStoreUnavailableException:
            if not policy_falls_through_to_direct_create(policy):
                raise
            logger.warning(
                "acquire: state store unavailable during namespace check, "
                "assuming ACTIVE and degrading to direct-create per policy=%s",
                policy.value,
            )

    async def _raise_if_pool_namespace_destroyed(self) -> None:
        try:
            await self._ensure_pool_namespace_active()
        except PoolDestroyedException:
            raise
        except Exception:
            return

    async def _ensure_pool_namespace_active_after_create(
        self,
        sandbox: Sandbox,
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
            await self._ensure_pool_namespace_active()
        except PoolStateStoreUnavailableException:
            if policy is not None and policy_falls_through_to_direct_create(policy):
                logger.warning(
                    "acquire: state store unavailable during post-create fence check, "
                    "keeping sandbox and degrading per policy=%s sandbox_id=%s",
                    policy.value,
                    sandbox.id,
                )
                return
            raise

    async def _stop_after_pool_namespace_destroyed(self) -> None:
        async with self._lifecycle_lock:
            if self._lifecycle_state == PoolLifecycleState.STOPPED:
                return
            self._accept_warmup_commits = False
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
            connection_config=(
                self._connection_for_warmup_create()
                if reason == PooledSandboxCreateReason.WARMUP
                else self._connection_for_pool_resource()
            ),
        )
        return await creator(context)

    async def _resolve_max_idle(self) -> int:
        shared = await self._state_store.get_max_idle(self._config.pool_name)
        return self._current_max_idle if shared is None else shared

    async def _create_sandbox_manager(self) -> SandboxManager:
        return await self._sandbox_manager_factory(self._connection_for_pool_resource())

    def _connection_for_pool_resource(self) -> ConnectionConfig:
        shared = self._pool_connection_config
        if shared is None or self._pool_transport_owner is None:
            return shared or self._connection_config
        transport = shared.transport
        if (
            transport is None
            or not self._connection_config.retry_policy.wraps_transport()
        ):
            return shared
        wrapped = RetryAsyncTransport(
            transport, self._connection_config.retry_policy, owns_inner=False
        )
        config = self._connection_config.model_copy(update={"transport": wrapped})
        config._owns_transport = True
        return config

    def _connection_for_warmup_create(self) -> ConnectionConfig:
        if self._pool_transport_owner is None:
            return self._pool_connection_config or self._connection_config
        # Match the Kotlin staged-warmup ownership model: temporary warmup
        # sandboxes get their own transports, while manager/acquire resources
        # use the pool-wide shared transport. Thousands of unique endpoint
        # origins must not compete with lifecycle calls in one global pool.
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
            # Kotlin's ConnectionPool size limits retained idle connections,
            # not active request concurrency. Keep active connections
            # unbounded so foreground acquire is not accidentally serialized
            # when a consumer pool uses warmup_concurrency=1.
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
        self._cleanup_tasks.add(task)
        task.add_done_callback(self._cleanup_tasks.discard)

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
        heartbeat = self._heartbeat_task
        if heartbeat is not None and heartbeat is not current:
            try:
                await asyncio.wait_for(asyncio.shield(heartbeat), timeout=5)
            except (asyncio.TimeoutError, TimeoutError):
                heartbeat.cancel()
                await asyncio.gather(heartbeat, return_exceptions=True)
        self._heartbeat_task = None
        background_tasks = [*self._warmup_tasks, *self._cleanup_tasks]
        if wait_for_warmup and background_tasks:
            await asyncio.gather(*background_tasks, return_exceptions=True)
        elif background_tasks:
            for background_task in background_tasks:
                background_task.cancel()
            await asyncio.gather(*background_tasks, return_exceptions=True)
        await self._release_primary_lock_best_effort()
        self._mark_primary_lost()

    async def _stop_scheduler_only(self) -> None:
        task = self._scheduler_task
        if task is None or task is asyncio.current_task():
            return
        task.cancel()
        await asyncio.gather(task, return_exceptions=True)
        self._scheduler_task = None

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
        if self._pool_transport_owner is not None:
            await self._pool_transport_owner.close_transport_if_owned()
        self._pool_transport_owner = None
        self._pool_connection_config = None

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
