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

from datetime import timedelta
from typing import Any

import httpx
import pytest

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import PoolDestroyIncompleteException
from opensandbox.pool import (
    InMemoryAsyncPoolStateStore,
    InMemoryPoolStateStore,
    PoolDestroyOptions,
    PoolDestroyState,
    SandboxPoolManagerAsync,
    SandboxPoolManagerSync,
)


def test_pool_manager_destroy_drains_idle_clears_state_and_writes_tombstone() -> None:
    store = InMemoryPoolStateStore()
    store.set_max_idle("pool", 2)
    store.set_idle_entry_ttl("pool", timedelta(minutes=5))
    store.put_idle("pool", "id-1")
    store.put_idle("pool", "id-2")
    manager = _RecordingManager()
    pool_manager = SandboxPoolManagerSync(
        state_store=store,
        owner_id="destroyer",
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
    )

    result = pool_manager.destroy("pool", PoolDestroyOptions())

    assert result.pool_name == "pool"
    assert result.state == PoolDestroyState.DESTROYED
    assert result.drained_idle_count == 2
    assert result.killed_idle_count == 2
    assert result.persistent_state_cleared
    assert manager.killed == ["id-1", "id-2"]
    assert manager.closed
    assert store.snapshot_counters("pool").idle_count == 0
    assert store.get_max_idle("pool") is None
    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYED


def test_pool_manager_destroy_failure_leaves_destroying_for_retry() -> None:
    store = _FailingClearStore()
    store.put_idle("pool", "id-1")
    manager = _RecordingManager()
    pool_manager = SandboxPoolManagerSync(
        state_store=store,
        owner_id="destroyer",
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
    )

    with pytest.raises(PoolDestroyIncompleteException):
        pool_manager.destroy("pool")

    assert manager.killed == ["id-1"]
    assert manager.closed
    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYING


def test_pool_manager_zero_drain_timeout_means_no_timeout() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "id-1")
    manager = _RecordingManager()
    pool_manager = SandboxPoolManagerSync(
        state_store=store,
        owner_id="destroyer",
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
    )

    result = pool_manager.destroy(
        "pool",
        PoolDestroyOptions(drain_timeout=timedelta(0)),
    )

    assert result.state == PoolDestroyState.DESTROYED
    assert result.drained_idle_count == 1
    assert manager.killed == ["id-1"]


@pytest.mark.asyncio
async def test_async_pool_manager_destroy_drains_idle_clears_state_and_writes_tombstone() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.set_max_idle("pool", 2)
    await store.set_idle_entry_ttl("pool", timedelta(minutes=5))
    await store.put_idle("pool", "id-1")
    await store.put_idle("pool", "id-2")
    manager = _RecordingAsyncManager()
    pool_manager = SandboxPoolManagerAsync(
        state_store=store,
        owner_id="destroyer",
        sandbox_manager_factory=lambda config: _async_manager_factory(manager),
    )

    result = await pool_manager.destroy("pool", PoolDestroyOptions())

    assert result.pool_name == "pool"
    assert result.state == PoolDestroyState.DESTROYED
    assert result.drained_idle_count == 2
    assert result.killed_idle_count == 2
    assert result.persistent_state_cleared
    assert manager.killed == ["id-1", "id-2"]
    assert manager.closed
    assert (await store.snapshot_counters("pool")).idle_count == 0
    assert await store.get_max_idle("pool") is None
    assert await store.get_destroy_state("pool") == PoolDestroyState.DESTROYED


class _RecordingManager:
    def __init__(self) -> None:
        self.killed: list[str] = []
        self.closed = False

    def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    def close(self) -> None:
        self.closed = True


class _RecordingAsyncManager:
    def __init__(self) -> None:
        self.killed: list[str] = []
        self.closed = False

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    async def close(self) -> None:
        self.closed = True


class _FailingClearStore(InMemoryPoolStateStore):
    def clear_pool_state(self, pool_name: str) -> None:
        raise RuntimeError("clear failed")


async def _async_manager_factory(manager: _RecordingAsyncManager) -> _RecordingAsyncManager:
    return manager


def test_pool_manager_preserves_user_managed_sync_transport() -> None:
    transport = _SyncTransport()
    captured: list[ConnectionConfigSync] = []

    def manager_factory(config: ConnectionConfigSync) -> _RecordingManager:
        captured.append(config)
        return _RecordingManager()

    pool_manager = SandboxPoolManagerSync(
        state_store=InMemoryPoolStateStore(),
        connection_config=ConnectionConfigSync(transport=transport),
        sandbox_manager_factory=manager_factory,  # type: ignore[arg-type,return-value]
    )

    pool_manager.destroy("pool")

    assert captured[0].transport is transport
    assert not captured[0]._owns_transport


class _SyncTransport(httpx.BaseTransport):
    def handle_request(self, request: Any) -> Any:
        return httpx.Response(200, request=request)
