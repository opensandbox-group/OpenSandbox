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

from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import IOBase, UnsupportedOperation
from tempfile import TemporaryFile
from threading import Thread

import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.health_adapter import HealthAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync
from opensandbox.sync.adapters.health_adapter import HealthAdapterSync
from opensandbox.sync.adapters.isolated_filesystem_adapter import (
    IsolatedFilesystemAdapterSync,
)

_PAYLOAD = b"network-upload-payload-" * 4096
_SESSION_ID = "12345678-1234-5678-1234-567812345678"
_SENSITIVE_HEADERS = {
    "OPEN-SANDBOX-API-KEY": "api-secret",
    "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
    "OpenSandbox-Secure-Access": "access-secret",
    "X-Custom-Header": "preserved",
}


@dataclass(frozen=True)
class _RecordedRequest:
    method: str
    path: str
    headers: dict[str, str]
    body: bytes


@dataclass
class _ServerState:
    redirect_base_url: str | None = None
    requests: list[_RecordedRequest] = field(default_factory=list)


def _read_chunked_body(handler: BaseHTTPRequestHandler) -> bytes:
    chunks: list[bytes] = []
    while True:
        size_line = handler.rfile.readline()
        if not size_line:
            raise ConnectionError("client closed connection during chunked upload")
        size = int(size_line.split(b";", 1)[0].strip(), 16)
        if size == 0:
            while handler.rfile.readline() not in (b"\r\n", b"\n", b""):
                pass
            return b"".join(chunks)

        chunks.append(handler.rfile.read(size))
        if handler.rfile.read(2) != b"\r\n":
            raise ValueError("invalid chunk delimiter")


def _read_request_body(handler: BaseHTTPRequestHandler) -> bytes:
    transfer_encoding = handler.headers.get("Transfer-Encoding", "")
    if "chunked" in transfer_encoding.lower():
        return _read_chunked_body(handler)

    content_length = int(handler.headers.get("Content-Length", "0"))
    return handler.rfile.read(content_length)


def _make_handler(state: _ServerState) -> type[BaseHTTPRequestHandler]:
    class _Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self) -> None:  # noqa: N802
            self._handle_request()

        def do_POST(self) -> None:  # noqa: N802
            self._handle_request()

        def _handle_request(self) -> None:
            body = _read_request_body(self)
            state.requests.append(
                _RecordedRequest(
                    method=self.command,
                    path=self.path,
                    headers={name.lower(): value for name, value in self.headers.items()},
                    body=body,
                )
            )

            if state.redirect_base_url is not None:
                self.send_response(307)
                self.send_header(
                    "Location",
                    f"{state.redirect_base_url}{self.path}",
                )
            else:
                self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, format: str, *args: object) -> None:
            pass

    return _Handler


class _LoopbackServer:
    def __init__(self, *, redirect_base_url: str | None = None) -> None:
        self.state = _ServerState(redirect_base_url=redirect_base_url)
        self._server = ThreadingHTTPServer(
            ("127.0.0.1", 0),
            _make_handler(self.state),
        )
        self._server.daemon_threads = True
        self._thread = Thread(
            target=self._server.serve_forever,
            kwargs={"poll_interval": 0.01},
            daemon=True,
        )

    @property
    def address(self) -> str:
        host, port = self._server.server_address
        return f"{host}:{port}"

    @property
    def base_url(self) -> str:
        return f"http://{self.address}"

    @property
    def requests(self) -> list[_RecordedRequest]:
        return self.state.requests

    def start(self) -> None:
        self._thread.start()

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)


class _NonSeekableFile(IOBase):
    """Known-length file that cannot be rewound after its first send."""

    def __init__(self, data: bytes) -> None:
        self._file = TemporaryFile()
        self._file.write(data)
        self._file.flush()
        self._file.seek(0)

    def fileno(self) -> int:
        return self._file.fileno()

    def readable(self) -> bool:
        return True

    def seekable(self) -> bool:
        return False

    def seek(self, offset: int, whence: int = 0) -> int:
        del offset, whence
        raise UnsupportedOperation("stream is not seekable")

    def read(self, size: int = -1) -> bytes:
        return self._file.read(size)

    def close(self) -> None:
        self._file.close()
        super().close()


@contextmanager
def _redirect_servers() -> Iterator[tuple[_LoopbackServer, _LoopbackServer]]:
    destination = _LoopbackServer()
    destination.start()
    source = _LoopbackServer(redirect_base_url=destination.base_url)
    source.start()
    try:
        yield source, destination
    finally:
        source.close()
        destination.close()


def _assert_cross_origin_headers(
    source: _LoopbackServer,
    destination: _LoopbackServer,
) -> None:
    assert len(source.requests) == 1
    assert len(destination.requests) == 1
    assert source.requests[0].method == "GET"
    assert destination.requests[0].method == "GET"

    source_headers = source.requests[0].headers
    assert source_headers["open-sandbox-api-key"] == "api-secret"
    assert source_headers["opensandbox-egress-auth"] == "egress-secret"
    assert source_headers["opensandbox-secure-access"] == "access-secret"
    assert source_headers["x-custom-header"] == "preserved"

    destination_headers = destination.requests[0].headers
    assert "open-sandbox-api-key" not in destination_headers
    assert "opensandbox-egress-auth" not in destination_headers
    assert "opensandbox-secure-access" not in destination_headers
    assert destination_headers["x-custom-header"] == "preserved"


