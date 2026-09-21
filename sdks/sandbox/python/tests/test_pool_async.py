# Copyright 2026 The OpenSandbox Authors
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
from datetime import timedelta
from typing import Any, cast

import httpx
import pytest

from opensandbox._async_pool_reconciler import run_async_reconcile_tick
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
from opensandbox.pool_types import PoolLifecycleState


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
async def test_release_all_idle_preserves_serial_behavior() -> None:
    store = InMemoryAsyncPoolStateStore()
    for index in range(3):
        await store.put_idle("pool", f"idle-{index}")

    class TrackingManager(FakeAsyncManager):
        def __init__(self) -> None:
            super().__init__()
            self.active = 0
            self.max_active = 0

        async def kill_sandbox(self, sandbox_id: str) -> None:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
            await asyncio.sleep(0)
            self.killed.append(sandbox_id)
            self.active -= 1

    manager = TrackingManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager)

    released = await pool.release_all_idle()

    assert released == 3
    assert manager.max_active == 1
    assert len(manager.killed) == 3
    assert manager.closed


@pytest.mark.asyncio
async def test_release_all_idle_parallel_rejects_nonpositive_workers() -> None:
    pool = _create_pool(max_idle=0)

    with pytest.raises(ValueError, match="max_workers must be positive"):
        await pool.release_all_idle_parallel(0)


@pytest.mark.asyncio
async def test_release_all_idle_bounds_kills_and_cleans_up_before_store_failure() -> (
    None
):
    class FailingStore(InMemoryAsyncPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            self.takes = 0

        async def try_take_idle(self, pool_name: str) -> str | None:
            if self.takes == 55:
                raise RuntimeError("injected store failure")
            self.takes += 1
            return await super().try_take_idle(pool_name)

    store = FailingStore()
    for index in range(55):
        await store.put_idle("pool", f"idle-{index}")

    class ConcurrentManager(FakeAsyncManager):
        def __init__(self) -> None:
            super().__init__()
            self.active = 0
            self.max_active = 0
            self.ready = asyncio.Event()

        async def kill_sandbox(self, sandbox_id: str) -> None:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
            if self.active == 50:
                self.ready.set()
            await self.ready.wait()
            self.killed.append(sandbox_id)
            self.active -= 1
            if sandbox_id == "idle-0":
                raise RuntimeError("injected kill failure")

    manager = ConcurrentManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager)

    with pytest.raises(RuntimeError, match="injected store failure"):
        await asyncio.wait_for(pool.release_all_idle_parallel(), timeout=2)

    assert manager.max_active == 50
    assert len(manager.killed) == 55
    assert (await store.snapshot_counters("pool")).idle_count == 0
    assert manager.closed


@pytest.mark.asyncio
async def test_release_all_idle_parallel_finishes_kills_before_cancellation() -> None:
    store = InMemoryAsyncPoolStateStore()
    for index in range(55):
        await store.put_idle("pool", f"idle-{index}")

    class BlockingManager(FakeAsyncManager):
        def __init__(self) -> None:
            super().__init__()
            self.started = 0
            self.first_batch_started = asyncio.Event()
            self.release_kills = asyncio.Event()

        async def kill_sandbox(self, sandbox_id: str) -> None:
            self.started += 1
            if self.started == 50:
                self.first_batch_started.set()
            await self.release_kills.wait()
            self.killed.append(sandbox_id)

    manager = BlockingManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager)
    release_task = asyncio.create_task(pool.release_all_idle_parallel())
    await asyncio.wait_for(manager.first_batch_started.wait(), timeout=2)

    try:
        release_task.cancel()
        await asyncio.sleep(0)
        assert not release_task.done()
        manager.release_kills.set()
        with pytest.raises(asyncio.CancelledError):
            await release_task
    finally:
        manager.release_kills.set()
        await asyncio.gather(release_task, return_exceptions=True)

    assert len(manager.killed) == 55
    assert (await store.snapshot_counters("pool")).idle_count == 0
    assert manager.closed


@pytest.mark.asyncio
async def test_async_reconcile_submits_at_most_warmup_create_qps() -> None:
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
    submitted: list[int] = []
    await run_async_reconcile_tick(
        config=config,
        state_store=store,
        on_discard_sandbox=_noop_discard,
        warming_count=0,
        submit_warmups=submitted.append,
    )
    assert submitted == [10]


