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
import asyncio
from datetime import timedelta

import httpx
import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.sandbox import Sandbox


@pytest.mark.asyncio
@pytest.mark.parametrize("initial_cancel", [False, True], ids=["api-error", "cancelled"])
@pytest.mark.parametrize("template", [False, True], ids=["image", "template"])
@pytest.mark.parametrize("owned", [True, False], ids=["owned", "borrowed"])
async def test_cancellation_during_failed_create_cleanup_closes_owned_transport(
    monkeypatch: pytest.MonkeyPatch, template: bool, owned: bool, initial_cancel: bool
) -> None:
    cleanup_started = asyncio.Event()
    endpoint_started = asyncio.Event()
    requests: list[str] = []

    async def respond(request: httpx.Request) -> httpx.Response:
        requests.append(request.method)
        if request.method == "POST":
            return httpx.Response(202, json={
                "id": "sandbox-cleanup",
                "status": {"state": "Running"},
                "createdAt": "2026-01-02T03:04:05Z",
                "entrypoint": ["tail", "-f", "/dev/null"],
            })
        if request.method == "DELETE":
            cleanup_started.set()
            await asyncio.Event().wait()
            raise AssertionError("unreachable")
        if initial_cancel:
            endpoint_started.set()
            await asyncio.Event().wait()
            raise AssertionError("unreachable")
        return httpx.Response(403, json={"code": "DENIED", "message": "denied"})

    class TrackingTransport(httpx.MockTransport):
        closes = 0

        async def aclose(self) -> None:
            self.closes += 1
            await super().aclose()

    transport = TrackingTransport(respond)
    if owned:
        monkeypatch.setattr(ConnectionConfig, "new_owned_transport", lambda *a, **kw: transport)
        config = ConnectionConfig(domain="sandbox.test", disable_metrics=True)
    else:
        config = ConnectionConfig(
            domain="sandbox.test", transport=transport, disable_metrics=True
        )
    operation = (
        Sandbox.create_from_template(
            "template-test", timeout=timedelta(minutes=5), connection_config=config
        )
        if template else Sandbox.create("python:3.11", connection_config=config)
    )
    task = asyncio.create_task(operation)
    try:
        if initial_cancel:
            await asyncio.wait_for(endpoint_started.wait(), timeout=2)
            task.cancel()
        await asyncio.wait_for(cleanup_started.wait(), timeout=2)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
        assert requests.count("DELETE") == 1
        assert transport.closes == int(owned)
    finally:
        if not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        await transport.aclose()
