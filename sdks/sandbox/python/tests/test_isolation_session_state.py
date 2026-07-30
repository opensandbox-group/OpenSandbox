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
"""Tests for IsolationSession.get returning the extended session-state shape.

Covers both the async adapter (:mod:`opensandbox.adapters.isolated_adapter`)
and the sync adapter (:mod:`opensandbox.sync.adapters.isolated_adapter`).
"""

from __future__ import annotations

from datetime import datetime
from unittest.mock import AsyncMock, MagicMock

import pytest

from opensandbox.adapters.isolated_adapter import IsolatedSessionsAdapter
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

_ZERO_IDLE_TIMEOUT_PAYLOAD = {
    "status": "active",
    "created_at": "2026-01-02T03:04:05Z",
    "last_run_at": "2026-01-02T03:04:05Z",
    "idle_remaining_seconds": None,
    "idle_timeout_seconds": 0,
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
async def test_get_populates_full_state_when_execd_returns_all_fields(
    async_adapter,
):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, _FULL_STATE_PAYLOAD)
    )

    state = await async_adapter._get("sess-full")

    async_adapter._httpx_client.get.assert_called_once()
    call_url = async_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-full")

    assert state.status == "active"
    assert isinstance(state.created_at, datetime)
    assert isinstance(state.last_run_at, datetime)
    assert state.idle_remaining_seconds == 30
    assert state.profile == "strict"
    assert isinstance(state.workspace, IsolatedWorkspaceSpec)
    assert state.workspace.path == "/workspace"
    assert state.workspace.mode == "rw"
    assert state.extra_writable == ["/tmp", "/var/tmp"]
    assert state.binds is not None
    assert len(state.binds) == 1
    assert isinstance(state.binds[0], BindMount)
    assert state.binds[0].source == "/host/a"
    assert state.binds[0].dest == "/sbx/a"
    assert state.binds[0].readonly is True
    assert state.share_net is False
    assert isinstance(state.env_passthrough, EnvPassthroughSpec)
    assert state.env_passthrough.mode == "allow"
    assert state.env_passthrough.keys == ["PATH", "HOME"]
    assert state.uid == 1000
    assert state.gid == 2000
    assert state.uid_mode == "userns"
    assert state.idle_timeout_seconds == 300


@pytest.mark.asyncio
async def test_get_tolerates_missing_echo_fields_when_execd_is_older(
    async_adapter,
):
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, _MINIMAL_STATE_PAYLOAD)
    )

    state = await async_adapter._get("sess-old")

    assert state.status == "active"
    assert isinstance(state.created_at, datetime)
    assert isinstance(state.last_run_at, datetime)
    assert state.idle_remaining_seconds is None
    # None of the creation-parameter echo fields are populated on older execd.
    assert state.profile is None
    assert state.workspace is None
    assert state.extra_writable is None
    assert state.binds is None
    assert state.share_net is None
    assert state.env_passthrough is None
    assert state.uid is None
    assert state.gid is None
    assert state.uid_mode is None
    assert state.idle_timeout_seconds is None


@pytest.mark.asyncio
async def test_get_preserves_idle_timeout_zero(async_adapter):
    """``idle_timeout_seconds: 0`` means "no idle timeout" and MUST be preserved.

    Regression: an ``if state[key]`` (falsy) check would wrongly drop 0 and
    return ``None``, hiding the "no timeout" configuration from callers
    performing long-window recovery.
    """
    async_adapter._httpx_client.get = AsyncMock(
        return_value=_mock_response(200, _ZERO_IDLE_TIMEOUT_PAYLOAD)
    )

    state = await async_adapter._get("sess-zero")

    assert state.status == "active"
    assert state.idle_timeout_seconds == 0
    assert state.idle_timeout_seconds is not None


# -------- sync --------


def test_sync_get_populates_full_state_when_execd_returns_all_fields(
    sync_adapter,
):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(200, _FULL_STATE_PAYLOAD)
    )

    state = sync_adapter._get("sess-full")

    sync_adapter._httpx_client.get.assert_called_once()
    call_url = sync_adapter._httpx_client.get.call_args.args[0]
    assert call_url.endswith("/v1/isolated/session/sess-full")

    assert state.status == "active"
    assert state.profile == "strict"
    assert state.workspace is not None
    assert state.workspace.path == "/workspace"
    assert state.binds is not None
    assert state.binds[0].readonly is True
    assert state.env_passthrough is not None
    assert state.env_passthrough.mode == "allow"
    assert state.uid == 1000
    assert state.gid == 2000
    assert state.uid_mode == "userns"
    assert state.idle_timeout_seconds == 300


def test_sync_get_tolerates_missing_echo_fields_when_execd_is_older(sync_adapter):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(200, _MINIMAL_STATE_PAYLOAD)
    )

    state = sync_adapter._get("sess-old")

    assert state.status == "active"
    assert state.profile is None
    assert state.workspace is None
    assert state.binds is None
    assert state.share_net is None
    assert state.env_passthrough is None
    assert state.uid is None
    assert state.gid is None
    assert state.uid_mode is None
    assert state.idle_timeout_seconds is None


def test_sync_get_preserves_idle_timeout_zero(sync_adapter):
    sync_adapter._httpx_client.get = MagicMock(
        return_value=_mock_response(200, _ZERO_IDLE_TIMEOUT_PAYLOAD)
    )

    state = sync_adapter._get("sess-zero")

    assert state.status == "active"
    assert state.idle_timeout_seconds == 0
    assert state.idle_timeout_seconds is not None
