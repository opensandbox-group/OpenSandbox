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

"""Unit tests for the bundled upstream-proxy mitm addon.

mitmproxy itself is stubbed the same way test_mitmscripts_system.py does it;
end-to-end chaining is covered by test_upstream_proxy_runtime.py with a real
mitmdump.
"""

from __future__ import annotations

import importlib.util
import os
import sys
import types
import unittest
from pathlib import Path
from typing import Any
from unittest import mock

UPSTREAM_ENV = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY"
UPSTREAM_AUTH_ENV = "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH"


class _Log:
    def __init__(self) -> None:
        self.messages: list[str] = []

    def warn(self, message: str) -> None:
        self.messages.append(message)

    def info(self, message: str) -> None:
        self.messages.append(message)

    def error(self, message: str) -> None:
        self.messages.append(message)


def _load_addon() -> Any:
    mitmproxy = types.ModuleType("mitmproxy")
    mitmproxy.ctx = types.SimpleNamespace(
        log=_Log(),
        options=types.SimpleNamespace(
            connection_strategy="lazy",
            ignore_hosts=[],
            tcp_hosts=[],
            udp_hosts=[],
        ),
    )
    mitmproxy.http = types.SimpleNamespace(HTTPFlow=object)

    sys.modules["mitmproxy"] = mitmproxy

    path = Path(__file__).parents[1] / "mitmscripts" / "upstream_proxy.py"
    spec = importlib.util.spec_from_file_location("opensandbox_egress_upstream_addon", path)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class _Server:
    def __init__(self, address: Any, transport_protocol: str = "tcp") -> None:
        self.address = address
        self.transport_protocol = transport_protocol
        self.via: Any = None
        self.error: str | None = None


class _Flow:
    def __init__(self, server: _Server | None = None) -> None:
        self.server_conn = server or _Server(("93.184.216.34", 443))
        self.request = types.SimpleNamespace(headers={})


def _env(**kwargs: str) -> Any:
    return mock.patch.dict(os.environ, kwargs, clear=False)


