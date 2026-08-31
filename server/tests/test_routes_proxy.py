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

import asyncio
import gzip
import warnings
from types import SimpleNamespace
from typing import Any, cast

import httpx
import pytest
from fastapi.testclient import TestClient
from starlette.requests import ClientDisconnect
from starlette.types import Message
from websockets.exceptions import ConnectionClosedError, ConnectionClosedOK
from websockets.frames import Close, CloseCode
from websockets.typing import Origin

import opensandbox_server.api.proxy as proxy_api
from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import Endpoint
from opensandbox_server.middleware.auth import SANDBOX_API_KEY_HEADER
from opensandbox_server.services.constants import OPEN_SANDBOX_EGRESS_AUTH_HEADER, OPEN_SANDBOX_INGRESS_HEADER
from opensandbox_server.services.constants import OPEN_SANDBOX_SECURE_ACCESS_HEADER


class _FakeStreamingResponse:
    def __init__(
        self,
        status_code: int = 200,
        headers: dict | None = None,
        chunks: list[bytes] | None = None,
        raw_chunks: list[bytes] | None = None,
    ):
        self.status_code = status_code
        self.headers = httpx.Headers(headers or {})
        self._chunks = chunks or []
        self._raw_chunks = raw_chunks if raw_chunks is not None else self._chunks
        self.aclose_called = False
        self.aiter_bytes_called = False
        self.aiter_raw_called = False

    async def aiter_bytes(self):
        self.aiter_bytes_called = True
        for chunk in self._chunks:
            yield chunk

    async def aiter_raw(self):
        self.aiter_raw_called = True
        for chunk in self._raw_chunks:
            yield chunk

    async def aclose(self):
        await asyncio.sleep(0)
        self.aclose_called = True


class _BlockingStreamingResponse(_FakeStreamingResponse):
    def __init__(self) -> None:
        super().__init__()
        self.body_started = asyncio.Event()

    async def aiter_raw(self):
        self.body_started.set()
        await asyncio.Future()
        yield b"unreachable"


class _FakeAsyncClient:
    def __init__(self):
        self.built = None
        self.response = _FakeStreamingResponse()
        self.raise_connect_error = False
        self.raise_generic_error = False

    def build_request(
        self,
        method: str,
        url: str,
        headers: dict,
        content,
        params: str | None = None,
    ):
        self.built = {
            "method": method,
            "url": url,
            "params": params,
            "headers": headers,
            "content": content,
        }
        return self.built

    async def send(self, req, stream: bool = True):
        if self.raise_connect_error:
            raise httpx.ConnectError("connection refused")
        if self.raise_generic_error:
            raise RuntimeError("unexpected proxy error")
        return self.response


def _set_http_client(client: TestClient, fake_client: _FakeAsyncClient) -> None:
    cast(Any, client.app).state.http_client = fake_client


class _FakeBackendWebSocket:
    def __init__(self, message: str = "backend-ready", subprotocol: str | None = "claw.v1"):
        self.message = message
        self.subprotocol = subprotocol
        self.sent: list[str | bytes] = []
        self.close_calls: list[tuple[int, str]] = []
        self._delivered = False

    async def send(self, payload: str | bytes) -> None:
        self.sent.append(payload)

    async def recv(self) -> str:
        if not self._delivered:
            self._delivered = True
            return self.message
        await asyncio.Future()
        raise AssertionError("unreachable")

    async def close(self, code: int = 1000, reason: str = "") -> None:
        self.close_calls.append((code, reason))


class _FakeWebSocketConnector:
    def __init__(self, backend: _FakeBackendWebSocket):
        self.backend = backend
        self.calls: list[dict] = []

    def __call__(self, uri: str, **kwargs):
        self.calls.append({"uri": uri, **kwargs})
        backend = self.backend

        class _ContextManager:
            async def __aenter__(self):
                return backend

            async def __aexit__(self, exc_type, exc, tb):
                return False

        return _ContextManager()


