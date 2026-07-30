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
    PoolDestroyedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.models.sandboxes import PlatformSpec
from opensandbox.pool import (
    AcquirePolicy,
    InMemoryPoolStateStore,
    PoolConfig,
    PoolCreationSpec,
    PooledSandboxCreateContext,
    PooledSandboxCreateReason,
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
    assert not state.is_backoff_active(
        datetime.now(timezone.utc) + timedelta(seconds=31)
    )


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
    assert not state.is_backoff_active(
        datetime.now(timezone.utc) + timedelta(seconds=31)
    )


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
        # Kill is now fire-and-forget on the warmup executor (retry loop must not block on
        # slow DELETEs) so poll briefly for the background task to observe the kill.
        _eventually(lambda: manager.killed == ["stale-1"])
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


def test_acquire_does_not_direct_create_when_pool_namespace_is_destroying() -> None:
    FakeSandbox.reset()
    store = InMemoryPoolStateStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()

    try:
        store.begin_destroy("pool", "destroyer")

        with pytest.raises(PoolDestroyedException):
            pool.acquire()
        assert FakeSandbox.created_count == 0
    finally:
        pool.shutdown(False)


def test_acquire_idle_destroy_race_raises_pool_destroyed() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "id-1")
    connected: list[FakeSandbox] = []

    class FencingSandbox(FakeSandbox):
        @classmethod
        def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any) -> FakeSandbox:
            sandbox = cls(sandbox_id)
            connected.append(sandbox)
            store.begin_destroy("pool", "destroyer")
            return sandbox

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FencingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        with pytest.raises(PoolDestroyedException):
            pool.acquire(policy=AcquirePolicy.DIRECT_CREATE)
        assert connected[0].killed
        assert connected[0].closed
    finally:
        pool.shutdown(False)


def test_acquire_stopped_destroyed_pool_raises_pool_destroyed() -> None:
    store = InMemoryPoolStateStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()
    store.begin_destroy("pool", "destroyer")
    pool.shutdown(False)

    with pytest.raises(PoolDestroyedException):
        pool.acquire()


def test_acquire_destroy_race_preserves_pool_destroyed_when_cleanup_fails() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "id-1")

    class CleanupFailingSandbox(FakeSandbox):
        @classmethod
        def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any) -> FakeSandbox:
            store.begin_destroy("pool", "destroyer")
            return cls(sandbox_id)

        def kill(self) -> None:
            raise RuntimeError("kill failed")

        def close(self) -> None:
            raise RuntimeError("close failed")

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=CleanupFailingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        with pytest.raises(PoolDestroyedException):
            pool.acquire(policy=AcquirePolicy.DIRECT_CREATE)
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


def test_acquire_direct_create_uses_sandbox_creator() -> None:
    contexts: list[PooledSandboxCreateContext] = []

    def creator(context: PooledSandboxCreateContext) -> FakeSandbox:
        contexts.append(context)
        return FakeSandbox("created-by-hook")

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(minutes=10),
        sandbox_creator=creator,
        sandbox_manager_factory=lambda config: FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FakeSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        sandbox = pool.acquire(sandbox_timeout=timedelta(minutes=5))
        fake_sandbox = cast(FakeSandbox, sandbox)

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
        assert isinstance(contexts[0].connection_config, ConnectionConfigSync)
    finally:
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
    max_acquire_retries: int = 3,
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
        max_acquire_retries=max_acquire_retries,
        sandbox_manager_factory=lambda config: manager or FakeManager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=FakeSandbox,  # type: ignore[arg-type]
    )


