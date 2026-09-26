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
"""Tests for the isolated-session filesystem adapter.

Regression coverage for the session-scoped upload/download URL templates and
for the audit finding that a str payload with a non-UTF-8 ``encoding`` used to
be sent as UTF-8 bytes (the declared encoding was never applied).
"""

from __future__ import annotations

import httpx
import pytest

from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.isolated_filesystem_adapter import (
    IsolatedFilesystemAdapterSync,
)

SESSION_ID = "12345678-1234-5678-1234-567812345678"


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


def _endpoint() -> SandboxEndpoint:
    return SandboxEndpoint(endpoint="127.0.0.1:44772", headers={})


def _multipart_file_bytes(body: bytes, boundary_prefix: str = "file") -> bytes:
    """Extract the bytes of the first multipart part named 'file'."""
    marker = b'name="file"'
    idx = body.find(marker)
    assert idx != -1, "no 'file' part in multipart body"
    start = body.find(b"\r\n\r\n", idx) + 4
    end = body.find(b"\r\n--", start)
    return body[start:end]


@pytest.mark.asyncio
async def test_async_write_files_encodes_str_with_declared_encoding() -> None:
    transport = _CaptureAsyncTransport()
    config = ConnectionConfig(domain="localhost:8080", transport=transport)
    adapter = IsolatedFilesystemAdapter(config, _endpoint(), SESSION_ID)

    await adapter.write_files(
        [WriteEntry(path="/tmp/cafe.txt", data="café", encoding="latin-1")]
    )

    assert transport.request is not None
    assert transport.request.url.path == (
        f"/v1/isolated/session/{SESSION_ID}/files/upload"
    )
    payload = _multipart_file_bytes(transport.body)
    assert payload == "café".encode("latin-1")
    # Regression guard: this is NOT UTF-8 (where é == b'\xc3\xa9').
    assert payload != "café".encode()


def test_sync_write_files_encodes_str_with_declared_encoding() -> None:
    transport = _CaptureSyncTransport()
    config = ConnectionConfigSync(domain="localhost:8080", transport=transport)
    adapter = IsolatedFilesystemAdapterSync(config, _endpoint(), SESSION_ID)

    adapter.write_files(
        [WriteEntry(path="/tmp/cafe.txt", data="café", encoding="latin-1")]
    )

    assert transport.request is not None
    assert transport.request.url.path == (
        f"/v1/isolated/session/{SESSION_ID}/files/upload"
    )
    payload = _multipart_file_bytes(transport.body)
    assert payload == "café".encode("latin-1")
