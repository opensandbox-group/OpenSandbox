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
import asyncio
import time
from datetime import timedelta

import httpx
import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException, SandboxReadyTimeoutException
from opensandbox.internal.readiness import ReadinessBudget
from opensandbox.sandbox import Sandbox
from opensandbox.sync.sandbox import SandboxSync

CODE = "KUBERNETES::POD_IP_NOT_AVAILABLE"


def responder(calls, failures=2, code=CODE, status=404):
    def handle(request):
        calls.append(request.url.path)
        if request.method == "POST":
            return httpx.Response(204)
        if request.url.path == "/ping":
            assert request.headers["x-endpoint-token"] == "new"
            return httpx.Response(200)
        if len([p for p in calls if p.endswith("/44772")]) <= failures:
            return httpx.Response(status, json={"code": code, "message": "starting"})
        return httpx.Response(
            200,
            json={
                "endpoint": "localhost:44772",
                "headers": {"x-endpoint-token": "new"},
            },
        )

    return handle


@pytest.mark.parametrize("resume", [False, True])
@pytest.mark.asyncio
async def test_async_connect_resume(resume):
    calls = []
    config = ConnectionConfig(
        domain="localhost:8080", transport=httpx.MockTransport(responder(calls))
    )
    method = Sandbox.resume if resume else Sandbox.connect
    sb = await method(
        "sb",
        connection_config=config,
        health_check_polling_interval=timedelta(milliseconds=1),
    )
    assert len([p for p in calls if p.endswith("/44772")]) == 3
    assert len([p for p in calls if p.endswith("/18080")]) == 1
    await sb.close()


@pytest.mark.parametrize("resume", [False, True])
def test_sync_connect_resume(resume):
    calls = []
    timeouts = []
    respond = responder(calls)

    def handle(request):
        if request.method == "GET":
            timeouts.append(request.extensions["timeout"]["read"])
        return respond(request)

    config = ConnectionConfigSync(
        domain="localhost:8080", transport=httpx.MockTransport(handle)
    )
    method = SandboxSync.resume if resume else SandboxSync.connect
    sb = method(
        "sb",
        connection_config=config,
        health_check_polling_interval=timedelta(milliseconds=1),
    )
    assert len([p for p in calls if p.endswith("/44772")]) == 3
    assert len([p for p in calls if p.endswith("/18080")]) == 1
    assert 0 < timeouts[-1] < timeouts[0] <= 30
    sb.close()


@pytest.fixture(params=[False, True], ids=["async", "sync"])
def connect(request):
    async def run(handler, **options):
        config_type = ConnectionConfigSync if request.param else ConnectionConfig
        config = config_type(
            domain="localhost:8080", transport=httpx.MockTransport(handler)
        )
        options = {
            "skip_health_check": True,
            "connect_timeout": timedelta(seconds=1),
            "health_check_polling_interval": timedelta(milliseconds=1),
            **options,
        }
        if request.param:
            SandboxSync.connect("sb", connection_config=config, **options).close()
        else:
            sandbox = await Sandbox.connect("sb", connection_config=config, **options)
            await sandbox.close()

    return run


@pytest.mark.parametrize("resume", [False, True], ids=["connect", "resume"])
@pytest.mark.parametrize("phase", ["health", "transport"])
@pytest.mark.parametrize(
    "result", [True, False, RuntimeError("late custom failure")],
    ids=["late-success", "late-false", "late-error"],
)
def test_sync_rejects_late_custom_results_on_calling_thread(
    monkeypatch, resume, phase, result
):
    from threading import get_ident
    from types import SimpleNamespace

    from opensandbox.internal import readiness

    now = 0.0
    calls = []
    threads = []
    closed = []
    respond = responder(calls, failures=0)
    caller_thread = get_ident()

    def slow_operation():
        nonlocal now
        # Advance the budget deterministically instead of depending on scheduling.
        now += 0.35
        threads.append(get_ident())
        if isinstance(result, Exception):
            raise result
        return result

    def handle(request):
        response = respond(request)
        if phase == "transport" and request.method == "GET":
            slow_operation()
        return response

    class Transport(httpx.MockTransport):
        def close(self):
            closed.append(True)
            super().close()

    monkeypatch.setattr(readiness, "time", SimpleNamespace(monotonic=lambda: now))
    transport = Transport(handle)
    config = ConnectionConfigSync(domain="localhost:8080", transport=transport)
    method = SandboxSync.resume if resume else SandboxSync.connect
    timeout_key = "resume_timeout" if resume else "connect_timeout"
    try:
        with pytest.raises(SandboxReadyTimeoutException):
            method(
                "sb",
                connection_config=config,
                health_check=lambda _: slow_operation(),
                **{timeout_key: timedelta(milliseconds=50)},
            )
        assert threads == [caller_thread]
        endpoints = [path.rsplit("/", 1)[-1] for path in calls if "/endpoints/" in path]
        assert endpoints == (["44772", "18080"] if phase == "health" else ["44772"])
        assert not closed, "caller-provided transports must remain open"
        request = httpx.Request("GET", "http://localhost")
        readiness.constrain_readiness_request(request)
        assert readiness.DEADLINE_EXTENSION not in request.extensions
    finally:
        transport.close()


