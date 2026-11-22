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
import json
import threading
import time
from contextlib import contextmanager
from datetime import timedelta
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace

import httpx
import pytest

from opensandbox.adapters import health_adapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxReadyTimeoutException
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sandbox import Sandbox
from opensandbox.sync.adapters import health_adapter as health_adapter_sync
from opensandbox.sync.sandbox import SandboxSync
from opensandbox.transport import RetryPolicy


@contextmanager
def sandbox_server(*, creation_delay=0):
    release = threading.Event()
    state = SimpleNamespace(pings=0)
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def respond(self, status, payload):
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", "0")))
            time.sleep(creation_delay)
            self.respond(
                202,
                {
                    "id": "sbx-created",
                    "status": {"state": "Running"},
                    "createdAt": "2026-01-02T03:04:05Z",
                    "entrypoint": ["tail", "-f", "/dev/null"],
                },
            )

        def do_GET(self):
            if self.path == "/ping":
                with lock:
                    state.pings += 1
                    first_ping = state.pings == 1
                if first_ping:
                    # Only a client timeout can open the healthy second connection.
                    release.wait(10)
                    return
                self.respond(200, {})
            else:
                self.respond(200, {"endpoint": f"127.0.0.1:{self.server.server_port}"})

        def do_DELETE(self):
            self.send_response(204)
            self.end_headers()

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server_thread = threading.Thread(
        target=lambda: server.serve_forever(poll_interval=0.01), daemon=True
    )
    server_thread.start()
    state.domain = f"127.0.0.1:{server.server_port}"
    try:
        yield state
    finally:
        release.set()
        server.shutdown()
        server.server_close()
        server_thread.join(timeout=2)
        assert not server_thread.is_alive()


async def open_sandbox(sync, operation, server, *, timeout, retry_policy):
    config_type = ConnectionConfigSync if sync else ConnectionConfig
    config = config_type(
        domain=server.domain,
        request_timeout=timedelta(seconds=3),
        retry_policy=retry_policy,
        disable_metrics=True,
    )
    sandbox_type = SandboxSync if sync else Sandbox
    options = {
        "connection_config": config,
        "health_check_polling_interval": timedelta(milliseconds=10),
        "ready_timeout" if operation == "create" else "connect_timeout": timeout,
    }
    result = getattr(sandbox_type, operation)(
        "python:3.11" if operation == "create" else "sbx-existing", **options
    )
    return result if sync else await result


@pytest.fixture
def short_health_timeout(monkeypatch):
    timeout = timedelta(milliseconds=100)
    for module in (health_adapter, health_adapter_sync):
        monkeypatch.setattr(module, "HEALTH_PING_TIMEOUT", timeout, raising=False)
    return timeout


@pytest.mark.parametrize("sync", [False, True], ids=["async", "sync"])
@pytest.mark.parametrize("request_seconds,expected_seconds", [(30, 5), (1, 1)])
@pytest.mark.asyncio
async def test_health_timeout_caps_default_without_expanding_short_requests(
    sync, request_seconds, expected_seconds
):
    requests = []

    def respond(request):
        requests.append(request)
        return httpx.Response(200)

    config_type = ConnectionConfigSync if sync else ConnectionConfig
    config = config_type(
        request_timeout=timedelta(seconds=request_seconds),
        transport=httpx.MockTransport(respond),
    )
    adapter_type = (
        health_adapter_sync.HealthAdapterSync if sync else health_adapter.HealthAdapter
    )
    adapter = adapter_type(config, SandboxEndpoint(endpoint="localhost:44772"))
    try:
        assert adapter.ping("sb") if sync else await adapter.ping("sb")
        assert requests[0].extensions["timeout"] == dict.fromkeys(
            ("connect", "read", "write", "pool"), expected_seconds
        )
    finally:
        if sync:
            adapter._httpx_client.close()
        else:
            await adapter._httpx_client.aclose()


@pytest.mark.parametrize("sync", [False, True], ids=["async", "sync"])
@pytest.mark.parametrize(
    "operation,retry",
    [("connect", True), ("create", True), ("connect", False)],
    ids=["connect-retry", "create-retry", "connect-no-retry"],
)
@pytest.mark.asyncio
async def test_readiness_recovers_after_first_health_connection_stalls(
    sync, operation, retry, short_health_timeout
):
    retry_policy = (
        RetryPolicy(initial_backoff=timedelta(0), max_backoff=timedelta(0))
        if retry
        else RetryPolicy.disabled()
    )
    # A slow create must retain the management request timeout, not the ping cap.
    creation_delay = short_health_timeout.total_seconds() * 3
    with sandbox_server(creation_delay=creation_delay) as server:
        sandbox = await open_sandbox(
            sync,
            operation,
            server,
            timeout=timedelta(seconds=2),
            retry_policy=retry_policy,
        )
        try:
            assert server.pings == 2
        finally:
            if sync:
                sandbox.close()
            else:
                await sandbox.close()


@pytest.mark.parametrize("sync", [False, True], ids=["async", "sync"])
@pytest.mark.asyncio
async def test_readiness_budget_shorter_than_ping_cap_still_expires(sync):
    with sandbox_server() as server:
        started = time.monotonic()
        with pytest.raises(SandboxReadyTimeoutException):
            await open_sandbox(
                sync,
                "connect",
                server,
                timeout=timedelta(milliseconds=500),
                retry_policy=RetryPolicy.disabled(),
            )
        assert server.pings == 1
        assert time.monotonic() - started < 2
