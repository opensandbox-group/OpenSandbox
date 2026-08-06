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
"""SSE parsing with compatibility for execd's legacy bare-JSON frames."""

from collections.abc import AsyncIterable, AsyncIterator, Iterable, Iterator

import httpx
from httpx_sse import EventSource, ServerSentEvent


class _LegacyJsonSseNormalizer:
    """Turn legacy ``JSON\n\n`` frames into standard SSE data events.

    Current and older execd versions write one bare JSON object followed by
    two LF bytes. Standard SSE streams are passed through byte-for-byte and
    parsed entirely by ``httpx-sse``.
    """

    def __init__(self) -> None:
        self._legacy: bool | None = None
        self._buffer = bytearray()

    def decode(self, chunk: bytes) -> list[bytes]:
        if self._legacy is False:
            return [chunk]

        self._buffer.extend(chunk)
        if self._legacy is None:
            first = bytes(self._buffer).lstrip()[:1]
            if not first:
                return []
            self._legacy = first in (b"{", b"[")
            if not self._legacy:
                data = bytes(self._buffer)
                self._buffer.clear()
                return [data]

        output: list[bytes] = []
        separator = b"\n\n"
        while True:
            boundary = self._buffer.find(separator)
            if boundary < 0:
                break
            frame = bytes(self._buffer[:boundary])
            del self._buffer[: boundary + len(separator)]
            output.append(b"data: " + frame + separator)
        return output

    def flush(self) -> list[bytes]:
        if not self._buffer:
            return []
        data = bytes(self._buffer)
        self._buffer.clear()
        if self._legacy:
            return [b"data: " + data + b"\n\n"]
        return [data]


class _SyncSseStream(httpx.SyncByteStream):
    def __init__(self, chunks: Iterable[bytes]) -> None:
        self._chunks = chunks

    def __iter__(self) -> Iterator[bytes]:
        normalizer = _LegacyJsonSseNormalizer()
        for chunk in self._chunks:
            yield from normalizer.decode(chunk)
        yield from normalizer.flush()


class _AsyncSseStream(httpx.AsyncByteStream):
    def __init__(self, chunks: AsyncIterable[bytes]) -> None:
        self._chunks = chunks

    async def __aiter__(self) -> AsyncIterator[bytes]:
        normalizer = _LegacyJsonSseNormalizer()
        async for chunk in self._chunks:
            for data in normalizer.decode(chunk):
                yield data
        for data in normalizer.flush():
            yield data


def _copy_response(
    response: httpx.Response, stream: httpx.SyncByteStream | httpx.AsyncByteStream
) -> httpx.Response:
    headers = response.headers.copy()
    # ``iter_bytes``/``aiter_bytes`` already apply HTTP content decoding. The
    # synthetic response must not try to decode the normalized bytes again.
    headers.pop("content-encoding", None)
    headers.pop("content-length", None)
    return httpx.Response(
        status_code=response.status_code,
        headers=headers,
        request=response.request,
        extensions=response.extensions,
        stream=stream,
    )


def iter_sse_events(response: httpx.Response) -> Iterator[ServerSentEvent]:
    """Parse sync SSE events, accepting standard and legacy execd framing."""
    compatible_response = _copy_response(
        response, _SyncSseStream(response.iter_bytes())
    )
    yield from EventSource(compatible_response).iter_sse()


async def aiter_sse_events(
    response: httpx.Response,
) -> AsyncIterator[ServerSentEvent]:
    """Parse async SSE events, accepting standard and legacy execd framing."""
    compatible_response = _copy_response(
        response, _AsyncSseStream(response.aiter_bytes())
    )
    async for event in EventSource(compatible_response).aiter_sse():
        yield event