@pytest.mark.parametrize(
    "code,status", [("SANDBOX_NOT_FOUND", 404), (CODE, 401), (CODE, 403)]
)
@pytest.mark.asyncio
async def test_permanent_endpoint_error_is_returned_without_retry(
    connect, code, status
):
    calls = []
    with pytest.raises(SandboxApiException) as caught:
        await connect(responder(calls, failures=9999, code=code, status=status))
    assert len(calls) == 1
    assert caught.value.error.code == code


@pytest.mark.asyncio
async def test_endpoint_timeout_preserves_last_error(connect):
    with pytest.raises(SandboxReadyTimeoutException) as caught:
        await connect(
            responder([], failures=9999), connect_timeout=timedelta(milliseconds=20)
        )
    assert caught.value.__cause__.error.code == CODE


@pytest.mark.asyncio
async def test_cancellation_closes_owned_transport():
    started = asyncio.Event()
    closed = []

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            started.set()
            await asyncio.Event().wait()

        async def aclose(self):
            closed.append(True)

    config = ConnectionConfig(domain="localhost:8080", transport=Transport())
    config._owns_transport = True
    task = asyncio.create_task(Sandbox.connect("sb", connection_config=config))
    await started.wait()
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert closed == [True]


@pytest.mark.asyncio
async def test_async_shared_budget_bounds_slow_health(monkeypatch):
    from types import SimpleNamespace

    from opensandbox.internal import readiness

    now = 0
    waits = []
    wait = asyncio.wait
    handle = responder([], failures=0)
    monkeypatch.setattr(readiness, "time", SimpleNamespace(monotonic=lambda: now))

    async def observe_wait(tasks, *, timeout):
        nonlocal now
        waits.append(timeout)
        if len(waits) == 3:
            now = 30
            return await wait(tasks, timeout=0)
        return await wait(tasks, timeout=timeout)

    async def delayed(request):
        nonlocal now
        if request.url.path.endswith("/44772"):
            now += 8
        if request.url.path == "/ping":
            await asyncio.Event().wait()
        return handle(request)

    monkeypatch.setattr(asyncio, "wait", observe_wait)
    with pytest.raises(SandboxReadyTimeoutException):
        await Sandbox.connect(
            "sb",
            connection_config=ConnectionConfig(
                domain="localhost:8080", transport=httpx.MockTransport(delayed)
            ),
            connect_timeout=timedelta(seconds=30),
        )
    assert waits == [30, 22, 22]


@pytest.mark.asyncio
async def test_egress_retry_does_not_refetch_execd(connect):
    calls = []

    def handle(request):
        calls.append(request.url.path.rsplit("/", 1)[-1])
        if calls[-1] == "18080" and len(calls) < 4:
            return httpx.Response(404, json={"code": CODE, "message": "starting"})
        return httpx.Response(200, json={"endpoint": "localhost:44772", "headers": {}})

    await connect(handle)
    assert calls == ["44772", "18080", "18080", "18080"]


@pytest.mark.asyncio
async def test_cancel_during_endpoint_poll_sleep_stops_requests():
    calls = []
    config = ConnectionConfig(
        domain="localhost:8080",
        transport=httpx.MockTransport(responder(calls, failures=9999)),
    )
    task = asyncio.create_task(
        Sandbox.connect(
            "sb",
            connection_config=config,
            health_check_polling_interval=timedelta(seconds=1),
        )
    )
    while not calls:
        await asyncio.sleep(0)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert len(calls) == 1


def test_sync_transport_retry_after_cannot_exceed_connection_budget():
    from opensandbox.transport import RetryPolicy, RetrySyncTransport

    calls = []

    def unavailable(request):
        calls.append(request)
        return httpx.Response(503, headers={"Retry-After": "10"})

    transport = RetrySyncTransport(httpx.MockTransport(unavailable), RetryPolicy())
    start = time.monotonic()
    with pytest.raises(SandboxReadyTimeoutException):
        SandboxSync.connect(
            "sb",
            connection_config=ConnectionConfigSync(
                domain="localhost:8080", transport=transport
            ),
            connect_timeout=timedelta(milliseconds=50),
        )
    assert time.monotonic() - start < 0.3
    assert len(calls) == 1