@pytest.mark.asyncio
async def test_async_reconcile_accounts_for_warming_before_admission() -> None:
    store = InMemoryAsyncPoolStateStore()
    config = AsyncPoolConfig(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=2,
        warmup_concurrency=2,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    )
    submitted: list[int] = []
    await run_async_reconcile_tick(
        config=config,
        state_store=store,
        on_discard_sandbox=_noop_discard,
        warming_count=1,
        submit_warmups=submitted.append,
    )
    assert submitted == [1]


@pytest.mark.asyncio
async def test_async_reconcile_returns_without_waiting_for_submitted_warmup() -> None:
    store = InMemoryAsyncPoolStateStore()
    config = AsyncPoolConfig(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=2,
        warmup_concurrency=2,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
    )
    called = asyncio.Event()

    def submit(count: int) -> None:
        assert count == 2
        called.set()

    await run_async_reconcile_tick(
        config=config,
        state_store=store,
        on_discard_sandbox=_noop_discard,
        warming_count=0,
        submit_warmups=submit,
    )
    assert called.is_set()


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
async def test_async_acquire_does_not_direct_create_when_pool_namespace_is_destroying() -> (
    None
):
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
async def test_async_acquire_destroy_race_preserves_pool_destroyed_when_cleanup_fails() -> (
    None
):
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
@pytest.mark.parametrize("source", ["idle", "direct", "creator"])
@pytest.mark.parametrize("phase", ["renew", "fence"])
async def test_acquire_cancellation_finishes_owned_sandbox_cleanup(
    source: str,
    phase: str,
) -> None:
    entered = asyncio.Event()
    kill_started = asyncio.Event()
    finish_kill = asyncio.Event()
    never = asyncio.Event()

    class BlockingSandbox(FakeAsyncSandbox):
        def __init__(self) -> None:
            super().__init__("owned-sandbox")
            self.client = httpx.AsyncClient()

        async def renew(self, timeout: timedelta) -> None:
            if phase == "renew":
                entered.set()
                await never.wait()

        async def kill(self) -> None:
            kill_started.set()
            await finish_kill.wait()
            self.killed = True

        async def close(self) -> None:
            await self.client.aclose()
            self.closed = True

    sandbox = BlockingSandbox()

    class BlockingStore(InMemoryAsyncPoolStateStore):
        async def get_destroy_state(self, pool_name: str) -> Any:
            if phase == "fence" and returned_sandbox:
                entered.set()
                await never.wait()
            return await super().get_destroy_state(pool_name)

    returned_sandbox = False

    async def provide(*args: Any, **kwargs: Any) -> BlockingSandbox:
        nonlocal returned_sandbox
        returned_sandbox = True
        return sandbox

    class Factory:
        create = staticmethod(provide)
        connect = staticmethod(provide)

    store = BlockingStore()
    pool = SandboxPoolAsync(
        pool_name="pool",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_creator=provide if source == "creator" else None,
        sandbox_factory=Factory,  # type: ignore[arg-type]
        sandbox_manager_factory=lambda config: _manager_factory(FakeAsyncManager()),
    )
    await pool.start()
    if source == "idle":
        await store.put_idle("pool", sandbox.id)
    acquisition = asyncio.create_task(
        pool.acquire(sandbox_timeout=timedelta(minutes=5))
    )
    try:
        await asyncio.wait_for(entered.wait(), timeout=2)
        acquisition.cancel()
        await asyncio.wait_for(kill_started.wait(), timeout=2)
        acquisition.cancel()
        finish_kill.set()
        with pytest.raises(asyncio.CancelledError):
            await acquisition
        assert sandbox.killed
        assert sandbox.client.is_closed
        assert (await store.snapshot_counters("pool")).idle_count == 0
    finally:
        returned_sandbox = False
        finish_kill.set()
        acquisition.cancel()
        await asyncio.gather(acquisition, return_exceptions=True)
        await sandbox.client.aclose()
        await pool.shutdown(False)


