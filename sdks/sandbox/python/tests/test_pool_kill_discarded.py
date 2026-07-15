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
"""Tests for the kill-discarded-alive path on the pool facades.

Verifies the two contracts surfaced by review on PR #986:
- ``_kill_sandbox_best_effort`` returns ``True`` only on a confirmed kill, so the
  ``logger.debug("Killed ...")`` line in ``_kill_discarded_alive`` cannot fire on failure.
- ``SandboxPoolAsync._kill_discarded_alive`` runs its per-ID kills concurrently via
  ``asyncio.gather`` instead of serially blocking ``acquire()``.
"""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta
from typing import Any

import pytest

from opensandbox.config.connection import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.pool import (
    InMemoryAsyncPoolStateStore,
    InMemoryPoolStateStore,
    PoolCreationSpec,
    PoolLifecycleState,
)
from opensandbox.pool_async import SandboxPoolAsync
from opensandbox.sync.pool import SandboxPoolSync


class _RecordingSyncManager:
    """Sync manager fake that tracks kills and can simulate failures."""

    def __init__(self, fail_for: set[str] | None = None) -> None:
        self.killed: list[str] = []
        self._fail_for = fail_for or set()

    def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)
        if sandbox_id in self._fail_for:
            raise RuntimeError(f"simulated kill failure for {sandbox_id}")

    def close(self) -> None:  # pragma: no cover - SandboxPoolSync may call on shutdown
        return None


class _RecordingAsyncManager:
    """Async manager fake recording call timing so we can detect serial vs parallel kills."""

    def __init__(self, per_call_delay: float = 0.05) -> None:
        self.killed: list[str] = []
        self._delay = per_call_delay
        self._in_flight = 0
        self.max_in_flight = 0

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self._in_flight += 1
        self.max_in_flight = max(self.max_in_flight, self._in_flight)
        try:
            await asyncio.sleep(self._delay)
            self.killed.append(sandbox_id)
        finally:
            self._in_flight -= 1

    async def close(self) -> None:  # pragma: no cover
        return None


def _build_sync_pool(manager: _RecordingSyncManager) -> SandboxPoolSync:
    return SandboxPoolSync(
        pool_name="kill-pool",
        max_idle=0,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
    )


def _build_async_pool(manager: _RecordingAsyncManager) -> SandboxPoolAsync:
    async def _factory(_: ConnectionConfig) -> _RecordingAsyncManager:
        return manager

    return SandboxPoolAsync(
        pool_name="kill-pool",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=_factory,  # type: ignore[arg-type]
    )


def test_sync_kill_best_effort_returns_true_on_success() -> None:
    manager = _RecordingSyncManager()
    pool = _build_sync_pool(manager)
    pool._sandbox_manager = manager  # type: ignore[assignment]

    assert pool._kill_sandbox_best_effort("sandbox-1") is True
    assert manager.killed == ["sandbox-1"]


def test_sync_kill_best_effort_returns_false_on_failure() -> None:
    manager = _RecordingSyncManager(fail_for={"sandbox-1"})
    pool = _build_sync_pool(manager)
    pool._sandbox_manager = manager  # type: ignore[assignment]

    assert pool._kill_sandbox_best_effort("sandbox-1") is False
    # Manager was called but the failure was swallowed.
    assert manager.killed == ["sandbox-1"]


def test_sync_kill_best_effort_returns_false_when_manager_missing() -> None:
    manager = _RecordingSyncManager()
    pool = _build_sync_pool(manager)
    # Pool never started → _sandbox_manager is None.
    assert pool._sandbox_manager is None
    assert pool._kill_sandbox_best_effort("sandbox-1") is False


def test_sync_kill_discarded_alive_logs_only_on_success(
    caplog: pytest.LogCaptureFixture,
) -> None:
    manager = _RecordingSyncManager(fail_for={"sandbox-fail"})
    pool = _build_sync_pool(manager)
    pool._sandbox_manager = manager  # type: ignore[assignment]

    with caplog.at_level(logging.DEBUG, logger="opensandbox.sync.pool"):
        pool._kill_discarded_alive(
            pool_name="kill-pool",
            sandbox_ids=("sandbox-ok", "sandbox-fail"),
            source="acquire",
        )

    debug_messages = [
        record.message for record in caplog.records if record.levelno == logging.DEBUG
    ]
    assert any("sandbox-ok" in msg for msg in debug_messages), (
        f"expected debug log for successful kill, got {debug_messages}"
    )
    assert not any("Killed near-expiry idle sandbox" in msg and "sandbox-fail" in msg for msg in debug_messages), (
        f"failed kill should not produce 'Killed' debug log, got {debug_messages}"
    )


