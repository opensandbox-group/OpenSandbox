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
"""Execd-plane request headers follow the use_server_proxy declaration.

The declaration is the only signal: proxy URL shapes are deployment-derived
and cannot be guessed. When the client declared server-proxy mode the
request passes the server's auth gate and must carry the API key; in direct
mode the key would travel straight into the untrusted sandbox and must
never be attached.
"""

from __future__ import annotations

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync

# Realistic shapes: proxy base comes from the server's configured eip and may
# carry a path prefix; direct is a pod address.
PROXY_FORM_ENDPOINT = "gateway.example.com/opensandbox/sandboxes/sbx-123/proxy/18080"
DIRECT_ENDPOINT = "10.42.1.7:18080"


class _CaptureAsyncTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.request: httpx.Request | None = None

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.request = request
        await request.aread()
        return httpx.Response(200, request=request, content=b"{}")


class _CaptureSyncTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.request: httpx.Request | None = None

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.request = request
        request.read()
        return httpx.Response(200, request=request, content=b"{}")


def _headers(request: httpx.Request | None) -> dict[str, str]:
    assert request is not None
    return {k.lower(): v for k, v in request.headers.items()}


@pytest.mark.asyncio
async def test_async_execd_request_carries_api_key_when_proxy_declared() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(
            api_key="tenant-key-1",
            protocol="https",
            transport=transport,
            use_server_proxy=True,
        ),
        SandboxEndpoint(endpoint=PROXY_FORM_ENDPOINT),
    )

    await adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    assert _headers(transport.request)["open-sandbox-api-key"] == "tenant-key-1"

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_execd_declaration_gates_key_regardless_of_endpoint_shape() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(
            api_key="tenant-key-1",
            protocol="http",
            transport=transport,
            # The declaration is the only gate: the client said proxy mode,
            # so the key is attached even though the endpoint string has no
            # recognizable proxy-route shape (deployments control their URLs).
            use_server_proxy=True,
        ),
        SandboxEndpoint(endpoint=DIRECT_ENDPOINT),
    )

    await adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    assert _headers(transport.request)["open-sandbox-api-key"] == "tenant-key-1"

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_execd_request_never_carries_api_key_in_direct_mode() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(
            api_key="tenant-key-1",
            protocol="http",
            transport=transport,
            use_server_proxy=False,
        ),
        SandboxEndpoint(endpoint=DIRECT_ENDPOINT),
    )

    await adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    # The key must not travel straight into the untrusted sandbox.
    assert "open-sandbox-api-key" not in _headers(transport.request)

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_execd_request_without_api_key_omits_header(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("OPEN_SANDBOX_API_KEY", raising=False)
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(
            api_key=None,
            protocol="https",
            transport=transport,
            use_server_proxy=True,
        ),
        SandboxEndpoint(endpoint=PROXY_FORM_ENDPOINT),
    )

    await adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    assert "open-sandbox-api-key" not in _headers(transport.request)

    await adapter._httpx_client.aclose()


def test_sync_execd_request_carries_api_key_when_proxy_declared() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(
            api_key="tenant-key-1",
            protocol="https",
            transport=transport,
            use_server_proxy=True,
        ),
        SandboxEndpoint(endpoint=PROXY_FORM_ENDPOINT),
    )

    adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    assert _headers(transport.request)["open-sandbox-api-key"] == "tenant-key-1"

    adapter._httpx_client.close()


def test_sync_execd_request_never_carries_api_key_in_direct_mode() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(
            api_key="tenant-key-1",
            protocol="http",
            transport=transport,
            use_server_proxy=False,
        ),
        SandboxEndpoint(endpoint=DIRECT_ENDPOINT),
    )

    adapter.write_files([WriteEntry(path="/tmp/a.txt", data="hello")])

    assert "open-sandbox-api-key" not in _headers(transport.request)

    adapter._httpx_client.close()
