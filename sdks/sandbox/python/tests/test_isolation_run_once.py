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
"""Tests for IsolationService.run_once and .session context manager."""

from __future__ import annotations

from unittest.mock import AsyncMock, patch

import pytest

from opensandbox.adapters.isolated_adapter import IsolatedSessionsAdapter
from opensandbox.models.execd import Execution
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest,
    IsolatedWorkspaceSpec,
)


@pytest.fixture
def adapter():
    from opensandbox.config import ConnectionConfig
    from opensandbox.models.sandboxes import SandboxEndpoint

    config = ConnectionConfig(api_key="test-key", domain="localhost")
    endpoint = SandboxEndpoint(endpoint="localhost:8080", headers={})
    return IsolatedSessionsAdapter(config, endpoint)


@pytest.fixture
def mock_session():
    session = AsyncMock()
    session.session_id = "sess-123"
    session.run = AsyncMock(
        return_value=Execution(id=None, execution_count=None, result=[], error=None)
    )
    session.delete = AsyncMock()
    return session


@pytest.mark.asyncio
async def test_run_once_creates_runs_deletes(adapter, mock_session):
    create_mock = AsyncMock(return_value=mock_session)
    with patch.object(adapter, "create", create_mock):
        result = await adapter.run_once("echo hello", workspace="/workspace")

    create_mock.assert_called_once()
    mock_session.run.assert_called_once_with("echo hello", opts=None, handlers=None)
    mock_session.delete.assert_called_once()
    assert result is not None


@pytest.mark.asyncio
async def test_run_once_deletes_on_run_failure(adapter, mock_session):
    mock_session.run = AsyncMock(side_effect=RuntimeError("boom"))
    with patch.object(adapter, "create", AsyncMock(return_value=mock_session)):
        with pytest.raises(RuntimeError, match="boom"):
            await adapter.run_once("bad cmd", workspace="/workspace")

    mock_session.delete.assert_called_once()


@pytest.mark.asyncio
async def test_run_once_tolerates_delete_failure(adapter, mock_session):
    mock_session.delete = AsyncMock(side_effect=RuntimeError("delete failed"))
    with patch.object(adapter, "create", AsyncMock(return_value=mock_session)):
        result = await adapter.run_once("echo ok", workspace="/workspace")

    assert result is not None


@pytest.mark.asyncio
async def test_run_once_passes_options(adapter, mock_session):
    from opensandbox.models.isolated import IsolatedRunOpts

    opts = IsolatedRunOpts(envs={"FOO": "bar"}, timeout_seconds=30)
    create_mock = AsyncMock(return_value=mock_session)
    with patch.object(adapter, "create", create_mock):
        await adapter.run_once(
            "echo hi",
            workspace="/work",
            workspace_mode="overlay",
            opts=opts,
            profile="strict",
            share_net=True,
        )

    call_args = create_mock.call_args[0][0]
    assert call_args.workspace.path == "/work"
    assert call_args.workspace.mode == "overlay"
    assert call_args.profile == "strict"
    assert call_args.share_net is True
    mock_session.run.assert_called_once_with("echo hi", opts=opts, handlers=None)


@pytest.mark.asyncio
async def test_session_context_manager_deletes_on_exit(adapter, mock_session):
    with patch.object(adapter, "create", AsyncMock(return_value=mock_session)):
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/workspace")
        )
        async with adapter.session(request) as session:
            await session.run("echo hi")

    mock_session.delete.assert_called_once()


@pytest.mark.asyncio
async def test_session_context_manager_deletes_on_exception(adapter, mock_session):
    mock_session.run = AsyncMock(side_effect=ValueError("oops"))
    with patch.object(adapter, "create", AsyncMock(return_value=mock_session)):
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/workspace")
        )
        with pytest.raises(ValueError, match="oops"):
            async with adapter.session(request) as session:
                await session.run("bad")

    mock_session.delete.assert_called_once()


@pytest.mark.asyncio
async def test_session_context_manager_tolerates_delete_failure(adapter, mock_session):
    mock_session.delete = AsyncMock(side_effect=RuntimeError("cleanup fail"))
    with patch.object(adapter, "create", AsyncMock(return_value=mock_session)):
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/workspace")
        )
        async with adapter.session(request) as session:
            await session.run("echo ok")
    # no exception propagated
