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

from __future__ import annotations

import asyncio
import threading

import httpx
import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient

from app.config import Settings, get_settings
from app.session import admin_session, encode_session, tenant_session
from app.shell import (
    ShellError,
    execd_http_base,
    execd_protocol,
    execd_request_headers,
    execd_ws_url,
    is_server_proxy_endpoint,
    server_proxy_endpoint,
    shell_endpoint_mode,
)


def test_direct_endpoint_does_not_receive_the_tenant_api_key():
    headers = execd_request_headers(
        "10.0.0.8:44772",
        {"X-EXECD-ACCESS-TOKEN": "execd-tok"},
        "tenant-key",
    )

    assert headers == {"X-EXECD-ACCESS-TOKEN": "execd-tok"}
    assert "OPEN-SANDBOX-API-KEY" not in headers


def test_server_proxy_endpoint_keeps_the_lifecycle_api_key():
    endpoint = "sandbox.example/v1/sandboxes/sbx-1/proxy/44772"
    assert is_server_proxy_endpoint(endpoint)
    headers = execd_request_headers(
        endpoint,
        {"X-EXECD-ACCESS-TOKEN": "execd-tok"},
        "tenant-key",
    )

    assert headers["OPEN-SANDBOX-API-KEY"] == "tenant-key"
    assert headers["X-EXECD-ACCESS-TOKEN"] == "execd-tok"


def test_execd_urls_follow_protocol_unless_the_endpoint_has_a_scheme():
    assert execd_http_base("10.0.0.8:44772", "http") == "http://10.0.0.8:44772"
    assert execd_http_base("edge.example/proxy/44772", "https") == "https://edge.example/proxy/44772"
    assert execd_http_base("https://edge.example/proxy/44772", "http") == "https://edge.example/proxy/44772"
    assert execd_ws_url("http://10.0.0.8:44772", "pty-1") == "ws://10.0.0.8:44772/pty/pty-1/ws"
    assert execd_ws_url("https://edge.example/proxy/44772", "pty-1") == (
        "wss://edge.example/proxy/44772/pty/pty-1/ws"
    )


def test_shell_endpoint_mode_defaults_to_the_lifecycle_server_proxy():
    settings = Settings(
        lifecycle_api_base="http://lifecycle.test/v1/",
        tenants_toml_path="/tmp/tenants.toml",
        bff_session_secret="x" * 32,
        bff_admin_token="admin",
    )

    assert shell_endpoint_mode(settings) == "server"
    assert server_proxy_endpoint(settings, "sbx-1") == (
        "http://lifecycle.test/v1/sandboxes/sbx-1/proxy/44772"
    )
    assert shell_endpoint_mode(settings.model_copy(update={"bff_shell_endpoint_mode": "gateway"})) == "gateway"
    with pytest.raises(ShellError, match="server or gateway"):
        shell_endpoint_mode(settings.model_copy(update={"bff_shell_endpoint_mode": "pod"}))


def test_execd_protocol_override_and_lifecycle_default():
    http_settings = Settings(
        lifecycle_api_base="http://lifecycle.test/v1",
        tenants_toml_path="/tmp/tenants.toml",
        bff_session_secret="x" * 32,
        bff_admin_token="admin",
    )
    https_settings = http_settings.model_copy(update={"lifecycle_api_base": "https://lifecycle.test/v1"})
    override = http_settings.model_copy(update={"bff_execd_protocol": "https"})

    assert execd_protocol(http_settings) == "http"
    assert execd_protocol(https_settings) == "https"
    assert execd_protocol(override) == "https"


class FakeExecdSocket:
    def __init__(self) -> None:
        self.sent: list[bytes | str] = []
        self.got_input = threading.Event()
        self._announced = False

    async def send(self, data: bytes | str) -> None:
        self.sent.append(data)
        self.got_input.set()

    async def __aenter__(self) -> FakeExecdSocket:
        return self

    async def __aexit__(self, exc_type, exc, tb) -> bool:
        return False

    def __aiter__(self) -> FakeExecdSocket:
        return self

    async def __anext__(self) -> bytes:
        if not self._announced:
            self._announced = True
            return b"\x01hi"
        await asyncio.sleep(3600)
        raise AssertionError("idle wait should be cancelled")


