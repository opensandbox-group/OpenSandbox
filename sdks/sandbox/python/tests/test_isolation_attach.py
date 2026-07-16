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
"""Tests for IsolationService.attach (async and sync)."""

from __future__ import annotations

from datetime import datetime
from unittest.mock import AsyncMock, MagicMock

import pytest

from opensandbox.adapters.isolated_adapter import IsolatedSessionsAdapter
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.isolated import (
    BindMount,
    EnvPassthroughSpec,
    IsolatedWorkspaceSpec,
)
from opensandbox.sync.adapters.isolated_adapter import IsolatedSessionsAdapterSync

_FULL_STATE_PAYLOAD = {
    "status": "active",
    "created_at": "2026-01-02T03:04:05Z",
    "last_run_at": "2026-01-02T03:05:06Z",
    "idle_remaining_seconds": 30,
    "profile": "strict",
    "workspace": {"path": "/workspace", "mode": "rw"},
    "extra_writable": ["/tmp", "/var/tmp"],
    "binds": [{"source": "/host/a", "dest": "/sbx/a", "readonly": True}],
    "share_net": False,
    "env_passthrough": {"mode": "allow", "keys": ["PATH", "HOME"]},
    "uid": 1000,
    "gid": 2000,
    "uid_mode": "userns",
    "idle_timeout_seconds": 300,
}

_MINIMAL_STATE_PAYLOAD = {
    "status": "active",
    "created_at": "2026-01-02T03:04:05Z",
    "last_run_at": "2026-01-02T03:04:05Z",
    "idle_remaining_seconds": None,
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


# -------- async --------


@pytest.mark.asyncio
async def test_attach_populates_full_info_when_execd_returns_all_fields(
    async_adapter,
):
    response = _mock_response(200, _FULL_STATE_PAYLOAD)
    async_adapter._httpx_client.get = AsyncMock(return_value=response)

    session = await async_adapter.attach("sess-full")

    async_adapter._httpx_client.get.assert_called_once()
    call_url = async_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-full")

    assert session.session_id == "sess-full"
    info = session.info
    assert info.session_id == "sess-full"
    assert isinstance(info.created_at, datetime)
    assert info.profile == "strict"
    assert isinstance(info.workspace, IsolatedWorkspaceSpec)
    assert info.workspace.path == "/workspace"
    assert info.workspace.mode == "rw"
    assert info.extra_writable == ["/tmp", "/var/tmp"]
    assert info.binds is not None
    assert len(info.binds) == 1
    assert isinstance(info.binds[0], BindMount)
    assert info.binds[0].source == "/host/a"
    assert info.binds[0].dest == "/sbx/a"
    assert info.binds[0].readonly is True
    assert info.share_net is False
    assert isinstance(info.env_passthrough, EnvPassthroughSpec)
    assert info.env_passthrough.mode == "allow"
    assert info.env_passthrough.keys == ["PATH", "HOME"]
    assert info.uid == 1000
    assert info.gid == 2000
    assert info.uid_mode == "userns"
    assert info.idle_timeout_seconds == 300


@pytest.mark.asyncio
async def test_attach_tolerates_missing_creation_params_when_execd_is_older(
    async_adapter,
):
    # First call: attach (GET). Second call: session.get() (GET). Third: delete.
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, _MINIMAL_STATE_PAYLOAD)
    )
    async_adapter._httpx_client.delete = AsyncMock(
        return_value=_mock_response(204, None)
    )

    session = await async_adapter.attach("sess-old")

    info = session.info
    assert info.session_id == "sess-old"
    assert isinstance(info.created_at, datetime)
    # None of the creation-parameter echo fields are populated on older execd.
    assert info.profile is None
    assert info.workspace is None
    assert info.extra_writable is None
    assert info.binds is None
    assert info.share_net is None
    assert info.env_passthrough is None
    assert info.uid is None
    assert info.gid is None
    assert info.uid_mode is None
    assert info.idle_timeout_seconds is None

    # get() and delete() still work: they only need session_id.
    state = await session.get()
    assert state.status == "active"

    await session.delete()

    # attach + get were two GETs against the same session endpoint.
    assert async_adapter._httpx_client.get.call_count == 2
    async_adapter._httpx_client.delete.assert_called_once()


@pytest.mark.asyncio
async def test_attach_propagates_not_found_when_session_missing(async_adapter):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(404, {"code": "SESSION_NOT_FOUND"})
    )

    with pytest.raises(SandboxApiException) as exc_info:
        await async_adapter.attach("missing-sess")

    assert exc_info.value.status_code == 404


@pytest.mark.asyncio
async def test_attach_rejects_blank_session_id(async_adapter):
    from opensandbox.exceptions import InvalidArgumentException

    with pytest.raises(InvalidArgumentException):
        await async_adapter.attach("")
    with pytest.raises(InvalidArgumentException):
        await async_adapter.attach("   ")


# -------- sync --------


def test_sync_attach_populates_full_info_when_execd_returns_all_fields(
    sync_adapter,
):
    response = _mock_response(200, _FULL_STATE_PAYLOAD)
    sync_adapter._httpx_client.get = MagicMock(return_value=response)

    session = sync_adapter.attach("sess-full")

    sync_adapter._httpx_client.get.assert_called_once()
    call_url = sync_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-full")

    info = session.info
    assert info.session_id == "sess-full"
    assert info.profile == "strict"
    assert info.workspace is not None and info.workspace.path == "/workspace"
    assert info.binds is not None and info.binds[0].readonly is True
    assert info.env_passthrough is not None
    assert info.env_passthrough.mode == "allow"
    assert info.uid_mode == "userns"
    assert info.idle_timeout_seconds == 300


def test_sync_attach_tolerates_missing_creation_params_when_execd_is_older(
    sync_adapter,
):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(200, _MINIMAL_STATE_PAYLOAD)
    )
    sync_adapter._httpx_client.delete = MagicMock(
        return_value=_mock_response(204, None)
    )

    session = sync_adapter.attach("sess-old")

    info = session.info
    assert info.session_id == "sess-old"
    assert info.profile is None
    assert info.workspace is None
    assert info.binds is None
    assert info.share_net is None
    assert info.uid is None

    state = session.get()
    assert state.status == "active"

    session.delete()

    assert sync_adapter._httpx_client.get.call_count == 2
    sync_adapter._httpx_client.delete.assert_called_once()


def test_sync_attach_propagates_not_found_when_session_missing(sync_adapter):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(404, {"code": "SESSION_NOT_FOUND"})
    )

    with pytest.raises(SandboxApiException) as exc_info:
        sync_adapter.attach("missing-sess")

    assert exc_info.value.status_code == 404
