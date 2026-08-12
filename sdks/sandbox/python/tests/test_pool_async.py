# Copyright 2026 Alibaba Group Holding Ltd.
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

from __future__ import annotations

import asyncio
import time
from datetime import datetime, timedelta, timezone
from typing import Any, cast

import httpx
import pytest

from opensandbox._async_pool_reconciler import run_async_reconcile_tick
from opensandbox._pool_reconciler import ReconcileState
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolDestroyedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.models.sandboxes import PlatformSpec
from opensandbox.pool import (
    AcquirePolicy,
    AsyncPoolConfig,
    InMemoryAsyncPoolStateStore,
    PoolCreationSpec,
    PooledSandboxCreateContext,
    PooledSandboxCreateReason,
    SandboxPoolAsync,
)


@pytest.mark.asyncio
async def test_async_acquire_fail_fast_empty_raises_pool_empty() -> None:
    pool = _create_pool(max_idle=0)
    await pool.start()
    try:
        with pytest.raises(PoolEmptyException) as exc:
            await pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc.value.error.code == "POOL_EMPTY"
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_reconcile_batch_failures_only_advance_backoff_once() -> None:
    store = InMemoryAsyncPoolStateStore()
    config = AsyncPoolConfig(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=10,
        warmup_concurrency=10,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    )
    state = ReconcileState(degraded_threshold=3)

    async def fail_create() -> str:
        raise RuntimeError("boom")

    await run_async_reconcile_tick(
        config=config,
        state_store=store,
        create_one=fail_create,
        on_discard_sandbox=_noop_discard,
        reconcile_state=state,
    )

    assert state.failure_count == 10
    assert state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=29))
    assert not state.is_backoff_active(
        datetime.now(timezone.utc) + timedelta(seconds=31)
    )


@pytest.mark.asyncio
async def test_async_acquire_fail_fast_stale_idle_raises_and_kills_candidate() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "stale-1")
    manager = FakeAsyncManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager)
    await pool.start()

    try:
        with pytest.raises(PoolAcquireFailedException) as exc:
            await pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc.value.error.code == "POOL_ACQUIRE_FAILED"
        assert (await store.snapshot_counters("pool")).idle_count == 0
        # Kill is now fire-and-forget (retry-loop must not block on slow DELETE) so wait for
        # the background task to observe the kill.
        async def _killed_stale_1() -> bool:
            return manager.killed == ["stale-1"]

        await _eventually(_killed_stale_1)
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_direct_create_when_empty() -> None:
    FakeAsyncSandbox.reset()
    pool = _create_pool(max_idle=0)
    await pool.start()

    try:
        sandbox = await pool.acquire(sandbox_timeout=timedelta(minutes=5))
        fake_sandbox = cast(FakeAsyncSandbox, sandbox)
        assert sandbox.id == "created-1"
        assert fake_sandbox.renewed == [timedelta(minutes=5)]
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_does_not_direct_create_when_pool_namespace_is_destroying() -> None:
    FakeAsyncSandbox.reset()
    store = InMemoryAsyncPoolStateStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()

    try:
        await store.begin_destroy("pool", "destroyer")

        with pytest.raises(PoolDestroyedException):
            await pool.acquire()
        assert FakeAsyncSandbox.created_count == 0
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_idle_destroy_race_raises_pool_destroyed() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "id-1")
    connected: list[FakeAsyncSandbox] = []

    class FencingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def connect(
            cls, sandbox_id: str, *args: Any, **kwargs: Any
        ) -> FakeAsyncSandbox:
            sandbox = cls(sandbox_id)
            connected.append(sandbox)
            await store.begin_destroy("pool", "destroyer")
            return sandbox

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
        sandbox_factory=FencingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        with pytest.raises(PoolDestroyedException):
            await pool.acquire(policy=AcquirePolicy.DIRECT_CREATE)
        assert connected[0].killed
        assert connected[0].closed
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_stopped_destroyed_pool_raises_pool_destroyed() -> None:
    store = InMemoryAsyncPoolStateStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()
    await store.begin_destroy("pool", "destroyer")
    await pool.shutdown(False)

    with pytest.raises(PoolDestroyedException):
        await pool.acquire()


