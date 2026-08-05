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
"""SSE framing helpers shared by synchronous and asynchronous adapters."""

import codecs
from collections.abc import AsyncIterable, AsyncIterator, Iterable, Iterator


class _SseLineDecoder:
    """Incrementally split UTF-8 SSE content on CR, LF, or CRLF only.

    ``httpx`` line iterators implement Python's universal ``splitlines``
    semantics, which also split on Unicode separators such as U+0085, U+2028,
    and U+2029. Those characters are valid inside a JSON string and are not
    SSE line endings, so streaming adapters need protocol-specific framing.
    """

    def __init__(self) -> None:
        self._decoder = codecs.getincrementaldecoder("utf-8")()
        self._buffer = ""

    def decode(self, chunk: bytes) -> list[str]:
        self._buffer += self._decoder.decode(chunk)
        return self._drain(final=False)

    def flush(self) -> list[str]:
        self._buffer += self._decoder.decode(b"", final=True)
        return self._drain(final=True)

    def _drain(self, *, final: bool) -> list[str]:
        lines: list[str] = []
        start = 0
        index = 0

        while index < len(self._buffer):
            character = self._buffer[index]
            if character == "\n":
                lines.append(self._buffer[start:index])
                index += 1
                start = index
                continue
            if character == "\r":
                if index + 1 == len(self._buffer) and not final:
                    break
                lines.append(self._buffer[start:index])
                index += 1
                if index < len(self._buffer) and self._buffer[index] == "\n":
                    index += 1
                start = index
                continue
            index += 1

        self._buffer = self._buffer[start:]
        if final and self._buffer:
            lines.append(self._buffer)
            self._buffer = ""
        return lines


def iter_sse_lines(chunks: Iterable[bytes]) -> Iterator[str]:
    """Yield SSE lines from byte chunks without splitting Unicode separators."""
    decoder = _SseLineDecoder()
    for chunk in chunks:
        yield from decoder.decode(chunk)
    yield from decoder.flush()


async def aiter_sse_lines(chunks: AsyncIterable[bytes]) -> AsyncIterator[str]:
    """Yield SSE lines asynchronously without splitting Unicode separators."""
    decoder = _SseLineDecoder()
    async for chunk in chunks:
        for line in decoder.decode(chunk):
            yield line
    for line in decoder.flush():
        yield line
