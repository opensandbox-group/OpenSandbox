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
import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.health_adapter import HealthAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.sync.adapters.health_adapter import HealthAdapterSync
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync

CapturedRequest = tuple[
    str | None,
    str,
    str | None,
    str | None,
    str | None,
    str | None,
]


def _capture_request(request: httpx.Request) -> CapturedRequest:
    return (
        request.url.host,
        request.url.path,
        request.headers.get("OPEN-SANDBOX-API-KEY"),
        request.headers.get("OPENSANDBOX-EGRESS-AUTH"),
        request.headers.get("OpenSandbox-Secure-Access"),
        request.headers.get("X-Custom-Header"),
    )


class _RedirectTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.paths: list[str] = []
        self.requests: list[CapturedRequest] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.paths.append(request.url.path)
        self.requests.append(_capture_request(request))
        if request.url.path == "/start":
            return httpx.Response(
                307,
                headers={"Location": "/final"},
                request=request,
            )
        return httpx.Response(204, request=request)


class _AsyncRedirectTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.paths: list[str] = []
        self.requests: list[CapturedRequest] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.paths.append(request.url.path)
        self.requests.append(_capture_request(request))
        if request.url.path == "/start":
            return httpx.Response(
                307,
                headers={"Location": "/final"},
                request=request,
            )
        return httpx.Response(204, request=request)


class _CrossOriginRedirectTransport(httpx.BaseTransport):
    def __init__(self, location: str = "http://redirect.local/final") -> None:
        self.location = location
        self.requests: list[CapturedRequest] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(_capture_request(request))
        if request.url.path.endswith("/start"):
            return httpx.Response(
                307,
                headers={"Location": self.location},
                request=request,
            )
        return httpx.Response(204, request=request)


class _AsyncCrossOriginRedirectTransport(httpx.AsyncBaseTransport):
    def __init__(self, location: str = "http://redirect.local/final") -> None:
        self.location = location
        self.requests: list[CapturedRequest] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(_capture_request(request))
        if request.url.path.endswith("/start"):
            return httpx.Response(
                307,
                headers={"Location": self.location},
                request=request,
            )
        return httpx.Response(204, request=request)


def test_follow_redirects_defaults_to_false() -> None:
    assert ConnectionConfig().follow_redirects is False
    assert ConnectionConfigSync().follow_redirects is False
    assert ConnectionConfig().event_hooks == {}
    assert ConnectionConfigSync().event_hooks == {}


@pytest.mark.asyncio
async def test_async_adapter_http_client_follows_redirects_from_config() -> None:
    transport = _AsyncRedirectTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport, follow_redirects=True)
    adapter = HealthAdapter(cfg, SandboxEndpoint(endpoint="sandbox.local:8080"))

    response = await adapter._httpx_client.get("/start")
    await adapter._httpx_client.aclose()

    assert response.status_code == 204
    assert [response.status_code for response in response.history] == [307]
    assert transport.paths == ["/start", "/final"]


@pytest.mark.asyncio
async def test_async_adapter_does_not_follow_redirects_by_default() -> None:
    transport = _AsyncRedirectTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport)
    adapter = HealthAdapter(cfg, SandboxEndpoint(endpoint="sandbox.local:8080"))

    response = await adapter._httpx_client.get("/start")
    await adapter._httpx_client.aclose()

    assert response.status_code == 307
    assert response.history == []
    assert transport.paths == ["/start"]


@pytest.mark.asyncio
async def test_async_sse_client_follows_redirects_from_config() -> None:
    transport = _AsyncRedirectTransport()
    cfg = ConnectionConfig(protocol="http", transport=transport, follow_redirects=True)
    adapter = CommandsAdapter(cfg, SandboxEndpoint(endpoint="sandbox.local:8080"))

    assert adapter._client.get_async_httpx_client() is adapter._httpx_client
    response = await adapter._sse_client.get("http://sandbox.local:8080/start")
    await adapter._httpx_client.aclose()
    await adapter._sse_client.aclose()

    assert response.status_code == 204
    assert [response.status_code for response in response.history] == [307]
    assert transport.paths == ["/start", "/final"]


@pytest.mark.asyncio
async def test_async_adapter_strips_protected_headers_on_cross_origin_redirect() -> (
    None
):
    transport = _AsyncCrossOriginRedirectTransport()
    cfg = ConnectionConfig(
        api_key="secret",
        domain="sandbox.local:8080",
        protocol="http",
        transport=transport,
        follow_redirects=True,
        headers={
            "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
            "OpenSandbox-Secure-Access": "access-secret",
            "X-Custom-Header": "preserved",
        },
    )
    adapter = SandboxesAdapter(cfg)

    generated_httpx_client = adapter._client.get_async_httpx_client()
    assert generated_httpx_client is adapter._httpx_client
    response = await generated_httpx_client.get("/start")
    await adapter._httpx_client.aclose()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/v1/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        ("redirect.local", "/final", None, None, None, "preserved"),
    ]