@pytest.mark.asyncio
async def test_async_acquire_destroy_race_preserves_pool_destroyed_when_cleanup_fails() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "id-1")

    class CleanupFailingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def connect(
            cls, sandbox_id: str, *args: Any, **kwargs: Any
        ) -> FakeAsyncSandbox:
            await store.begin_destroy("pool", "destroyer")
            return cls(sandbox_id)

        async def kill(self) -> None:
            raise RuntimeError("kill failed")

        async def close(self) -> None:
            raise RuntimeError("close failed")

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
        sandbox_factory=CleanupFailingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        with pytest.raises(PoolDestroyedException):
            await pool.acquire(policy=AcquirePolicy.DIRECT_CREATE)
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_direct_create_forwards_pool_creation_platform() -> None:
    captured_kwargs: dict[str, Any] = {}

    class CapturingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def create(cls, *args: Any, **kwargs: Any) -> CapturingAsyncSandbox:
            captured_kwargs.update(kwargs)
            return cls("created-with-platform")

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(
            image="ubuntu:22.04",
            platform=PlatformSpec(os="linux", arch="arm64"),
        ),
        sandbox_factory=CapturingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await pool.acquire()

        assert captured_kwargs["platform"] == PlatformSpec(os="linux", arch="arm64")
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_direct_create_kills_and_closes_when_renew_fails() -> None:
    FakeAsyncSandbox.reset()
    FakeAsyncSandbox.fail_renew = True
    pool = _create_pool(max_idle=0)
    await pool.start()

    try:
        with pytest.raises(RuntimeError, match="renew failed"):
            await pool.acquire(sandbox_timeout=timedelta(minutes=5))
        assert FakeAsyncSandbox.last_created is not None
        assert FakeAsyncSandbox.last_created.killed
        assert FakeAsyncSandbox.last_created.closed
    finally:
        FakeAsyncSandbox.fail_renew = False
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_direct_create_uses_sandbox_creator() -> None:
    contexts: list[PooledSandboxCreateContext] = []

    async def creator(context: PooledSandboxCreateContext) -> FakeAsyncSandbox:
        contexts.append(context)
        return FakeAsyncSandbox("created-by-hook")

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(minutes=10),
        sandbox_creator=creator,
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        sandbox = await pool.acquire(sandbox_timeout=timedelta(minutes=5))
        fake_sandbox = cast(FakeAsyncSandbox, sandbox)

        assert sandbox.id == "created-by-hook"
        assert fake_sandbox.renewed == [timedelta(minutes=5)]
        assert len(contexts) == 1
        assert contexts[0].pool_name == "pool"
        assert contexts[0].owner_id == "owner-1"
        assert contexts[0].idle_timeout == timedelta(minutes=10)
        assert contexts[0].reason is PooledSandboxCreateReason.DIRECT_CREATE
        assert contexts[0].ready_timeout == pool._config.acquire_ready_timeout
        assert (
            contexts[0].health_check_polling_interval
            == pool._config.acquire_health_check_polling_interval
        )
        assert contexts[0].skip_health_check is False
        assert contexts[0].health_check is None
        assert isinstance(contexts[0].connection_config, ConnectionConfig)
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_acquire_when_stopped_raises_pool_not_running() -> None:
    pool = _create_pool(max_idle=0)

    with pytest.raises(PoolNotRunningException) as exc:
        await pool.acquire(policy=AcquirePolicy.FAIL_FAST)

    assert exc.value.error.code == "POOL_NOT_RUNNING"


