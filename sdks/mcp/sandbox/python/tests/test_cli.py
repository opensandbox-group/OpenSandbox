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

import sys
from typing import Any

import pytest


class _FakeServer:
    def __init__(self) -> None:
        self.calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []

    def run(self, *args: Any, **kwargs: Any) -> None:
        self.calls.append((args, kwargs))


@pytest.fixture
def fake_server(monkeypatch: pytest.MonkeyPatch) -> _FakeServer:
    from opensandbox_mcp import __main__

    server = _FakeServer()
    monkeypatch.setattr(
        __main__, "create_server", lambda connection_config=None: server
    )
    return server


def test_streamable_http_uses_mcp_defaults(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        ["opensandbox-mcp", "--transport", "streamable-http"],
    )

    __main__.main()

    assert fake_server.calls == [
        (
            (),
            {
                "transport": "streamable-http",
                "host": "127.0.0.1",
                "port": 8000,
            },
        )
    ]


def test_streamable_http_forwards_host_and_port(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--transport",
            "streamable-http",
            "--host",
            "0.0.0.0",
            "--port",
            "9000",
        ],
    )

    __main__.main()

    assert fake_server.calls == [
        (
            (),
            {
                "transport": "streamable-http",
                "host": "0.0.0.0",
                "port": 9000,
            },
        )
    ]


def test_stdio_does_not_pass_http_bind_options(
    monkeypatch: pytest.MonkeyPatch, fake_server: _FakeServer
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--host",
            "0.0.0.0",
            "--port",
            "9000",
        ],
    )

    __main__.main()

    assert fake_server.calls == [
        ((), {"transport": "stdio"}),
    ]


def test_oauth_jwt_wires_hardened_http_app(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox_mcp import __main__
    from opensandbox_mcp.auth import OAuthConfig

    captured: dict[str, Any] = {}
    sentinel_app = object()

    class _FakeSettings:
        log_level = "INFO"

    class _FakeOAuthServer:
        settings = _FakeSettings()

    def _fake_create_server(*, connection_config=None, oauth_config=None):
        captured["connection_config"] = connection_config
        captured["oauth_config"] = oauth_config
        return _FakeOAuthServer()

    def _fake_create_app(mcp, *, host="127.0.0.1", oauth_config=None):
        captured["app_host"] = host
        captured["app_oauth_config"] = oauth_config
        return sentinel_app

    def _fake_serve(app, *, host, port, log_level, access_log, ws):
        captured["served"] = (app, host, port, log_level, access_log, ws)

    monkeypatch.setattr(__main__, "create_server", _fake_create_server)
    monkeypatch.setattr(__main__, "create_streamable_http_app", _fake_create_app)
    monkeypatch.setattr("uvicorn.run", _fake_serve)
    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--transport",
            "streamable-http",
            "--host",
            "127.0.0.1",
            "--port",
            "8443",
            "--oauth-mode",
            "jwt",
            "--oauth-issuer-url",
            "https://idp.example.com",
            "--oauth-resource-url",
            "https://mcp.example.com/mcp",
            "--oauth-required-scope",
            "opensandbox:mcp",
        ],
    )

    __main__.main()

    assert isinstance(captured["oauth_config"], OAuthConfig)
    assert captured["oauth_config"].issuer_url == "https://idp.example.com"
    assert captured["oauth_config"].resource_url == "https://mcp.example.com/mcp"
    assert captured["oauth_config"].required_scopes == ("opensandbox:mcp",)
    assert captured["served"] == (
        sentinel_app,
        "127.0.0.1",
        8443,
        "info",
        False,
        "none",
    )


def test_cli_fails_closed_on_invalid_oauth_config(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--transport",
            "stdio",
            "--oauth-mode",
            "jwt",
            "--oauth-issuer-url",
            "https://idp.example.com",
            "--oauth-resource-url",
            "https://mcp.example.com/mcp",
            "--oauth-required-scope",
            "opensandbox:mcp",
        ],
    )

    with pytest.raises(SystemExit) as exc_info:
        __main__.main()

    assert exc_info.value.code == 2
    assert "streamable-http" in capsys.readouterr().err


def test_cli_fails_closed_on_partial_oauth_flags_with_mode_none(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
) -> None:
    from opensandbox_mcp import __main__

    monkeypatch.setattr(
        sys,
        "argv",
        [
            "opensandbox-mcp",
            "--transport",
            "streamable-http",
            "--oauth-issuer-url",
            "https://idp.example.com",
        ],
    )

    with pytest.raises(SystemExit) as exc_info:
        __main__.main()

    assert exc_info.value.code == 2
    assert "OAuth mode is 'none'" in capsys.readouterr().err


def test_proxy_rollback_clears_oauth_settings_and_keeps_backend_connection(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    fake_server: _FakeServer,
) -> None:
    from opensandbox_mcp import __main__
    from opensandbox_mcp.auth import (
        ENV_ISSUER_URL,
        ENV_MODE,
        ENV_REQUIRED_SCOPES,
        ENV_RESOURCE_URL,
    )

    monkeypatch.delenv(ENV_MODE, raising=False)
    oauth_env = {
        ENV_ISSUER_URL: "https://idp.example.com",
        ENV_RESOURCE_URL: "https://mcp.example.com/mcp",
        ENV_REQUIRED_SCOPES: "opensandbox:mcp",
    }
    for name, value in oauth_env.items():
        monkeypatch.setenv(name, value)
    monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "test-proxy-backend-key")
    monkeypatch.setenv("OPEN_SANDBOX_DOMAIN", "sandbox.internal:8080")
    monkeypatch.setattr(
        sys, "argv", ["opensandbox-mcp", "--transport", "streamable-http"]
    )
    backend_configs = []

    def create_server(connection_config):
        backend_configs.append(connection_config)
        return fake_server

    monkeypatch.setattr(__main__, "create_server", create_server)
    with pytest.raises(SystemExit) as exc_info:
        __main__.main()
    assert exc_info.value.code == 2
    assert "OAuth mode is 'none'" in capsys.readouterr().err
    assert not backend_configs

    for name in oauth_env:
        monkeypatch.delenv(name)
    __main__.main()

    assert fake_server.calls == [
        ((), {"transport": "streamable-http", "host": "127.0.0.1", "port": 8000})
    ]
    assert backend_configs[0].get_api_key() == "test-proxy-backend-key"
    assert backend_configs[0].get_domain() == "sandbox.internal:8080"
