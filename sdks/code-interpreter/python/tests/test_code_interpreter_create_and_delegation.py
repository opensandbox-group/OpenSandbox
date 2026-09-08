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
from datetime import timedelta

import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import (
    InvalidArgumentException,
    SandboxReadyTimeoutException,
)
from opensandbox.models.execd import Execution
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter import CodeInterpreter
from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.code_interpreter import RUNTIME_PROCESS_CHECK_COMMAND


class _FakeCommands:
    def __init__(self, error=None) -> None:
        self._error = error
        self.commands: list[str] = []

    async def run(self, command, *, opts=None, handlers=None):
        self.commands.append(command)
        return Execution(error=self._error)


class _FakeSandbox:
    def __init__(self, commands_error=None) -> None:
        self._id = str(__import__("uuid").uuid4())
        self.connection_config = ConnectionConfig(protocol="http")
        self.commands = _FakeCommands(commands_error)
        self.files = object()
        self.metrics = object()

    @property
    def id(self):
        return self._id

    async def get_endpoint(self, port: int) -> SandboxEndpoint:
        return SandboxEndpoint(endpoint="localhost:44772", port=port)

    async def is_healthy(self) -> bool:
        return True

    async def get_info(self):  # pragma: no cover
        raise RuntimeError("not used")

    async def get_metrics(self):  # pragma: no cover
        raise RuntimeError("not used")

    async def renew(self, timeout):  # pragma: no cover
        raise RuntimeError("not used")


@pytest.mark.asyncio
async def test_create_requires_sandbox() -> None:
    with pytest.raises(InvalidArgumentException):
        await CodeInterpreter.create(sandbox=None)  # type: ignore[arg-type]


@pytest.mark.asyncio
async def test_create_wires_code_service_and_delegates_properties(
    monkeypatch,
) -> None:
    async def healthy_ping(self) -> bool:
        return True

    monkeypatch.setattr(CodesAdapter, "ping", healthy_ping)
    sbx = _FakeSandbox()
    ci = await CodeInterpreter.create(sandbox=sbx)  # type: ignore[arg-type]

    assert ci.id == sbx.id
    assert ci.files is sbx.files
    assert ci.commands is sbx.commands
    assert ci.metrics is sbx.metrics

    # codes service should be present and callable (no network)
    assert ci.codes is not None


@pytest.mark.asyncio
async def test_create_pings_until_code_service_ready(monkeypatch) -> None:
    attempts = {"count": 0}

    async def flaky_ping(self) -> bool:
        attempts["count"] += 1
        return attempts["count"] >= 3

    monkeypatch.setattr(CodesAdapter, "ping", flaky_ping)
    sbx = _FakeSandbox()
    ci = await CodeInterpreter.create(  # type: ignore[arg-type]
        sandbox=sbx,
        ready_timeout=timedelta(seconds=5),
        health_check_polling_interval=timedelta(milliseconds=10),
    )

    assert attempts["count"] == 3
    assert await ci.is_healthy()


@pytest.mark.asyncio
async def test_create_times_out_when_code_service_never_ready(monkeypatch) -> None:
    async def dead_ping(self) -> bool:
        return False

    monkeypatch.setattr(CodesAdapter, "ping", dead_ping)
    sbx = _FakeSandbox()

    with pytest.raises(SandboxReadyTimeoutException):
        await CodeInterpreter.create(  # type: ignore[arg-type]
            sandbox=sbx,
            ready_timeout=timedelta(milliseconds=50),
            health_check_polling_interval=timedelta(milliseconds=10),
        )


@pytest.mark.asyncio
async def test_create_skip_health_check_does_not_ping(monkeypatch) -> None:
    def boom(self) -> bool:
        raise AssertionError("ping must not be called when skip_health_check=True")

    monkeypatch.setattr(CodesAdapter, "ping", boom)
    sbx = _FakeSandbox()
    ci = await CodeInterpreter.create(  # type: ignore[arg-type]
        sandbox=sbx,
        skip_health_check=True,
    )

    assert ci.id == sbx.id


@pytest.mark.asyncio
async def test_create_runs_runtime_process_check_script(monkeypatch) -> None:
    async def healthy_ping(self) -> bool:
        return True

    monkeypatch.setattr(CodesAdapter, "ping", healthy_ping)
    sbx = _FakeSandbox()
    ci = await CodeInterpreter.create(  # type: ignore[arg-type]
        sandbox=sbx,
    )

    assert await ci.is_healthy()
    # One command per strict check: once during create(), once for is_healthy().
    assert sbx.commands.commands == [
        RUNTIME_PROCESS_CHECK_COMMAND,
        RUNTIME_PROCESS_CHECK_COMMAND,
    ]
    assert "/dev/tcp/127.0.0.1/" in sbx.commands.commands[0]
    assert "${JUPYTER_PORT:-44771}" in sbx.commands.commands[0]


@pytest.mark.asyncio
async def test_create_times_out_when_runtime_process_missing(monkeypatch) -> None:
    from opensandbox.models.execd import ExecutionError

    async def healthy_ping(self) -> bool:
        return True

    monkeypatch.setattr(CodesAdapter, "ping", healthy_ping)
    sbx = _FakeSandbox(
        commands_error=ExecutionError(
            name="CommandExecError", value="1", timestamp=0
        )
    )

    with pytest.raises(SandboxReadyTimeoutException):
        await CodeInterpreter.create(  # type: ignore[arg-type]
            sandbox=sbx,
            ready_timeout=timedelta(milliseconds=50),
            health_check_polling_interval=timedelta(milliseconds=10),
        )