def test_acquire_retry_next_idle_empty_raises_pool_empty() -> None:
    pool = _create_pool(max_idle=0)
    pool.start()
    try:
        with pytest.raises(PoolEmptyException) as exc:
            pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        # Message should mention the policy, not the legacy "FAIL_FAST" string.
        assert "RETRY_NEXT_IDLE" in str(exc.value)
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_all_stale_bounds_retries_and_raises() -> None:
    store = InMemoryPoolStateStore()
    manager = FakeManager()
    # 5 stale ids seeded; retry budget of 3 must attempt exactly 3 and leave 2 behind.
    for i in range(5):
        store.put_idle("pool", f"stale-{i}")
    pool = _create_pool(max_idle=0, store=store, manager=manager, max_acquire_retries=3)
    pool.start()
    try:
        with pytest.raises(PoolAcquireFailedException):
            pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert store.snapshot_counters("pool").idle_count == 2
        # Best-effort kill fires once per attempted stale id, fire-and-forget on the warmup
        # executor. Poll for the background tasks to observe all three.
        _eventually(lambda: sorted(manager.killed) == ["stale-0", "stale-1", "stale-2"])
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_drained_mid_loop_raises_pool_acquire_failed() -> None:
    store = InMemoryPoolStateStore()
    # Only 2 stale ids but budget is 5; loop must break early and still raise
    # PoolAcquireFailedException (attempted_any=True) rather than PoolEmptyException.
    store.put_idle("pool", "stale-a")
    store.put_idle("pool", "stale-b")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=5)
    pool.start()
    try:
        with pytest.raises(PoolAcquireFailedException) as exc:
            pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert "drained" in str(exc.value)
        assert store.snapshot_counters("pool").idle_count == 0
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_then_create_falls_through_after_exhaustion() -> None:
    FakeSandbox.reset()
    store = InMemoryPoolStateStore()
    for i in range(3):
        store.put_idle("pool", f"stale-{i}")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=3)
    pool.start()
    try:
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
        assert store.snapshot_counters("pool").idle_count == 0
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_returns_first_healthy_candidate() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "stale-a")
    store.put_idle("pool", "stale-b")
    store.put_idle("pool", "healthy-x")
    pool = _create_pool(max_idle=0, store=store, max_acquire_retries=5)
    pool.start()
    try:
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        assert sandbox.id == "healthy-x"
        # Two stale entries removed; healthy one taken by acquire.
        assert store.snapshot_counters("pool").idle_count == 0
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_then_create_empty_falls_through_immediately() -> None:
    FakeSandbox.reset()
    pool = _create_pool(max_idle=0)
    pool.start()
    try:
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_renew_failure_kills_remote_without_retrying() -> None:
    """Regression: renew failure against a healthy connected sandbox must NOT trigger the
    retry loop to drain more idle candidates (retrying another idle cannot fix a lifecycle-
    API renew rejection). But the connected sandbox MUST be killed on the remote side,
    since try_take_idle already popped its id out of the pool store — otherwise it leaks
    alive-but-untracked until its server-side TTL expires.
    """
    connected: list[FakeSandbox] = []

    class TrackingSandbox(FakeSandbox):
        fail_renew_ids: bool = True  # class-level flag to survive reset()

        @classmethod
        def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any) -> FakeSandbox:
            sb = super().connect(sandbox_id, *args, **kwargs)
            sb.fail_renew = True  # per-instance renew failure
            connected.append(sb)
            return sb

    store = InMemoryPoolStateStore()
    manager = FakeManager()
    for i in range(3):
        store.put_idle("pool", f"healthy-{i}")
    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        warmup_concurrency=2,
        state_store=store,
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        reconcile_interval=timedelta(milliseconds=20),
        primary_lock_ttl=timedelta(seconds=5),
        drain_timeout=timedelta(milliseconds=50),
        max_acquire_retries=5,
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
        sandbox_factory=TrackingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        with pytest.raises(RuntimeError, match="renew failed"):
            pool.acquire(
                sandbox_timeout=timedelta(minutes=5),
                policy=AcquirePolicy.RETRY_NEXT_IDLE,
            )
        # Only ONE candidate was connected — retry did NOT drain the other two healthy idles.
        assert len(connected) == 1
        assert store.snapshot_counters("pool").idle_count == 2
        # The connected sandbox was killed best-effort via sandbox.kill() so the remote
        # resource does not leak (try_take_idle already popped its id from the store).
        # Kill is direct on the Sandbox, not routed through the pool's SandboxManager, so
        # FakeManager.killed stays empty; assert on the sandbox instance instead.
        assert manager.killed == []
        assert connected[0].killed, (
            "renew failure must trigger sandbox.kill() to release remote resources; "
            "otherwise the sandbox leaks alive-but-untracked until server-side TTL expiry"
        )
        assert connected[0].closed
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_does_not_block_on_slow_stale_kill() -> None:
    """Regression: stale-candidate kill must fire-and-forget on the warmup executor so a
    slow lifecycle-API DELETE does not stall the retry loop between candidates. Verify by
    injecting a manager whose kill_sandbox blocks for longer than would be tolerable in
    the retry path, then asserting acquire returns quickly with the healthy candidate.
    """
    slow_kill_seconds = 2.0

    class SlowKillManager(FakeManager):
        def kill_sandbox(self, sandbox_id: str) -> None:
            time.sleep(slow_kill_seconds)
            super().kill_sandbox(sandbox_id)

    store = InMemoryPoolStateStore()
    # Two stale ids ahead of a healthy id; a blocking kill on stale would add 2 * 2s = 4s
    # to acquire latency if kill were awaited inline.
    store.put_idle("pool", "stale-a")
    store.put_idle("pool", "stale-b")
    store.put_idle("pool", "healthy-x")

    manager = SlowKillManager()
    pool = _create_pool(max_idle=0, store=store, manager=manager, max_acquire_retries=5)
    pool.start()
    try:
        start = time.monotonic()
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
        elapsed = time.monotonic() - start
        assert sandbox.id == "healthy-x"
        # Generous bound but well below the 2 * slow_kill_seconds a naive inline-kill loop
        # would take (and far below the 30s lifecycle default request_timeout).
        assert elapsed < slow_kill_seconds, (
            f"acquire took {elapsed:.2f}s; expected retry loop to not block on the "
            f"slow stale kill (each blocks {slow_kill_seconds:.2f}s)"
        )
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_then_create_falls_through_on_state_store_outage() -> (
    None
):
    """Regression: PoolStateStoreUnavailableException during try_take_idle must degrade to
    direct-create under RETRY_NEXT_IDLE_THEN_CREATE (and DIRECT_CREATE), per OSEP-0005.
    Previously the exception propagated and skipped the fallback branch, making the new
    then-create policy strictly less available than documented during store outages.
    """
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    FakeSandbox.reset()

    class OutageStore(InMemoryPoolStateStore):
        def try_take_idle(self, pool_name: str) -> str | None:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdle", RuntimeError("redis unavailable")
            )

        # Override the min-ttl variant too, since the pool prefers it when
        # acquire_min_remaining_ttl > 0 (which is the default).
        def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
            )

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()
    try:
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        # Fell through to direct create.
        assert sandbox.id.startswith("created-")
    finally:
        pool.shutdown(False)


