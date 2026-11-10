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
"""Retry policy and httpx transport wrappers."""

from typing import TypeVar

import httpx

from opensandbox.transport._async_retry import RetryAsyncTransport
from opensandbox.transport._sync_retry import RetrySyncTransport
from opensandbox.transport.retry import (
    JitterMode,
    RetryCause,
    RetryEvent,
    RetryPolicy,
)

_T = TypeVar("_T")


def unwrap_retry_transport(transport: _T) -> _T:
    """
    Return the raw inner transport when ``transport`` is a retry wrapper.

    Streaming clients (SSE bootstraps) must not go through the retry
    wrapper, because request bodies are not replayable and a caller
    ``retryable_status_codes_non_idempotent`` opt-in would cause
    duplicate execution. Adapters instantiating an httpx streaming
    client pass their transport through this helper.

    Non-wrapper transports (raw httpx transports, user-supplied
    transports, mocks in tests) are returned unchanged.
    """
    inner = getattr(transport, "inner", None)
    if isinstance(transport, (RetryAsyncTransport, RetrySyncTransport)):
        return inner  # type: ignore[return-value]
    return transport


def streaming_transport(transport: _T) -> _T:
    """
    Return the transport for a streaming (SSE) client.

    SSE requests run commands, so general retries must not resend them: a
    caller ``retryable_status_codes_non_idempotent`` opt-in would cause
    duplicate execution. A retry wrapper is replaced by one that keeps only
    the OSEP-0024 wake-timeout retry, which is safe because the gateway
    never forwarded the request. Other transports are returned unchanged.
    """
    if isinstance(transport, (RetryAsyncTransport, RetrySyncTransport)):
        return transport.for_streaming()  # type: ignore[return-value]
    return transport


# execd sends an SSE ``ping`` every 3 seconds while a command runs, so a stream
# that stays silent for much longer has stopped. The read timeout is an idle
# limit, not a command deadline: every byte (pings included) resets it. The
# floor keeps a short ``request_timeout`` from cutting a healthy stream between
# pings and matches the Go SDK's response-header timeout. Waiting for the
# response headers is a read in httpx, so this also bounds a sandbox that
# accepts the connection and never answers (for example a paused one).
SSE_READ_TIMEOUT_FLOOR_SECONDS = 30.0


def sse_timeout(request_timeout_seconds: float) -> httpx.Timeout:
    """
    Timeout for a streaming (SSE) client.

    Connect and write follow ``request_timeout``; read is bounded by
    ``max(request_timeout, SSE_READ_TIMEOUT_FLOOR_SECONDS)`` so a silent
    stream raises ``httpx.ReadTimeout`` instead of blocking forever.
    """
    return httpx.Timeout(
        connect=request_timeout_seconds,
        read=max(request_timeout_seconds, SSE_READ_TIMEOUT_FLOOR_SECONDS),
        write=request_timeout_seconds,
        pool=None,
    )


__all__ = [
    "JitterMode",
    "RetryAsyncTransport",
    "RetryCause",
    "RetryEvent",
    "RetryPolicy",
    "RetrySyncTransport",
    "SSE_READ_TIMEOUT_FLOOR_SECONDS",
    "sse_timeout",
    "streaming_transport",
    "unwrap_retry_transport",
]