@pytest.mark.asyncio
async def test_async_kill_discarded_alive_runs_kills_concurrently() -> None:
    manager = _RecordingAsyncManager(per_call_delay=0.05)
    pool = _build_async_pool(manager)
    pool._sandbox_manager = manager  # type: ignore[assignment]

    ids = ("a", "b", "c", "d", "e")
    start = asyncio.get_event_loop().time()
    await pool._kill_discarded_alive(
        pool_name="kill-pool", sandbox_ids=ids, source="acquire"
    )
    elapsed = asyncio.get_event_loop().time() - start

    # Serial would take ~5 * 0.05 = 0.25s; parallel should finish well below that.
    assert elapsed < 0.15, (
        f"kills appear to run serially (took {elapsed:.3f}s for {len(ids)} ids); "
        "expected asyncio.gather to overlap them"
    )
    assert manager.max_in_flight >= 2, (
        f"expected concurrent kills, max_in_flight={manager.max_in_flight}"
    )
    assert set(manager.killed) == set(ids)


@pytest.mark.asyncio
async def test_async_kill_best_effort_returns_false_on_failure(
    caplog: pytest.LogCaptureFixture,
) -> None:
    class FailingAsyncManager:
        def __init__(self) -> None:
            self.attempted: list[str] = []

        async def kill_sandbox(self, sandbox_id: str) -> None:
            self.attempted.append(sandbox_id)
            raise RuntimeError("simulated async failure")

        async def close(self) -> None:  # pragma: no cover
            return None

    manager: Any = FailingAsyncManager()
    pool = SandboxPoolAsync(
        pool_name="kill-pool",
        max_idle=0,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda _: _async_returns(manager),  # type: ignore[arg-type]
    )
    pool._sandbox_manager = manager

    with caplog.at_level(logging.DEBUG, logger="opensandbox.pool_async"):
        await pool._kill_discarded_alive(
            pool_name="kill-pool", sandbox_ids=("a",), source="acquire"
        )

    assert manager.attempted == ["a"]
    debug_messages = [
        record.message for record in caplog.records if record.levelno == logging.DEBUG
    ]
    assert not any("Killed near-expiry idle sandbox" in msg for msg in debug_messages), (
        f"failed async kill should not produce 'Killed' debug log, got {debug_messages}"
    )


async def _async_returns(value: Any) -> Any:
    return value


def test_sync_schedule_kill_discarded_alive_does_not_block_caller() -> None:
    """``acquire`` must not pay for kill RPC time. ``_schedule_kill_discarded_alive``
    submits the cleanup to the warmup executor (or runs inline only when no executor
    is available). When an executor is present, the call returns immediately.
    """
    import time
    from concurrent.futures import ThreadPoolExecutor

    class SlowSyncManager:
        def __init__(self, delay: float) -> None:
            self.killed: list[str] = []
            self._delay = delay

        def kill_sandbox(self, sandbox_id: str) -> None:
            time.sleep(self._delay)
            self.killed.append(sandbox_id)

        def close(self) -> None:  # pragma: no cover
            return None

    manager = SlowSyncManager(delay=0.1)
    pool = _build_sync_pool(manager)  # type: ignore[arg-type]
    pool._sandbox_manager = manager  # type: ignore[assignment]
    pool._warmup_executor = ThreadPoolExecutor(max_workers=2)
    try:
        ids = ("a", "b", "c")
        start = time.monotonic()
        pool._schedule_kill_discarded_alive("kill-pool", ids, source="acquire")
        elapsed = time.monotonic() - start

        # Should return immediately — well below the slowest single kill (0.1s).
        assert elapsed < 0.05, f"_schedule_kill_discarded_alive blocked for {elapsed:.3f}s"

        # And the kills do happen, just on the executor.
        deadline = time.monotonic() + 1.0
        while sorted(manager.killed) != sorted(ids) and time.monotonic() < deadline:
            time.sleep(0.02)
        assert sorted(manager.killed) == sorted(ids)
    finally:
        pool._warmup_executor.shutdown(wait=True)


class _RenewTrackingSyncSandbox:
    """Minimal SandboxSync stand-in that records every ``renew`` call."""

    last_instance: _RenewTrackingSyncSandbox | None = None

    def __init__(self, sandbox_id: str = "warm-1") -> None:
        self.id = sandbox_id
        self.renewed: list[timedelta] = []
        self.killed = False
        self.closed = False
        type(self).last_instance = self

    @classmethod
    def create(cls, *args: Any, **kwargs: Any) -> _RenewTrackingSyncSandbox:
        del args, kwargs
        return cls("warm-1")

    def renew(self, timeout: timedelta) -> None:
        self.renewed.append(timeout)

    def kill(self) -> None:
        self.killed = True

    def close(self) -> None:
        self.closed = True


