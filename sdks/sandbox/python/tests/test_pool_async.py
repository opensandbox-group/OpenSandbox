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
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.models.sandboxes import PlatformSpec
from opensandbox.pool import (
    AcquirePolicy,
    AsyncPoolConfig,
    InMemoryAsyncPoolStateStore,
    PoolCreationSpec,
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
    assert not state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=31))


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
        assert manager.killed == ["stale-1"]
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
async def test_async_resize_only_updates_target_without_immediate_reconcile_trigger() -> None:
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
        sandbox_manager_factory=lambda config: _manager_factory(
            manager or FakeAsyncManager()
        ),
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
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
    async def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any) -> FakeAsyncSandbox:
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
