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
"""Tests for the background command status and log tools (#2555)."""

from __future__ import annotations

import pytest
from mcp.server.mcpserver.exceptions import ToolError
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import CommandLogs, CommandStatus, Execution

from opensandbox_mcp.server import register_tools


class _FakeMCP:
    """Captures tools registered via the ``@tool()`` decorator."""

    def __init__(self) -> None:
        self.tools: dict[str, object] = {}

    def tool(self):
        def decorator(func):
            self.tools[func.__name__] = func
            return func

        return decorator


class _FakeCommands:
    """Records SDK calls and returns canned results."""

    def __init__(self) -> None:
        self.calls: list[tuple] = []
        self.status = CommandStatus(id="cmd-1", running=True)
        self.logs = CommandLogs(content="", cursor=0)
        self.error: Exception | None = None

    async def run(self, command, *, opts=None):
        self.calls.append(("run", command, opts.background))
        return Execution(id="cmd-1")

    async def interrupt(self, execution_id: str) -> None:
        self.calls.append(("interrupt", execution_id))

    async def get_command_status(self, execution_id: str) -> CommandStatus:
        self.calls.append(("get_command_status", execution_id))
        if self.error is not None:
            raise self.error
        return self.status

    async def get_background_command_logs(
        self, execution_id: str, cursor: int | None = None
    ) -> CommandLogs:
        self.calls.append(("get_background_command_logs", execution_id, cursor))
        if self.error is not None:
            raise self.error
        return self.logs


class _FakeSandbox:
    def __init__(self, sandbox_id: str) -> None:
        self.id = sandbox_id
        self.commands = _FakeCommands()


@pytest.fixture()
async def server():
    fake = _FakeMCP()
    state = register_tools(
        fake, connection_config=ConnectionConfig().with_transport_if_missing()
    )
    sandbox = _FakeSandbox("sbx-1")
    await state.add(sandbox)
    return fake, state, sandbox


async def test_get_status_returns_running_status(server) -> None:
    fake, _, sandbox = server

    status = await fake.tools["command_get_status"]("sbx-1", "cmd-1")

    assert status.running is True
    assert status.exit_code is None
    assert sandbox.commands.calls == [("get_command_status", "cmd-1")]


async def test_get_status_reports_failed_exit(server) -> None:
    fake, _, sandbox = server
    sandbox.commands.status = CommandStatus(
        id="cmd-1", running=False, exit_code=3, error="exit status 3"
    )

    status = await fake.tools["command_get_status"]("sbx-1", "cmd-1")

    assert status.running is False
    assert status.exit_code == 3
    assert status.error == "exit status 3"


async def test_get_logs_without_cursor_requests_full_log(server) -> None:
    fake, _, sandbox = server
    sandbox.commands.logs = CommandLogs(content="step 1\nstep 2\n", cursor=14)

    logs = await fake.tools["command_get_logs"]("sbx-1", "cmd-1")

    assert logs.content == "step 1\nstep 2\n"
    assert logs.cursor == 14
    assert sandbox.commands.calls == [("get_background_command_logs", "cmd-1", None)]


async def test_get_logs_passes_cursor_through_unchanged(server) -> None:
    """The cursor is a server-side byte offset: it must be forwarded and
    returned as-is, never recomputed from the decoded content length."""
    fake, _, sandbox = server
    # "第2步\n" is 4 characters but 8 UTF-8 bytes.
    sandbox.commands.logs = CommandLogs(content="第2步\n", cursor=16)

    logs = await fake.tools["command_get_logs"]("sbx-1", "cmd-1", cursor=8)

    assert sandbox.commands.calls == [("get_background_command_logs", "cmd-1", 8)]
    assert logs.content == "第2步\n"
    assert logs.cursor == 16


async def test_get_logs_empty_content_at_tail(server) -> None:
    fake, _, sandbox = server
    sandbox.commands.logs = CommandLogs(content="", cursor=16)

    logs = await fake.tools["command_get_logs"]("sbx-1", "cmd-1", cursor=16)

    assert logs.content == ""
    assert logs.cursor == 16


async def test_get_logs_keeps_null_cursor(server) -> None:
    """A missing or malformed cursor header surfaces as null, not a guess."""
    fake, _, sandbox = server
    sandbox.commands.logs = CommandLogs(content="partial", cursor=None)

    logs = await fake.tools["command_get_logs"]("sbx-1", "cmd-1", cursor=4)

    assert logs.cursor is None


async def test_get_logs_returns_large_output_intact(server) -> None:
    fake, _, sandbox = server
    big = "x" * (1024 * 1024)
    sandbox.commands.logs = CommandLogs(content=big, cursor=len(big))

    logs = await fake.tools["command_get_logs"]("sbx-1", "cmd-1")

    assert logs.content == big
    assert logs.cursor == 1024 * 1024


@pytest.mark.parametrize(
    ("tool_name", "message"),
    [
        ("command_get_status", "command not found: cmd-missing"),
        ("command_get_logs", "command cmd-missing is not running in background"),
    ],
)
async def test_sdk_errors_become_tool_errors(server, tool_name, message) -> None:
    fake, _, sandbox = server
    sandbox.commands.error = SandboxApiException(message, status_code=400)

    with pytest.raises(ToolError, match=message):
        await fake.tools[tool_name]("sbx-1", "cmd-missing")


@pytest.mark.parametrize("tool_name", ["command_get_status", "command_get_logs"])
async def test_unregistered_sandbox_requires_connect(server, tool_name) -> None:
    fake, _, _ = server

    with pytest.raises(ToolError, match="Sandbox not found in local registry"):
        await fake.tools[tool_name]("sbx-unknown", "cmd-1")


async def test_connect_if_missing_registers_sandbox(server, monkeypatch) -> None:
    fake, state, _ = server
    connected: list[str] = []

    class _FakeServerSandboxModule:
        @staticmethod
        async def connect(sandbox_id, connection_config=None, **kwargs):
            connected.append(sandbox_id)
            return _FakeSandbox(sandbox_id)

    monkeypatch.setattr("opensandbox_mcp.server.Sandbox", _FakeServerSandboxModule)

    await fake.tools["command_get_status"]("sbx-new", "cmd-1", connect_if_missing=True)
    await fake.tools["command_get_logs"]("sbx-new", "cmd-1", connect_if_missing=True)

    assert connected == ["sbx-new"]
    assert state.sandboxes["sbx-new"].commands.calls == [
        ("get_command_status", "cmd-1"),
        ("get_background_command_logs", "cmd-1", None),
    ]


async def test_background_workflow_keeps_existing_command_tools(server) -> None:
    """command_run and command_interrupt behave as before alongside the new tools."""
    fake, _, sandbox = server

    execution = await fake.tools["command_run"]("sbx-1", "make build", background=True)
    await fake.tools["command_get_status"]("sbx-1", execution.id)
    await fake.tools["command_get_logs"]("sbx-1", execution.id, cursor=0)
    interrupted = await fake.tools["command_interrupt"]("sbx-1", execution.id)

    assert interrupted.status == "interrupted"
    assert sandbox.commands.calls == [
        ("run", "make build", True),
        ("get_command_status", "cmd-1"),
        ("get_background_command_logs", "cmd-1", 0),
        ("interrupt", "cmd-1"),
    ]