@pytest.mark.parametrize("phase", ["endpoint", "health"])
def test_default_sync_transport_aborts_slow_stream_and_closes_connection(phase):
    import json
    import threading
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    request_started = []
    disconnected = threading.Event()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            slow = (
                self.path.split("?")[0].endswith("/44772")
                if phase == "endpoint"
                else self.path == "/ping"
            )
            if slow:
                request_started.append(time.monotonic())
                self.send_response(200)
                self.send_header("Content-Length", "10000")
                self.end_headers()
                try:
                    for _ in range(500):
                        self.wfile.write(b" ")
                        self.wfile.flush()
                        time.sleep(0.01)
                except OSError:
                    disconnected.set()
                return
            body = json.dumps(
                {"endpoint": f"127.0.0.1:{self.server.server_port}", "headers": {}}
            ).encode()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with pytest.raises(SandboxReadyTimeoutException):
            SandboxSync.connect(
                "sb",
                connection_config=ConnectionConfigSync(
                    domain=f"127.0.0.1:{server.server_port}"
                ),
                connect_timeout=timedelta(milliseconds=150),
            )
        assert request_started
        assert time.monotonic() - request_started[0] < 0.3
        assert disconnected.wait(0.5), "the deadline must close the active socket"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


def test_sync_dns_timeout_does_not_open_a_late_connection(monkeypatch):
    import socket
    import threading

    import httpcore

    release = threading.Event()
    finished = threading.Event()
    connections = []

    def resolve(*args, **kwargs):
        release.wait(2)
        finished.set()
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", ("127.0.0.1", 80))]

    monkeypatch.setattr(socket, "getaddrinfo", resolve)
    monkeypatch.setattr(
        httpcore.SyncBackend, "connect_tcp", lambda *a, **kw: connections.append(a)
    )
    try:
        with pytest.raises(SandboxReadyTimeoutException):
            SandboxSync.connect(
                "sb",
                connection_config=ConnectionConfigSync(domain="sandbox.invalid:80"),
                connect_timeout=timedelta(milliseconds=50),
                skip_health_check=True,
            )
        assert not release.is_set()
        assert not finished.is_set(), "connect must return while DNS is still blocked"
    finally:
        release.set()
        assert finished.wait(1)
    assert not connections


@pytest.mark.asyncio
async def test_timeout_does_not_wait_for_probe_that_suppresses_cancellation():
    from opensandbox.internal.readiness import ReadinessBudget

    release = asyncio.Event()
    cancelled = asyncio.Event()
    finished = asyncio.Event()

    async def probe():
        try:
            await asyncio.sleep(10)
        except asyncio.CancelledError:
            cancelled.set()
            await release.wait()
        finally:
            finished.set()
        raise RuntimeError("late probe failure")

    budget = ReadinessBudget(timedelta(milliseconds=50), timedelta(milliseconds=1))
    try:
        with pytest.raises(
            SandboxReadyTimeoutException, match="Endpoint has not been resolved"
        ):
            await asyncio.wait_for(budget.run(probe), timeout=1)
        await asyncio.wait_for(cancelled.wait(), timeout=1)
        assert not finished.is_set()
    finally:
        release.set()
        await asyncio.wait_for(finished.wait(), timeout=1)


@pytest.mark.asyncio
async def test_blocking_async_probe_finishes_before_timeout_is_reported():
    calls = []
    finished = []

    async def probe():
        calls.append(True)
        time.sleep(0.1)
        finished.append(True)
        return True

    budget = ReadinessBudget(timedelta(milliseconds=50), timedelta(milliseconds=1))
    with pytest.raises(SandboxReadyTimeoutException):
        await budget.health(probe, "blocking probe")
    assert calls == [True]
    assert finished == [True]


def test_sync_transport_maps_httpcore_exception_subclasses(monkeypatch):
    import ssl

    import httpcore

    from opensandbox.transport._deadline_sync import (
        DEADLINE_EXTENSION,
        DeadlineSyncTransport,
    )

    class CustomReadTimeout(httpcore.ReadTimeout):
        pass

    error = CustomReadTimeout("read stalled")

    def fail(*args, **kwargs):
        raise error

    monkeypatch.setattr(httpcore.ConnectionPool, "request", fail)
    request = httpx.Request(
        "GET", "http://localhost", extensions={DEADLINE_EXTENSION: time.monotonic() + 1}
    )
    with DeadlineSyncTransport(
        httpx.MockTransport(lambda r: httpx.Response(200)), ssl.create_default_context()
    ) as transport:
        with pytest.raises(httpx.ReadTimeout) as actual:
            transport.handle_request(request)
    assert actual.value.__cause__ is error
    assert actual.value.request is request


@pytest.mark.parametrize("sync", [False, True])
@pytest.mark.asyncio
async def test_health_timeout_does_not_report_previous_endpoint_error(sync):
    budget = ReadinessBudget(timedelta(0), timedelta(milliseconds=1))
    budget.last_error = RuntimeError("previous endpoint failure")
    calls = 0

    def probe():
        nonlocal calls
        calls += 1
        return True

    async def async_probe():
        return probe()

    with pytest.raises(SandboxReadyTimeoutException) as raised:
        if sync:
            budget.health_sync(probe, "test health context")
        else:
            await budget.health(async_probe, "test health context")

    assert calls == 0
    assert raised.value.__cause__ is None
    assert "previous endpoint failure" not in str(raised.value)
    assert "health check timed out" in str(raised.value)