@pytest.mark.asyncio
async def test_async_start_warms_idle_and_resize_zero_shrinks() -> None:
    FakeAsyncSandbox.reset()
    store = InMemoryAsyncPoolStateStore()
    manager = FakeAsyncManager()
    pool = _create_pool(max_idle=2, store=store, manager=manager)
    await pool.start()

    try:
        await _eventually(lambda: _idle_count_equals(pool, 2))
        await pool.resize(0)
        await _eventually(lambda: _idle_count_equals(pool, 0))
        assert len(manager.killed) >= 2
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_start_overwrites_shared_max_idle_with_user_config() -> None:
    store = SharedAsyncMaxIdleStore(initial_max_idle=0)
    pool = _create_pool(max_idle=3, store=store)
    await pool.start()

    try:
        assert store.max_idle_by_pool["pool"] == 3
        assert store.set_max_idle_calls == [("pool", 3)]
        assert (await pool.snapshot()).max_idle == 3
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_resize_only_updates_target_without_immediate_reconcile_trigger() -> (
    None
):
    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(seconds=10),
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    calls = 0

    async def record_reconcile() -> None:
        nonlocal calls
        calls += 1

    pool._run_reconcile_tick = record_reconcile  # type: ignore[method-assign]
    try:
        await pool.resize(1)
        await asyncio.sleep(0.05)

        assert calls == 0
        assert (await pool.snapshot()).max_idle == 1
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_graceful_shutdown_waits_for_running_warmup_before_stop() -> None:
    FakeAsyncSandbox.reset()
    entered_preparer = asyncio.Event()
    release_preparer = asyncio.Event()

    async def blocking_preparer(sandbox: FakeAsyncSandbox) -> None:
        entered_preparer.set()
        await release_preparer.wait()

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=1,
        warmup_concurrency=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        warmup_sandbox_preparer=blocking_preparer,  # type: ignore[arg-type]
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await asyncio.wait_for(entered_preparer.wait(), timeout=2)

        async def release_after_delay() -> None:
            await asyncio.sleep(0.05)
            release_preparer.set()

        release_task = asyncio.create_task(release_after_delay())
        started = time.monotonic()
        await pool.shutdown(graceful=True)
        elapsed = time.monotonic() - started
        await release_task

        assert elapsed >= 0.04
        assert (await pool.snapshot()).lifecycle_state.value == "STOPPED"
    finally:
        release_preparer.set()
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_graceful_shutdown_restart_does_not_reuse_stop_event() -> None:
    pool = _create_pool(max_idle=0)
    await pool.start()
    first_stop_event = pool._stop_event

    try:
        await pool.shutdown(graceful=True)
        assert first_stop_event.is_set()

        await pool.start()

        assert pool._stop_event is not first_stop_event
        assert first_stop_event.is_set()
    finally:
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_user_managed_transport_is_preserved_for_pool_resources() -> None:
    transport = _AsyncTransport()
    connection_config = ConnectionConfig(transport=transport)
    manager_configs: list[ConnectionConfig] = []
    sandbox_configs: list[ConnectionConfig] = []

    class CapturingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def create(cls, *args: Any, **kwargs: Any) -> CapturingAsyncSandbox:
            sandbox_configs.append(kwargs["connection_config"])
            return cls("created-with-custom-transport")

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        manager_configs.append(config)
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=connection_config,
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=CapturingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await pool.acquire()

        assert manager_configs[0].transport is transport
        assert not manager_configs[0]._owns_transport
        assert sandbox_configs[0].transport is transport
        assert not sandbox_configs[0]._owns_transport
    finally:
        await pool.shutdown(False)


def _create_pool(
    *,
    max_idle: int,
    store: InMemoryAsyncPoolStateStore | None = None,
    manager: FakeAsyncManager | None = None,
    max_acquire_retries: int = 3,
) -> SandboxPoolAsync:
    return SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=max_idle,
        warmup_concurrency=2,
        state_store=store or InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        max_acquire_retries=max_acquire_retries,
        sandbox_manager_factory=lambda config: _manager_factory(
            manager or FakeAsyncManager()
        ),
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
    )