@pytest.mark.asyncio
async def test_async_direct_create_failure_after_run_retired_is_pool_not_running() -> (
    None
):
    pool = _create_pool(max_idle=0)
    await pool.start()

    async def fail_after_retire(*args: Any, **kwargs: Any) -> FakeAsyncSandbox:
        pool._lifecycle_state = PoolLifecycleState.STOPPED
        raise RuntimeError("transport closed during shutdown")

    pool._direct_create = fail_after_retire  # type: ignore[method-assign]
    try:
        with pytest.raises(PoolNotRunningException) as raised:
            await pool.acquire()
        assert isinstance(raised.value.__cause__, RuntimeError)
    finally:
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


@pytest.mark.asyncio
async def test_async_default_warmup_owns_transport_while_manager_uses_shared(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    manager_configs: list[ConnectionConfig] = []
    sandbox_configs: list[ConnectionConfig] = []
    transport_limits: dict[str, object] = {}
    original_with_transport = ConnectionConfig.with_transport_if_missing

    def capture_transport_limits(
        config: ConnectionConfig, **kwargs: object
    ) -> ConnectionConfig:
        transport_limits.update(kwargs)
        return original_with_transport(config, **kwargs)  # type: ignore[arg-type]

    monkeypatch.setattr(
        ConnectionConfig, "with_transport_if_missing", capture_transport_limits
    )

    class CapturingAsyncSandbox(FakeAsyncSandbox):
        @classmethod
        async def create(cls, *args: Any, **kwargs: Any) -> CapturingAsyncSandbox:
            sandbox_configs.append(kwargs["connection_config"])
            return cls("created-with-shared-transport")

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        manager_configs.append(config)
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=CapturingAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await _eventually(lambda: _idle_count_equals(pool, 1))
        assert manager_configs[0].transport is not None
        assert transport_limits["max_connections"] is None
        assert transport_limits["max_keepalive_connections"] == 128
        assert sandbox_configs[0].transport is None
        assert manager_configs[0]._owns_transport
        assert sandbox_configs[0]._owns_transport
        assert sandbox_configs[0].retry_policy.max_retries == 0
        another_warmup_config = pool._connection_for_warmup_create()
        assert another_warmup_config is not sandbox_configs[0]
        assert another_warmup_config.transport is None
        assert another_warmup_config._owns_transport
    finally:
        await pool.shutdown(False)


async def test_async_staged_warmup_runs_in_order() -> None:
    events: list[str] = []

    class StagedSandbox(FakeAsyncSandbox):
        @classmethod
        async def create(cls, *args: Any, **kwargs: Any) -> StagedSandbox:
            assert kwargs["skip_health_check"] is True
            events.append("create")
            return cls("staged-1")

        async def is_healthy(self) -> bool:
            events.append("readiness")
            return True

        async def renew(self, timeout: timedelta) -> None:
            events.append("renew")
            await super().renew(timeout)

    async def prepare(sandbox: FakeAsyncSandbox) -> None:
        events.append("prepare")

    async def post_prepare(sandbox: FakeAsyncSandbox) -> bool:
        events.append("post-prepare")
        return True

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="staged",
        max_idle=1,
        warmup_create_qps=1,
        warmup_concurrency=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        warmup_sandbox_preparer=prepare,  # type: ignore[arg-type]
        warmup_post_prepare_health_check=post_prepare,  # type: ignore[arg-type]
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=StagedSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await _eventually(lambda: _idle_count_equals(pool, 1))
        assert events[:5] == [
            "create",
            "readiness",
            "prepare",
            "post-prepare",
            "renew",
        ]
    finally:
        await pool.shutdown(False)


async def test_async_warmup_polling_delay_does_not_hold_concurrency_slot() -> None:
    created = 0
    attempts: dict[str, int] = {}
    first_round: list[str] = []

    class PollingSandbox(FakeAsyncSandbox):
        @classmethod
        async def create(cls, *args: Any, **kwargs: Any) -> PollingSandbox:
            nonlocal created
            created += 1
            return cls(f"polling-{created}")

    async def health(sandbox: FakeAsyncSandbox) -> bool:
        attempts[sandbox.id] = attempts.get(sandbox.id, 0) + 1
        if attempts[sandbox.id] == 1:
            first_round.append(sandbox.id)
        return attempts[sandbox.id] >= 2

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="polling-slots",
        max_idle=2,
        warmup_create_qps=2,
        warmup_concurrency=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        warmup_health_check=health,  # type: ignore[arg-type]
        warmup_health_check_polling_interval=timedelta(milliseconds=100),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=PollingSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await _eventually(lambda: _idle_count_equals(pool, 2))
        assert len(set(first_round[:2])) == 2
    finally:
        await pool.shutdown(False)


async def test_async_primary_heartbeat_continues_while_preparer_is_blocked() -> None:
    class CountingStore(InMemoryAsyncPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            self.renew_calls = 0

        async def renew_primary_lock(
            self, pool_name: str, owner_id: str, ttl: timedelta
        ) -> bool:
            self.renew_calls += 1
            return await super().renew_primary_lock(pool_name, owner_id, ttl)

    store = CountingStore()
    entered = asyncio.Event()
    release = asyncio.Event()

    async def prepare(sandbox: FakeAsyncSandbox) -> None:
        entered.set()
        await release.wait()

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="heartbeat",
        owner_id="owner-1",
        max_idle=1,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        primary_lock_ttl=timedelta(milliseconds=90),
        warmup_sandbox_preparer=prepare,  # type: ignore[arg-type]
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=FakeAsyncSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        await asyncio.wait_for(entered.wait(), timeout=2)
        await asyncio.sleep(0.2)
        assert store.renew_calls >= 2
        release.set()
        await _eventually(lambda: _idle_count_equals(pool, 1))
    finally:
        release.set()
        await pool.shutdown(False)


async def test_async_retired_acquire_cannot_consume_restarted_run_idle() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "old-run")
    entered = asyncio.Event()
    release = asyncio.Event()

    class BlockingFactory(FakeAsyncSandbox):
        @classmethod
        async def connect(
            cls, sandbox_id: str, *args: Any, **kwargs: Any
        ) -> FakeAsyncSandbox:
            if sandbox_id == "old-run":
                entered.set()
                await release.wait()
                raise RuntimeError("old candidate failed")
            return cls(sandbox_id)

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        return FakeAsyncManager()

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        max_acquire_retries=2,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=BlockingFactory,  # type: ignore[arg-type]
    )
    await pool.start()
    acquire = asyncio.create_task(pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE))
    await asyncio.wait_for(entered.wait(), timeout=1)
    await pool.shutdown(False)
    await pool.start()
    await store.put_idle("pool", "new-run")
    release.set()
    with pytest.raises(PoolNotRunningException):
        await asyncio.wait_for(acquire, timeout=2)
    entries = await store.snapshot_idle_entries("pool")
    assert entries[0].sandbox_id == "new-run"
    await pool.shutdown(False)


