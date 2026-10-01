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

from opensandbox_mcp.server import create_server

_EXPECTED_TOOL_COUNT = 19
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
