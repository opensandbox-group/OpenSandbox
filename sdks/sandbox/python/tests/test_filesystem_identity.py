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

import asyncio

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.filesystem_identity import filesystem_identity_path
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.filesystem import WriteEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sandbox import Sandbox
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync
from opensandbox.sync.sandbox import SandboxSync

PREFIX = "/sandboxes/example/port/44772"
PAYLOAD = b"binary\x00\xff" * 4096


def endpoint():
    return SandboxEndpoint(
        endpoint="localhost:44772" + PREFIX,
        headers={"X-EXECD-ACCESS-TOKEN": "secret", "X-Routing": "sandbox"},
    )


def capture_handler(requests):
    def handle(request):
        requests.append(request)
        if request.url.path.endswith("/files/download"):
            return httpx.Response(200, content=PAYLOAD)
        return httpx.Response(200, json={})
    return handle


@pytest.mark.asyncio
async def test_async_identity_preserves_transport_headers_and_original_client():
    requests = []
    transport = httpx.MockTransport(capture_handler(requests))
    original = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=transport), endpoint()
    )
    sandbox = object.__new__(Sandbox)
    sandbox._filesystem_service = original
    a = sandbox.files_with_identity(1001, 2000)
    b = sandbox.files_with_identity(1002, 2000)
    try:
        await asyncio.gather(a.get_file_info(["/a"]), b.get_file_info(["/b"]))
        assert await a.read_bytes("/file") == PAYLOAD
        await a.write_files([WriteEntry(path="/file", data=PAYLOAD)])
        await original.get_file_info(["/original"])
        assert {r.url.path for r in requests[:2]} == {
            PREFIX + "/v1/filesystem/1001/2000/files/info",
            PREFIX + "/v1/filesystem/1002/2000/files/info",
        }
        assert requests[2].url.path == PREFIX + "/v1/filesystem/1001/2000/files/download"
        assert requests[3].url.path == PREFIX + "/v1/filesystem/1001/2000/files/upload"
        assert PAYLOAD in requests[3].content
        assert requests[4].url.path == PREFIX + "/files/info"
        for request in requests:
            assert request.headers["X-EXECD-ACCESS-TOKEN"] == "secret"
            assert request.headers["X-Routing"] == "sandbox"
    finally:
        for adapter in (original, a, b):
            await adapter._httpx_client.aclose()


def test_sync_identity_preserves_transport_headers_and_original_client():
    requests = []
    transport = httpx.MockTransport(capture_handler(requests))
    original = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=transport), endpoint()
    )
    sandbox = object.__new__(SandboxSync)
    sandbox._filesystem_service = original
    scoped = sandbox.files_with_identity(1001, 2000)
    try:
        scoped.get_file_info(["/file"])
        assert scoped.read_bytes("/file") == PAYLOAD
        scoped.write_files([WriteEntry(path="/file", data=PAYLOAD)])
        original.get_file_info(["/original"])
        assert [r.url.path for r in requests] == [
            PREFIX + "/v1/filesystem/1001/2000/files/info",
            PREFIX + "/v1/filesystem/1001/2000/files/download",
            PREFIX + "/v1/filesystem/1001/2000/files/upload",
            PREFIX + "/files/info",
        ]
        assert PAYLOAD in requests[2].content
        assert all(r.headers["X-EXECD-ACCESS-TOKEN"] == "secret" for r in requests)
    finally:
        scoped._httpx_client.close()
        original._httpx_client.close()


@pytest.mark.parametrize("status", [404, 501, 503])
@pytest.mark.asyncio
async def test_async_identity_never_falls_back_on_unsupported_server(status):
    requests = []

    def unsupported(request):
        requests.append(request)
        return httpx.Response(status, json={"code": "FILESYSTEM_IDENTITY_UNAVAILABLE", "message": "unsupported"})

    original = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(unsupported)),
        endpoint(),
    )
    scoped = original.with_identity(1001, 2000)
    try:
        with pytest.raises(SandboxApiException):
            await scoped.read_bytes("/file")
        assert len(requests) == 1
        assert requests[0].url.path == PREFIX + "/v1/filesystem/1001/2000/files/download"
    finally:
        await scoped._httpx_client.aclose()
        await original._httpx_client.aclose()


@pytest.mark.parametrize("status", [404, 501, 503])
def test_sync_identity_never_falls_back_on_unsupported_server(status):
    requests = []

    def unsupported(request):
        requests.append(request)
        return httpx.Response(status, json={"code": "FILESYSTEM_IDENTITY_UNAVAILABLE", "message": "unsupported"})

    original = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(unsupported)),
        endpoint(),
    )
    scoped = original.with_identity(1001, 2000)
    try:
        with pytest.raises(SandboxApiException):
            scoped.read_bytes("/file")
        assert len(requests) == 1
        assert requests[0].url.path == PREFIX + "/v1/filesystem/1001/2000/files/download"
    finally:
        scoped._httpx_client.close()
        original._httpx_client.close()


@pytest.mark.parametrize("value", [-1, 4294967295, 1.5, True, "1001", None])
def test_invalid_identity_rejected_before_network_access(value):
    with pytest.raises(InvalidArgumentException):
        filesystem_identity_path(value, 1000)
    with pytest.raises(InvalidArgumentException):
        filesystem_identity_path(1000, value)


def test_identity_bounds():
    assert filesystem_identity_path(0, 4294967294) == "/v1/filesystem/0/4294967294"
