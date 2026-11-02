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

"""OSEP-0025 T3b: exchange binding resolve integration in the system addon.

Covers the addon side of the `_resolve` contract: injection via
delete-then-set, fail-closed matrix (407 buffered / kill streamed, never
pass-through), per-request redaction + by-name suppression, static-path
byte-for-byte behavior inside exchange vaults, the concurrency semaphore,
event-loop responsiveness during slow resolves, and pull-mode H2.
"""

from __future__ import annotations

import asyncio
import importlib.util
import json
import os
import sys
import threading
import time
import types
import unittest
from pathlib import Path
from typing import Any
from unittest import mock
from urllib.parse import quote, quote_plus

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"

HOST = "code.example.com"
BINDING = "internal-api"
SECRET = 'tok"en secret'
SECOND_SECRET = "sig-1"


class _Log:
    def __init__(self) -> None:
        self.messages: list[str] = []

    def warn(self, message: str) -> None:
        self.messages.append(message)

    def info(self, message: str) -> None:
        self.messages.append(message)


class _Headers:
    def __init__(self, values: dict[str, str] | None = None) -> None:
        self._values: dict[str, str] = dict(values or {})

    def get(self, name: str, default: str = "") -> str:
        for key, value in self._values.items():
            if key.lower() == name.lower():
                return value
        return default

    def items(self) -> list[tuple[str, str]]:
        return list(self._values.items())

    def __getitem__(self, name: str) -> str:
        for key, value in self._values.items():
            if key.lower() == name.lower():
                return value
        raise KeyError(name)

    def __setitem__(self, name: str, value: str) -> None:
        # mitmproxy Headers replaces every same-name field.
        for key in list(self._values):
            if key.lower() == name.lower():
                del self._values[key]
        self._values[name] = value

    def __contains__(self, name: str) -> bool:
        return any(key.lower() == name.lower() for key in self._values)

    def __delitem__(self, name: str) -> None:
        matched = [key for key in self._values if key.lower() == name.lower()]
        if not matched:
            raise KeyError(name)
        for key in matched:
            del self._values[key]


class _Request:
    def __init__(self) -> None:
        self.pretty_host = HOST
        self.host = HOST
        self.port = 443
        self.scheme = "https"
        self.method = "GET"
        self.path = "/api/v8/projects"
        self.headers = _Headers()
        self.raw_content: bytes | None = None
        self.content = b""
        self.stream = False
        self.http_version = "HTTP/1.1"


class _ClientConn:
    def __init__(self, sni: str | None = HOST) -> None:
        self.sni = sni
        self.peername = ("10.1.2.3", 40000)


class _Response:
    def __init__(self) -> None:
        self.headers = _Headers({"content-type": "application/json"})
        self.stream = False
        self.status_code = 200


class _Flow:
    def __init__(self, sni: str | None = HOST) -> None:
        self.request = _Request()
        self.response = None
        self.metadata: dict[str, Any] = {}
        self.client_conn = _ClientConn(sni)
        self.error = None
        self.live = True
        self.killable = True
        self.killed = False

    def kill(self) -> None:
        self.killed = True
        self.error = "Killed"
        self.live = False
        self.killable = False


def _load_system_module() -> Any:
    mitmproxy = types.ModuleType("mitmproxy")
    mitmproxy.ctx = types.SimpleNamespace(
        log=_Log(), options=types.SimpleNamespace(ignore_hosts=[], ssl_insecure=False)
    )

    def _make_response(
        status: int, body: bytes = b"", headers: dict | None = None
    ) -> _Response:
        response = _Response()
        response.status_code = status
        if headers:
            response.headers = _Headers(headers)
        return response

    mitmproxy.http = types.SimpleNamespace(
        HTTPFlow=object, Response=types.SimpleNamespace(make=_make_response)
    )
    mitmproxy_tls = types.ModuleType("mitmproxy.tls")
    mitmproxy_tls.ClientHelloData = object
    with mock.patch.dict(
        sys.modules, {"mitmproxy": mitmproxy, "mitmproxy.tls": mitmproxy_tls}
    ):
        spec = importlib.util.spec_from_file_location(
            "opensandbox_egress_system_resolve_test", MITMSCRIPTS / "system.py"
        )
        assert spec is not None and spec.loader is not None
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


