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
"""Map httpx transport exceptions to :class:`Outcome`."""

from __future__ import annotations

import httpx

from opensandbox.transport._decision import Outcome
from opensandbox.transport.retry import RetryCause


def classify_transport_exception(exc: BaseException) -> Outcome:
    """
    Classify an httpx transport exception.

    - Connect/pool errors: pre-send, safe to retry on any method.
    - Read/write timeout, unexpected EOF: post-send, idempotent only.
    - Other transport errors: opaque, idempotent only.
    - Anything else: not a transport error; caller re-raises.
    """
    if isinstance(exc, (httpx.ConnectError, httpx.ConnectTimeout)):
        return Outcome(
            is_transport_error=True, is_pre_send=True, cause=RetryCause.PRE_SEND
        )
    if isinstance(exc, httpx.PoolTimeout):
        # No byte written; treat as pre-send.
        return Outcome(
            is_transport_error=True, is_pre_send=True, cause=RetryCause.PRE_SEND
        )
    if isinstance(exc, httpx.ReadTimeout):
        return Outcome(is_transport_error=True, cause=RetryCause.READ_TIMEOUT)
    if isinstance(exc, httpx.WriteTimeout):
        return Outcome(is_transport_error=True, cause=RetryCause.WRITE_TIMEOUT)
    if isinstance(exc, httpx.RemoteProtocolError):
        return Outcome(is_transport_error=True, cause=RetryCause.UNEXPECTED_EOF)
    if isinstance(exc, httpx.TransportError):
        return Outcome(
            is_transport_error=True,
            is_opaque_transport=True,
            cause=RetryCause.OPAQUE_TRANSPORT,
        )
    return Outcome(is_transport_error=False)


def outcome_for_response(response: httpx.Response) -> Outcome:
    return Outcome(
        is_transport_error=False,
        status_code=response.status_code,
        cause=RetryCause.for_status(response.status_code),
    )


def is_body_replayable(request: httpx.Request) -> bool:
    """
    Whether ``request`` can be re-dispatched with the same body.

    httpx materializes in-memory bodies (``bytes``/``str``/``json=``)
    into a ``ByteStream``, which can be iterated repeatedly. Streaming
    bodies — async/sync generators, file-like uploads, and multipart
    ``files=`` bodies — can only be consumed once; resending raises
    ``StreamConsumed`` or emits a truncated body.
    """
    # ``httpx.ByteStream`` is the public marker (listed in
    # ``httpx.__all__``) for a materialized, replayable in-memory body.
    return isinstance(request.stream, httpx.ByteStream)