@pytest.mark.asyncio
async def test_async_sse_client_strips_protected_headers_on_cross_origin_redirect() -> (
    None
):
    transport = _AsyncCrossOriginRedirectTransport()
    cfg = ConnectionConfig(
        headers={"OPEN-SANDBOX-API-KEY": "secret"},
        protocol="http",
        transport=transport,
        follow_redirects=True,
    )
    adapter = CommandsAdapter(
        cfg,
        SandboxEndpoint(
            endpoint="sandbox.local:8080",
            headers={
                "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
                "OpenSandbox-Secure-Access": "access-secret",
                "X-Custom-Header": "preserved",
            },
        ),
    )

    response = await adapter._sse_client.get("http://sandbox.local:8080/start")
    await adapter._httpx_client.aclose()
    await adapter._sse_client.aclose()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        ("redirect.local", "/final", None, None, None, "preserved"),
    ]


@pytest.mark.asyncio
async def test_async_same_origin_redirect_preserves_protected_headers() -> None:
    transport = _AsyncRedirectTransport()
    cfg = ConnectionConfig(
        headers={"OPEN-SANDBOX-API-KEY": "secret"},
        protocol="http",
        transport=transport,
        follow_redirects=True,
    )
    adapter = CommandsAdapter(
        cfg,
        SandboxEndpoint(
            endpoint="sandbox.local:8080",
            headers={
                "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
                "OpenSandbox-Secure-Access": "access-secret",
                "X-Custom-Header": "preserved",
            },
        ),
    )

    response = await adapter._sse_client.get("http://sandbox.local:8080/start")
    await adapter._httpx_client.aclose()
    await adapter._sse_client.aclose()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        (
            "sandbox.local",
            "/final",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
    ]


@pytest.mark.asyncio
async def test_async_event_hooks_are_preserved_and_strip_hook_runs_last() -> None:
    transport = _AsyncCrossOriginRedirectTransport()
    request_hosts: list[str | None] = []
    response_statuses: list[int] = []

    async def request_hook(request: httpx.Request) -> None:
        request_hosts.append(request.url.host)
        request.headers["OPEN-SANDBOX-API-KEY"] = "hook-secret"

    async def response_hook(response: httpx.Response) -> None:
        response_statuses.append(response.status_code)

    cfg = ConnectionConfig(
        api_key="secret",
        domain="sandbox.local:8080",
        protocol="http",
        transport=transport,
        follow_redirects=True,
        event_hooks={"request": [request_hook], "response": [response_hook]},
    )
    adapter = SandboxesAdapter(cfg)

    response = await adapter._httpx_client.get("/start")
    await adapter._httpx_client.aclose()

    assert response.status_code == 204
    assert request_hosts == ["sandbox.local", "redirect.local"]
    assert response_statuses == [307, 204]
    assert transport.requests == [
        ("sandbox.local", "/v1/start", "hook-secret", None, None, None),
        ("redirect.local", "/final", None, None, None, None),
    ]


def test_sync_adapter_http_client_follows_redirects_from_config() -> None:
    transport = _RedirectTransport()
    cfg = ConnectionConfigSync(
        protocol="http", transport=transport, follow_redirects=True
    )
    adapter = HealthAdapterSync(
        cfg,
        SandboxEndpoint(endpoint="sandbox.local:8080"),
    )

    response = adapter._httpx_client.get("/start")
    adapter._httpx_client.close()

    assert response.status_code == 204
    assert [response.status_code for response in response.history] == [307]
    assert transport.paths == ["/start", "/final"]


def test_sync_adapter_does_not_follow_redirects_by_default() -> None:
    transport = _RedirectTransport()
    cfg = ConnectionConfigSync(protocol="http", transport=transport)
    adapter = HealthAdapterSync(
        cfg,
        SandboxEndpoint(endpoint="sandbox.local:8080"),
    )

    response = adapter._httpx_client.get("/start")
    adapter._httpx_client.close()

    assert response.status_code == 307
    assert response.history == []
    assert transport.paths == ["/start"]


def test_sync_adapter_strips_protected_headers_on_cross_origin_redirect() -> None:
    transport = _CrossOriginRedirectTransport()
    cfg = ConnectionConfigSync(
        api_key="secret",
        domain="sandbox.local:8080",
        protocol="http",
        transport=transport,
        follow_redirects=True,
        headers={
            "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
            "OpenSandbox-Secure-Access": "access-secret",
            "X-Custom-Header": "preserved",
        },
    )
    adapter = SandboxesAdapterSync(cfg)

    generated_httpx_client = adapter._client.get_httpx_client()
    assert generated_httpx_client is adapter._httpx_client
    response = generated_httpx_client.get("/start")
    adapter._httpx_client.close()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/v1/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        ("redirect.local", "/final", None, None, None, "preserved"),
    ]


