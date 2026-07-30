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
"""Tests for IsolationService.list (async and sync)."""

from __future__ import annotations

from datetime import datetime
from unittest.mock import AsyncMock, MagicMock

import pytest

from opensandbox.adapters.isolated_adapter import IsolatedSessionsAdapter
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.isolated import IsolatedSessionSummary
from opensandbox.sync.adapters.isolated_adapter import IsolatedSessionsAdapterSync

_SESSIONS_PAYLOAD = {
    "sessions": [
        {
            "session_id": "sess-1",
            "status": "active",
            "created_at": "2026-01-01T00:00:00Z",
            "last_run_at": "2026-01-01T00:05:00Z",
            "idle_remaining_seconds": 120,
        },
        {
            "session_id": "sess-2",
            "status": "dead",
            "created_at": "2026-01-02T00:00:00Z",
            "last_run_at": "2026-01-02T00:05:00Z",
            "idle_remaining_seconds": None,
        },
    ]
}


def _mock_response(status_code=200, json_data=None):
    response = MagicMock()
    response.status_code = status_code
    response.headers = {}
    response.json = MagicMock(return_value=json_data)
    return response


@pytest.fixture
def async_adapter():
    from opensandbox.config import ConnectionConfig
    from opensandbox.models.sandboxes import SandboxEndpoint

    config = ConnectionConfig(api_key="test-key", domain="localhost")
    endpoint = SandboxEndpoint(endpoint="localhost:8080", headers={})
    return IsolatedSessionsAdapter(config, endpoint)


@pytest.fixture
def sync_adapter():
    from opensandbox.config.connection_sync import ConnectionConfigSync
    from opensandbox.models.sandboxes import SandboxEndpoint

    config = ConnectionConfigSync(api_key="test-key", domain="localhost")
    endpoint = SandboxEndpoint(endpoint="localhost:8080", headers={})
    return IsolatedSessionsAdapterSync(config, endpoint)


@pytest.mark.asyncio
async def test_async_list_returns_summaries(async_adapter):
    response = _mock_response(200, _SESSIONS_PAYLOAD)
    async_adapter._httpx_client.get = AsyncMock(return_value=response)

    result = await async_adapter.list()

    async_adapter._httpx_client.get.assert_called_once()
    assert len(result) == 2
    assert all(isinstance(s, IsolatedSessionSummary) for s in result)
    assert result[0].session_id == "sess-1"
    assert result[0].status == "active"
    assert isinstance(result[0].created_at, datetime)
    assert isinstance(result[0].last_run_at, datetime)
    assert result[0].idle_remaining_seconds == 120
    assert result[1].session_id == "sess-2"
    assert result[1].idle_remaining_seconds is None


@pytest.mark.asyncio
async def test_async_list_empty(async_adapter):
    response = _mock_response(200, {"sessions": []})
    async_adapter._httpx_client.get = AsyncMock(return_value=response)

    result = await async_adapter.list()

    assert result == []


@pytest.mark.asyncio
async def test_async_list_raises_on_error_status(async_adapter):
    response = _mock_response(500, None)
    async_adapter._httpx_client.get = AsyncMock(return_value=response)

    with pytest.raises(SandboxApiException):
        await async_adapter.list()


def test_sync_list_returns_summaries(sync_adapter):
    response = _mock_response(200, _SESSIONS_PAYLOAD)
    sync_adapter._httpx_client.get = MagicMock(return_value=response)

    result = sync_adapter.list()

    sync_adapter._httpx_client.get.assert_called_once()
    assert len(result) == 2
    assert all(isinstance(s, IsolatedSessionSummary) for s in result)
    assert result[0].session_id == "sess-1"
    assert result[0].status == "active"
    assert result[0].idle_remaining_seconds == 120
    assert result[1].idle_remaining_seconds is None


def test_sync_list_empty(sync_adapter):
    response = _mock_response(200, {"sessions": []})
    sync_adapter._httpx_client.get = MagicMock(return_value=response)

    result = sync_adapter.list()

    assert result == []


def test_sync_list_raises_on_error_status(sync_adapter):
    response = _mock_response(500, None)
    sync_adapter._httpx_client.get = MagicMock(return_value=response)

    with pytest.raises(SandboxApiException):
        sync_adapter.list()
