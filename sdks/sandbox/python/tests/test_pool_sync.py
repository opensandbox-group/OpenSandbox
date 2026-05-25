from __future__ import annotations

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
from typing import Any, cast

import httpx
import pytest

from opensandbox._pool_reconciler import ReconcileState, run_reconcile_tick
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.models.sandboxes import PlatformSpec
from opensandbox.pool import (
    AcquirePolicy,
    InMemoryPoolStateStore,
    PoolConfig,
    PoolCreationSpec,
)
from opensandbox.sync.pool import SandboxPoolSync


def test_degraded_backoff_caps_at_one_day() -> None:
    state = ReconcileState(degraded_threshold=1)

    for _ in range(20):
        state.record_failure("boom")

    assert state.failure_count == 20
    assert state.is_backoff_active(datetime.now(timezone.utc) + timedelta(hours=23))
    assert not state.is_backoff_active(datetime.now(timezone.utc) + timedelta(hours=25))


def test_degraded_backoff_starts_at_thirty_seconds() -> None:
    state = ReconcileState(degraded_threshold=1)

    state.record_failure("boom")

    assert state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=29))
    assert not state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=31))


def test_reconcile_batch_failures_only_advance_backoff_once() -> None:
    store = InMemoryPoolStateStore()
    config = PoolConfig(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=10,
        warmup_concurrency=10,
        state_store=store,
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    )
    state = ReconcileState(degraded_threshold=3)

    def fail_create() -> str:
        raise RuntimeError("boom")

    with ThreadPoolExecutor(max_workers=10) as executor:
        run_reconcile_tick(
            config=config,
            state_store=store,
            create_one=fail_create,
            on_discard_sandbox=lambda _sandbox_id: None,
            reconcile_state=state,
            warmup_executor=executor,
        )

    assert state.failure_count == 10
    assert state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=29))
    assert not state.is_backoff_active(datetime.now(timezone.utc) + timedelta(seconds=31))


def test_acquire_fail_fast_empty_raises_pool_empty() -> None:
    pool = _create_pool(max_idle=0)
    pool.start()
    try:
        with pytest.raises(PoolEmptyException) as exc:
            pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc.value.error.code == "POOL_EMPTY"
    finally:
        pool.shutdown(False)


def test_acquire_fail_fast_stale_idle_raises_and_kills_candidate() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "stale-1")
    manager = FakeManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager)
    pool.start()

    try:
        with pytest.raises(PoolAcquireFailedException) as exc:
            pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc.value.error.code == "POOL_ACQUIRE_FAILED"
        assert store.snapshot_counters("pool").idle_count == 0
        assert manager.killed == ["stale-1"]
    finally:
        pool.shutdown(False)


def test_acquire_direct_create_when_empty() -> None:
    FakeSandbox.reset()
    pool = _create_pool(max_idle=0)
    pool.start()

    try:
        sandbox = pool.acquire(sandbox_timeout=timedelta(minutes=5))
        fake_sandbox = cast(FakeSandbox, sandbox)
        assert sandbox.id == "created-1"
        assert fake_sandbox.renewed == [timedelta(minutes=5)]
    finally:
        pool.shutdown(False)


def test_acquire_direct_create_forwards_pool_creation_platform() -> None:
    captured_kwargs: dict[str, Any] = {}

    class CapturingSandbox(FakeSandbox):
        @classmethod
        def create(cls, *args: Any, **kwargs: Any) -> CapturingSandbox:
            captured_kwargs.update(kwargs)
            return cls("created-with-platform")

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(
            image="ubuntu:22.04",
            platform=PlatformSpec(os="linux", arch="arm64"),
        ),
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=CapturingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        pool.acquire()

        assert captured_kwargs["platform"] == PlatformSpec(os="linux", arch="arm64")
    finally:
        pool.shutdown(False)


def test_acquire_direct_create_kills_and_closes_when_renew_fails() -> None:
    FakeSandbox.reset()
    FakeSandbox.fail_renew = True
    pool = _create_pool(max_idle=0)
    pool.start()

    try:
        with pytest.raises(RuntimeError, match="renew failed"):
            pool.acquire(sandbox_timeout=timedelta(minutes=5))
        assert FakeSandbox.last_created is not None
        assert FakeSandbox.last_created.killed
        assert FakeSandbox.last_created.closed
    finally:
        FakeSandbox.fail_renew = False
        pool.shutdown(False)


def test_acquire_when_stopped_raises_pool_not_running() -> None:
    pool = _create_pool(max_idle=0)

    with pytest.raises(PoolNotRunningException) as exc:
        pool.acquire(policy=AcquirePolicy.FAIL_FAST)

    assert exc.value.error.code == "POOL_NOT_RUNNING"


def test_start_warms_idle_and_resize_zero_shrinks() -> None:
    FakeSandbox.reset()
    store = InMemoryPoolStateStore()
    manager = FakeManager()
    pool = _create_pool(max_idle=2, store=store, manager=manager)
    pool.start()

    try:
        _eventually(lambda: pool.snapshot().idle_count == 2)
        pool.resize(0)
        _eventually(lambda: pool.snapshot().idle_count == 0)
        assert len(manager.killed) >= 2
    finally:
        pool.shutdown(False)