@pytest.fixture
def shell_client(tmp_path, monkeypatch: pytest.MonkeyPatch):
    tenants = tmp_path / "tenants.toml"
    tenants.write_text(
        '[[tenants]]\nname = "acme"\nnamespace = "acme-ns"\napi_keys = ["tenant-key"]\n',
        encoding="utf-8",
    )
    monkeypatch.setenv("TENANTS_TOML_PATH", str(tenants))
    monkeypatch.setenv("BFF_SESSION_SECRET", "x" * 32)
    monkeypatch.setenv("BFF_ADMIN_TOKEN", "admin-token")
    monkeypatch.setenv("LIFECYCLE_API_BASE", "http://lifecycle.test/v1")
    monkeypatch.setenv("BFF_EXECD_PROTOCOL", "http")
    get_settings.cache_clear()

    created: list[tuple[str, dict[str, str]]] = []
    deleted: list[str] = []
    sockets: list[FakeExecdSocket] = []

    async def create_pty_session(http_base: str, headers: dict[str, str]) -> str:
        created.append((http_base, headers))
        return "pty-1"

    async def delete_pty_session(http_base: str, session_id: str, headers: dict[str, str]) -> None:
        deleted.append(session_id)

    def connect_execd(url: str, headers: dict[str, str]) -> FakeExecdSocket:
        socket = FakeExecdSocket()
        sockets.append(socket)
        connect_execd.urls.append(url)
        connect_execd.headers.append(headers)
        return socket

    connect_execd.urls = []
    connect_execd.headers = []

    async def lifecycle_request(self, api_key: str, method: str, path: str, **kwargs):
        lifecycle_request.calls.append((api_key, method, path))
        return httpx.Response(
            200,
            json={
                "endpoint": "10.0.0.8:44772",
                "headers": {"X-EXECD-ACCESS-TOKEN": "execd-tok"},
            },
        )

    lifecycle_request.calls = []

    monkeypatch.setattr("app.shell.create_pty_session", create_pty_session)
    monkeypatch.setattr("app.shell.delete_pty_session", delete_pty_session)
    monkeypatch.setattr("app.shell.connect_execd", connect_execd)
    monkeypatch.setattr("app.lifecycle.LifecycleClient.request", lifecycle_request)

    from app.routes.shell import router

    app = FastAPI()
    api = FastAPI()
    api.include_router(router)
    app.mount("/api", api)
    client = TestClient(app)
    client.created = created
    client.deleted = deleted
    client.sockets = sockets
    client.connect_urls = connect_execd.urls
    client.connect_headers = connect_execd.headers
    client.lifecycle_calls = lifecycle_request.calls
    yield client
    get_settings.cache_clear()


def _cookie(payload: dict) -> dict[str, str]:
    token = encode_session(get_settings(), payload)
    return {get_settings().bff_session_cookie_name: token}


def test_shell_requires_a_session(shell_client: TestClient):
    with shell_client.websocket_connect("/api/sandboxes/sbx-1/shell/ws") as ws:
        message = ws.receive_json()

    assert message["code"] == "UNAUTHORIZED"
    assert shell_client.lifecycle_calls == []


def test_tenant_shell_bridges_frames_and_deletes_the_pty(shell_client: TestClient):
    cookies = _cookie(tenant_session("acme", "acme-ns"))
    with shell_client.websocket_connect("/api/sandboxes/sbx-1/shell/ws", cookies=cookies) as ws:
        assert ws.receive_bytes() == b"\x01hi"
        ws.send_bytes(b"\x00ls\n")
        assert shell_client.sockets[0].got_input.wait(2)

    assert shell_client.lifecycle_calls == [("tenant-key", "GET", "/sandboxes/sbx-1/endpoints/44772")]
    assert shell_client.created == [
        (
            "http://lifecycle.test/v1/sandboxes/sbx-1/proxy/44772",
            {"X-EXECD-ACCESS-TOKEN": "execd-tok", "OPEN-SANDBOX-API-KEY": "tenant-key"},
        ),
    ]
    assert shell_client.connect_headers[0]["OPEN-SANDBOX-API-KEY"] == "tenant-key"
    assert shell_client.connect_urls == [
        "ws://lifecycle.test/v1/sandboxes/sbx-1/proxy/44772/pty/pty-1/ws"
    ]
    assert shell_client.sockets[0].sent == [b"\x00ls\n"]
    assert shell_client.deleted == ["pty-1"]


def test_gateway_mode_dials_the_ingress_endpoint(shell_client: TestClient, monkeypatch: pytest.MonkeyPatch):
    monkeypatch.setenv("BFF_SHELL_ENDPOINT_MODE", "gateway")
    get_settings.cache_clear()
    cookies = _cookie(tenant_session("acme", "acme-ns"))
    with shell_client.websocket_connect("/api/sandboxes/sbx-1/shell/ws", cookies=cookies) as ws:
        assert ws.receive_bytes() == b"\x01hi"

    assert shell_client.created[-1][0] == "http://10.0.0.8:44772"
    assert "OPEN-SANDBOX-API-KEY" not in shell_client.created[-1][1]
    assert shell_client.connect_urls[-1] == "ws://10.0.0.8:44772/pty/pty-1/ws"


def test_admin_shell_uses_the_selected_tenant_key(shell_client: TestClient):
    cookies = _cookie(admin_session())
    with shell_client.websocket_connect(
        "/api/admin/sandboxes/sbx-1/shell/ws?tenant=acme",
        cookies=cookies,
    ) as ws:
        assert ws.receive_bytes() == b"\x01hi"

    assert shell_client.lifecycle_calls[0][0] == "tenant-key"
    assert shell_client.deleted == ["pty-1"]


def test_admin_shell_requires_tenant(shell_client: TestClient):
    cookies = _cookie(admin_session())
    with shell_client.websocket_connect("/api/admin/sandboxes/sbx-1/shell/ws", cookies=cookies) as ws:
        message = ws.receive_json()

    assert message["code"] == "TENANT_REQUIRED"
    assert shell_client.lifecycle_calls == []
