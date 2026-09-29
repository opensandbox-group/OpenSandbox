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
"""Tests for CommandsAdapter.create_session / delete_session error handling."""

from __future__ import annotations

import json

import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint


def _adapter(
    response: httpx.Response, requests: list[httpx.Request] | None = None
) -> CommandsAdapter:
    def handler(request: httpx.Request) -> httpx.Response:
        if requests is not None:
            requests.append(request)
        return response

    cfg = ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler))
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    return CommandsAdapter(cfg, endpoint)


def _error(status_code: int, code: str, message: str) -> httpx.Response:
    return httpx.Response(status_code, json={"code": code, "message": message})


@pytest.mark.asyncio
async def test_create_session_returns_session_id() -> None:
    requests: list[httpx.Request] = []
    adapter = _adapter(httpx.Response(200, json={"session_id": "sess-1"}), requests)

    session_id = await adapter.create_session(working_directory="/work")

    assert session_id == "sess-1"
    assert requests[0].method == "POST"
    assert requests[0].url.path == "/session"
    assert json.loads(requests[0].content) == {"cwd": "/work"}


@pytest.mark.asyncio
async def test_create_session_error_keeps_status_and_server_message() -> None:
    adapter = _adapter(_error(400, "INVALID_REQUEST_BODY", "bad cwd"))

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.create_session()

    assert exc_info.value.status_code == 400
    assert "bad cwd" in str(exc_info.value)


@pytest.mark.asyncio
async def test_delete_session_succeeds_on_200() -> None:
    requests: list[httpx.Request] = []
    adapter = _adapter(httpx.Response(200), requests)

    await adapter.delete_session("sess-1")

    assert requests[0].method == "DELETE"
    assert requests[0].url.path == "/session/sess-1"


@pytest.mark.asyncio
async def test_delete_session_raises_on_documented_error() -> None:
    adapter = _adapter(_error(404, "SESSION_NOT_FOUND", "session not found"))

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.delete_session("missing")

    assert exc_info.value.status_code == 404
    assert "session not found" in str(exc_info.value)


@pytest.mark.asyncio
async def test_delete_session_raises_on_undocumented_status() -> None:
    adapter = _adapter(httpx.Response(502))

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.delete_session("sess-1")

    assert exc_info.value.status_code == 502
