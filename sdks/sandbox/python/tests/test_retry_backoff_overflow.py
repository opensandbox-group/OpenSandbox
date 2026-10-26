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
"""Capped backoff must remain bounded after many transport retries."""

import random
from datetime import timedelta

import httpx
import pytest

from opensandbox.transport import (
    JitterMode,
    RetryAsyncTransport,
    RetryPolicy,
    RetrySyncTransport,
)
from opensandbox.transport._decision import compute_backoff


@pytest.mark.parametrize("jitter", list(JitterMode))
@pytest.mark.parametrize(
    "retry_index,multiplier", [(1024, 2.0), (10000, 2.0), (2, 1e200)]
)
@pytest.mark.parametrize("cap", [timedelta(0), timedelta(milliseconds=1)])
def test_backoff_remains_capped_at_large_retry_indices(
    jitter: JitterMode, retry_index: int, multiplier: float, cap: timedelta
) -> None:
    policy = RetryPolicy(
        max_retries=retry_index + 1,
        max_backoff=cap,
        jitter=jitter,
        backoff_multiplier=multiplier,
    )
    delay = compute_backoff(retry_index, policy, cap, random.Random(0))
    assert timedelta(0) <= delay <= cap
    if jitter is not JitterMode.FULL:
        assert delay == cap


@pytest.mark.parametrize("jitter", list(JitterMode))
@pytest.mark.parametrize("asynchronous", [False, True])
@pytest.mark.asyncio
async def test_long_retry_sequence_reaches_success(
    jitter: JitterMode, asynchronous: bool
) -> None:
    responses: list[httpx.Response] = []

    def respond(request: httpx.Request) -> httpx.Response:
        response = httpx.Response(
            503 if len(responses) < 1026 else 200, stream=httpx.ByteStream(b"body")
        )
        assert not response.is_closed
        responses.append(response)
        return response

    policy = RetryPolicy(max_retries=1026, max_backoff=timedelta(0), jitter=jitter)
    inner = httpx.MockTransport(respond)
    if asynchronous:
        async with httpx.AsyncClient(
            transport=RetryAsyncTransport(inner, policy)
        ) as client:
            response = await client.get("https://sandbox.test/ping")
    else:
        with httpx.Client(transport=RetrySyncTransport(inner, policy)) as client:
            response = client.get("https://sandbox.test/ping")
    assert response.status_code == 200
    assert len(responses) == 1027
    assert all(response.is_closed for response in responses)