def _exchange_binding(host: str = HOST) -> dict[str, Any]:
    return {
        "name": BINDING,
        "match": {
            "schemes": ["https"],
            "hosts": [host],
            "methods": ["GET"],
            "paths": ["/api/v8/*"],
        },
        "headers": [],
        "substitutions": [],
        "exchange": {"credential": "spi-signer"},
    }


def _static_binding() -> dict[str, Any]:
    return {
        "name": "gitlab-api",
        "match": {
            "schemes": ["https"],
            "hosts": [HOST],
            "methods": ["GET"],
            "paths": ["/api/v8/*"],
        },
        "headers": [{"name": "Private-Token", "value": "static-secret"}],
        "substitutions": [],
    }


def _exchange_vault(system: Any, *, static: bool = False) -> Any:
    bindings = [_exchange_binding()]
    if static:
        bindings = [_static_binding(), _exchange_binding("other.example.com")]
    return system.ActiveVault(12, bindings, ["static-secret"] if static else [])


def _parsed_decision(
    *,
    revision: int = 12,
    binding: str = BINDING,
    headers: list[tuple[str, str]] | None = None,
    redactions: list[str] | None = None,
    suppress: set[str] | None = None,
) -> dict[str, Any]:
    if headers is None:
        headers = [("authorization", SECRET), ("x-signature", SECOND_SECRET)]
    return {
        "revision": revision,
        "binding": binding,
        "headers": [{"name": name, "value": value} for name, value in headers],
        "redactions": redactions if redactions is not None else [SECRET, SECOND_SECRET],
        "suppressHeaders": sorted(suppress)
        if suppress is not None
        else ["authorization", "x-signature"],
    }


class _ScriptedResponse:
    def __init__(self, status: int, body: bytes = b"") -> None:
        self.status = status
        self._body = body
        self.amt: int | None = None

    def read(self, amt: int | None = None) -> bytes:
        # Mirror http.client bounded reads: never buffer beyond amt.
        self.amt = amt
        return self._body if amt is None else self._body[:amt]

    def getheader(self, name: str) -> str | None:
        return None


class ExchangeResolveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.system = _load_system_module()
        self.log: _Log = self.system.ctx.log
        env = mock.patch.dict(os.environ)
        env.start()
        self.addCleanup(env.stop)
        for name in (
            self.system.CREDENTIAL_RESOLVER_TIMEOUT_ENV,
            self.system.CREDENTIAL_RESOLVER_MAX_CONCURRENCY_ENV,
            self.system.CREDENTIAL_PROXY_SOCKET_ENV,
            "OPENSANDBOX_EGRESS_PROFILE",
        ):
            os.environ.pop(name, None)

    def _install_vault(self, *, static: bool = False, host: str = HOST) -> Any:
        if static:
            vault = _exchange_vault(self.system, static=True)
        else:
            vault = self.system.ActiveVault(12, [_exchange_binding(host)], [])
        self.system._load_active_vault = lambda _client_ip=None: vault
        return vault

    def _install_transport(self, behavior: Any) -> list[tuple[Any, ...]]:
        calls: list[tuple[Any, ...]] = []
        lock = threading.Lock()

        def transport(socket_path: str, timeout: float, path: str, payload: dict):
            with lock:
                calls.append((socket_path, timeout, path, payload))
            return behavior(payload)

        self.system._resolve_exchange_http = transport
        return calls

    def _decision_transport(self) -> list[tuple[Any, ...]]:
        return self._install_transport(lambda payload: _parsed_decision())

    # -- happy path -------------------------------------------------------

    def test_happy_path_injects_resolved_headers(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        flow = _Flow()
        flow.request.headers["Authorization"] = "client-side-token"

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual(1, len(calls))
        socket_path, timeout, path, payload = calls[0]
        self.assertEqual(
            self.system.DEFAULT_CREDENTIAL_PROXY_SOCKET, socket_path
        )
        self.assertEqual(2.25, timeout)
        self.assertEqual(self.system.CREDENTIAL_RESOLVE_PATH, path)
        self.assertEqual(
            {
                "binding": BINDING,
                "sni": HOST,
                "request": {
                    "scheme": "https",
                    "host": HOST,
                    "port": 443,
                    "method": "GET",
                    "path": "/api/v8/projects",
                },
            },
            payload,
        )
        # Delete-then-set: the resolved value replaced the client's own header.
        self.assertEqual(SECRET, flow.request.headers["authorization"])
        self.assertEqual(SECOND_SECRET, flow.request.headers["x-signature"])
        self.assertFalse(flow.killed)
        self.assertIsNone(flow.response)
        self.assertNotIn(self.system.FLOW_REJECTION_KEY, flow.metadata)
        self.assertEqual(_exchange_binding(), flow.metadata[self.system.FLOW_BINDING_KEY])
        self.assertEqual(
            ["authorization", "x-signature"],
            flow.metadata[self.system.FLOW_VAULT_SUPPRESS_KEY],
        )
        redactions = flow.metadata[self.system.FLOW_REDACTIONS_KEY]
        self.assertIn(SECRET, redactions)
        self.assertIn(quote(SECRET, safe=""), redactions)
        self.assertIn(quote_plus(SECRET, safe=""), redactions)
        self.assertIn(json.dumps(SECRET)[1:-1], redactions)
        self.assertTrue(
            any(
                "exchange=1" in message and f"binding={BINDING}" in message
                for message in self.log.messages
            )
        )
        self.assertFalse(any(SECRET in message for message in self.log.messages))

    def test_resolve_request_path_keeps_query(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        flow = _Flow()
        flow.request.path = "/api/v8/projects?cursor=zzz"

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual(
            "/api/v8/projects?cursor=zzz", calls[0][3]["request"]["path"]
        )

    def test_fast_sandbox_mode_appends_client_ip(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        self.system._set_fast_sandbox_mode(True)
        self.addCleanup(self.system._set_fast_sandbox_mode, False)
        flow = _Flow()

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual(
            f"{self.system.CREDENTIAL_RESOLVE_PATH}?clientIp=10.1.2.3", calls[0][2]
        )

    # -- fail-closed matrix -------------------------------------------------

    def test_fail_closed_matrix_buffered(self) -> None:
        self._install_vault()
        cases = {
            "transport-down": ConnectionRefusedError("no socket"),
            "go-504-timeout": self.system.CredentialResolveError("http_504"),
            "go-503-unavailable": self.system.CredentialResolveError("http_503"),
            "go-409-invalidated": self.system.CredentialResolveError("http_409"),
            "invalid-response": self.system.CredentialResolveError("invalid_response"),
            "unexpected": RuntimeError("boom"),
        }
        for name, failure in cases.items():
            with self.subTest(name=name):
                calls = self._install_transport(lambda payload: (_ for _ in ()).throw(failure))
                flow = _Flow()
                flow.request.headers["Content-Length"] = "0"

                asyncio.run(self.system.requestheaders(flow))

                self.assertEqual(1, len(calls))
                self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
                self.assertEqual(407, flow.response.status_code)
                self.assertFalse(flow.killed)
                # No pass-through: nothing was injected.
                self.assertNotIn("authorization", flow.request.headers)
                self.assertNotIn(self.system.FLOW_BINDING_KEY, flow.metadata)
                self.assertTrue(
                    any(
                        "exchange resolve failed" in message
                        and f"binding={BINDING}" in message
                        for message in self.log.messages
                    )
                )
                self.assertFalse(any(SECRET in message for message in self.log.messages))

    def test_fail_closed_matrix_streamed_kills(self) -> None:
        self._install_vault()
        for name, configure in (
            ("body-stream-flag", lambda flow: setattr(flow.request, "stream", True)),
            ("transfer-encoding", lambda flow: flow.request.headers.__setitem__(
                "transfer-encoding", "chunked"
            )),
            ("h2-without-content-length", lambda flow: setattr(
                flow.request, "http_version", "HTTP/2.0"
            )),
        ):
            with self.subTest(name=name):
                self._install_transport(
                    lambda payload: (_ for _ in ()).throw(
                        self.system.CredentialResolveError("http_504")
                    )
                )
                flow = _Flow()
                configure(flow)

                asyncio.run(self.system.requestheaders(flow))

                self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
                self.assertTrue(flow.killed)
                self.assertIsNone(flow.response)
                self.assertNotIn("authorization", flow.request.headers)

    def test_failure_does_not_pollute_next_request(self) -> None:
        self._install_vault()
        outcomes: list[Any] = [
            self.system.CredentialResolveError("transport"),
            _parsed_decision(),
        ]
        calls = self._install_transport(
            lambda payload: (_ for _ in ()).throw(outcomes.pop(0))
            if isinstance(outcomes[0], Exception)
            else outcomes.pop(0)
        )
        first = _Flow()
        first.request.headers["Content-Length"] = "0"
        second = _Flow()
        second.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(first))
        asyncio.run(self.system.requestheaders(second))

        self.assertEqual(2, len(calls))
        self.assertEqual(407, first.response.status_code)
        self.assertIsNone(second.response)
        self.assertEqual(SECRET, second.request.headers["authorization"])

    def test_missing_sni_fails_closed_without_rpc(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        flow = _Flow(sni=None)
        flow.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual([], calls)
        self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
        self.assertEqual(407, flow.response.status_code)

    def test_ambiguous_paths_rejected_before_rpc(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        for name, path in (
            ("dot-segments", "/api/v8/../v9/projects"),
            ("encoded-boundary-crossing", "/api/v8/%2e%2e/v9/projects"),
        ):
            with self.subTest(name=name):
                flow = _Flow()
                flow.request.path = path
                flow.request.headers["Content-Length"] = "0"

                asyncio.run(self.system.requestheaders(flow))

                self.assertEqual([], calls)
                self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
                self.assertEqual(403, flow.response.status_code)

    # -- redaction and suppression ------------------------------------------

    def test_response_suppression_and_by_value_redaction(self) -> None:
        self._install_vault()
        # Redactions deliberately miss every variant; the addon must
        # re-derive them (R9).
        self._install_transport(
            lambda payload: _parsed_decision(redactions=[SECRET, SECOND_SECRET])
        )
        flow = _Flow()
        asyncio.run(self.system.requestheaders(flow))
        flow.response = _Response()
        flow.response.headers = _Headers(
            {
                "content-type": "application/json",
                "Authorization": SECRET,
                "x-signature": SECOND_SECRET,
                "x-echo": f"wrapped {SECRET}",
                "x-url-echo": quote(SECRET, safe=""),
                "x-form-echo": quote_plus(SECRET, safe=""),
                "x-json-echo": json.dumps(SECRET)[1:-1],
                "x-clean": "no-secret-here",
            }
        )

        self.system.responseheaders(flow)

        headers = flow.response.headers
        self.assertNotIn("authorization", headers)
        self.assertNotIn("x-signature", headers)
        self.assertEqual("wrapped [REDACTED]", headers["x-echo"])
        self.assertEqual("[REDACTED]", headers["x-url-echo"])
        self.assertEqual("[REDACTED]", headers["x-form-echo"])
        self.assertEqual("[REDACTED]", headers["x-json-echo"])
        self.assertEqual("no-secret-here", headers["x-clean"])
        self.assertEqual("application/json", headers["content-type"])

    def test_injected_names_suppress_even_when_resolver_omits_them(self) -> None:
        self._install_vault()
        self._install_transport(
            lambda payload: _parsed_decision(
                headers=[("authorization", SECRET)], suppress=set()
            )
        )
        flow = _Flow()
        asyncio.run(self.system.requestheaders(flow))
        flow.response = _Response()
        flow.response.headers = _Headers({"authorization": SECRET})

        self.system.responseheaders(flow)

        self.assertNotIn("authorization", flow.response.headers)
        self.assertNotIn(SECRET, flow.response.headers.get("authorization", ""))

    def test_static_binding_in_exchange_vault_is_untouched(self) -> None:
        self._install_vault(static=True)
        calls = self._decision_transport()
        flow = _Flow()
        flow.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual([], calls)
        self.assertEqual("static-secret", flow.request.headers["Private-Token"])
        self.assertIsNone(flow.response)
        self.assertNotIn(self.system.FLOW_VAULT_SUPPRESS_KEY, flow.metadata)
        self.assertNotIn(self.system.FLOW_REJECTION_KEY, flow.metadata)

    # -- concurrency and loop responsiveness ---------------------------------

    def test_semaphore_bounds_concurrent_resolves(self) -> None:
        os.environ[self.system.CREDENTIAL_RESOLVER_MAX_CONCURRENCY_ENV] = "2"
        self._install_vault()
        state = {"in_flight": 0, "max_in_flight": 0}
        lock = threading.Lock()

        def behavior(payload: dict) -> dict:
            with lock:
                state["in_flight"] += 1
                state["max_in_flight"] = max(state["max_in_flight"], state["in_flight"])
            time.sleep(0.12)
            with lock:
                state["in_flight"] -= 1
            return _parsed_decision()

        calls = self._install_transport(behavior)
        flows = [_Flow() for _ in range(4)]

        async def main() -> None:
            await asyncio.gather(*(self.system.requestheaders(f) for f in flows))

        asyncio.run(main())

        self.assertEqual(4, len(calls))
        self.assertLessEqual(state["max_in_flight"], 2)
        for flow in flows:
            self.assertEqual(SECRET, flow.request.headers["authorization"])

    def test_slow_resolve_does_not_block_event_loop(self) -> None:
        self._install_vault()

        def behavior(payload: dict) -> dict:
            time.sleep(0.3)
            return _parsed_decision()

        self._install_transport(behavior)
        flow = _Flow()
        ticks: list[int] = []

        async def main() -> None:
            task = asyncio.ensure_future(self.system.requestheaders(flow))
            deadline = time.monotonic() + 0.15
            while not task.done() and time.monotonic() < deadline:
                await asyncio.sleep(0.02)
                ticks.append(len(ticks))
            self.assertFalse(task.done())
            await asyncio.wait_for(task, timeout=2)

        asyncio.run(main())

        self.assertGreaterEqual(len(ticks), 3)
        self.assertEqual(SECRET, flow.request.headers["authorization"])

    # -- pull-mode H2 ---------------------------------------------------------

    def test_h2_pull_mode_resolves_and_uses_connection_sni(self) -> None:
        self._install_vault(host="authority.example.com")
        calls = self._decision_transport()
        flow = _Flow()
        flow.request.http_version = "HTTP/2.0"
        flow.request.pretty_host = "authority.example.com"
        flow.request.host = "authority.example.com"

        asyncio.run(self.system.requestheaders(flow))

        self.assertEqual("authority.example.com", calls[0][3]["request"]["host"])
        self.assertEqual(HOST, calls[0][3]["sni"])
        self.assertEqual(SECRET, flow.request.headers["authorization"])

    def test_h2_failure_without_content_length_kills(self) -> None:
        self._install_vault()
        self._install_transport(
            lambda payload: (_ for _ in ()).throw(
                self.system.CredentialResolveError("http_409")
            )
        )
        flow = _Flow()
        flow.request.http_version = "HTTP/2.0"

        asyncio.run(self.system.requestheaders(flow))

        self.assertTrue(flow.killed)
        self.assertIsNone(flow.response)
        self.assertNotIn("authorization", flow.request.headers)

    def test_h2_failure_with_content_length_is_buffered_407(self) -> None:
        self._install_vault()
        self._install_transport(
            lambda payload: (_ for _ in ()).throw(
                self.system.CredentialResolveError("http_409")
            )
        )
        flow = _Flow()
        flow.request.http_version = "HTTP/2.0"
        flow.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(flow))

        self.assertFalse(flow.killed)
        self.assertEqual(407, flow.response.status_code)

    # -- timeout budget derivation ---------------------------------------------

    def test_timeout_budget_follows_env(self) -> None:
        self._install_vault()
        cases = {
            "": 2.25,
            "5": 5.25,
            " 7 ": 7.25,
            "+3": 3.25,
            "0": 2.25,
            "121": 2.25,
            "abc": 2.25,
            "2.5": 2.25,
        }
        for value, expected in cases.items():
            with self.subTest(value=value):
                os.environ[self.system.CREDENTIAL_RESOLVER_TIMEOUT_ENV] = value
                calls = self._decision_transport()
                flow = _Flow()

                asyncio.run(self.system.requestheaders(flow))

                self.assertEqual(expected, calls[0][1])

    # -- transport wire format ---------------------------------------------------

    def test_transport_wire_format_and_error_statuses(self) -> None:
        self._install_vault()
        decisions: list[_ScriptedResponse] = [
            _ScriptedResponse(200, json.dumps(_parsed_decision()).encode("utf-8")),
            _ScriptedResponse(409, b"captured binding replaced"),
        ]
        requests: list[tuple[str, str, bytes | None, dict[str, str]]] = []

        class FakeConnection:
            def __init__(self, socket_path: str, timeout: float) -> None:
                pass

            def request(self, method, path, body=None, headers=None) -> None:
                requests.append((method, path, body, dict(headers or {})))

            def getresponse(self) -> _ScriptedResponse:
                return decisions.pop(0)

            def close(self) -> None:
                pass

        self.system.UnixSocketHTTPConnection = FakeConnection

        flow = _Flow()
        asyncio.run(self.system.requestheaders(flow))
        method, path, body, headers = requests[0]
        self.assertEqual("POST", method)
        self.assertEqual(self.system.CREDENTIAL_RESOLVE_PATH, path)
        self.assertEqual("application/json", headers["Content-Type"])
        self.assertEqual(
            {
                "binding": BINDING,
                "sni": HOST,
                "request": {
                    "scheme": "https",
                    "host": HOST,
                    "port": 443,
                    "method": "GET",
                    "path": "/api/v8/projects",
                },
            },
            json.loads(body.decode("utf-8")),
        )
        self.assertEqual(SECRET, flow.request.headers["authorization"])

        second = _Flow()
        second.request.headers["Content-Length"] = "0"
        asyncio.run(self.system.requestheaders(second))
        self.assertEqual(407, second.response.status_code)

    def test_transport_read_is_bounded_before_size_check(self) -> None:
        self._install_vault()
        limit = self.system.MAX_RESOLVE_RESPONSE_BYTES
        scripted = _ScriptedResponse(200, b"x" * (limit + 1))

        class FakeConnection:
            def __init__(self, socket_path: str, timeout: float) -> None:
                pass

            def request(self, method, path, body=None, headers=None) -> None:
                pass

            def getresponse(self) -> _ScriptedResponse:
                return scripted

            def close(self) -> None:
                pass

        self.system.UnixSocketHTTPConnection = FakeConnection
        flow = _Flow()
        flow.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(flow))

        # The read itself is bounded; oversized bodies never buffer beyond
        # the cap.
        self.assertEqual(limit + 1, scripted.amt)
        self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
        self.assertEqual(407, flow.response.status_code)
        self.assertNotIn("authorization", flow.request.headers)

    def test_parse_resolve_response_rejects_malformed_bodies(self) -> None:
        parse = self.system._parse_resolve_response
        error = self.system.CredentialResolveError
        valid = json.dumps(_parsed_decision()).encode("utf-8")
        self.assertEqual(
            {"authorization", "x-signature"}, parse(valid, BINDING)["suppressHeaders"]
        )
        broken: dict[str, Any] = {
            "oversize": b"x" * (self.system.MAX_RESOLVE_RESPONSE_BYTES + 1),
            "not-json": b"<html>",
            "missing-field": json.dumps(
                {k: v for k, v in _parsed_decision().items() if k != "redactions"}
            ).encode("utf-8"),
            "extra-field": json.dumps(
                {**_parsed_decision(), "extra": 1}
            ).encode("utf-8"),
            "binding-mismatch": json.dumps(_parsed_decision(binding="other")).encode(
                "utf-8"
            ),
            "bad-revision": json.dumps(_parsed_decision(revision=0)).encode("utf-8"),
            "reserved-injected-name": json.dumps(
                _parsed_decision(headers=[("content-length", "9")])
            ).encode("utf-8"),
            "empty-value": json.dumps(
                _parsed_decision(headers=[("authorization", "")])
            ).encode("utf-8"),
            "reserved-suppress-name": json.dumps(
                _parsed_decision(suppress={"transfer-encoding"})
            ).encode("utf-8"),
            "header-extra-field": json.dumps(
                {
                    **_parsed_decision(),
                    "headers": [{"name": "authorization", "value": SECRET, "n": 1}],
                }
            ).encode("utf-8"),
            "redaction-not-string": json.dumps(
                _parsed_decision(redactions=[7])
            ).encode("utf-8"),
        }
        for name, raw in broken.items():
            with self.subTest(name=name):
                with self.assertRaises(error):
                    parse(raw, BINDING)

    # -- defensive branches ----------------------------------------------------

    def test_live_mode_exchange_keeps_placeholder_fail_closed(self) -> None:
        self._install_vault()
        calls = self._decision_transport()
        flow = _Flow()
        flow.request.headers["Content-Length"] = "0"

        deferred = self.system._apply_vault_binding(
            flow, _exchange_vault(self.system), live=object()
        )

        self.assertIsNone(deferred)
        self.assertEqual([], calls)
        self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
        self.assertEqual(503, flow.response.status_code)

    def test_static_lookup_failure_still_503(self) -> None:
        def raise_lookup(_client_ip=None):
            raise self.system.ActiveVaultLookupError("active vault lookup failed")

        self.system._load_active_vault = raise_lookup
        flow = _Flow()
        flow.request.headers["Content-Length"] = "0"

        asyncio.run(self.system.requestheaders(flow))

        self.assertTrue(flow.metadata[self.system.FLOW_REJECTION_KEY])
        self.assertEqual(503, flow.response.status_code)


if __name__ == "__main__":
    unittest.main()