def test_sync_sse_client_strips_protected_headers_on_cross_origin_redirect() -> None:
    transport = _CrossOriginRedirectTransport()
    cfg = ConnectionConfigSync(
        headers={"OPEN-SANDBOX-API-KEY": "secret"},
        protocol="http",
        transport=transport,
        follow_redirects=True,
    )
    adapter = CommandsAdapterSync(
        cfg,
        SandboxEndpoint(
            endpoint="sandbox.local:8080",
            headers={
                "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
                "OpenSandbox-Secure-Access": "access-secret",
                "X-Custom-Header": "preserved",
            },
        ),
    )

    response = adapter._sse_client.get("http://sandbox.local:8080/start")
    adapter._httpx_client.close()
    adapter._sse_client.close()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        ("redirect.local", "/final", None, None, None, "preserved"),
    ]


def test_sync_same_origin_redirect_preserves_protected_headers() -> None:
    transport = _RedirectTransport()
    cfg = ConnectionConfigSync(
        headers={"OPEN-SANDBOX-API-KEY": "secret"},
        protocol="http",
        transport=transport,
        follow_redirects=True,
    )
    adapter = CommandsAdapterSync(
        cfg,
        SandboxEndpoint(
            endpoint="sandbox.local:8080",
            headers={
                "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
                "OpenSandbox-Secure-Access": "access-secret",
                "X-Custom-Header": "preserved",
            },
        ),
    )

    response = adapter._sse_client.get("http://sandbox.local:8080/start")
    adapter._httpx_client.close()
    adapter._sse_client.close()

    assert response.status_code == 204
    assert transport.requests == [
        (
            "sandbox.local",
            "/start",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
        (
            "sandbox.local",
            "/final",
            "secret",
            "egress-secret",
            "access-secret",
            "preserved",
        ),
    ]


@pytest.mark.parametrize(
    "location",
    [
        "https://sandbox.local:8080/final",
        "http://sandbox.local:8081/final",
        "http://redirect.local:8080/final",
    ],
)
def test_scheme_host_or_port_change_is_cross_origin(location: str) -> None:
    transport = _CrossOriginRedirectTransport(location)
    cfg = ConnectionConfigSync(
        protocol="http",
        transport=transport,
        follow_redirects=True,
        headers={
            "OPEN-SANDBOX-API-KEY": "secret",
            "OPENSANDBOX-EGRESS-AUTH": "egress-secret",
            "OpenSandbox-Secure-Access": "access-secret",
            "X-Custom-Header": "preserved",
        },
    )
    adapter = HealthAdapterSync(
        cfg,
        SandboxEndpoint(endpoint="sandbox.local:8080"),
    )

    response = adapter._httpx_client.get("http://sandbox.local:8080/start")
    adapter._httpx_client.close()

    assert response.status_code == 204
    assert transport.requests[0][2:] == (
        "secret",
        "egress-secret",
        "access-secret",
        "preserved",
    )
    assert transport.requests[1][2:] == (None, None, None, "preserved")


def test_sync_event_hooks_are_preserved_and_strip_hook_runs_last() -> None:
    transport = _CrossOriginRedirectTransport()
    request_hosts: list[str | None] = []
    response_statuses: list[int] = []

    def request_hook(request: httpx.Request) -> None:
        request_hosts.append(request.url.host)
        request.headers["OPEN-SANDBOX-API-KEY"] = "hook-secret"

    def response_hook(response: httpx.Response) -> None:
        response_statuses.append(response.status_code)

    cfg = ConnectionConfigSync(
        api_key="secret",
        domain="sandbox.local:8080",
        protocol="http",
        transport=transport,
        follow_redirects=True,
        event_hooks={"request": [request_hook], "response": [response_hook]},
    )
    adapter = SandboxesAdapterSync(cfg)

    response = adapter._httpx_client.get("/start")
    adapter._httpx_client.close()

    assert response.status_code == 204
    assert request_hosts == ["sandbox.local", "redirect.local"]
    assert response_statuses == [307, 204]
    assert transport.requests == [
        ("sandbox.local", "/v1/start", "hook-secret", None, None, None),
        ("redirect.local", "/final", None, None, None, None),
    ]


def test_sync_sse_client_follows_redirects_from_config() -> None:
    transport = _RedirectTransport()
    cfg = ConnectionConfigSync(
        protocol="http", transport=transport, follow_redirects=True
    )
    adapter = CommandsAdapterSync(
        cfg,
        SandboxEndpoint(endpoint="sandbox.local:8080"),
    )

    assert adapter._client.get_httpx_client() is adapter._httpx_client
    response = adapter._sse_client.get("http://sandbox.local:8080/start")
    adapter._httpx_client.close()
    adapter._sse_client.close()

    assert response.status_code == 204
    assert [response.status_code for response in response.history] == [307]
    assert transport.paths == ["/start", "/final"]
