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
from datetime import timedelta

import httpx
import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException, SandboxReadyTimeoutException
from opensandbox.sandbox import Sandbox
from opensandbox.sync.sandbox import SandboxSync

CODE = "KUBERNETES::POD_IP_NOT_AVAILABLE"

CREATE_RESPONSE = {
    "id": "sbx-created",
    "status": {"state": "Running"},
    "createdAt": "2026-01-02T03:04:05Z",
    "entrypoint": ["tail", "-f", "/dev/null"],
}


def responder(calls, failures=2, code=CODE, status=404):
    def handle(request):
        calls.append(request.url.path)
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.method == "POST":
            return httpx.Response(202, json=CREATE_RESPONSE)
        if request.url.path == "/ping":
            assert request.headers["x-endpoint-token"] == "new"
            return httpx.Response(200)
        port = request.url.path.rsplit("/", 1)[-1]
        port_calls = [p for p in calls if p.endswith(f"/{port}")]
        if len(port_calls) <= failures:
            return httpx.Response(status, json={"code": code, "message": "starting"})
        return httpx.Response(
            200,
            json={
                "endpoint": f"localhost:{port}",
                "headers": {"x-endpoint-token": "new"},
            },
        )

    return handle


def endpoint_calls(calls, port):
    return [p for p in calls if p.endswith(f"/{port}")]


@pytest.mark.asyncio
async def test_async_create_retries_transient_endpoint_unavailability():
    calls = []
    config = ConnectionConfig(
        domain="localhost:8080",
        transport=httpx.MockTransport(responder(calls)),
        disable_metrics=True,
    )
    sb = await Sandbox.create(
        "python:3.11",
        connection_config=config,
        health_check_polling_interval=timedelta(milliseconds=1),
    )
    assert sb.id == "sbx-created"
    assert len(endpoint_calls(calls, 44772)) == 3
    assert len(endpoint_calls(calls, 18080)) == 3
    assert "/ping" in calls
    await sb.close()


def test_sync_create_retries_transient_endpoint_unavailability():
    calls = []
    timeouts = []
    respond = responder(calls)

    def handle(request):
        if request.method == "GET":
            timeouts.append(request.extensions["timeout"]["read"])
        return respond(request)

    config = ConnectionConfigSync(
        domain="localhost:8080",
        transport=httpx.MockTransport(handle),
        disable_metrics=True,
    )
    sb = SandboxSync.create(
        "python:3.11",
        connection_config=config,
        health_check_polling_interval=timedelta(milliseconds=1),
    )
    assert sb.id == "sbx-created"
    assert len(endpoint_calls(calls, 44772)) == 3
    assert len(endpoint_calls(calls, 18080)) == 3
    assert "/ping" in calls
    assert 0 < timeouts[-1] < timeouts[0] <= 30
    sb.close()


@pytest.fixture(params=[False, True], ids=["async", "sync"])
def create(request):
    async def run(handler, **options):
        config_type = ConnectionConfigSync if request.param else ConnectionConfig
        config = config_type(
            domain="localhost:8080",
            transport=httpx.MockTransport(handler),
            disable_metrics=True,
        )
        options = {
            "skip_health_check": True,
            "ready_timeout": timedelta(seconds=1),
            "health_check_polling_interval": timedelta(milliseconds=1),
            **options,
        }
        if request.param:
            SandboxSync.create(
                "python:3.11", connection_config=config, **options
            ).close()
        else:
            sandbox = await Sandbox.create(
                "python:3.11", connection_config=config, **options
            )
            await sandbox.close()

    return run


@pytest.mark.parametrize(
    "code,status", [("SANDBOX_NOT_FOUND", 404), (CODE, 401), (CODE, 403)]
)
@pytest.mark.asyncio
async def test_permanent_endpoint_error_is_returned_without_retry(
    create, code, status
):
    calls = []
    with pytest.raises(SandboxApiException) as caught:
        await create(responder(calls, failures=9999, code=code, status=status))
    assert len(endpoint_calls(calls, 44772)) == 1
    assert caught.value.error.code == code
    # The zombie sandbox created before the failure must be terminated.
    assert calls.count("/v1/sandboxes/sbx-created") == 1


@pytest.mark.asyncio
async def test_endpoint_timeout_preserves_last_error(create):
    calls = []
    with pytest.raises(SandboxReadyTimeoutException) as caught:
        await create(
            responder(calls, failures=9999), ready_timeout=timedelta(milliseconds=20)
        )
    assert caught.value.__cause__.error.code == CODE
    assert "/ping" not in calls
    assert calls.count("/v1/sandboxes/sbx-created") == 1


