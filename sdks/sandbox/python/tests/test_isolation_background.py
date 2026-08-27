#
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
#
"""Tests for isolated session background runs (async and sync adapters)."""

from __future__ import annotations

from datetime import datetime
from unittest.mock import AsyncMock, MagicMock

import pytest

from opensandbox.adapters.isolated_adapter import (
    IsolatedSessionsAdapter,
    IsolationSessionHandle,
)
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.isolated import (
    IsolatedBackgroundRun,
    IsolatedRunOpts,
    IsolatedRunStatus,
)
from opensandbox.sync.adapters.isolated_adapter import (
    IsolatedSessionsAdapterSync,
    IsolationSessionHandleSync,
)

_BACKGROUND_RUN_PAYLOAD = {
    "session_id": "sess-1",
    "run_id": "run-1",
    "started_at": "2026-01-02T03:04:05Z",
}

_RUNNING_STATUS_PAYLOAD = {
    "session_id": "sess-1",
    "run_id": "run-1",
    "running": True,
    "started_at": "2026-01-02T03:04:05Z",
}

_FINISHED_STATUS_PAYLOAD = {
    "session_id": "sess-1",
    "run_id": "run-1",
    "running": False,
    "exit_code": 7,
    "started_at": "2026-01-02T03:04:05Z",
    "finished_at": "2026-01-02T03:04:09Z",
}


def _mock_response(status_code=200, json_data=None, text="", headers=None):
    response = MagicMock()
    response.status_code = status_code
    response.json = MagicMock(return_value=json_data)
    response.text = text
    response.content = text.encode()
    response.headers = headers or {}
    return response


def _make_handle_async(adapter, session_id="sess-1"):
    info = MagicMock()
    info.session_id = session_id
    return IsolationSessionHandle(info, adapter)


def _make_handle_sync(adapter, session_id="sess-1"):
    info = MagicMock()
    info.session_id = session_id
    return IsolationSessionHandleSync(info, adapter)


@pytest.fixture
def async_adapter():
    from opensandbox.config import ConnectionConfig
    from opensandbox.models.sandboxes import SandboxEndpoint

    config = ConnectionConfig(api_key="test-key", domain="localhost")
    endpoint = SandboxEndpoint(endpoint="localhost:8080", headers={})
    adapter = IsolatedSessionsAdapter(config, endpoint)
    return adapter


@pytest.fixture
def sync_adapter():
    from opensandbox.config.connection_sync import ConnectionConfigSync
    from opensandbox.models.sandboxes import SandboxEndpoint

    config = ConnectionConfigSync(api_key="test-key", domain="localhost")
    endpoint = SandboxEndpoint(endpoint="localhost:8080", headers={})
    return IsolatedSessionsAdapterSync(config, endpoint)


# -------- async --------


@pytest.mark.asyncio
async def test_run_background_posts_background_flag_without_timeout(
    async_adapter,
):
    async_adapter._httpx_client.post = AsyncMock(
        return_value=_mock_response(202, _BACKGROUND_RUN_PAYLOAD)
    )

    handle = _make_handle_async(async_adapter)
    run = await handle.run_background(
        "echo hi", opts=IsolatedRunOpts(timeout_seconds=30, envs={"A": "b"})
    )

    async_adapter._httpx_client.post.assert_called_once()
    call_url = async_adapter._httpx_client.post.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-1/run")
    body = async_adapter._httpx_client.post.call_args.kwargs["json"]
    assert body["code"] == "echo hi"
    assert body["background"] is True
    assert body["envs"] == {"A": "b"}
    assert "timeout_seconds" not in body

    assert isinstance(run, IsolatedBackgroundRun)
    assert run.session_id == "sess-1"
    assert run.run_id == "run-1"
    assert isinstance(run.started_at, datetime)


@pytest.mark.asyncio
async def test_run_status_parses_running_and_finished(async_adapter):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, _FINISHED_STATUS_PAYLOAD)
    )

    handle = _make_handle_async(async_adapter)
    status = await handle.run_status("run-1")

    async_adapter._httpx_client.get.assert_called_once()
    call_url = async_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-1/runs/run-1")

    assert isinstance(status, IsolatedRunStatus)
    assert status.running is False
    assert status.exit_code == 7
    assert isinstance(status.finished_at, datetime)