async def test_async_acquire_assertion_error_cleans_popped_idle() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "broken-check")
    manager = FakeAsyncManager()

    class AssertionFactory(FakeAsyncSandbox):
        @classmethod
        async def connect(
            cls, sandbox_id: str, *args: Any, **kwargs: Any
        ) -> FakeAsyncSandbox:
            raise AssertionError("user health check failed")

    async def manager_factory(config: ConnectionConfig) -> FakeAsyncManager:
        return manager

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type]
        sandbox_factory=AssertionFactory,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        with pytest.raises(AssertionError, match="user health check failed"):
            await pool.acquire(policy=AcquirePolicy.FAIL_FAST)

        async def killed() -> bool:
            return manager.killed == ["broken-check"]

        await _eventually(killed)
        assert (await store.snapshot_counters("pool")).idle_count == 0
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


async def test_async_acquire_retry_next_idle_all_stale_bounds_retries_and_raises() -> (
    None
):
    store = InMemoryAsyncPoolStateStore()
    manager = FakeAsyncManager()
    for i in range(5):
        await store.put_idle("pool", f"stale-{i}")
    pool = _create_pool(max_idle=0, store=store, manager=manager, max_acquire_retries=3)
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


async def test_async_acquire_retry_next_idle_does_not_block_on_slow_stale_kill() -> (
    None
):
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
    pool = _create_pool(max_idle=0, store=store, manager=manager, max_acquire_retries=5)
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

    async def is_healthy(self) -> bool:
        return True

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
