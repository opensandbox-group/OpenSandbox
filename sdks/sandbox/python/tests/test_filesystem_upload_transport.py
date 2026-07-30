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
from __future__ import annotations

from io import BytesIO

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync

LARGE_PAYLOAD = b"x" * (20 * 1024)


class _CaptureAsyncTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.request: httpx.Request | None = None
        self.body: bytes = b""

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.request = request
        self.body = await request.aread()
        return httpx.Response(200, request=request, content=b"{}")


class _CaptureSyncTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.request: httpx.Request | None = None
        self.body: bytes = b""

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.request = request
        self.body = request.read()
        return httpx.Response(200, request=request, content=b"{}")


class _RedirectAsyncTransport(httpx.AsyncBaseTransport):
    def __init__(self, status_code: int) -> None:
        self.status_code = status_code
        self.requests: list[httpx.Request] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        async for _ in request.stream:
            pass
        return httpx.Response(
            self.status_code,
            headers={"Location": "/files/redirected-upload"},
            request=request,
        )


class _RedirectSyncTransport(httpx.BaseTransport):
    def __init__(self, status_code: int) -> None:
        self.status_code = status_code
        self.requests: list[httpx.Request] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        for _ in request.stream:
            pass
        return httpx.Response(
            self.status_code,
            headers={"Location": "/files/redirected-upload"},
            request=request,
        )


def _headers(request: httpx.Request) -> dict[str, str]:
    return {k.lower(): v for k, v in request.headers.items()}


@pytest.mark.asyncio
async def test_async_write_files_direct_execd_uses_chunked_upload() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    await adapter.write_files([WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD)])

    assert transport.request is not None
    headers = _headers(transport.request)
    assert headers["transfer-encoding"] == "chunked"
    assert "content-length" not in headers
    assert headers["content-type"].startswith("multipart/form-data; boundary=opensandbox_")
    assert b'name="metadata"; filename="metadata"' in transport.body
    assert b'name="file"; filename="large.bin"' in transport.body
    assert LARGE_PAYLOAD in transport.body

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_write_files_direct_execd_rewinds_seekable_streams() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    stream = BytesIO(LARGE_PAYLOAD)
    stream.read(7)

    await adapter.write_files([WriteEntry(path="/tmp/stream.bin", data=stream)])

    assert transport.request is not None
    assert LARGE_PAYLOAD in transport.body

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_write_files_direct_execd_escapes_filename_header() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    await adapter.write_files([WriteEntry(path='/tmp/weird"name\r\n.txt', data='hello')])

    assert transport.request is not None
    assert b'filename="weird\\"name__.txt"' in transport.body
    assert b'filename="weird"name' not in transport.body

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_write_files_server_proxy_uses_content_length_and_preserves_charset() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport, use_server_proxy=True),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    await adapter.write_files([
        WriteEntry(path="/tmp/large.txt", data="hello", encoding="latin-1"),
        WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD),
    ])

    assert transport.request is not None
    headers = _headers(transport.request)
    assert "transfer-encoding" not in headers
    assert headers["content-length"] == str(len(transport.body))
    assert headers["content-type"].startswith("multipart/form-data; boundary=")
    assert b"text/plain; charset=latin-1" in transport.body
    assert LARGE_PAYLOAD in transport.body

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_write_files_direct_execd_encodes_strings_with_entry_encoding() -> None:
    transport = _CaptureAsyncTransport()
    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    await adapter.write_files([
        WriteEntry(path="/tmp/latin1.txt", data="olá", encoding="latin-1"),
    ])

    assert transport.request is not None
    assert b"text/plain; charset=latin-1" in transport.body
    assert "olá".encode("latin-1") in transport.body
    assert "olá".encode() not in transport.body

    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize("status_code", [301, 302, 303, 307, 308])
async def test_async_chunked_upload_does_not_follow_redirects(
    status_code: int,
) -> None:
    transport = _RedirectAsyncTransport(status_code)
    adapter = FilesystemAdapter(
        ConnectionConfig(
            protocol="http",
            transport=transport,
            use_server_proxy=False,
            follow_redirects=True,
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.write_files(
            [WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD)]
        )

    assert exc_info.value.status_code == status_code
    assert isinstance(exc_info.value.__cause__, httpx.HTTPStatusError)
    assert len(transport.requests) == 1

    await adapter._httpx_client.aclose()


def test_sync_write_files_direct_execd_uses_chunked_upload() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    adapter.write_files([WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD)])

    assert transport.request is not None
    headers = _headers(transport.request)
    assert headers["transfer-encoding"] == "chunked"
    assert "content-length" not in headers
    assert headers["content-type"].startswith("multipart/form-data; boundary=opensandbox_")
    assert b'name="metadata"; filename="metadata"' in transport.body
    assert b'name="file"; filename="large.bin"' in transport.body
    assert LARGE_PAYLOAD in transport.body

    adapter._httpx_client.close()


def test_sync_write_files_direct_execd_rewinds_seekable_streams() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    stream = BytesIO(LARGE_PAYLOAD)
    stream.read(7)

    adapter.write_files([WriteEntry(path="/tmp/stream.bin", data=stream)])

    assert transport.request is not None
    assert LARGE_PAYLOAD in transport.body

    adapter._httpx_client.close()


def test_sync_write_files_direct_execd_escapes_filename_header() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=transport, use_server_proxy=False),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    adapter.write_files([WriteEntry(path='/tmp/weird"name\r\n.txt', data='hello')])

    assert transport.request is not None
    assert b'filename="weird\\"name__.txt"' in transport.body
    assert b'filename="weird"name' not in transport.body

    adapter._httpx_client.close()


def test_sync_write_files_server_proxy_uses_content_length_and_preserves_charset() -> None:
    transport = _CaptureSyncTransport()
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=transport, use_server_proxy=True),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    adapter.write_files([
        WriteEntry(path="/tmp/large.txt", data="hello", encoding="latin-1"),
        WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD),
    ])

    assert transport.request is not None
    headers = _headers(transport.request)
    assert "transfer-encoding" not in headers
    assert headers["content-length"] == str(len(transport.body))
    assert headers["content-type"].startswith("multipart/form-data; boundary=")
    assert b"text/plain; charset=latin-1" in transport.body
    assert LARGE_PAYLOAD in transport.body

    adapter._httpx_client.close()


@pytest.mark.parametrize("status_code", [301, 302, 303, 307, 308])
def test_sync_chunked_upload_does_not_follow_redirects(status_code: int) -> None:
    transport = _RedirectSyncTransport(status_code)
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(
            protocol="http",
            transport=transport,
            use_server_proxy=False,
            follow_redirects=True,
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )

    with pytest.raises(SandboxApiException) as exc_info:
        adapter.write_files([WriteEntry(path="/tmp/large.bin", data=LARGE_PAYLOAD)])

    assert exc_info.value.status_code == status_code
    assert isinstance(exc_info.value.__cause__, httpx.HTTPStatusError)
    assert len(transport.requests) == 1

    adapter._httpx_client.close()
