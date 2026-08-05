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
from __future__ import annotations

import pytest

from opensandbox.adapters.sse import aiter_sse_lines, iter_sse_lines

_UNICODE_SEPARATORS = "before\u0085middle\u2028middle\u2029after"
_SSE_CONTENT = f"data: {_UNICODE_SEPARATORS}\r\ndata: second\r\n\r\ndata: final"


def _chunks() -> list[bytes]:
    content = _SSE_CONTENT.encode()
    return [content[index : index + 1] for index in range(len(content))]


def test_iter_sse_lines_only_splits_protocol_line_endings() -> None:
    assert list(iter_sse_lines(_chunks())) == [
        f"data: {_UNICODE_SEPARATORS}",
        "data: second",
        "",
        "data: final",
    ]


@pytest.mark.asyncio
async def test_aiter_sse_lines_only_splits_protocol_line_endings() -> None:
    async def chunks():
        for chunk in _chunks():
            yield chunk

    assert [line async for line in aiter_sse_lines(chunks())] == [
        f"data: {_UNICODE_SEPARATORS}",
        "data: second",
        "",
        "data: final",
    ]