def test_start_overwrites_shared_max_idle_with_user_config() -> None:
    store = SharedMaxIdleStore(initial_max_idle=0)
    pool = _create_pool(max_idle=3, store=store)
    pool.start()

    try:
        assert store.max_idle_by_pool["pool"] == 3
        assert store.set_max_idle_calls == [("pool", 3)]
        assert pool.snapshot().max_idle == 3
    finally:
        pool.shutdown(False)


def test_resize_only_updates_target_without_immediate_reconcile_trigger() -> None:
    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(seconds=10),
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FakeSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    calls = 0

    def record_reconcile() -> None:
        nonlocal calls
        calls += 1

    pool._run_reconcile_tick = record_reconcile  # type: ignore[method-assign]
    try:
        pool.resize(1)
        time.sleep(0.05)

        assert calls == 0
        assert pool.snapshot().max_idle == 1
    finally:
        pool.shutdown(False)


def test_graceful_shutdown_waits_for_running_warmup_before_stop() -> None:
    FakeSandbox.reset()
    entered_preparer = threading.Event()
    release_preparer = threading.Event()

    def blocking_preparer(sandbox: FakeSandbox) -> None:
        entered_preparer.set()
        release_preparer.wait(timeout=5)

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=1,
        warmup_concurrency=1,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        warmup_sandbox_preparer=blocking_preparer,  # type: ignore[arg-type]
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FakeSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        assert entered_preparer.wait(timeout=2)

        def release_after_delay() -> None:
            time.sleep(0.05)
            release_preparer.set()

        release_thread = threading.Thread(target=release_after_delay)
        release_thread.start()
        started = time.monotonic()
        pool.shutdown(graceful=True)
        elapsed = time.monotonic() - started
        release_thread.join(timeout=1)

        assert elapsed >= 0.04
        assert pool.snapshot().lifecycle_state.value == "STOPPED"
    finally:
        release_preparer.set()
        pool.shutdown(False)


def test_graceful_shutdown_restart_does_not_reuse_stop_event() -> None:
    pool = _create_pool(max_idle=0)
    pool.start()
    first_stop_event = pool._stop_event

    try:
        pool.shutdown(graceful=True)
        assert first_stop_event.is_set()

        pool.start()

        assert pool._stop_event is not first_stop_event
        assert first_stop_event.is_set()
    finally:
        pool.shutdown(False)


def test_user_managed_transport_is_preserved_for_pool_resources() -> None:
    transport = _SyncTransport()
    connection_config = ConnectionConfigSync(transport=transport)
    manager_configs: list[ConnectionConfigSync] = []
    sandbox_configs: list[ConnectionConfigSync] = []

    class CapturingSandbox(FakeSandbox):
        @classmethod
        def create(cls, *args: Any, **kwargs: Any) -> CapturingSandbox:
            sandbox_configs.append(kwargs["connection_config"])
            return cls("created-with-custom-transport")

    def manager_factory(config: ConnectionConfigSync) -> FakeManager:
        manager_configs.append(config)
        return FakeManager()

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryPoolStateStore(),
        connection_config=connection_config,
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type,return-value]
        sandbox_factory=CapturingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        pool.acquire()

        assert manager_configs[0].transport is transport
        assert not manager_configs[0]._owns_transport
        assert sandbox_configs[0].transport is transport
        assert not sandbox_configs[0]._owns_transport
    finally:
        pool.shutdown(False)


def _create_pool(
    *,
    max_idle: int,
    store: InMemoryPoolStateStore | None = None,
    manager: FakeManager | None = None,
) -> SandboxPoolSync:
    return SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=max_idle,
        warmup_concurrency=2,
        state_store=store or InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        sandbox_manager_factory=lambda config: manager or FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FakeSandbox,  # type: ignore[arg-type]
    )


def _eventually(condition: Any, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return
        time.sleep(0.01)
    raise AssertionError("condition did not become true")


class FakeManager:
    def __init__(self) -> None:
        self.killed: list[str] = []
        self.closed = False

    def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    def close(self) -> None:
        self.closed = True


class FakeSandbox:
    created_count = 0
    fail_renew = False
    last_created: FakeSandbox | None = None

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
    def create(cls, *args: Any, **kwargs: Any) -> FakeSandbox:
        cls.created_count += 1
        sandbox = cls(f"created-{cls.created_count}")
        cls.last_created = sandbox
        return sandbox

    @classmethod
    def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any) -> FakeSandbox:
        if sandbox_id.startswith("stale"):
            raise RuntimeError("stale sandbox")
        return cls(sandbox_id)

    def renew(self, timeout: timedelta) -> None:
        if self.fail_renew:
            raise RuntimeError("renew failed")
        self.renewed.append(timeout)

    def kill(self) -> None:
        self.killed = True

    def close(self) -> None:
        self.closed = True


class SharedMaxIdleStore(InMemoryPoolStateStore):
    def __init__(self, initial_max_idle: int | None = None) -> None:
        super().__init__()
        self.max_idle_by_pool: dict[str, int] = {}
        self.set_max_idle_calls: list[tuple[str, int]] = []
        if initial_max_idle is not None:
            self.max_idle_by_pool["pool"] = initial_max_idle

    def get_max_idle(self, pool_name: str) -> int | None:
        return self.max_idle_by_pool.get(pool_name)

    def set_max_idle(self, pool_name: str, max_idle: int) -> None:
        self.set_max_idle_calls.append((pool_name, max_idle))
        self.max_idle_by_pool[pool_name] = max_idle


class _SyncTransport(httpx.BaseTransport):
    def handle_request(self, request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, request=request)