@pytest.mark.asyncio
async def test_skip_health_check_still_awaits_endpoint_publication(create):
    calls = []
    await create(responder(calls))
    assert len(endpoint_calls(calls, 44772)) == 3
    assert len(endpoint_calls(calls, 18080)) == 3
    assert "/ping" not in calls


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
        return await wait(tasks, timeout=timeout)

    async def delayed(request):
        nonlocal now
        if request.url.path.endswith("/44772"):
            now += 16
        return handle(request)

    monkeypatch.setattr(asyncio, "wait", observe_wait)
    sb = await Sandbox.create(
        "python:3.11",
        connection_config=ConnectionConfig(
            domain="localhost:8080",
            transport=httpx.MockTransport(delayed),
            disable_metrics=True,
        ),
        ready_timeout=timedelta(seconds=30),
        health_check_polling_interval=timedelta(milliseconds=1),
    )
    await sb.close()
    # Two endpoint requests through the budget, then the health probe receives
    # only the remaining budget (30s - 16s consumed by the execd endpoint).
    assert len(waits) == 3
    assert waits[-1] == 14
    assert all(wait_ <= 30 for wait_ in waits)


@pytest.mark.asyncio
async def test_cancel_during_endpoint_poll_sleep_stops_requests():
    calls = []
    config = ConnectionConfig(
        domain="localhost:8080",
        transport=httpx.MockTransport(responder(calls, failures=9999)),
        disable_metrics=True,
    )
    task = asyncio.create_task(
        Sandbox.create(
            "python:3.11",
            connection_config=config,
            health_check_polling_interval=timedelta(seconds=1),
        )
    )
    while not endpoint_calls(calls, 44772):
        await asyncio.sleep(0)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert len(endpoint_calls(calls, 44772)) == 1


@pytest.mark.asyncio
async def test_async_create_cancels_sibling_endpoint_retry_on_permanent_failure(
    monkeypatch,
):
    from types import SimpleNamespace

    from opensandbox.internal import readiness

    calls = []
    now = 0.0
    monkeypatch.setattr(readiness, "time", SimpleNamespace(monotonic=lambda: now))

    def handle(request):
        nonlocal now
        calls.append(request.url.path)
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.method == "POST":
            return httpx.Response(202, json=CREATE_RESPONSE)
        if request.url.path.endswith("/44772"):
            # Fail permanently only once the sibling retry loop is running, so
            # the test exercises cancelling an active poller.
            if len(endpoint_calls(calls, 18080)) >= 1:
                return httpx.Response(403, json={"code": CODE, "message": "denied"})
            return httpx.Response(404, json={"code": CODE, "message": "starting"})
        now += 0.1  # each egress poll drains the fake readiness clock
        return httpx.Response(404, json={"code": CODE, "message": "starting"})

    with pytest.raises(SandboxApiException) as caught:
        await Sandbox.create(
            "python:3.11",
            connection_config=ConnectionConfig(
                domain="localhost:8080",
                transport=httpx.MockTransport(handle),
                disable_metrics=True,
            ),
            ready_timeout=timedelta(seconds=10),
            health_check_polling_interval=timedelta(milliseconds=10),
            skip_health_check=True,
        )
    assert caught.value.status_code == 403
    # Let any leaked sibling coroutine betray itself before asserting.
    await asyncio.sleep(0.2)
    egress = endpoint_calls(calls, 18080)
    # At 0.1s of fake time per poll, spinning to the 10s shared deadline
    # would need ~100 egress requests; a cancelled loop stops after a couple.
    assert 1 <= len(egress) <= 3
    assert now < 10
    # The sibling is joined before create() cleans up: no endpoint request is
    # issued against the sandbox after its deletion.
    assert calls.count("/v1/sandboxes/sbx-created") == 1
    assert max(calls.index(path) for path in egress) < calls.index(
        "/v1/sandboxes/sbx-created"
    )


@pytest.mark.asyncio
async def test_async_create_sibling_cancellation_preserves_original_error():
    calls = []

    def handle(request):
        calls.append(request.url.path)
        if request.method == "DELETE":
            return httpx.Response(204)
        if request.method == "POST":
            return httpx.Response(202, json=CREATE_RESPONSE)
        if request.url.path.endswith("/44772"):
            return httpx.Response(
                403, json={"code": "ACCESS_DENIED", "message": "denied"}
            )
        return httpx.Response(404, json={"code": CODE, "message": "starting"})

    with pytest.raises(SandboxApiException) as caught:
        await Sandbox.create(
            "python:3.11",
            connection_config=ConnectionConfig(
                domain="localhost:8080",
                transport=httpx.MockTransport(handle),
                disable_metrics=True,
            ),
            ready_timeout=timedelta(seconds=10),
            health_check_polling_interval=timedelta(milliseconds=10),
            skip_health_check=True,
        )
    # The permanent execd failure surfaces, not the sibling's CancelledError
    # and not a readiness timeout from awaiting the retrying egress loop.
    assert caught.value.status_code == 403
    assert caught.value.error.code == "ACCESS_DENIED"
    # The sibling was genuinely mid-retry when the failure surfaced.
    assert endpoint_calls(calls, 18080)
    assert calls.count("/v1/sandboxes/sbx-created") == 1
