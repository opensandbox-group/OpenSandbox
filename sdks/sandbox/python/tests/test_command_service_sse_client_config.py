#
# Copyright 2025 The OpenSandbox Authors
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

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.transport import RetryAsyncTransport, RetryPolicy, RetrySyncTransport

_SSE_BODY = (
    b'data: {"type":"init","text":"exec-1","timestamp":1}\n\n'
    b'data: {"type":"execution_complete","timestamp":2,"execution_time":1}\n\n'
)


def _wake_then_stream_handler(calls: list[str]):
    """First /command answer is a gateway wake timeout, then the SSE stream."""

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        if len(calls) == 1:
            return httpx.Response(
                503,
                headers={"OpenSandbox-Wake": "expired", "Retry-After": "0"},
                json={"code": "sandbox_wake_timeout", "message": "restore in progress"},
            )
        return httpx.Response(
            200, headers={"Content-Type": "text/event-stream"}, content=_SSE_BODY
        )

    return handler


def test_sse_client_has_event_stream_headers_and_no_read_timeout() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    sse_client = adapter._sse_client
    assert sse_client is not None
    assert sse_client.headers.get("Accept") == "text/event-stream"
    assert sse_client.timeout.read is None


def test_sse_client_keeps_only_wake_retry() -> None:
    """
    SSE bootstraps must not go through the general retries: a caller that
    opts into non-idempotent status retries would otherwise trigger
    duplicate execution. The wake-timeout retry stays, since the gateway
    never forwarded a request it answered with a wake timeout.
    """
    cfg = ConnectionConfig(protocol="http").with_transport_if_missing()
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    client_transport = adapter._httpx_client._transport
    sse_transport = adapter._sse_client._transport
    assert isinstance(client_transport, RetryAsyncTransport)
    assert isinstance(sse_transport, RetryAsyncTransport)
    assert sse_transport is not client_transport
    assert sse_transport.inner is client_transport.inner


@pytest.mark.asyncio
async def test_run_retries_gateway_wake_timeout() -> None:
    calls: list[str] = []
    transport = RetryAsyncTransport(
        httpx.MockTransport(_wake_then_stream_handler(calls)), RetryPolicy()
    )
    cfg = ConnectionConfig(protocol="http", transport=transport)
    adapter = CommandsAdapter(cfg, SandboxEndpoint(endpoint="localhost:44772"))

    execution = await adapter.run("echo hi")

    assert execution.id == "exec-1"
    assert calls == ["/command", "/command"]


def test_sync_run_retries_gateway_wake_timeout() -> None:
    calls: list[str] = []
    transport = RetrySyncTransport(
        httpx.MockTransport(_wake_then_stream_handler(calls)), RetryPolicy()
    )
    cfg = ConnectionConfigSync(protocol="http", transport=transport)
    adapter = CommandsAdapterSync(cfg, SandboxEndpoint(endpoint="localhost:44772"))

    execution = adapter.run("echo hi")

    assert execution.id == "exec-1"
    assert calls == ["/command", "/command"]
