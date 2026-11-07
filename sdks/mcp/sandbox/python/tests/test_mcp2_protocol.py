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

"""Protocol-level regressions for the MCP 2.x server migration."""

from __future__ import annotations

from collections.abc import AsyncIterator
from contextlib import asynccontextmanager

import anyio
import pytest
from mcp import types
from mcp.client.session import ClientSession
from mcp.shared.memory import create_client_server_memory_streams
from mcp_types.version import HANDSHAKE_PROTOCOL_VERSIONS, LATEST_HANDSHAKE_VERSION
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import CommandLogs, CommandStatus

from opensandbox_mcp.server import create_server

_EXPECTED_TOOL_COUNT = 21
_EXPECTED_NEGOTIATED_VERSION = LATEST_HANDSHAKE_VERSION


class _FakeManager:
    def __init__(self, config) -> None:
        self.config = config
        self.closed = False
        self.killed: list[str] = []

    @classmethod
    async def create(cls, connection_config=None):
        return cls(connection_config)

    async def kill_sandbox(self, sandbox_id: str) -> None:
        self.killed.append(sandbox_id)

    async def close(self) -> None:
        self.closed = True


class _FakeCommands:
    async def get_command_status(self, execution_id: str) -> CommandStatus:
        if execution_id == "cmd-missing":
            raise SandboxApiException(
                f"command not found: {execution_id}", status_code=404
            )
        return CommandStatus(
            id=execution_id, running=False, exit_code=3, error="exit status 3"
        )

    async def get_background_command_logs(
        self, execution_id: str, cursor: int | None = None
    ) -> CommandLogs:
        return CommandLogs(content="第2步\n", cursor=(cursor or 0) + 8)


class _FakeSandbox:
    def __init__(self, sandbox_id: str) -> None:
        self.id = sandbox_id
        self.commands = _FakeCommands()


class _FakeSandboxModule:
    @staticmethod
    async def connect(sandbox_id, connection_config=None, **kwargs):
        return _FakeSandbox(sandbox_id)


@asynccontextmanager
async def _client_session() -> AsyncIterator[ClientSession]:
    server = create_server()
    async with create_client_server_memory_streams() as (
        client_streams,
        server_streams,
    ):
        async with anyio.create_task_group() as task_group:
            task_group.start_soon(
                server._lowlevel_server.run,
                server_streams[0],
                server_streams[1],
                server._lowlevel_server.create_initialization_options(),
            )
            async with ClientSession(*client_streams) as session:
                yield session
            task_group.cancel_scope.cancel()


def _text_content(result: types.CallToolResult) -> str:
    return "\n".join(
        block.text for block in result.content if isinstance(block, types.TextContent)
    )


async def test_mcp2_initialize_and_list_tools() -> None:
    async with _client_session() as mcp_session:
        initialized = await mcp_session.initialize()
        negotiated_version = mcp_session.protocol_version
        listed = await mcp_session.list_tools()

    names = {tool.name for tool in listed.tools}
    assert initialized.protocol_version == _EXPECTED_NEGOTIATED_VERSION
    assert negotiated_version == _EXPECTED_NEGOTIATED_VERSION
    assert initialized.server_info.name == "OpenSandbox Sandbox"
    assert initialized.capabilities.tools is not None
    assert len(names) == _EXPECTED_TOOL_COUNT
    assert {
        "sandbox_create",
        "sandbox_kill",
        "command_run",
        "command_get_status",
        "command_get_logs",
        "file_read",
        "file_write",
    } <= names