class _ClosingBackendWebSocket:
    def __init__(
        self,
        close_exception: ConnectionClosedError | ConnectionClosedOK,
    ) -> None:
        self._messages: list[str | bytes] = ["backend-text", b"\x00\x01"]
        self._close_exception = close_exception

    async def recv(self) -> str | bytes:
        if self._messages:
            return self._messages.pop(0)
        raise self._close_exception


class _RecordingClientWebSocket:
    def __init__(self) -> None:
        self.text_messages: list[str] = []
        self.binary_messages: list[bytes] = []
        self.close_calls: list[tuple[int, str]] = []

    async def send_text(self, payload: str) -> None:
        self.text_messages.append(payload)

    async def send_bytes(self, payload: bytes) -> None:
        self.binary_messages.append(payload)

    async def close(self, code: int, reason: str = "") -> None:
        Close(code, reason).serialize()
        self.close_calls.append((code, reason))


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("backend_close", "expected_code", "expected_reason"),
    [
        (ConnectionClosedError(None, None), 1011, ""),
        (
            ConnectionClosedError(Close(CloseCode.NO_STATUS_RCVD, ""), None),
            1011,
            "",
        ),
        (ConnectionClosedOK(Close(1000, "normal"), None), 1000, "normal"),
        (ConnectionClosedError(Close(4001, "application close"), None), 4001, "application close"),
    ],
)
async def test_relay_backend_messages_maps_non_transmittable_close_code(
    backend_close: ConnectionClosedError | ConnectionClosedOK,
    expected_code: int,
    expected_reason: str,
) -> None:
    websocket = _RecordingClientWebSocket()
    backend = _ClosingBackendWebSocket(backend_close)
    cancelled: list[bool] = []
    cancel_scope = SimpleNamespace(cancel=lambda: cancelled.append(True))

    await asyncio.wait_for(
        proxy_api._relay_backend_messages(
            cast(Any, websocket),
            cast(Any, backend),
            cast(Any, cancel_scope),
        ),
        timeout=0.5,
    )

    assert websocket.text_messages == ["backend-text"]
    assert websocket.binary_messages == [b"\x00\x01"]
    assert websocket.close_calls == [(expected_code, expected_reason)]
    assert cancelled == [True]


@pytest.mark.parametrize(
    ("backend_code", "expected_code"),
    [
        (None, 1000),
        (999, 1011),
        (1000, 1000),
        (1004, 1011),
        (1005, 1011),
        (1006, 1011),
        (1011, 1011),
        (1015, 1011),
        (2999, 1011),
        (3000, 3000),
        (4001, 4001),
        (4999, 4999),
        (5000, 1011),
    ],
)
def test_client_websocket_close_code_maps_only_transmittable_codes(
    backend_code: int | None,
    expected_code: int,
) -> None:
    assert proxy_api._client_websocket_close_code(backend_code) == expected_code


def test_proxy_openapi_operation_ids_are_unique(client: TestClient) -> None:
    app = cast(Any, client.app)
    app.openapi_schema = None
    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        schema = app.openapi()

    proxy_paths = {
        "/sandboxes/{sandbox_id}/proxy/{port}",
        "/sandboxes/{sandbox_id}/proxy/{port}/{full_path}",
        "/v1/sandboxes/{sandbox_id}/proxy/{port}",
        "/v1/sandboxes/{sandbox_id}/proxy/{port}/{full_path}",
    }
    proxy_methods = {"get", "post", "put", "delete", "patch"}
    operation_ids = [
        schema["paths"][path][method]["operationId"]
        for path in proxy_paths
        for method in proxy_methods
    ]
    duplicate_warnings = [
        str(item.message)
        for item in caught
        if "Duplicate Operation ID" in str(item.message)
        and "proxy_sandbox_endpoint" in str(item.message)
    ]

    assert len(operation_ids) == 20
    assert len(set(operation_ids)) == len(operation_ids)
    assert duplicate_warnings == []


