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

import asyncio
import gc

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxConnectionException
from opensandbox.models import ByteRange
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.services import DownloadFilesystem
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync
from opensandbox.sync.adapters.isolated_filesystem_adapter import (
    IsolatedFilesystemAdapterSync,
)
from opensandbox.sync.services import DownloadFilesystemSync


def _endpoint() -> SandboxEndpoint:
    return SandboxEndpoint(endpoint="localhost:44772")


@pytest.mark.asyncio
async def test_async_read_bytes_detailed_exposes_partial_metadata() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        assert request.headers["Range"] == "bytes=0-4"
        return httpx.Response(
            206,
            request=request,
            content=b"hello",
            headers={
                "Content-Type": "application/octet-stream",
                "Content-Disposition": 'attachment; filename="data.bin"',
                "Content-Length": "5",
                "Content-Range": "bytes 0-4/10",
            },
        )

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )
    response = await adapter.read_bytes_detailed("/data.bin", range_header="bytes=0-4")

    assert isinstance(adapter, DownloadFilesystem)
    assert response.body == b"hello"
    assert response.status_code == 206
    assert response.is_partial is True
    assert response.content_type == "application/octet-stream"
    assert response.content_disposition == 'attachment; filename="data.bin"'
    assert response.content_length == 5
    assert response.total_size == 10
    assert response.content_range == ByteRange(
        start=0, end=4, total=10, raw="bytes 0-4/10"
    )
    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("status", "raw", "expected_range", "total_size"),
    [
        (200, None, None, 5),
        (206, None, None, -1),
        (206, "garbage", ByteRange(-1, -1, -1, "garbage"), -1),
        (206, "bytes 5-9/*", ByteRange(5, 9, -1, "bytes 5-9/*"), -1),
    ],
)
async def test_async_read_bytes_detailed_handles_range_responses(
    status, raw, expected_range, total_size
) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            status,
            request=request,
            content=b"hello",
            headers={"Content-Range": raw} if raw is not None else {},
        )

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    response = await adapter.read_bytes_detailed("/data.bin", range_header="bytes=5-")
    assert response.is_partial is (status == 206)
    assert response.content_range == expected_range
    assert response.total_size == total_size
    await adapter._httpx_client.aclose()


class _TrackingAsyncStream(httpx.AsyncByteStream):
    def __init__(self) -> None:
        self.closed = False

    async def __aiter__(self):
        yield b"hello"

    async def aclose(self) -> None:
        self.closed = True


@pytest.mark.asyncio
async def test_async_detailed_and_legacy_streams_close_responses() -> None:
    streams: list[_TrackingAsyncStream] = []

    def handler(request: httpx.Request) -> httpx.Response:
        stream = _TrackingAsyncStream()
        streams.append(stream)
        return httpx.Response(
            206,
            request=request,
            stream=stream,
            headers={"Content-Length": "5", "Content-Range": "bytes 0-4/10"},
        )

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    detailed = await adapter.read_bytes_stream_detailed("/data.bin")
    assert detailed.is_partial is True
    assert streams[0].closed is False
    assert b"".join([chunk async for chunk in detailed.body]) == b"hello"
    assert streams[0].closed is True

    legacy = await adapter.read_bytes_stream("/data.bin")
    assert b"".join([chunk async for chunk in legacy]) == b"hello"
    assert streams[1].closed is True

    unconsumed = await adapter.read_bytes_stream_detailed("/data.bin")
    await unconsumed.body.aclose()
    assert streams[2].closed is True

    partial = await adapter.read_bytes_stream_detailed("/data.bin")
    async with partial.body:
        async for _chunk in partial.body:
            break
    assert streams[3].closed is True
    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_detailed_stream_can_close_during_a_read() -> None:
    reading = asyncio.Event()
    release = asyncio.Event()

    class Stream(_TrackingAsyncStream):
        async def __aiter__(self):
            yield b"a"
            reading.set()
            await release.wait()
            yield b"b"

    source = Stream()
    response = httpx.Response(200, stream=source)
    adapter = FilesystemAdapter(
        ConnectionConfig(
            protocol="http", transport=httpx.MockTransport(lambda _: response)
        ),
        _endpoint(),
    )
    pending = None
    try:
        download = await adapter.read_bytes_stream_detailed("/data.bin", chunk_size=1)
        assert await anext(download.body) == b"a"
        pending = asyncio.create_task(anext(download.body))
        await asyncio.wait_for(reading.wait(), timeout=2)

        await asyncio.wait_for(download.body.aclose(), timeout=2)
        assert response.is_closed
        assert source.closed

        release.set()
        await pending
        await download.body.aclose()
        with pytest.raises(StopAsyncIteration):
            await anext(download.body)
    finally:
        release.set()
        if pending is not None:
            await pending
        await response.aclose()
        await adapter._httpx_client.aclose()


