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

import httpx
import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models import ByteRange, ReadBytesResponse
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync


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

    assert isinstance(response, ReadBytesResponse)
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
async def test_async_read_bytes_detailed_identifies_ignored_and_invalid_ranges() -> (
    None
):
    ranges = iter([None, "garbage", "bytes 5-9/*"])

    def handler(request: httpx.Request) -> httpx.Response:
        content_range = next(ranges)
        status = 200 if content_range is None else 206
        headers = {"Content-Length": "10"}
        if content_range is not None:
            headers["Content-Range"] = content_range
        return httpx.Response(
            status, request=request, content=b"0123456789", headers=headers
        )

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    ignored = await adapter.read_bytes_detailed("/data.bin", range_header="bytes=5-")
    assert ignored.is_partial is False
    assert ignored.content_range is None
    assert ignored.total_size == 10

    malformed = await adapter.read_bytes_detailed("/data.bin")
    assert malformed.content_range == ByteRange(-1, -1, -1, "garbage")
    assert malformed.total_size == -1

    unknown = await adapter.read_bytes_detailed("/data.bin")
    assert unknown.content_range == ByteRange(5, 9, -1, "bytes 5-9/*")
    assert unknown.total_size == -1
    await adapter._httpx_client.aclose()


@pytest.mark.asyncio
async def test_async_read_bytes_detailed_identifies_partial_without_range() -> None:
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(206, request=request, content=b"hello")

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )

    response = await adapter.read_bytes_detailed("/data.bin")

    assert response.is_partial is True
    assert response.content_range is None
    assert response.total_size == -1
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