def test_sync_create_one_sandbox_renews_before_returning_id() -> None:
    """Warmup pipeline must renew the sandbox to ``idle_timeout`` after preparer runs and
    before returning the id to the reconciler. Otherwise the store stamps an expiry the
    server-side TTL has already partially elapsed against, and ``acquire_min_remaining_ttl``
    overestimates remaining TTL by the warmup duration.
    """
    _RenewTrackingSyncSandbox.last_instance = None

    class _Manager:
        def kill_sandbox(self, sandbox_id: str) -> None:  # pragma: no cover
            return None

        def close(self) -> None:  # pragma: no cover
            return None

    pool = SandboxPoolSync(
        pool_name="warm-renew",
        max_idle=1,
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(minutes=5),
        sandbox_manager_factory=lambda _: _Manager(),  # type: ignore[arg-type,return-value]
        sandbox_factory=_RenewTrackingSyncSandbox,  # type: ignore[arg-type]
    )

    # _create_one_sandbox short-circuits unless the pool is running.
    pool._lifecycle_state = PoolLifecycleState.RUNNING
    sandbox_id = pool._create_one_sandbox()
    assert sandbox_id == "warm-1"
    assert _RenewTrackingSyncSandbox.last_instance is not None
    assert _RenewTrackingSyncSandbox.last_instance.renewed == [timedelta(minutes=5)], (
        "expected renew([5min]) before putIdle, got "
        f"{_RenewTrackingSyncSandbox.last_instance.renewed}"
    )


class _RenewTrackingAsyncSandbox:
    last_instance: _RenewTrackingAsyncSandbox | None = None

    def __init__(self, sandbox_id: str = "warm-1") -> None:
        self.id = sandbox_id
        self.renewed: list[timedelta] = []
        self.killed = False
        self.closed = False
        type(self).last_instance = self

    @classmethod
    async def create(cls, *args: Any, **kwargs: Any) -> _RenewTrackingAsyncSandbox:
        del args, kwargs
        return cls("warm-1")

    async def renew(self, timeout: timedelta) -> None:
        self.renewed.append(timeout)

    async def kill(self) -> None:
        self.killed = True

    async def close(self) -> None:
        self.closed = True


@pytest.mark.asyncio
async def test_async_create_one_sandbox_renews_before_returning_id() -> None:
    _RenewTrackingAsyncSandbox.last_instance = None

    class _AsyncManager:
        async def kill_sandbox(self, sandbox_id: str) -> None:  # pragma: no cover
            return None

        async def close(self) -> None:  # pragma: no cover
            return None

    async def _factory(_: Any) -> _AsyncManager:
        return _AsyncManager()

    pool = SandboxPoolAsync(
        pool_name="warm-renew",
        max_idle=1,
        state_store=InMemoryAsyncPoolStateStore(),
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        idle_timeout=timedelta(minutes=5),
        sandbox_manager_factory=_factory,  # type: ignore[arg-type]
        sandbox_factory=_RenewTrackingAsyncSandbox,  # type: ignore[arg-type]
    )

    pool._lifecycle_state = PoolLifecycleState.RUNNING
    sandbox_id = await pool._create_one_sandbox()
    assert sandbox_id == "warm-1"
    assert _RenewTrackingAsyncSandbox.last_instance is not None
    assert _RenewTrackingAsyncSandbox.last_instance.renewed == [timedelta(minutes=5)], (
        "expected renew([5min]) before putIdle, got "
        f"{_RenewTrackingAsyncSandbox.last_instance.renewed}"
    )


@pytest.mark.asyncio
async def test_async_schedule_kill_discarded_alive_does_not_block_caller() -> None:
    """Async counterpart: scheduling must return synchronously without awaiting kills."""
    manager = _RecordingAsyncManager(per_call_delay=0.1)
    pool = _build_async_pool(manager)
    pool._sandbox_manager = manager  # type: ignore[assignment]

    ids = ("a", "b", "c")
    start = asyncio.get_event_loop().time()
    pool._schedule_kill_discarded_alive("kill-pool", ids, source="acquire")
    elapsed = asyncio.get_event_loop().time() - start

    # _schedule is sync (asyncio.create_task), so it should return in microseconds.
    assert elapsed < 0.01, (
        f"_schedule_kill_discarded_alive blocked for {elapsed:.3f}s; "
        "expected immediate return via create_task"
    )

    # Wait for the background tasks to complete.
    deadline = asyncio.get_event_loop().time() + 1.0
    while sorted(manager.killed) != sorted(ids) and asyncio.get_event_loop().time() < deadline:
        await asyncio.sleep(0.02)
    assert sorted(manager.killed) == sorted(ids)
