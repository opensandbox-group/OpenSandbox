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

"""Async downloads must release their response when iteration stops."""

import asyncio
from collections.abc import AsyncIterator, Callable
from contextlib import aclosing

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint

_AdapterFactory = Callable[
    ..., tuple[FilesystemAdapter | IsolatedFilesystemAdapter, httpx.Response]
]


class _DownloadStream(httpx.AsyncByteStream):
    def __init__(self, *, failure: Exception | None = None, block: bool = False) -> None:
        self.failure = failure
        self.block = block
        self.closed = False
        self.reading = asyncio.Event()

    async def __aiter__(self) -> AsyncIterator[bytes]:
        yield b"abcd"
        if self.failure is not None:
            raise self.failure
        if self.block:
            self.reading.set()
            await asyncio.Event().wait()
        yield b"ef"

    async def aclose(self) -> None:
        self.closed = True


@pytest.fixture(params=["sandbox", "isolated"])
def download_adapter(request: pytest.FixtureRequest) -> _AdapterFactory:
    def make(
        stream: _DownloadStream, status_code: int = 200
    ) -> tuple[FilesystemAdapter | IsolatedFilesystemAdapter, httpx.Response]:
        response = httpx.Response(status_code, stream=stream)
        config = ConnectionConfig(
            protocol="http",
            transport=httpx.MockTransport(lambda _: response),
        )
        endpoint = SandboxEndpoint(endpoint="localhost:44772")
        if request.param == "isolated":
            adapter = IsolatedFilesystemAdapter(
                config, endpoint, "12345678-1234-5678-1234-567812345678"
            )
        else:
            adapter = FilesystemAdapter(config, endpoint)
        return adapter, response

    return make


@pytest.mark.asyncio
async def test_download_closes_after_full_consumption(download_adapter: _AdapterFactory) -> None:
    source = _DownloadStream()
    adapter, response = download_adapter(source)
    try:
        chunks = await adapter.read_bytes_stream("/tmp/file", chunk_size=2)
        assert [chunk async for chunk in chunks] == [b"ab", b"cd", b"ef"]
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_download_aclosing_releases_partial_response(download_adapter: _AdapterFactory) -> None:
    source = _DownloadStream()
    adapter, response = download_adapter(source)
    try:
        chunks = await adapter.read_bytes_stream("/tmp/file", chunk_size=2)
        async with aclosing(chunks):
            assert await anext(chunks) == b"ab"
            assert not source.closed
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_download_read_failure_releases_response(download_adapter: _AdapterFactory) -> None:
    failure = httpx.ReadError("download disconnected")
    source = _DownloadStream(failure=failure)
    adapter, response = download_adapter(source)
    try:
        chunks = await adapter.read_bytes_stream("/tmp/file", chunk_size=4)
        assert await anext(chunks) == b"abcd"
        with pytest.raises(httpx.ReadError) as exc:
            await anext(chunks)
        assert exc.value is failure
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_download_cancellation_releases_response(download_adapter: _AdapterFactory) -> None:
    source = _DownloadStream(block=True)
    adapter, response = download_adapter(source)
    try:
        chunks = await adapter.read_bytes_stream("/tmp/file", chunk_size=4)
        assert await anext(chunks) == b"abcd"
        task = asyncio.create_task(anext(chunks))
        try:
            await asyncio.wait_for(source.reading.wait(), timeout=5)
        finally:
            task.cancel()
            with pytest.raises(asyncio.CancelledError):
                await task
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_download_http_error_releases_response(download_adapter: _AdapterFactory) -> None:
    source = _DownloadStream()
    adapter, response = download_adapter(source, status_code=404)
    try:
        with pytest.raises(SandboxApiException) as exc:
            await adapter.read_bytes_stream("/tmp/missing")
        assert exc.value.status_code == 404
        assert source.closed
        assert response.is_closed
    finally:
        await response.aclose()
        await adapter._httpx_client.aclose()