async def test_async_acquire_retry_next_idle_empty_raises_pool_empty() -> None:
    pool = _create_pool(max_idle=0)
    await pool.start()
    try:
        with pytest.raises(PoolEmptyException) as exc:
            await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert "RETRY_NEXT_IDLE" in str(exc.value)
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_all_stale_bounds_retries_and_raises() -> None:
    store = InMemoryAsyncPoolStateStore()
    manager = FakeAsyncManager()
    for i in range(5):
        await store.put_idle("pool", f"stale-{i}")
    pool = _create_pool(
        max_idle=0, store=store, manager=manager, max_acquire_retries=3
    )
    await pool.start()
    try:
        with pytest.raises(PoolAcquireFailedException):
            await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        counters = await store.snapshot_counters("pool")
        assert counters.idle_count == 2

        # Kills for stale candidates are fire-and-forget (Codex review: retry loop must not
        # block on slow DELETEs). Wait for the background tasks to observe all three.
        async def _killed_three() -> bool:
            return sorted(manager.killed) == ["stale-0", "stale-1", "stale-2"]

        await _eventually(_killed_three)
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_drained_mid_loop_raises_pool_acquire_failed() -> (
    None
):
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "stale-a")
    await store.put_idle("pool", "stale-b")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=5)
    await pool.start()
    try:
        with pytest.raises(PoolAcquireFailedException) as exc:
            await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert "drained" in str(exc.value)
        counters = await store.snapshot_counters("pool")
        assert counters.idle_count == 0
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_then_create_falls_through_after_exhaustion() -> (
    None
):
    FakeAsyncSandbox.reset()
    store = InMemoryAsyncPoolStateStore()
    for i in range(3):
        await store.put_idle("pool", f"stale-{i}")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=3)
    await pool.start()
    try:
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
        counters = await store.snapshot_counters("pool")
        assert counters.idle_count == 0
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_returns_first_healthy_candidate() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "stale-a")
    await store.put_idle("pool", "stale-b")
    await store.put_idle("pool", "healthy-x")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=5)
    await pool.start()
    try:
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert sandbox.id == "healthy-x"
        counters = await store.snapshot_counters("pool")
        assert counters.idle_count == 0
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_then_create_empty_falls_through_immediately() -> (
    None
):
    FakeAsyncSandbox.reset()
    pool = _create_pool(max_idle=0)
    await pool.start()
    try:
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_renew_failure_kills_remote_without_retrying() -> (
    None
):
    """Regression: renew failure against a healthy connected sandbox must NOT trigger the
    retry loop to drain more idle candidates. But the connected sandbox MUST be killed on
    the remote side, since try_take_idle already popped its id out of the pool store —
    otherwise it leaks alive-but-untracked until server-side TTL expires.
    """
    connected: list[FakeAsyncSandbox] = []

    class TrackingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def connect(
            cls, sandbox_id: str, *args: Any, **kwargs: Any
        ) -> FakeAsyncSandbox:
            sb = await super().connect(sandbox_id, *args, **kwargs)
            sb.fail_renew = True  # per-instance renew failure
            connected.append(sb)
            return sb

    store = InMemoryAsyncPoolStateStore()
    manager = FakeAsyncManager()
    for i in range(3):
        await store.put_idle("pool", f"healthy-{i}")
    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        warmup_concurrency=2,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        max_acquire_retries=5,
        sandbox_manager_factory=lambda config: _manager_factory(manager),
        sandbox_factory=TrackingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        with pytest.raises(RuntimeError, match="renew failed"):
            await pool.acquire(
                sandbox_timeout=timedelta(minutes=5),
                policy=AcquirePolicy.RETRY_NEXT_IDLE,
            )
        assert len(connected) == 1
        counters = await store.snapshot_counters("pool")
        assert counters.idle_count == 2
        assert manager.killed == []
        assert connected[0].killed, (
            "renew failure must trigger sandbox.kill() to release remote resources; "
            "otherwise the sandbox leaks alive-but-untracked until server-side TTL expiry"
        )
        assert connected[0].closed
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_does_not_block_on_slow_stale_kill() -> None:
    """Async counterpart of the sync slow-kill regression: stale-candidate kill must be
    scheduled as a background task so a slow DELETE does not stall the retry loop.
    """
    slow_kill_seconds = 2.0

    class SlowKillManager(FakeAsyncManager):
        async def kill_sandbox(self, sandbox_id: str) -> None:
            await asyncio.sleep(slow_kill_seconds)
            await super().kill_sandbox(sandbox_id)

    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "stale-a")
    await store.put_idle("pool", "stale-b")
    await store.put_idle("pool", "healthy-x")

    manager = SlowKillManager()
    pool = _create_pool(
        max_idle=0, store=store, manager=manager, max_acquire_retries=5
    )
    await pool.start()
    try:
        start = time.monotonic()
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        elapsed = time.monotonic() - start
        assert sandbox.id == "healthy-x"
        assert elapsed < slow_kill_seconds, (
            f"acquire took {elapsed:.2f}s; expected retry loop to not block on the "
            f"slow stale kill (each blocks {slow_kill_seconds:.2f}s)"
        )
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_then_create_falls_through_on_state_store_outage() -> (
    None
):
    """Regression: PoolStateStoreUnavailableException during try_take_idle must degrade to
    direct-create under RETRY_NEXT_IDLE_THEN_CREATE (per OSEP-0005).
    """
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    FakeAsyncSandbox.reset()

    class OutageStore(InMemoryAsyncPoolStateStore):
        async def try_take_idle(self, pool_name: str) -> str | None:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdle", RuntimeError("redis unavailable")
            )

        async def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
            )

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()
    try:
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
    finally:
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_raises_on_state_store_outage() -> None:
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    class OutageStore(InMemoryAsyncPoolStateStore):
        async def try_take_idle(self, pool_name: str) -> str | None:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdle", RuntimeError("redis unavailable")
            )

        async def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
            )

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()
    try:
        with pytest.raises(PoolStateStoreUnavailableException):
            await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
    finally:
        await pool.shutdown(False)


