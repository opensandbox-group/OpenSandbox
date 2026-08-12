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
"""Retry policy and httpx transport wrappers."""

from typing import TypeVar

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


__all__ = [
    "JitterMode",
    "RetryAsyncTransport",
    "RetryCause",
    "RetryEvent",
    "RetryPolicy",
    "RetrySyncTransport",
    "unwrap_retry_transport",
]
