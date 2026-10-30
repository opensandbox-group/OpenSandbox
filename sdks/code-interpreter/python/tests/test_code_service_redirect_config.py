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

"""Code interpreter clients honor connection hooks and redirect opt-in."""

import httpx
import pytest
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync


class _Requests:
    def __init__(self, operation: str, destination: str) -> None:
        self.operation = operation
        self.destination = destination
        self.requests: list[httpx.Request] = []
        self.hooks: list[str] = []
        self.statuses: list[int] = []

    def request_hook(self, request: httpx.Request) -> None:
        self.hooks.append(str(request.url))
        # Even caller hooks cannot reintroduce protected headers on another origin.
        request.headers["OpenSandbox-Secure-Access"] = "synthetic-access"
        request.headers["OPEN-SANDBOX-API-KEY"] = "synthetic-key"
        request.headers["X-Custom-Header"] = "keep"

    def response_hook(self, response: httpx.Response) -> None:
        self.statuses.append(response.status_code)

    def handle(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if request.url.path != "/final":
            return httpx.Response(307, headers={"Location": self.destination})
        if self.operation == "context":
            return httpx.Response(200, json={"id": "ctx-1", "language": "python"})
        return httpx.Response(
            200,
            headers={"Content-Type": "text/event-stream"},
            content='data: {"type":"stdout","text":"hello","timestamp":1}\n\n',
        )

    def verify(self, follow: bool) -> None:
        assert len(self.requests) == (2 if follow else 1)
        assert self.hooks == [str(request.url) for request in self.requests]
        assert self.statuses == ([307, 200] if follow else [307])
        for request in self.requests:
            same_origin = (request.url.host, request.url.port, request.url.scheme) == (
                "sandbox.test",
                None,
                "http",
            )
            assert request.headers.get("OPEN-SANDBOX-API-KEY") == (
                "synthetic-key" if same_origin else None
            )
            assert request.headers.get("OpenSandbox-Secure-Access") == (
                "synthetic-access" if same_origin else None
            )
            assert request.headers.get("X-Custom-Header") == "keep"


@pytest.mark.parametrize("operation", ["context", "run"])
@pytest.mark.parametrize(
    "destination",
    ["/final", "http://other.test/final", "http://sandbox.test:8080/final"],
)
@pytest.mark.parametrize("follow", [False, True])
def test_sync_connection_options(
    operation: str, destination: str, follow: bool
) -> None:
    calls = _Requests(operation, destination)
    hooks = {"request": [calls.request_hook], "response": [calls.response_hook]}
    config = ConnectionConfigSync(
        transport=httpx.MockTransport(calls.handle),
        event_hooks=hooks,
        **({"follow_redirects": True} if follow else {}),
    )
    adapter = CodesAdapterSync(SandboxEndpoint(endpoint="sandbox.test"), config)
    try:
        if follow:
            result = (
                adapter.create_context("python")
                if operation == "context"
                else adapter.run("print('hello')")
            )
            assert (result.id if operation == "context" else result.text) == (
                "ctx-1" if operation == "context" else "hello"
            )
        else:
            with pytest.raises(SandboxApiException) as error:
                if operation == "context":
                    adapter.create_context("python")
                else:
                    adapter.run("print('hello')")
            assert error.value.status_code == 307
        calls.verify(follow)
        assert config.event_hooks == hooks
        assert len(config.event_hooks["request"]) == 1
    finally:
        adapter._httpx_client.close()
        adapter._sse_client.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("operation", ["context", "run"])
@pytest.mark.parametrize(
    "destination",
    ["/final", "http://other.test/final", "http://sandbox.test:8080/final"],
)
@pytest.mark.parametrize("follow", [False, True])
async def test_async_connection_options(
    operation: str, destination: str, follow: bool
) -> None:
    calls = _Requests(operation, destination)

    async def request_hook(request: httpx.Request) -> None:
        calls.request_hook(request)

    async def response_hook(response: httpx.Response) -> None:
        calls.response_hook(response)

    hooks = {"request": [request_hook], "response": [response_hook]}
    config = ConnectionConfig(
        transport=httpx.MockTransport(calls.handle),
        event_hooks=hooks,
        **({"follow_redirects": True} if follow else {}),
    )
    adapter = CodesAdapter(SandboxEndpoint(endpoint="sandbox.test"), config)
    try:
        if follow:
            result = (
                await adapter.create_context("python")
                if operation == "context"
                else await adapter.run("print('hello')")
            )
            assert (result.id if operation == "context" else result.text) == (
                "ctx-1" if operation == "context" else "hello"
            )
        else:
            with pytest.raises(SandboxApiException) as error:
                if operation == "context":
                    await adapter.create_context("python")
                else:
                    await adapter.run("print('hello')")
            assert error.value.status_code == 307
        calls.verify(follow)
        assert config.event_hooks == hooks
        assert len(config.event_hooks["request"]) == 1
    finally:
        await adapter._httpx_client.aclose()
        await adapter._sse_client.aclose()
