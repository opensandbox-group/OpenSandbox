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

"""Streamed download failures keep the server's structured error."""

from datetime import timedelta

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException, SandboxRateLimitException
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import (
    FilesystemAdapterSync,
)
from opensandbox.sync.adapters.isolated_filesystem_adapter import (
    IsolatedFilesystemAdapterSync,
)

_SESSION_ID = "12345678-1234-5678-1234-567812345678"
_ENDPOINT = SandboxEndpoint(endpoint="localhost:44772")


def _not_found(_: httpx.Request) -> httpx.Response:
    return httpx.Response(
        404,
        json={"code": "FILE_NOT_FOUND", "message": "file not found: /tmp/missing"},
        headers={"x-request-id": "req-404"},
    )


def _rate_limited(_: httpx.Request) -> httpx.Response:
    return httpx.Response(
        429,
        json={"code": "RATE_LIMITED", "message": "slow down"},
        headers={"Retry-After": "7"},
    )


@pytest.mark.asyncio
@pytest.mark.parametrize("isolated", [False, True])
async def test_async_stream_error_keeps_structured_error(isolated: bool) -> None:
    config = ConnectionConfig(protocol="http", transport=httpx.MockTransport(_not_found))
    adapter = (
        IsolatedFilesystemAdapter(config, _ENDPOINT, _SESSION_ID)
        if isolated
        else FilesystemAdapter(config, _ENDPOINT)
    )
    try:
        with pytest.raises(SandboxApiException) as exc:
            await adapter.read_bytes_stream("/tmp/missing")
        assert exc.value.status_code == 404
        assert exc.value.request_id == "req-404"
        assert exc.value.error is not None
        assert exc.value.error.code == "FILE_NOT_FOUND"
        assert "file not found: /tmp/missing" in str(exc.value)
    finally:
        await adapter._httpx_client.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("isolated", [False, True])
async def test_async_stream_429_raises_rate_limit(isolated: bool) -> None:
    config = ConnectionConfig(protocol="http", transport=httpx.MockTransport(_rate_limited))
    adapter = (
        IsolatedFilesystemAdapter(config, _ENDPOINT, _SESSION_ID)
        if isolated
        else FilesystemAdapter(config, _ENDPOINT)
    )
    try:
        with pytest.raises(SandboxRateLimitException) as exc:
            await adapter.read_bytes_stream("/tmp/file")
        assert exc.value.retry_after == timedelta(seconds=7)
    finally:
        await adapter._httpx_client.aclose()


@pytest.mark.parametrize("isolated", [False, True])
def test_sync_stream_error_keeps_structured_error(isolated: bool) -> None:
    config = ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(_not_found))
    adapter = (
        IsolatedFilesystemAdapterSync(config, _ENDPOINT, _SESSION_ID)
        if isolated
        else FilesystemAdapterSync(config, _ENDPOINT)
    )
    try:
        with pytest.raises(SandboxApiException) as exc:
            adapter.read_bytes_stream("/tmp/missing")
        assert exc.value.status_code == 404
        assert exc.value.error is not None
        assert exc.value.error.code == "FILE_NOT_FOUND"
    finally:
        adapter._httpx_client.close()


@pytest.mark.parametrize("isolated", [False, True])
def test_sync_stream_429_raises_rate_limit(isolated: bool) -> None:
    config = ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(_rate_limited))
    adapter = (
        IsolatedFilesystemAdapterSync(config, _ENDPOINT, _SESSION_ID)
        if isolated
        else FilesystemAdapterSync(config, _ENDPOINT)
    )
    try:
        with pytest.raises(SandboxRateLimitException) as exc:
            adapter.read_bytes_stream("/tmp/file")
        assert exc.value.retry_after == timedelta(seconds=7)
    finally:
        adapter._httpx_client.close()