def test_acquire_retry_next_idle_raises_on_state_store_outage() -> None:
    """Complement of the fallthrough test: RETRY_NEXT_IDLE (no _THEN_CREATE) must surface
    the state store outage instead of silently direct-creating. Same guarantee applies to
    FAIL_FAST — non-fallthrough policies never degrade to direct-create.
    """
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    class OutageStore(InMemoryPoolStateStore):
        def try_take_idle(self, pool_name: str) -> str | None:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdle", RuntimeError("redis unavailable")
            )

        def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            raise PoolStateStoreUnavailableException(
                "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
            )

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()
    try:
        with pytest.raises(PoolStateStoreUnavailableException):
            pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
    finally:
        pool.shutdown(False)


def test_acquire_then_create_falls_through_when_full_state_store_outage_also_fails_namespace_check() -> (
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

    FakeSandbox.reset()

    class OutageStore(InMemoryPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            # Only start raising after pool.start() completes so setup still works;
            # this mirrors a real Redis instance that crashes after the pool warms.
            self._outage = False

        def try_take_idle(self, pool_name: str) -> str | None:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdle", RuntimeError("redis unavailable")
                )
            return super().try_take_idle(pool_name)

        def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
                )
            return super().try_take_idle_min_ttl(pool_name, min_remaining_ttl)  # type: ignore[arg-type]

        def get_destroy_state(self, pool_name: str):  # type: ignore[override]
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "GetDestroyState", RuntimeError("redis unavailable")
                )
            return super().get_destroy_state(pool_name)

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()
    store._outage = True
    try:
        sandbox = pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE_THEN_CREATE)
        assert sandbox.id.startswith("created-")
    finally:
        store._outage = False
        pool.shutdown(False)


def test_acquire_retry_next_idle_raises_when_full_state_store_outage_also_fails_namespace_check() -> (
    None
):
    """Non-fallthrough counterpart: full state-store outage under RETRY_NEXT_IDLE must
    still surface PoolStateStoreUnavailableException (fail-closed)."""
    from opensandbox.exceptions import PoolStateStoreUnavailableException

    FakeSandbox.reset()

    class OutageStore(InMemoryPoolStateStore):
        def __init__(self) -> None:
            super().__init__()
            self._outage = False

        def try_take_idle(self, pool_name: str) -> str | None:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdle", RuntimeError("redis unavailable")
                )
            return super().try_take_idle(pool_name)

        def try_take_idle_min_ttl(  # type: ignore[override]
            self, pool_name: str, min_remaining_ttl: object
        ) -> object:
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "TryTakeIdleWithMinTTL", RuntimeError("redis unavailable")
                )
            return super().try_take_idle_min_ttl(pool_name, min_remaining_ttl)  # type: ignore[arg-type]

        def get_destroy_state(self, pool_name: str):  # type: ignore[override]
            if self._outage:
                raise PoolStateStoreUnavailableException(
                    "GetDestroyState", RuntimeError("redis unavailable")
                )
            return super().get_destroy_state(pool_name)

    store = OutageStore()
    pool = _create_pool(max_idle=0, store=store)
    pool.start()
    store._outage = True
    try:
        with pytest.raises(PoolStateStoreUnavailableException):
            pool.acquire(policy=AcquirePolicy.RETRY_NEXT_IDLE)
    finally:
        store._outage = False
        pool.shutdown(False)


def test_pool_config_rejects_max_acquire_retries_below_one() -> None:
    with pytest.raises(ValueError, match="max_acquire_retries must be >= 1"):
        PoolConfig(
            pool_name="pool",
            owner_id="owner-1",
            max_idle=1,
            state_store=InMemoryPoolStateStore(),
            connection_config=ConnectionConfigSync(),
            creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
            max_acquire_retries=0,
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