async def test_mcp2_tool_call_round_trips_structured_output(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("opensandbox_mcp.server.SandboxManager", _FakeManager)

    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        result = await mcp_session.call_tool(
            "sandbox_kill", {"sandbox_id": "sandbox-1"}
        )

    assert isinstance(result, types.CallToolResult)
    assert result.is_error is False
    assert result.structured_content == {"status": "killed"}


async def test_mcp2_expected_tool_errors_keep_actionable_message() -> None:
    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        result = await mcp_session.call_tool(
            "sandbox_create",
            {"image": "example", "auth_username": "user"},
        )

    assert isinstance(result, types.CallToolResult)
    assert result.is_error is True
    assert "auth_username and auth_password must be provided together" in _text_content(
        result
    )


async def test_mcp2_sandbox_errors_keep_actionable_message(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _MissingSandboxManager(_FakeManager):
        async def kill_sandbox(self, sandbox_id: str) -> None:
            raise SandboxApiException(f"Sandbox {sandbox_id} not found", status_code=404)

    monkeypatch.setattr("opensandbox_mcp.server.SandboxManager", _MissingSandboxManager)

    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        result = await mcp_session.call_tool(
            "sandbox_kill", {"sandbox_id": "sbx-missing"}
        )

    assert isinstance(result, types.CallToolResult)
    assert result.is_error is True
    assert "Sandbox sbx-missing not found" in _text_content(result)


async def test_mcp2_command_get_logs_schema_has_optional_cursor() -> None:
    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        listed = await mcp_session.list_tools()

    tools = {tool.name: tool for tool in listed.tools}
    logs_schema = tools["command_get_logs"].input_schema
    assert logs_schema["required"] == ["sandbox_id", "execution_id"]
    assert {"type": "integer"} in logs_schema["properties"]["cursor"]["anyOf"]
    assert tools["command_get_status"].input_schema["required"] == [
        "sandbox_id",
        "execution_id",
    ]


async def test_mcp2_command_status_and_logs_round_trip(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("opensandbox_mcp.server.Sandbox", _FakeSandboxModule)

    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        status = await mcp_session.call_tool(
            "command_get_status",
            {
                "sandbox_id": "sbx-1",
                "execution_id": "cmd-1",
                "connect_if_missing": True,
            },
        )
        logs = await mcp_session.call_tool(
            "command_get_logs",
            {"sandbox_id": "sbx-1", "execution_id": "cmd-1", "cursor": 8},
        )

    assert status.is_error is False
    assert status.structured_content is not None
    assert status.structured_content["running"] is False
    assert status.structured_content["exit_code"] == 3
    assert status.structured_content["error"] == "exit status 3"
    assert logs.is_error is False
    assert logs.structured_content == {"content": "第2步\n", "cursor": 16}


async def test_mcp2_command_status_errors_keep_actionable_message(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr("opensandbox_mcp.server.Sandbox", _FakeSandboxModule)

    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        result = await mcp_session.call_tool(
            "command_get_status",
            {
                "sandbox_id": "sbx-1",
                "execution_id": "cmd-missing",
                "connect_if_missing": True,
            },
        )

    assert result.is_error is True
    assert "command not found: cmd-missing" in _text_content(result)


async def test_mcp2_unknown_tool_is_an_error_result() -> None:
    async with _client_session() as mcp_session:
        await mcp_session.initialize()
        result = await mcp_session.call_tool("does_not_exist", {})

    assert isinstance(result, types.CallToolResult)
    assert result.is_error is True
    assert "Unknown tool: does_not_exist" in _text_content(result)


@pytest.mark.parametrize(
    "offered_version",
    (*HANDSHAKE_PROTOCOL_VERSIONS, "2099-01-01"),
)
async def test_mcp2_handshake_version_negotiation_and_legacy_tools(
    offered_version: str,
) -> None:
    expected_version = (
        offered_version
        if offered_version in HANDSHAKE_PROTOCOL_VERSIONS
        else _EXPECTED_NEGOTIATED_VERSION
    )

    async with _client_session() as mcp_session:
        initialized = await mcp_session.send_request(
            types.InitializeRequest(
                params=types.InitializeRequestParams(
                    protocol_version=offered_version,
                    capabilities=types.ClientCapabilities(),
                    client_info=types.Implementation(
                        name="protocol-regression-client",
                        version="1.0.0",
                    ),
                )
            ),
            types.InitializeResult,
        )
        mcp_session.adopt(initialized)
        await mcp_session.send_notification(types.InitializedNotification())
        listed = await mcp_session.list_tools()

    assert initialized.protocol_version == expected_version
    assert len(listed.tools) == _EXPECTED_TOOL_COUNT