@pytest.mark.asyncio
async def test_run_logs_uses_header_cursor(async_adapter):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(
            200,
            text="line1\nline2\n",
            headers={"EXECD-ISOLATED-TAIL-CURSOR": "12"},
        )
    )

    handle = _make_handle_async(async_adapter)
    logs = await handle.run_logs("run-1", cursor=4)

    async_adapter._httpx_client.get.assert_called_once()
    call_url = async_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-1/runs/run-1/logs")
    assert async_adapter._httpx_client.get.call_args.kwargs["params"] == {"cursor": 4}

    assert logs.text == "line1\nline2\n"
    assert logs.cursor == 12


@pytest.mark.asyncio
async def test_run_logs_falls_back_to_byte_length_without_header(async_adapter):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, text="hello")
    )

    handle = _make_handle_async(async_adapter)
    logs = await handle.run_logs("run-1", cursor=0)

    # No header: cursor advances by the bytes actually returned.
    assert logs.text == "hello"
    assert logs.cursor == 5


@pytest.mark.asyncio
async def test_run_background_propagates_http_error(async_adapter):
    async_adapter._httpx_client.post = AsyncMock(
        return_value=_mock_response(404, {"code": "SESSION_NOT_FOUND"})
    )

    handle = _make_handle_async(async_adapter)
    with pytest.raises(SandboxApiException) as exc_info:
        await handle.run_background("echo hi")
    assert exc_info.value.status_code == 404


@pytest.mark.asyncio
async def test_background_validation(async_adapter):
    handle = _make_handle_async(async_adapter)
    with pytest.raises(InvalidArgumentException):
        await handle.run_background("")
    with pytest.raises(InvalidArgumentException):
        await handle.run_status("")
    with pytest.raises(InvalidArgumentException):
        await handle.run_logs("run-1", cursor=-1)


# -------- generated client --------


def test_generated_run_endpoint_parses_background_202_handle():
    """The generated run endpoint must parse the 202 JSON handle (not SSE)."""
    import json as _json

    from opensandbox.api.execd.api.isolated_execution.run_in_isolated_session import (
        _parse_response,
    )
    from opensandbox.api.execd.models.isolated_background_run_response import (
        IsolatedBackgroundRunResponse,
    )

    payload = {
        "session_id": "12345678-1234-1234-1234-123456789012",
        "run_id": "87654321-4321-4321-4321-210987654321",
        "started_at": "2026-01-02T03:04:05Z",
    }
    response = MagicMock()
    response.status_code = 202
    response.json = MagicMock(return_value=_json.loads(_json.dumps(payload)))

    parsed = _parse_response(client=MagicMock(), response=response)
    assert isinstance(parsed, IsolatedBackgroundRunResponse)
    assert str(parsed.run_id) == "87654321-4321-4321-4321-210987654321"


# -------- sync --------


def test_sync_run_background_posts_background_flag_without_timeout(sync_adapter):
    sync_adapter._httpx_client.post = MagicMock(
        return_value=_mock_response(202, _BACKGROUND_RUN_PAYLOAD)
    )

    handle = _make_handle_sync(sync_adapter)
    run = handle.run_background(
        "echo hi", opts=IsolatedRunOpts(timeout_seconds=30, envs={"A": "b"})
    )

    body = sync_adapter._httpx_client.post.call_args.kwargs["json"]
    assert body["code"] == "echo hi"
    assert body["background"] is True
    assert "timeout_seconds" not in body

    assert isinstance(run, IsolatedBackgroundRun)
    assert run.run_id == "run-1"


def test_sync_run_status_parses_finished(sync_adapter):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(200, _FINISHED_STATUS_PAYLOAD)
    )

    handle = _make_handle_sync(sync_adapter)
    status = handle.run_status("run-1")

    call_url = sync_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-1/runs/run-1")

    assert status.running is False
    assert status.exit_code == 7


def test_sync_run_logs_uses_header_cursor(sync_adapter):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(
            200,
            text="line1\nline2\n",
            headers={"EXECD-ISOLATED-TAIL-CURSOR": "12"},
        )
    )

    handle = _make_handle_sync(sync_adapter)
    logs = handle.run_logs("run-1", cursor=4)

    assert sync_adapter._httpx_client.get.call_args.kwargs["params"] == {"cursor": 4}
    assert logs.text == "line1\nline2\n"
    assert logs.cursor == 12


def test_sync_run_background_propagates_http_error(sync_adapter):
    sync_adapter._httpx_client.post = MagicMock(
        return_value=_mock_response(404, {"code": "SESSION_NOT_FOUND"})
    )

    handle = _make_handle_sync(sync_adapter)
    with pytest.raises(SandboxApiException) as exc_info:
        handle.run_background("echo hi")
    assert exc_info.value.status_code == 404