def test_sync_detailed_and_legacy_reads_preserve_behavior() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            206,
            request=request,
            content=b"hello",
            headers={"Content-Length": "5", "Content-Range": "bytes 0-4/10"},
        )

    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    detailed = adapter.read_bytes_detailed("/data.bin")
    assert isinstance(adapter, DownloadFilesystemSync)
    assert detailed.body == b"hello"
    assert detailed.content_range == ByteRange(0, 4, 10, "bytes 0-4/10")
    assert adapter.read_bytes("/data.bin") == b"hello"
    assert b"".join(adapter.read_bytes_stream("/data.bin")) == b"hello"
    adapter._httpx_client.close()


class _TrackingSyncStream(httpx.SyncByteStream):
    def __init__(self) -> None:
        self.closed = False

    def __iter__(self):
        yield b"hello"

    def close(self) -> None:
        self.closed = True


def test_sync_detailed_streams_close_unconsumed_and_partial_responses() -> None:
    streams: list[_TrackingSyncStream] = []

    def handler(request: httpx.Request) -> httpx.Response:
        stream = _TrackingSyncStream()
        streams.append(stream)
        return httpx.Response(206, request=request, stream=stream)

    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    unconsumed = adapter.read_bytes_stream_detailed("/data.bin")
    unconsumed.body.close()
    assert streams[0].closed is True

    partial = adapter.read_bytes_stream_detailed("/data.bin")
    with partial.body:
        for _chunk in partial.body:
            break
    assert streams[1].closed is True
    adapter._httpx_client.close()


@pytest.mark.parametrize("isolated", [False, True])
def test_sync_legacy_stream_closes_response_when_abandoned(isolated: bool) -> None:
    streams: list[_TrackingSyncStream] = []

    def handler(request: httpx.Request) -> httpx.Response:
        stream = _TrackingSyncStream()
        streams.append(stream)
        return httpx.Response(200, request=request, stream=stream)

    config = ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler))
    if isolated:
        adapter = IsolatedFilesystemAdapterSync(
            config, _endpoint(), "12345678-1234-5678-1234-567812345678"
        )
    else:
        adapter = FilesystemAdapterSync(config, _endpoint())

    # The legacy API returns a plain iterator, so callers that stop early
    # never call close(); dropping the iterator has to release the response.
    chunks = adapter.read_bytes_stream("/data.bin", chunk_size=2)
    for _chunk in chunks:
        break
    assert streams[0].closed is False
    del chunks
    gc.collect()
    assert streams[0].closed is True

    # The same holds when nothing was read.
    unread = adapter.read_bytes_stream("/data.bin")
    del unread
    gc.collect()
    assert streams[1].closed is True

    # close() before the first chunk releases it too.
    closed = adapter.read_bytes_stream("/data.bin")
    closed.close()  # type: ignore[attr-defined]
    assert streams[2].closed is True
    adapter._httpx_client.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("options", [{"offset": 2}, {"limit": 2}])
async def test_async_line_reads_report_unknown_total_size(options) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, request=request, content=b"hello")

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )
    try:
        for method in [adapter.read_bytes_detailed, adapter.read_bytes_stream_detailed]:
            response = await method("/data.txt", **options)
            assert response.status_code == 200
            assert response.is_partial is False
            assert response.content_length == 5
            assert response.total_size == -1
            if not isinstance(response.body, bytes):
                await response.body.aclose()
    finally:
        await adapter._httpx_client.aclose()


@pytest.mark.parametrize("options", [{"offset": 2}, {"limit": 2}])
def test_sync_line_reads_report_unknown_total_size(options) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, request=request, content=b"hello")

    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )
    try:
        for method in [adapter.read_bytes_detailed, adapter.read_bytes_stream_detailed]:
            response = method("/data.txt", **options)
            assert response.status_code == 200
            assert response.is_partial is False
            assert response.content_length == 5
            assert response.total_size == -1
            if not isinstance(response.body, bytes):
                response.body.close()
    finally:
        adapter._httpx_client.close()


@pytest.mark.parametrize("isolated", [False, True])
def test_sync_stream_maps_transport_errors_like_async(isolated: bool) -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused", request=request)

    config = ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler))
    if isolated:
        adapter = IsolatedFilesystemAdapterSync(
            config, _endpoint(), "12345678-1234-5678-1234-567812345678"
        )
    else:
        adapter = FilesystemAdapterSync(config, _endpoint())

    with pytest.raises(SandboxConnectionException):
        adapter.read_bytes_stream_detailed("/data.bin")
    with pytest.raises(SandboxConnectionException):
        adapter.read_bytes_stream("/data.bin")
    adapter._httpx_client.close()
