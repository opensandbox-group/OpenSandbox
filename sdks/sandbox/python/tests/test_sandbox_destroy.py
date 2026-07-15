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
from __future__ import annotations

import pytest

from opensandbox.sandbox import Sandbox
from opensandbox.sync.sandbox import SandboxSync


class _Noop:
    pass


class _AsyncSandboxService:
    def __init__(self, events: list[str], *, fail_kill: bool = False) -> None:
        self._events = events
        self._fail_kill = fail_kill

    def invalidate_endpoint_cache(self, sandbox_id: str) -> None:
        self._events.append("invalidate")

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self._events.append("kill")
        if self._fail_kill:
            raise RuntimeError("kill failed")


class _AsyncConnectionConfig:
    def __init__(self, events: list[str]) -> None:
        self._events = events

    async def close_transport_if_owned(self) -> None:
        self._events.append("close")


class _SyncSandboxService:
    def __init__(self, events: list[str], *, fail_kill: bool = False) -> None:
        self._events = events
        self._fail_kill = fail_kill

    def invalidate_endpoint_cache(self, sandbox_id: str) -> None:
        self._events.append("invalidate")

    def kill_sandbox(self, sandbox_id: str) -> None:
        self._events.append("kill")
        if self._fail_kill:
            raise RuntimeError("kill failed")


class _SyncConnectionConfig:
    def __init__(self, events: list[str]) -> None:
        self._events = events

    def close_transport_if_owned(self) -> None:
        self._events.append("close")


def _make_async_sandbox(events: list[str], *, fail_kill: bool = False) -> Sandbox:
    return Sandbox(
        sandbox_id="sandbox-id",
        sandbox_service=_AsyncSandboxService(events, fail_kill=fail_kill),
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=_Noop(),
        diagnostics_service=_Noop(),
        connection_config=_AsyncConnectionConfig(events),
    )


def _make_sync_sandbox(events: list[str], *, fail_kill: bool = False) -> SandboxSync:
    return SandboxSync(
        sandbox_id="sandbox-id",
        sandbox_service=_SyncSandboxService(events, fail_kill=fail_kill),
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=_Noop(),
        diagnostics_service=_Noop(),
        connection_config=_SyncConnectionConfig(events),
    )


@pytest.mark.asyncio
async def test_destroy_kills_before_closing_async() -> None:
    events: list[str] = []

    await _make_async_sandbox(events).destroy()

    assert events == ["invalidate", "kill", "close"]


@pytest.mark.asyncio
async def test_destroy_closes_and_reraises_when_kill_fails_async() -> None:
    events: list[str] = []

    with pytest.raises(RuntimeError, match="kill failed"):
        await _make_async_sandbox(events, fail_kill=True).destroy()

    assert events == ["invalidate", "kill", "close"]


def test_destroy_kills_before_closing_sync() -> None:
    events: list[str] = []

    _make_sync_sandbox(events).destroy()

    assert events == ["invalidate", "kill", "close"]


def test_destroy_closes_and_reraises_when_kill_fails_sync() -> None:
    events: list[str] = []

    with pytest.raises(RuntimeError, match="kill failed"):
        _make_sync_sandbox(events, fail_kill=True).destroy()

    assert events == ["invalidate", "kill", "close"]