async def test_async_acquire_then_create_falls_through_when_full_state_store_outage_also_fails_namespace_check() -> (
    None
):
    """Regression for Codex round-5 P2: previously, when the full state store was down
    (Redis outage affecting *all* methods, not just try_take_idle), acquire aborted at
    the pre-loop `_ensure_pool_namespace_active` call before the fallthrough branch
    could run. RETRY_NEXT_IDLE_THEN_CREATE is documented to degrade to direct-create
    during store outages (OSEP-0005); this test proves the namespace check no longer
    breaks that guarantee.
    """
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    FakeAsyncSandbox.reset()

    class OutageStore(InMemoryAsyncPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            # Only start raising after pool.start() completes so setup still works;
            # this mirrors a real Redis instance that crashes after the pool warms.
            self._outage = False

        async def try_take_idle(self, pool_name: str) -> str | None:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdle", RuntimeError("redis unavailable")
                )
            return await super().try_take_idle(pool_name)

        async def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
                )
            return await super().try_take_idle_min_ttl(pool_name, min_remaining_ttl)  # type: ignore[arg-type]

        async def get_destroy_state(self, pool_name: str):  # type: ignore[override]
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "GetDestroyState", RuntimeError("redis unavailable")
                )
            return await super().get_destroy_state(pool_name)

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()
    store._outage = True
    try:
        sandbox = await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
    finally:
        store._outage = False
        await pool.shutdown(False)