@pytest.mark.asyncio
async def test_async_public_api_strips_headers_on_real_redirect() -> None:
    with _redirect_servers() as (source, destination):
        adapter = HealthAdapter(
            ConnectionConfig(
                protocol="http",
                follow_redirects=True,
                headers=_SENSITIVE_HEADERS,
            ),
            SandboxEndpoint(endpoint=source.address),
        )
        try:
            assert await adapter.ping("sandbox-id") is True
        finally:
            await adapter._httpx_client.aclose()

        assert source.requests[0].path == "/ping"
        assert destination.requests[0].path == "/ping"
        _assert_cross_origin_headers(source, destination)


def test_sync_public_api_strips_headers_on_real_redirect() -> None:
    with _redirect_servers() as (source, destination):
        adapter = HealthAdapterSync(
            ConnectionConfigSync(
                protocol="http",
                follow_redirects=True,
                headers=_SENSITIVE_HEADERS,
            ),
            SandboxEndpoint(endpoint=source.address),
        )
        try:
            assert adapter.ping("sandbox-id") is True
        finally:
            adapter._httpx_client.close()

        assert source.requests[0].path == "/ping"
        assert destination.requests[0].path == "/ping"
        _assert_cross_origin_headers(source, destination)


@pytest.mark.asyncio
async def test_async_chunked_upload_is_not_replayed_over_network() -> None:
    with _redirect_servers() as (source, destination):
        adapter = FilesystemAdapter(
            ConnectionConfig(
                protocol="http",
                follow_redirects=True,
                use_server_proxy=False,
            ),
            SandboxEndpoint(endpoint=source.address),
        )
        try:
            with pytest.raises(SandboxApiException) as exc_info:
                await adapter.write_files(
                    [WriteEntry(path="/tmp/data.bin", data=_PAYLOAD)]
                )
        finally:
            await adapter._httpx_client.aclose()

        assert exc_info.value.status_code == 307
        assert len(source.requests) == 1
        assert destination.requests == []
        request = source.requests[0]
        assert request.method == "POST"
        assert request.path == "/files/upload"
        assert request.headers["transfer-encoding"] == "chunked"
        assert "content-length" not in request.headers
        assert _PAYLOAD in request.body


def test_sync_chunked_upload_is_not_replayed_over_network() -> None:
    with _redirect_servers() as (source, destination):
        adapter = FilesystemAdapterSync(
            ConnectionConfigSync(
                protocol="http",
                follow_redirects=True,
                use_server_proxy=False,
            ),
            SandboxEndpoint(endpoint=source.address),
        )
        try:
            with pytest.raises(SandboxApiException) as exc_info:
                adapter.write_files(
                    [WriteEntry(path="/tmp/data.bin", data=_PAYLOAD)]
                )
        finally:
            adapter._httpx_client.close()

        assert exc_info.value.status_code == 307
        assert len(source.requests) == 1
        assert destination.requests == []
        request = source.requests[0]
        assert request.method == "POST"
        assert request.path == "/files/upload"
        assert request.headers["transfer-encoding"] == "chunked"
        assert "content-length" not in request.headers
        assert _PAYLOAD in request.body


@pytest.mark.asyncio
async def test_async_isolated_upload_is_not_replayed_over_network() -> None:
    with _redirect_servers() as (source, destination):
        adapter = IsolatedFilesystemAdapter(
            ConnectionConfig(
                protocol="http",
                follow_redirects=True,
            ),
            SandboxEndpoint(endpoint=source.address),
            _SESSION_ID,
        )
        try:
            with _NonSeekableFile(_PAYLOAD) as stream:
                with pytest.raises(SandboxApiException) as exc_info:
                    await adapter.write_files(
                        [WriteEntry(path="/tmp/data.bin", data=stream)]
                    )
        finally:
            await adapter._httpx_client.aclose()

        assert exc_info.value.status_code == 307
        assert len(source.requests) == 1
        assert destination.requests == []
        request = source.requests[0]
        assert request.method == "POST"
        assert (
            request.path
            == f"/v1/isolated/session/{_SESSION_ID}/files/upload"
        )
        assert "transfer-encoding" not in request.headers
        assert "content-length" in request.headers
        assert _PAYLOAD in request.body


def test_sync_isolated_upload_is_not_replayed_over_network() -> None:
    with _redirect_servers() as (source, destination):
        adapter = IsolatedFilesystemAdapterSync(
            ConnectionConfigSync(
                protocol="http",
                follow_redirects=True,
            ),
            SandboxEndpoint(endpoint=source.address),
            _SESSION_ID,
        )
        try:
            with _NonSeekableFile(_PAYLOAD) as stream:
                with pytest.raises(SandboxApiException) as exc_info:
                    adapter.write_files(
                        [WriteEntry(path="/tmp/data.bin", data=stream)]
                    )
        finally:
            adapter._httpx_client.close()

        assert exc_info.value.status_code == 307
        assert len(source.requests) == 1
        assert destination.requests == []
        request = source.requests[0]
        assert request.method == "POST"
        assert (
            request.path
            == f"/v1/isolated/session/{_SESSION_ID}/files/upload"
        )
        assert "transfer-encoding" not in request.headers
        assert "content-length" in request.headers
        assert _PAYLOAD in request.body