@pytest.mark.parametrize(
    "request_path",
    [
        "/sandboxes/sbx-123/proxy/44772",
        "/sandboxes/sbx-123/proxy/44772/nested/path",
        "/v1/sandboxes/sbx-123/proxy/44772",
        "/v1/sandboxes/sbx-123/proxy/44772/nested/path",
    ],
)
def test_proxy_method_not_allowed_lists_all_supported_methods(
    client: TestClient,
    auth_headers: dict,
    request_path: str,
) -> None:
    response = client.options(
        request_path,
        headers=auth_headers,
    )

    assert response.status_code == 405
    assert set(response.headers["allow"].split(", ")) == {
        "GET",
        "POST",
        "PUT",
        "DELETE",
        "PATCH",
    }


def test_proxy_forwards_filtered_headers_and_query(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=201,
        headers={"x-backend": "yes"},
        chunks=[b"proxy-ok"],
    )
    _set_http_client(client, fake_client)

    headers = {
        **auth_headers,
        "Authorization": "Bearer top-secret",
        "Cookie": "sid=secret",
        "Connection": "keep-alive, X-Hop-Temp",
        "Upgrade": "h2c",
        "Trailer": "X-Checksum",
        "X-Hop-Temp": "drop-me",
        "X-Trace": "trace-1",
        "Forwarded": "for=attacker;proto=https",
        "X-Forwarded-For": "203.0.113.99",
        "X-Forwarded-Host": "attacker.example",
        "X-Forwarded-Proto": "https",
        "X-Real-Ip": "203.0.113.99",
    }

    response = client.post(
        "/v1/sandboxes/sbx-123/proxy/44772/api/run",
        params={"q": "search"},
        headers=headers,
        content=b'{"hello":"world"}',
    )

    assert response.status_code == 201
    assert response.content == b"proxy-ok"
    assert response.headers.get("x-backend") == "yes"

    assert fake_client.built is not None
    assert fake_client.built["method"] == "POST"
    assert fake_client.built["url"] == "http://10.57.1.91:40109/api/run"
    assert fake_client.built["params"] == "q=search"
    forwarded_headers = fake_client.built["headers"]
    lowered_headers = {k.lower(): v for k, v in forwarded_headers.items()}
    assert "host" not in lowered_headers
    assert "connection" not in lowered_headers
    assert "upgrade" not in lowered_headers
    assert "trailer" not in lowered_headers
    assert "authorization" not in lowered_headers
    assert "cookie" not in lowered_headers
    assert SANDBOX_API_KEY_HEADER.lower() not in lowered_headers
    assert "x-hop-temp" not in lowered_headers
    assert lowered_headers.get("x-trace") == "trace-1"
    assert "forwarded" not in lowered_headers
    assert "x-real-ip" not in lowered_headers
    assert lowered_headers.get("x-forwarded-proto") == "http"
    assert lowered_headers.get("x-forwarded-host") != "attacker.example"
    assert lowered_headers.get("x-forwarded-for") != "203.0.113.99"
    assert fake_client.response.aclose_called is True


def test_proxy_honors_configured_resolve_internal_false(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
):
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is False
            assert use_proxy_host is True
            return Endpoint(endpoint="127.0.0.1:51999")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    monkeypatch.setattr(
        proxy_api,
        "get_config",
        lambda: SimpleNamespace(proxy=SimpleNamespace(resolve_internal=False)),
    )

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={},
        chunks=[b"ok"],
    )
    _set_http_client(client, fake_client)

    response = client.get("/v1/sandboxes/sbx-123/proxy/44772/status", headers=auth_headers)
    assert response.status_code == 200
    assert fake_client.built is not None
    assert fake_client.built["url"] == "http://127.0.0.1:51999/status"