class UpstreamProxyConfigTest(unittest.TestCase):
    def test_disabled_when_env_unset(self) -> None:
        with mock.patch.dict(os.environ, {}, clear=True):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            flow = _Flow()
            addon.requestheaders(flow)
            self.assertIsNone(flow.server_conn.via)
            data = types.SimpleNamespace(server=_Server(("1.2.3.4", 443)))
            addon.server_connect(data)
            self.assertIsNone(data.server.error)

    def test_via_set_on_requestheaders(self) -> None:
        with _env(**{UPSTREAM_ENV: "https://proxy.example.com:8443"}):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            flow = _Flow()
            addon.requestheaders(flow)
            self.assertEqual(
                ("https", ("proxy.example.com", 8443)), flow.server_conn.via
            )

    def test_via_set_on_client_connect(self) -> None:
        with _env(**{UPSTREAM_ENV: "http://proxy.example.com:3128"}):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            flow = _Flow()
            addon.http_connect(flow)
            self.assertEqual(
                ("http", ("proxy.example.com", 3128)), flow.server_conn.via
            )

    def test_scheme_default_ports(self) -> None:
        for raw, expected in (
            ("http://proxy.example.com", 80),
            ("https://proxy.example.com", 443),
        ):
            with _env(**{UPSTREAM_ENV: raw}):
                addon = _load_addon()
                addon.load(types.SimpleNamespace())
                flow = _Flow()
                addon.requestheaders(flow)
                self.assertEqual(expected, flow.server_conn.via[1][1], raw)

    def test_invalid_proxy_url_fails_load(self) -> None:
        for raw in (
            "not-a-url",
            "socks5://proxy.example.com:1080",
            "http://",
            "http://user:pass@proxy.example.com:3128",
            "http://proxy.example.com:3128?x=1",
            "http://proxy.example.com:0",
            "http://proxy.example.com:70000",
        ):
            with _env(**{UPSTREAM_ENV: raw}):
                addon = _load_addon()
                with self.assertRaises(ValueError, msg=raw):
                    addon.load(types.SimpleNamespace())

    def test_auth_without_proxy_fails_load(self) -> None:
        with _env(**{UPSTREAM_AUTH_ENV: "Basic dXNlcjpwYXNz"}):
            addon = _load_addon()
            with self.assertRaises(ValueError):
                addon.load(types.SimpleNamespace())

    def test_proxy_authorization_injected_on_upstream_connect(self) -> None:
        with _env(
            **{
                UPSTREAM_ENV: "http://proxy.example.com:3128",
                UPSTREAM_AUTH_ENV: "Basic dXNlcjpwYXNz",
            }
        ):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            flow = _Flow()
            addon.http_connect_upstream(flow)
            self.assertEqual(
                "Basic dXNlcjpwYXNz",
                flow.request.headers["Proxy-Authorization"],
            )

    def test_no_auth_header_when_unset(self) -> None:
        with _env(**{UPSTREAM_ENV: "http://proxy.example.com:3128"}):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            flow = _Flow()
            addon.http_connect_upstream(flow)
            self.assertNotIn("Proxy-Authorization", flow.request.headers)

    def test_eager_strategy_fails_load(self) -> None:
        with _env(**{UPSTREAM_ENV: "http://proxy.example.com:3128"}):
            addon = _load_addon()
            addon.ctx.options.connection_strategy = "eager"
            with self.assertRaises(ValueError):
                addon.load(types.SimpleNamespace())

    def test_passthrough_lists_fail_load(self) -> None:
        for opt in ("ignore_hosts", "tcp_hosts", "udp_hosts"):
            with _env(**{UPSTREAM_ENV: "http://proxy.example.com:3128"}):
                addon = _load_addon()
                setattr(addon.ctx.options, opt, [".*"])
                with self.assertRaises(ValueError, msg=opt):
                    addon.load(types.SimpleNamespace())

    def test_tls_clienthello_sets_via(self) -> None:
        # Anchors via on the server placeholder at ClientHello time, before
        # requestheaders — keeps TLS-intercepted flows chained even if a server
        # connection were ever established early.
        with _env(**{UPSTREAM_ENV: "https://proxy.example.com:8443"}):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            server = _Server(("93.184.216.34", 443))
            data = types.SimpleNamespace(context=types.SimpleNamespace(server=server))
            addon.tls_clienthello(data)
            self.assertEqual(("https", ("proxy.example.com", 8443)), server.via)


class UpstreamProxyFailClosedTest(unittest.TestCase):
    """server_connect must refuse any direct dial while chaining is on."""

    def _addon(self, raw: str = "https://proxy.example.com:8443") -> Any:
        with _env(**{UPSTREAM_ENV: raw}):
            addon = _load_addon()
            addon.load(types.SimpleNamespace())
            return addon

    def test_proxy_connection_allowed(self) -> None:
        addon = self._addon()
        data = types.SimpleNamespace(
            server=_Server(("proxy.example.com", 8443))
        )
        addon.server_connect(data)
        self.assertIsNone(data.server.error)

    def test_direct_dial_refused(self) -> None:
        addon = self._addon()
        data = types.SimpleNamespace(server=_Server(("93.184.216.34", 443)))
        addon.server_connect(data)
        self.assertIsNotNone(data.server.error)

    def test_udp_dial_refused(self) -> None:
        addon = self._addon()
        data = types.SimpleNamespace(
            server=_Server(("proxy.example.com", 8443), transport_protocol="udp")
        )
        addon.server_connect(data)
        self.assertIsNotNone(data.server.error)

    def test_direct_dial_to_proxy_port_refused(self) -> None:
        # A different server on the same port must not be mistaken for the proxy.
        addon = self._addon()
        data = types.SimpleNamespace(server=_Server(("93.184.216.34", 8443)))
        addon.server_connect(data)
        self.assertIsNotNone(data.server.error)


if __name__ == "__main__":
    unittest.main()
