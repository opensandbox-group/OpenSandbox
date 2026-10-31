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

import re
from typing import TypeVar

import httpx

from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.models.filesystem import (
    AsyncReadBytesStream,
    ByteRange,
    ReadBytesResponse,
    ReadBytesStream,
)

_BodyT = TypeVar("_BodyT")
_CONTENT_RANGE_RE = re.compile(r"^bytes\s+(\d+)-(\d+)/(\d+|\*)$", re.IGNORECASE)


def _parse_header_integer(raw: str | None) -> int:
    if raw is None or not raw.isdigit():
        return -1
    return int(raw)


def _parse_content_range(raw: str | None) -> ByteRange | None:
    if not raw:
        return None

    invalid = ByteRange(start=-1, end=-1, total=-1, raw=raw)
    match = _CONTENT_RANGE_RE.fullmatch(raw)
    if match is None:
        return invalid

    start = int(match.group(1))
    end = int(match.group(2))
    total = -1 if match.group(3) == "*" else int(match.group(3))
    if end < start or (total != -1 and total <= end):
        return invalid
    return ByteRange(start=start, end=end, total=total, raw=raw)


def create_read_bytes_response(
    response: httpx.Response, body: _BodyT, *, line_based: bool
) -> ReadBytesResponse[_BodyT]:
    """Map an HTTP download response to the stable SDK response model."""
    content_length = _parse_header_integer(response.headers.get("content-length"))
    content_range = (
        _parse_content_range(response.headers.get("content-range"))
        if response.status_code == 206
        else None
    )
    if response.status_code == 206:
        total_size = content_range.total if content_range is not None else -1
    else:
        total_size = -1 if line_based else content_length

    return ReadBytesResponse(
        body=body,
        status_code=response.status_code,
        content_type=response.headers.get("content-type"),
        content_disposition=response.headers.get("content-disposition"),
        content_length=content_length,
        total_size=total_size,
        content_range=content_range,
    )


class _AsyncResponseByteStream:
    def __init__(self, response: httpx.Response, chunk_size: int) -> None:
        self._response = response
        self._iterator = response.aiter_bytes(chunk_size=chunk_size).__aiter__()
        self._closed = False

    def __aiter__(self) -> "_AsyncResponseByteStream":
        return self

    async def __anext__(self) -> bytes:
        if self._closed:
            raise StopAsyncIteration
        try:
            return await self._iterator.__anext__()
        except StopAsyncIteration:
            await self.aclose()
            raise
        except Exception as e:
            await self.aclose()
            # Same mapping as the request itself, e.g. a reset mid-body.
            raise ExceptionConverter.to_sandbox_exception(e) from e
        except BaseException:
            await self.aclose()
            raise

    async def aclose(self) -> None:
        self._closed = True
        await self._response.aclose()

    async def __aenter__(self) -> "_AsyncResponseByteStream":
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: object | None,
    ) -> None:
        await self.aclose()


class _ResponseByteStream:
    def __init__(self, response: httpx.Response, chunk_size: int) -> None:
        self._response = response
        self._iterator = response.iter_bytes(chunk_size=chunk_size)
        self._closed = False

    def __iter__(self) -> "_ResponseByteStream":
        return self

    def __next__(self) -> bytes:
        if self._closed:
            raise StopIteration
        try:
            return next(self._iterator)
        except StopIteration:
            self.close()
            raise
        except Exception as e:
            self.close()
            # Same mapping as the request itself, e.g. a reset mid-body.
            raise ExceptionConverter.to_sandbox_exception(e) from e
        except BaseException:
            self.close()
            raise

    def close(self) -> None:
        self._closed = True
        self._response.close()

    def __del__(self) -> None:
        # The sync read_bytes_stream hands this out as a plain iterator, so a
        # caller that stops early may drop it without calling close().
        if not getattr(self, "_closed", True):
            self.close()

    def __enter__(self) -> "_ResponseByteStream":
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: object | None,
    ) -> None:
        self.close()


def iter_async_response_bytes(
    response: httpx.Response, chunk_size: int
) -> AsyncReadBytesStream:
    return _AsyncResponseByteStream(response, chunk_size)


def iter_response_bytes(response: httpx.Response, chunk_size: int) -> ReadBytesStream:
    return _ResponseByteStream(response, chunk_size)