def test_proxy_preserves_origin_date_and_filters_server_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(endpoint="backend.example:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    origin_date = "Wed, 21 Oct 2015 07:28:00 GMT"
    fake_client.response = _FakeStreamingResponse(
        headers={
            "Date": origin_date,
            "Server": "backend-server",
            "X-Backend": "yes",
        },
        chunks=[b"proxy-ok"],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers.get("x-backend") == "yes"
    assert response.headers.get("date") == origin_date
    assert "server" not in response.headers


@pytest.mark.parametrize(
    ("request_path", "location", "expected_location"),
    [
        (
            "/v1/sandboxes/sbx-123/proxy/44772/",
            "/login?next=%2F",
            "/v1/sandboxes/sbx-123/proxy/44772/login?next=%2F",
        ),
        (
            "/sandboxes/sbx-123/proxy/44772/",
            "/login?next=%2F",
            "/sandboxes/sbx-123/proxy/44772/login?next=%2F",
        ),
        (
            "/v1/sandboxes/sbx-123/proxy/44772/nested/page",
            "/login?next=%2F",
            "/v1/sandboxes/sbx-123/proxy/44772/login?next=%2F",
        ),
        ("/v1/sandboxes/sbx-123/proxy/44772/", "login?next=%2F", "login?next=%2F"),
        (
            "/v1/sandboxes/sbx-123/proxy/44772/",
            "https://example.com/login",
            "https://example.com/login",
        ),
        (
            "/v1/sandboxes/sbx-123/proxy/44772/",
            "//example.com/login",
            "//example.com/login",
        ),
        ("/v1/sandboxes/sbx-123/proxy/44772/", "?next=%2F", "?next=%2F"),
    ],
)
def test_proxy_rewrites_only_root_relative_redirects(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
    request_path: str,
    location: str,
    expected_location: str,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(endpoint="backend.example:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=302,
        headers={"Location": location},
    )
    _set_http_client(client, fake_client)

    response = client.get(
        request_path,
        headers=auth_headers,
        follow_redirects=False,
    )

    assert response.status_code == 302
    assert response.headers["location"] == expected_location


def test_proxy_rewrites_root_relative_redirect_with_server_eip_path(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(endpoint="backend.example:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    monkeypatch.setattr(
        lifecycle,
        "get_config",
        lambda: SimpleNamespace(
            server=SimpleNamespace(eip="sandbox.example.com/opensandbox/")
        ),
    )

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=302,
        headers={"Location": "/login?next=%2F"},
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/",
        headers=auth_headers,
        follow_redirects=False,
    )

    assert response.status_code == 302
    assert (
        response.headers["location"]
        == "/opensandbox/sandboxes/sbx-123/proxy/44772/login?next=%2F"
    )


def test_proxy_root_path_forwards_endpoint_headers_and_query(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:40109/base",
                headers={OPEN_SANDBOX_INGRESS_HEADER: "sbx-123-44772"},
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(chunks=[b"root-ok"])
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772",
        params={"q": "search"},
        headers={**auth_headers, "X-Trace": "trace-root"},
    )

    assert response.status_code == 200
    assert response.content == b"root-ok"
    assert fake_client.built is not None
    assert fake_client.built["url"] == "http://10.57.1.91:40109/base"
    assert fake_client.built["params"] == "q=search"
    lowered_headers = {
        key.lower(): value for key, value in fake_client.built["headers"].items()
    }
    assert lowered_headers["opensandbox-ingress-to"] == "sbx-123-44772"
    assert lowered_headers["x-trace"] == "trace-root"


def test_proxy_rejects_missing_secure_access_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Regression test: requests without the required secure-access token are rejected."""
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:40109/base",
                headers={
                    OPEN_SANDBOX_INGRESS_HEADER: "sbx-123-44772",
                    OPEN_SANDBOX_SECURE_ACCESS_HEADER: "secure-token",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(chunks=[b"root-ok"])
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772",
        params={"q": "search"},
        headers={**auth_headers, "X-Trace": "trace-root"},
    )

    assert response.status_code == 401
    assert fake_client.built is None  # request was never forwarded


def test_proxy_rejects_mismatched_secure_access_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Regression test: requests with a wrong secure-access token are rejected."""
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:40109/base",
                headers={
                    OPEN_SANDBOX_INGRESS_HEADER: "sbx-123-44772",
                    OPEN_SANDBOX_SECURE_ACCESS_HEADER: "server-side-token",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(chunks=[b"root-ok"])
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772",
        headers={
            **auth_headers,
            OPEN_SANDBOX_SECURE_ACCESS_HEADER: "wrong-token",
        },
    )

    assert response.status_code == 401
    assert fake_client.built is None  # request was never forwarded


def test_proxy_allows_valid_secure_access_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Valid secure-access token passes; header is stripped from forwarded request."""
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:40109/base",
                headers={
                    OPEN_SANDBOX_INGRESS_HEADER: "sbx-123-44772",
                    OPEN_SANDBOX_SECURE_ACCESS_HEADER: "server-side-token",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(chunks=[b"root-ok"])
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772",
        headers={
            **auth_headers,
            OPEN_SANDBOX_SECURE_ACCESS_HEADER: "server-side-token",
        },
    )

    assert response.status_code == 200
    assert fake_client.built is not None
    lowered_headers = {
        key.lower(): value for key, value in fake_client.built["headers"].items()
    }
    # Token is stripped from forwarded headers — sandbox app should not receive it
    assert OPEN_SANDBOX_SECURE_ACCESS_HEADER.lower() not in lowered_headers
    # Other endpoint headers are still forwarded
    assert lowered_headers["opensandbox-ingress-to"] == "sbx-123-44772"


def test_proxy_forwards_get_request_with_query_params(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Test that GET requests with query parameters are forwarded correctly.

    This test verifies the fix for issue #484 where GET requests with query
    parameters were failing with 400 MISSING_QUERY when using use_server_proxy.
    The query string should be passed via httpx params, not embedded in URL.
    """
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={"content-type": "application/json"},
        chunks=[b'[{"name":"file.txt","size":100}]'],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/files/search",
        params={"path": "/workspace"},
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert fake_client.built is not None
    assert fake_client.built["method"] == "GET"
    assert fake_client.built["url"] == "http://10.57.1.91:40109/files/search"
    assert fake_client.built["params"] == "path=%2Fworkspace"
    assert fake_client.built["content"] is None


def test_proxy_forwards_delete_request_with_body(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Test that DELETE requests with body payload are forwarded correctly.

    This verifies that DELETE requests with JSON/body payload are not
    incorrectly stripped when proxying.
    """
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={"content-type": "application/json"},
        chunks=[b'{"deleted":true}'],
    )
    _set_http_client(client, fake_client)

    response = client.request(
        "DELETE",
        "/v1/sandboxes/sbx-123/proxy/44772/resources",
        headers=auth_headers,
        content=b'{"id": "resource-123"}',
    )

    assert response.status_code == 200
    assert fake_client.built is not None
    assert fake_client.built["method"] == "DELETE"
    assert fake_client.built["content"] is not None


def test_proxy_filters_response_hop_by_hop_headers(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={
            "x-backend": "yes",
            "Connection": "keep-alive, X-Hop-Temp",
            "Keep-Alive": "timeout=5",
            "Trailer": "X-Checksum",
            "X-Hop-Temp": "drop-me",
        },
        chunks=[b"proxy-ok"],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/healthz",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.content == b"proxy-ok"
    assert response.headers.get("x-backend") == "yes"
    assert response.headers.get("connection") is None
    assert response.headers.get("keep-alive") is None
    assert response.headers.get("trailer") is None
    assert response.headers.get("x-hop-temp") is None


def test_proxy_streams_raw_body_for_content_encoded_response(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService)

    decoded_body = b"<html>vnc</html>"
    encoded_body = gzip.compress(decoded_body)
    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={
            "content-type": "text/html",
            "content-encoding": "gzip",
        },
        chunks=[decoded_body],
        raw_chunks=[encoded_body],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/8080/vnc/index.html",
        headers={**auth_headers, "Accept-Encoding": "gzip"},
    )

    assert response.status_code == 200
    assert response.headers.get("content-encoding") == "gzip"
    assert response.content == decoded_body
    assert fake_client.response.aiter_raw_called is True
    assert fake_client.response.aiter_bytes_called is False
    assert fake_client.response.aclose_called is True


def test_proxy_closes_backend_response_when_downstream_rejects_headers() -> None:
    """A disconnect before body iteration must not retain the backend connection."""

    async def run() -> None:
        backend_response = _FakeStreamingResponse()
        response = proxy_api._ProxyStreamingResponse(
            cast(httpx.Response, backend_response),
            status_code=200,
            headers={},
        )

        async def receive() -> Message:
            return {"type": "http.disconnect"}

        async def reject_response_start(message: Message) -> None:
            assert message["type"] == "http.response.start"
            raise ConnectionError("downstream disconnected")

        try:
            await response(
                {"type": "http", "asgi": {"spec_version": "2.4"}},
                receive,
                reject_response_start,
            )
        except ClientDisconnect:
            pass
        else:
            raise AssertionError("expected the downstream send to fail")

        assert backend_response.aclose_called is True

    asyncio.run(run())


def test_proxy_closes_backend_response_when_stream_is_cancelled() -> None:
    """Task cancellation must not interrupt returning the backend connection."""

    async def run() -> None:
        backend_response = _BlockingStreamingResponse()
        response = proxy_api._ProxyStreamingResponse(
            cast(httpx.Response, backend_response),
            status_code=200,
            headers={},
        )

        async def send(message: Message) -> None:
            return None

        async def receive() -> Message:
            return {"type": "http.disconnect"}

        task = asyncio.create_task(
            response(
                {"type": "http", "asgi": {"spec_version": "2.4"}},
                receive,
                send,
            )
        )
        await backend_response.body_started.wait()
        task.cancel()

        try:
            await task
        except asyncio.CancelledError:
            pass
        else:
            raise AssertionError("expected stream task cancellation")

        assert backend_response.aclose_called is True

    asyncio.run(run())


def test_proxy_rejects_websocket_upgrade(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    _set_http_client(client, _FakeAsyncClient())

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/ws",
        headers={**auth_headers, "Upgrade": "websocket"},
    )

    assert response.status_code == 400
    assert response.json()["message"] == "Websocket upgrade is not supported yet"


def test_proxy_rejects_websocket_upgrade_for_post_and_mixed_case_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    _set_http_client(client, _FakeAsyncClient())

    response = client.post(
        "/v1/sandboxes/sbx-123/proxy/44772/ws",
        headers={**auth_headers, "Upgrade": "WebSocket"},
        content=b"{}",
    )

    assert response.status_code == 400
    assert response.json()["message"] == "Websocket upgrade is not supported yet"


def test_proxy_websocket_relays_messages_and_forwards_safe_headers(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert sandbox_id == "sbx-123"
            assert port == 44772
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:40109/proxy/44772",
                headers={
                    OPEN_SANDBOX_INGRESS_HEADER: "sbx-123-44772",
                    "X-Forwarded-Proto": "https",
                    "X-Forwarded-For": "198.51.100.20",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    backend = _FakeBackendWebSocket()
    connector = _FakeWebSocketConnector(backend)
    monkeypatch.setattr(proxy_api.websockets, "connect", connector)

    with client.websocket_connect(
        "/v1/sandboxes/sbx-123/proxy/44772/ws?token=abc",
        headers={
            **auth_headers,
            "Authorization": "Bearer top-secret",
            "Cookie": "sid=secret",
            "Origin": "https://ui.example.com",
            "X-Trace": "trace-ws",
            "Forwarded": "for=attacker;proto=https",
            "X-Forwarded-For": "203.0.113.99",
            "X-Forwarded-Host": "attacker.example",
            "X-Forwarded-Proto": "https",
            "X-Real-Ip": "203.0.113.99",
        },
        subprotocols=["claw.v1"],
    ) as websocket:
        assert websocket.receive_text() == "backend-ready"
        websocket.send_text("client-ready")

    assert backend.sent == ["client-ready"]
    assert backend.close_calls[0][0] == 1000

    call = connector.calls[0]
    assert call["uri"] == "ws://10.57.1.91:40109/proxy/44772/ws?token=abc"
    assert call["origin"] == Origin("https://ui.example.com")
    assert call["subprotocols"] == ["claw.v1"]
    lowered_headers = {
        key.lower(): value for key, value in (call["additional_headers"] or {}).items()
    }
    assert "authorization" not in lowered_headers
    assert "cookie" not in lowered_headers
    assert "origin" not in lowered_headers
    assert "forwarded" not in lowered_headers
    assert "x-real-ip" not in lowered_headers
    assert lowered_headers["x-forwarded-proto"] == "http"
    assert lowered_headers["x-forwarded-host"] == "testserver"
    assert lowered_headers["x-forwarded-for"] == "testclient"
    assert lowered_headers["opensandbox-ingress-to"] == "sbx-123-44772"
    assert lowered_headers["x-trace"] == "trace-ws"


def test_proxy_maps_connect_error_to_502(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    fake_client = _FakeAsyncClient()
    fake_client.raise_connect_error = True
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/healthz",
        headers=auth_headers,
    )

    assert response.status_code == 502
    assert "Could not connect to the backend sandbox" in response.json()["message"]


def test_proxy_maps_unexpected_error_to_500(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    fake_client = _FakeAsyncClient()
    fake_client.raise_generic_error = True
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/44772/healthz",
        headers=auth_headers,
    )

    assert response.status_code == 500
    assert "An internal error occurred in the proxy" in response.json()["message"]


def test_proxy_forwards_18080_without_server_side_egress_auth_check(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert port == 18080
            assert resolve_internal is True
            return Endpoint(
                endpoint="10.57.1.91:18080",
                headers={OPEN_SANDBOX_EGRESS_AUTH_HEADER: "endpoint-token"},
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())
    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=401,
        headers={"content-type": "application/json"},
        chunks=[b'{"code":"UNAUTHORIZED"}'],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/18080/policy",
        headers=auth_headers,
    )

    assert response.status_code == 401
    assert response.json()["code"] == "UNAUTHORIZED"
    assert fake_client.built is not None
    assert fake_client.built["url"] == "http://10.57.1.91:18080/policy"
    lowered_headers = {k.lower(): v for k, v in fake_client.built["headers"].items()}
    assert OPEN_SANDBOX_EGRESS_AUTH_HEADER.lower() not in lowered_headers


def test_proxy_forwards_egress_auth_header_for_18080(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert port == 18080
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:18080")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=200,
        headers={"content-type": "application/json"},
        chunks=[b'{"status":"ok"}'],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/18080/policy",
        headers={**auth_headers, OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"},
    )

    assert response.status_code == 200
    assert fake_client.built is not None
    lowered_headers = {k.lower(): v for k, v in fake_client.built["headers"].items()}
    assert lowered_headers[OPEN_SANDBOX_EGRESS_AUTH_HEADER.lower()] == "egress-token"


def test_proxy_active_credential_vault_returns_sidecar_forbidden(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, resolve_internal: bool = False, use_proxy_host: bool = False) -> Endpoint:
            assert port == 18080
            assert resolve_internal is True
            return Endpoint(endpoint="10.57.1.91:18080")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    fake_client = _FakeAsyncClient()
    fake_client.response = _FakeStreamingResponse(
        status_code=403,
        headers={"content-type": "text/plain"},
        chunks=[b"forbidden\n"],
    )
    _set_http_client(client, fake_client)

    response = client.get(
        "/v1/sandboxes/sbx-123/proxy/18080/credential-vault/_active",
        headers={**auth_headers, OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"},
    )

    assert response.status_code == 403
    assert response.content == b"forbidden\n"
    assert fake_client.built is not None
    assert fake_client.built["url"] == "http://10.57.1.91:18080/credential-vault/_active"