async def test_async_acquire_retry_next_idle_raises_when_full_state_store_outage_also_fails_namespace_check() -> (
    None
):
    """Non-fallthrough counterpart: full state-store outage under RETRY_NEXT_IDLE must
    still surface PoolStateStoreUnavailableException (fail-closed)."""
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    FakeAsyncSandbox.reset()

    class OutageStore(InMemoryAsyncPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            self._outage = False

        async def try_take_idle(self, pool_name: str) -> str | None:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdle", RuntimeError("redis unavailable")
                )
            return await super().try_take_idle(pool_name)

        async def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
                )
            return await super().try_take_idle_min_ttl(pool_name, min_remaining_ttl)  # type: ignore[arg-type]

        async def get_destroy_state(self, pool_name: str):  # type: ignore[override]
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "GetDestroyState", RuntimeError("redis unavailable")
                )
            return await super().get_destroy_state(pool_name)

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    await pool.start()
    store._outage = True
    try:
        with pytest.raises(PoolStateStoreUnavailableException):
            await pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
    finally:
        store._outage = False
        await pool.shutdown(False)


async def test_async_pool_config_rejects_max_acquire_retries_below_one() -> None:
    from opensandbox.pool_types import AsyncPoolConfig

    with pytest.raises(ValueError, match="max_acquire_retries must be >= 1"):
        AsyncPoolConfig(
            pool_name="pool",
            owner_id="owner-1",
            max_idle=1,
            state_store=InMemoryAsyncPoolStateStore(),
            connection_config=ConnectionConfig(),
            creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
            max_acquire_retries=0,
        )


async def _manager_factory(manager: FakeAsyncManager) -> FakeAsyncManager:
    return manager


async def _noop_discard(_sandbox_id: str) -> None:
    return None


async def _idle_count_equals(pool: SandboxPoolAsync, expected: int) -> bool:
    return (await pool.snapshot()).idle_count == expected


async def _eventually(condition: Any, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if await condition():
            return
        await asyncio.sleep(0.01)
    raise AssertionError("condition did not become true")


class FakeAsyncManager:
    def __init__(self) -> None:
        self.killed: list[str] = []
        self.closed = False

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    async def close(self) -> None:
        self.closed = True


class FakeAsyncSandbox:
    created_count = 0
    fail_renew = False
    last_created: FakeAsyncSandbox | None = None

    def __init__(self, sandbox_id: str) -> None:
        self.id = sandbox_id
        self.renewed: list[timedelta] = []
        self.closed = False
        self.killed = False

    @classmethod
    def reset(cls) -> None:
        cls.created_count = 0
        cls.fail_renew = False
        cls.last_created = None

    @classmethod
    async def create(cls, *args: Any, **kwargs: Any) -> FakeAsyncSandbox:
        cls.created_count += 1
        sandbox = cls(f"created-{cls.created_count}")
        cls.last_created = sandbox
        return sandbox

    @classmethod
    async def connect(
        cls, sandbox_id: str, *args: Any, **kwargs: Any
    ) -> FakeAsyncSandbox:
        if sandbox_id.startswith("stale"):
            raise RuntimeError("stale sandbox")
        return cls(sandbox_id)

    async def renew(self, timeout: timedelta) -> None:
        if self.fail_renew:
            raise RuntimeError("renew failed")
        self.renewed.append(timeout)

    async def kill(self) -> None:
        self.killed = True

    async def close(self) -> None:
        self.closed = True


class SharedAsyncMaxIdleStore(InMemoryAsyncPoolStateStore):
    def __init__(self, initial_max_idle: int | None = None) -> None:
        super().__init__()
        self.max_idle_by_pool: dict[str, int] = {}
        self.set_max_idle_calls: list[tuple[str, int]] = []
        if initial_max_idle is not None:
            self.max_idle_by_pool["pool"] = initial_max_idle

    async def get_max_idle(self, pool_name: str) -> int | None:
        return self.max_idle_by_pool.get(pool_name)

    async def set_max_idle(self, pool_name: str, max_idle: int) -> None:
        self.set_max_idle_calls.append((pool_name, max_idle))
        self.max_idle_by_pool[pool_name] = max_idle


class _AsyncTransport(httpx.AsyncBaseTransport):
    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, request=request)
