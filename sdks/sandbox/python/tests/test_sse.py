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
from __future__ import annotations

import gzip
from codecs import BOM_UTF8
from collections.abc import AsyncIterator, Iterator

import httpx
import pytest

from opensandbox.adapters.sse import aiter_sse_events, iter_sse_events

_UNICODE_SEPARATORS = "before\u0085middle\u2028middle\u2029after"


class _OneByteStream(httpx.SyncByteStream):
    def __init__(self, content: bytes) -> None:
        self._content = content

    def __iter__(self) -> Iterator[bytes]:
        for byte in self._content:
            yield bytes([byte])


class _AsyncOneByteStream(httpx.AsyncByteStream):
    def __init__(self, content: bytes) -> None:
        self._content = content

    async def __aiter__(self) -> AsyncIterator[bytes]:
        for byte in self._content:
            yield bytes([byte])


def _response(stream: httpx.SyncByteStream | httpx.AsyncByteStream) -> httpx.Response:
    return httpx.Response(
        200,
        headers={"Content-Type": "text/event-stream"},
        request=httpx.Request("GET", "http://localhost/events"),
        stream=stream,
    )


def test_iter_sse_events_uses_standard_parser() -> None:
    content = (
        f"event: stdout\r\ndata: {_UNICODE_SEPARATORS}\r\ndata: second line\r\n\r\n"
    ).encode()

    events = list(iter_sse_events(_response(_OneByteStream(content))))

    assert len(events) == 1
    assert events[0].event == "stdout"
    assert events[0].data == f"{_UNICODE_SEPARATORS}\nsecond line"


def test_iter_sse_events_does_not_decode_compressed_content_twice() -> None:
    content = f'data: {{"text":"{_UNICODE_SEPARATORS}"}}\n\n'.encode()
    response = httpx.Response(
        200,
        headers={
            "Content-Type": "text/event-stream",
            "Content-Encoding": "gzip",
            "Content-Length": str(len(gzip.compress(content))),
        },
        request=httpx.Request("GET", "http://localhost/events"),
        stream=_OneByteStream(gzip.compress(content)),
    )

    events = list(iter_sse_events(response))

    assert [event.data for event in events] == [f'{{"text":"{_UNICODE_SEPARATORS}"}}']


@pytest.mark.asyncio
async def test_aiter_sse_events_accepts_legacy_bare_json() -> None:
    content = (
        f'{{"type":"stdout","text":"{_UNICODE_SEPARATORS}"}}\n\n'
        '{"type":"execution_complete"}\n\n'
    ).encode()

    events = [
        event
        async for event in aiter_sse_events(_response(_AsyncOneByteStream(content)))
    ]

    assert [event.data for event in events] == [
        f'{{"type":"stdout","text":"{_UNICODE_SEPARATORS}"}}',
        '{"type":"execution_complete"}',
    ]


def _bom_content(legacy: bool) -> tuple[bytes, list[str]]:
    values = ['{"text":"first\ufeffkeep"}', '{"text":"second"}']
    prefix = "" if legacy else "data: "
    return BOM_UTF8 + "".join(
        f"{prefix}{value}\n\n" for value in values
    ).encode(), values


@pytest.mark.parametrize("legacy", [False, True])
@pytest.mark.parametrize("split", [False, True])
def test_iter_sse_events_accepts_initial_bom(legacy: bool, split: bool) -> None:
    content, expected = _bom_content(legacy)
    stream = _OneByteStream(content) if split else httpx.ByteStream(content)

    assert [event.data for event in iter_sse_events(_response(stream))] == expected


@pytest.mark.parametrize("legacy", [False, True])
@pytest.mark.parametrize("split", [False, True])
@pytest.mark.asyncio
async def test_aiter_sse_events_accepts_initial_bom(legacy: bool, split: bool) -> None:
    content, expected = _bom_content(legacy)
    stream = _AsyncOneByteStream(content) if split else httpx.ByteStream(content)

    assert [
        event.data async for event in aiter_sse_events(_response(stream))
    ] == expected


def test_iter_sse_events_accepts_bom_without_events() -> None:
    assert list(iter_sse_events(_response(_OneByteStream(BOM_UTF8)))) == []


@pytest.mark.asyncio
async def test_aiter_sse_events_accepts_bom_without_events() -> None:
    assert [
        event
        async for event in aiter_sse_events(_response(_AsyncOneByteStream(BOM_UTF8)))
    ] == []


def test_iter_sse_events_removes_only_one_initial_bom() -> None:
    content = BOM_UTF8 + BOM_UTF8 + b"data: first\n\ndata: second\n\n"

    assert [
        event.data for event in iter_sse_events(_response(_OneByteStream(content)))
    ] == ["second"]
