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

"""An async download closed before its first chunk must release its response."""

from collections.abc import AsyncIterator
from contextlib import aclosing

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint


class _DownloadStream(httpx.AsyncByteStream):
    def __init__(self) -> None:
        self.closed = False

    async def __aiter__(self) -> AsyncIterator[bytes]:
        yield b"abcd"

    async def aclose(self) -> None:
        self.closed = True


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["sandbox", "isolated"])
async def test_download_closed_before_first_chunk_releases_response(kind: str) -> None:
    source = _DownloadStream()
    response = httpx.Response(200, stream=source)
    config = ConnectionConfig(protocol="http", transport=httpx.MockTransport(lambda _: response))
    endpoint = SandboxEndpoint(endpoint="localhost:44772")
    if kind == "isolated":
        adapter = IsolatedFilesystemAdapter(
            config, endpoint, "12345678-1234-5678-1234-567812345678"
        )
    else:
        adapter = FilesystemAdapter(config, endpoint)
    try:
        chunks = await adapter.read_bytes_stream("/tmp/file")
        async with aclosing(chunks):
            pass
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()
