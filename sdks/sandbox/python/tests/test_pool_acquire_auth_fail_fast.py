#
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
#

"""Pool acquire must surface 401/403 instead of discarding healthy idles.

A shared invalid credential fails every idle candidate identically, so the
acquire loop's candidate-specific cleanup (remove idle + fire-and-forget
kill + next candidate) would destroy up to max_acquire_retries healthy
sandboxes and wrap the verdict in PoolAcquireFailedException.
"""

from __future__ import annotations

import asyncio
import time
from typing import Any

import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    SandboxApiException,
    SandboxError,
)
from opensandbox.pool import (
    AcquirePolicy,
    InMemoryAsyncPoolStateStore,
    InMemoryPoolStateStore,
    PoolCreationSpec,
    SandboxPoolAsync,
)
from opensandbox.sync.pool import SandboxPoolSync


async def _eventually(cond, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return
        await asyncio.sleep(0.01)
    raise AssertionError("condition did not become true")


def _eventually_sync(cond, timeout: float = 2.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return
        time.sleep(0.01)
    raise AssertionError("condition did not become true")


def _auth_error(status: int = 401) -> SandboxApiException:
    return SandboxApiException(
        message=f"Ping failed: HTTP {status}",
        status_code=status,
        error=SandboxError(code="MISSING_API_KEY"),
        request_id=f"req-{status}",
    )


class _KillRecorderAsync:
    def __init__(self) -> None:
        self.killed: list[str] = []

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    async def close(self) -> None:
        return None


class _KillRecorderSync:
    def __init__(self) -> None:
        self.killed: list[str] = []

    def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    def close(self) -> None:
        return None


@pytest.mark.asyncio
async def test_async_acquire_401_surfaces_auth_error_and_keeps_idles() -> None:
    store = InMemoryAsyncPoolStateStore()
    await store.put_idle("pool", "idle-1")
    await store.put_idle("pool", "idle-2")
    manager = _KillRecorderAsync()
    connect_calls = {"n": 0}
    connected: list[str] = []

    class _AuthFailingSandbox:
        @classmethod
        async def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any):
            connect_calls["n"] += 1
            connected.append(sandbox_id)
            raise _auth_error(401)

    async def _manager_factory(config: Any) -> _KillRecorderAsync:
        return manager

    pool = SandboxPoolAsync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfig(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=_manager_factory,
        sandbox_factory=_AuthFailingSandbox,  # type: ignore[arg-type]
    )
    await pool.start()
    try:
        with pytest.raises(SandboxApiException) as exc_info:
            await pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc_info.value.status_code == 401
        # No retry against the next candidate...
        assert connect_calls["n"] == 1
        # ...and the taken candidate gets an explicit disposition: it was already
        # popped from the store by try_take, so leaving it alive would leak it
        # untracked. It is killed; the remaining idle stays tracked.
        assert connected == ["idle-1"]
        # kill 是 fire-and-forget,等后台任务落地
        await _eventually(lambda: manager.killed == ["idle-1"])
        snapshot = await store.snapshot_counters("pool")
        assert snapshot.idle_count == 1
    finally:
        await pool.shutdown(False)


def test_sync_acquire_401_surfaces_auth_error_and_keeps_idles() -> None:
    store = InMemoryPoolStateStore()
    store.put_idle("pool", "idle-1")
    store.put_idle("pool", "idle-2")
    manager = _KillRecorderSync()
    connect_calls = {"n": 0}
    connected: list[str] = []

    class _AuthFailingSandbox:
        @classmethod
        def connect(cls, sandbox_id: str, *args: Any, **kwargs: Any):
            connect_calls["n"] += 1
            connected.append(sandbox_id)
            raise _auth_error(401)

    pool = SandboxPoolSync(
        pool_name="pool",
        owner_id="owner-1",
        max_idle=0,
        state_store=store,
        connection_config=ConnectionConfigSync(),
        creation_spec=PoolCreationSpec(image="ubuntu:22.04"),
        sandbox_manager_factory=lambda config: manager,  # type: ignore[arg-type,return-value]
        sandbox_factory=_AuthFailingSandbox,  # type: ignore[arg-type]
    )
    pool.start()
    try:
        with pytest.raises(SandboxApiException) as exc_info:
            pool.acquire(policy=AcquirePolicy.FAIL_FAST)
        assert exc_info.value.status_code == 401
        assert connect_calls["n"] == 1
        assert connected == ["idle-1"]
        _eventually_sync(lambda: manager.killed == ["idle-1"])
        assert store.snapshot_counters("pool").idle_count == 1
    finally:
        pool.shutdown(False)
