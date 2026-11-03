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

import asyncio
import concurrent.futures
import socket
import threading
from datetime import timedelta

import httpx
import pytest

from opensandbox import transport as transport_module
from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxTimeoutException
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.transport import (
    SSE_READ_TIMEOUT_FLOOR_SECONDS,
    RetryAsyncTransport,
    RetryPolicy,
    RetrySyncTransport,
)

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


def test_sse_client_has_event_stream_headers_and_bounded_read_timeout() -> None:
    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = CommandsAdapter(cfg, endpoint)

    sse_client = adapter._sse_client
    assert sse_client is not None
    assert sse_client.headers.get("Accept") == "text/event-stream"
    # The read timeout is an idle limit: with the default 30 s request timeout
    # it equals the floor, so a stream that stops sending (pings included)
    # fails instead of blocking forever.
    assert sse_client.timeout.read == SSE_READ_TIMEOUT_FLOOR_SECONDS
    assert sse_client.timeout.connect == 30.0
    assert sse_client.timeout.write == 30.0
    assert sse_client.timeout.pool is None


@pytest.mark.parametrize(
    ("request_timeout", "expected_read"),
    [(timedelta(seconds=5), 30.0), (timedelta(seconds=60), 60.0)],
)
def test_sse_read_timeout_is_request_timeout_with_a_floor(
    request_timeout: timedelta, expected_read: float
) -> None:
    """A short request_timeout must not cut a healthy stream between execd's
    3-second pings; a longer one is honored as is."""
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    async_adapter = CommandsAdapter(
        ConnectionConfig(protocol="http", request_timeout=request_timeout), endpoint
    )
    sync_adapter = CommandsAdapterSync(
        ConnectionConfigSync(protocol="http", request_timeout=request_timeout), endpoint
    )

    assert async_adapter._sse_client.timeout.read == expected_read
    assert sync_adapter._sse_client.timeout.read == expected_read
    assert async_adapter._sse_client.timeout.connect == request_timeout.total_seconds()
    assert sync_adapter._sse_client.timeout.connect == request_timeout.total_seconds()


def _silent_stream_handler(request: httpx.Request) -> httpx.Response:
    """execd accepted the connection but never answered: httpx raises
    ReadTimeout once the read timeout elapses."""
    raise httpx.ReadTimeout("timed out", request=request)


@pytest.mark.asyncio
async def test_run_raises_timeout_when_stream_stays_silent() -> None:
    cfg = ConnectionConfig(
        protocol="http", transport=httpx.MockTransport(_silent_stream_handler)
    )
    adapter = CommandsAdapter(cfg, SandboxEndpoint(endpoint="localhost:44772"))

    with pytest.raises(SandboxTimeoutException):
        await adapter.run("echo hi")


def test_sync_run_raises_timeout_when_stream_stays_silent() -> None:
    cfg = ConnectionConfigSync(
        protocol="http", transport=httpx.MockTransport(_silent_stream_handler)
    )
    adapter = CommandsAdapterSync(cfg, SandboxEndpoint(endpoint="localhost:44772"))

    with pytest.raises(SandboxTimeoutException):
        adapter.run("echo hi")


@pytest.fixture
def silent_server():
    """A server that accepts the connection and never answers: the paused
    sandbox case. Yields its endpoint; closes on teardown."""
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind(("127.0.0.1", 0))
    server.listen(4)
    held: list[socket.socket] = []
    stop = threading.Event()

    def accept_and_hold() -> None:
        server.settimeout(0.05)
        while not stop.is_set():
            try:
                conn, _ = server.accept()
            except (TimeoutError, OSError):
                continue
            held.append(conn)

    thread = threading.Thread(target=accept_and_hold, daemon=True)
    thread.start()
    try:
        yield f"127.0.0.1:{server.getsockname()[1]}"
    finally:
        stop.set()
        thread.join(1)
        for conn in held:
            conn.close()
        server.close()


@pytest.mark.asyncio
async def test_run_times_out_when_the_sandbox_never_answers(
    silent_server: str, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(transport_module, "SSE_READ_TIMEOUT_FLOOR_SECONDS", 0.3)
    cfg = ConnectionConfig(protocol="http", request_timeout=timedelta(milliseconds=100))
    adapter = CommandsAdapter(cfg, SandboxEndpoint(endpoint=silent_server))

    # The outer wait_for is a watchdog: if the read timeout regressed to None
    # the test fails here instead of hanging until the CI job limit.
    with pytest.raises(SandboxTimeoutException) as excinfo:
        await asyncio.wait_for(adapter.run("echo hi"), timeout=5)
    assert isinstance(excinfo.value.__cause__, httpx.ReadTimeout)


def test_sync_run_times_out_when_the_sandbox_never_answers(
    silent_server: str, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(transport_module, "SSE_READ_TIMEOUT_FLOOR_SECONDS", 0.3)
    cfg = ConnectionConfigSync(
        protocol="http", request_timeout=timedelta(milliseconds=100)
    )
    adapter = CommandsAdapterSync(cfg, SandboxEndpoint(endpoint=silent_server))

    # Same watchdog as the async test: a regression to an unbounded read must
    # fail within 5 seconds, not hang the suite. The executor is shut down
    # without waiting, so a worker still blocked on the socket does not hold
    # the test; the fixture's teardown closes that connection and releases it.
    pool = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    future = pool.submit(adapter.run, "echo hi")
    try:
        with pytest.raises(SandboxTimeoutException) as excinfo:
            future.result(timeout=5)
    finally:
        pool.shutdown(wait=False)
    assert isinstance(excinfo.value.__cause__, httpx.ReadTimeout)


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
