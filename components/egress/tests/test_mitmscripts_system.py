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

from __future__ import annotations

import importlib.util
import json
import os
import sys
import types
import unittest
from pathlib import Path
from typing import Any


class _Log:
    def __init__(self) -> None:
        self.messages: list[str] = []

    def warn(self, message: str) -> None:
        self.messages.append(message)

    def info(self, message: str) -> None:
        self.messages.append(message)


class _Headers:
    def __init__(self, values: dict[str, str]) -> None:
        self._values = dict(values)

    def get(self, name: str, default: str = "") -> str:
        for key, value in self._values.items():
            if key.lower() == name.lower():
                return value
        return default

    def items(self) -> list[tuple[str, str]]:
        return list(self._values.items())

    def __setitem__(self, name: str, value: str) -> None:
        self._values[name] = value

    def __contains__(self, name: str) -> bool:
        return any(key.lower() == name.lower() for key in self._values)

    def __delitem__(self, name: str) -> None:
        for key in list(self._values):
            if key.lower() == name.lower():
                del self._values[key]
                return


class _Request:
    def __init__(self) -> None:
        self.pretty_host = "code.example.com"
        self.host = "code.example.com"
        self.port = 443
        self.scheme = "https"
        self.method = "GET"
        self.path = "/api/v8/projects"
        self.headers = _Headers({})
        self.raw_content: bytes | None = None
        self.content = b""


class _Response:
    def __init__(self) -> None:
        self.headers = _Headers(
            {
                "content-type": "application/json",
                "x-token-echo": "secret-token",
            }
        )
        self.stream = False
        self.body = "upstream body includes secret-token"
        self.set_text_called = False

    def get_text(self, strict: bool = False) -> str:
        return self.body

    def set_text(self, value: str) -> None:
        self.set_text_called = True
        self.body = value


class _Flow:
    def __init__(self) -> None:
        self.request = _Request()
        self.response = _Response()
        self.metadata: dict[str, Any] = {}


def _load_system_module() -> Any:
    mitmproxy = types.ModuleType("mitmproxy")
    mitmproxy.ctx = types.SimpleNamespace(log=_Log(), options=types.SimpleNamespace(ignore_hosts=[]))
    def _make_response(status: int, body: bytes = b"", headers: dict | None = None):
        resp = _Response()
        resp.status_code = status
        resp.body = body.decode("utf-8") if isinstance(body, bytes) else body
        if headers:
            resp.headers = _Headers(headers)
        return resp

    mitmproxy.http = types.SimpleNamespace(HTTPFlow=object, Response=types.SimpleNamespace(make=_make_response))
    mitmproxy_tls = types.ModuleType("mitmproxy.tls")
    mitmproxy_tls.ClientHelloData = object

    sys.modules["mitmproxy"] = mitmproxy
    sys.modules["mitmproxy.tls"] = mitmproxy_tls

    path = Path(__file__).parents[1] / "mitmscripts" / "system.py"
    spec = importlib.util.spec_from_file_location("opensandbox_egress_system_addon", path)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


class SystemAddonRedactionTest(unittest.TestCase):
    def test_load_active_vault_reads_unix_socket(self) -> None:
        system = _load_system_module()
        calls: list[tuple[str, Any, Any]] = []

        class FakeResponse:
            status = 200

            def read(self) -> bytes:
                return json.dumps(
                    {
                        "revision": 7,
                        "bindings": [
                            {
                                "name": "gitlab-api",
                                "headers": [
                                    {"name": "Private-Token", "value": "secret-token"}
                                ],
                            }
                        ],
                        "redactions": ["secret-token"],
                    }
                ).encode("utf-8")

        class FakeConnection:
            def __init__(self, socket_path: str, timeout: float) -> None:
                calls.append(("init", socket_path, timeout))

            def request(self, method: str, path: str) -> None:
                calls.append(("request", method, path))

            def getresponse(self) -> FakeResponse:
                calls.append(("getresponse", None, None))
                return FakeResponse()

            def close(self) -> None:
                calls.append(("close", None, None))

        old_socket = os.environ.get(system.CREDENTIAL_PROXY_SOCKET_ENV)
        old_connection = system.UnixSocketHTTPConnection
        os.environ[system.CREDENTIAL_PROXY_SOCKET_ENV] = "/tmp/active.sock"
        system.UnixSocketHTTPConnection = FakeConnection
        try:
            vault = system._load_active_vault()
        finally:
            system.UnixSocketHTTPConnection = old_connection
            if old_socket is None:
                os.environ.pop(system.CREDENTIAL_PROXY_SOCKET_ENV, None)
            else:
                os.environ[system.CREDENTIAL_PROXY_SOCKET_ENV] = old_socket

        self.assertIsNotNone(vault)
        assert vault is not None
        self.assertEqual(7, vault.revision)
        self.assertEqual(["secret-token"], vault.redactions)
        self.assertEqual(("init", "/tmp/active.sock", 0.25), calls[0])
        self.assertEqual(("request", "GET", system.ACTIVE_VAULT_PATH), calls[1])
        self.assertEqual(("close", None, None), calls[-1])

    def test_request_injection_log_does_not_include_secret_value(self) -> None:
        system = _load_system_module()
        flow = _Flow()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "gitlab-api",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET"],
                        "paths": ["/api/v8/*"],
                    },
                    "headers": [{"name": "Private-Token", "value": "secret-token"}],
                }
            ],
            ["secret-token"],
        )

        system.request(flow)

        self.assertEqual("secret-token", flow.request.headers.get("Private-Token"))
        self.assertNotIn("secret-token", "\n".join(system.ctx.log.messages))

    def test_responseheaders_redacts_headers_without_body_hook(self) -> None:
        system = _load_system_module()
        flow = _Flow()
        flow.metadata[system.FLOW_REDACTIONS_KEY] = ["secret-token"]

        system.responseheaders(flow)

        self.assertEqual("[REDACTED]", flow.response.headers.get("x-token-echo"))
        self.assertEqual("upstream body includes secret-token", flow.response.body)
        self.assertFalse(flow.response.set_text_called)
        self.assertFalse(hasattr(system, "response"))

    def test_responseheaders_uses_injected_flow_redactions(self) -> None:
        system = _load_system_module()
        flow = _Flow()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "gitlab-api",
                    "match": {"hosts": ["code.example.com"]},
                    "headers": [{"name": "Private-Token", "value": "old-secret"}],
                }
            ],
            ["old-secret"],
        )

        system.request(flow)
        system._load_active_vault = lambda: system.ActiveVault(2, [], ["new-secret"])
        flow.response.headers["x-token-echo"] = "old-secret"
        system.responseheaders(flow)

        self.assertEqual("[REDACTED]", flow.response.headers.get("x-token-echo"))

    def test_responseheaders_redacts_longer_values_first(self) -> None:
        system = _load_system_module()
        flow = _Flow()
        flow.response.headers["x-token-echo"] = "prefix token-abc suffix"
        flow.metadata[system.FLOW_REDACTIONS_KEY] = ["token", "token-abc"]

        system.responseheaders(flow)

        self.assertEqual("prefix [REDACTED] suffix", flow.response.headers.get("x-token-echo"))


class SystemAddonSubstitutionTest(unittest.TestCase):
    def _make_system_with_substitutions(self):
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "placeholder-api",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET", "POST"],
                        "paths": ["/lookup", "/tenants/*", "/token", "/form"],
                    },
                    "substitutions": [
                        {
                            "placeholder": "__query_secret__",
                            "value": "query secret+value",
                            "in": ["query"],
                        },
                        {
                            "placeholder": "__tenant_id__",
                            "value": "tenant 42",
                            "in": ["path"],
                        },
                        {
                            "placeholder": "__body_secret__",
                            "value": 'body "secret" \\ value',
                            "in": ["body"],
                        },
                        {
                            "placeholder": "__form_secret__",
                            "value": "form secret+value",
                            "in": ["body"],
                        },
                    ],
                }
            ],
            [
                "__query_secret__",
                "query secret+value",
                "__tenant_id__",
                "tenant 42",
                "__body_secret__",
                'body "secret" \\ value',
                "__form_secret__",
                "form secret+value",
            ],
        )
        return system

    def test_query_substitution_url_encodes_value(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.path = "/lookup?api_key=__query_secret__&static=1"

        system.request(flow)

        self.assertEqual("/lookup?api_key=query%20secret%2Bvalue&static=1", flow.request.path)
        self.assertIn("query secret+value", flow.metadata[system.FLOW_REDACTIONS_KEY])
        self.assertNotIn("query secret+value", "\n".join(system.ctx.log.messages))

    def test_path_substitution_rewrites_only_path(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.path = "/tenants/__tenant_id__/items?tenant=__tenant_id__"

        system.request(flow)

        self.assertEqual("/tenants/tenant%2042/items?tenant=__tenant_id__", flow.request.path)

    def test_json_body_substitution_escapes_string_value_and_updates_length(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.method = "POST"
        flow.request.path = "/token"
        flow.request.headers["content-type"] = "application/json"
        flow.request.headers["transfer-encoding"] = "chunked"
        flow.request.content = b'{"client_secret":"__body_secret__"}'

        system.request(flow)

        body = flow.request.content.decode("utf-8")
        self.assertEqual({"client_secret": 'body "secret" \\ value'}, json.loads(body))
        self.assertEqual(str(len(flow.request.content)), flow.request.headers.get("content-length"))
        self.assertEqual("", flow.request.headers.get("transfer-encoding"))

    def test_structured_json_body_substitution_escapes_string_value(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.method = "POST"
        flow.request.path = "/token"
        flow.request.headers["content-type"] = "application/merge-patch+json; charset=utf-8"
        flow.request.content = b'{"client_secret":"__body_secret__"}'

        system.request(flow)

        body = flow.request.content.decode("utf-8")
        self.assertEqual({"client_secret": 'body "secret" \\ value'}, json.loads(body))

    def test_form_body_substitution_url_encodes_value(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.method = "POST"
        flow.request.path = "/form"
        flow.request.headers["content-type"] = "application/x-www-form-urlencoded"
        flow.request.content = b"secret=__form_secret__"

        system.request(flow)

        self.assertEqual(b"secret=form+secret%2Bvalue", flow.request.content)

    def test_body_substitution_does_not_rewrite_inserted_values(self) -> None:
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "nested-body",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["POST"],
                        "paths": ["/token"],
                    },
                    "substitutions": [
                        {
                            "placeholder": "__a__",
                            "value": "prefix __b__",
                            "in": ["body"],
                        },
                        {
                            "placeholder": "__b__",
                            "value": "secret-b",
                            "in": ["body"],
                        },
                    ],
                }
            ],
            ["__a__", "prefix __b__", "__b__", "secret-b"],
        )
        flow = _Flow()
        flow.response = None
        flow.request.method = "POST"
        flow.request.path = "/token"
        flow.request.headers["content-type"] = "text/plain"
        flow.request.content = b"first=__a__&second=__b__"

        system.request(flow)

        self.assertEqual(b"first=prefix __b__&second=secret-b", flow.request.content)

    def test_header_substitution_does_not_rewrite_injected_headers(self) -> None:
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "header-substitution",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET"],
                        "paths": ["/lookup"],
                    },
                    "headers": [
                        {"name": "Authorization", "value": "Bearer __header_secret__"}
                    ],
                    "substitutions": [
                        {
                            "placeholder": "__header_secret__",
                            "value": "substituted-secret",
                            "in": ["header"],
                        },
                    ],
                }
            ],
            ["__header_secret__", "substituted-secret"],
        )
        flow = _Flow()
        flow.response = None
        flow.request.path = "/lookup"
        flow.request.headers["X-Template"] = "client __header_secret__"

        system.request(flow)

        self.assertEqual("client substituted-secret", flow.request.headers.get("X-Template"))
        self.assertEqual("Bearer __header_secret__", flow.request.headers.get("Authorization"))

    def test_compressed_body_substitution_is_skipped(self) -> None:
        system = self._make_system_with_substitutions()
        flow = _Flow()
        flow.response = None
        flow.request.method = "POST"
        flow.request.path = "/token"
        flow.request.headers["content-type"] = "application/json"
        flow.request.headers["content-encoding"] = "gzip"
        flow.request.content = b'{"client_secret":"__body_secret__"}'

        system.request(flow)

        self.assertEqual(b'{"client_secret":"__body_secret__"}', flow.request.content)
        self.assertNotIn(system.FLOW_REDACTIONS_KEY, flow.metadata)
        self.assertIn("substitution miss", "\n".join(system.ctx.log.messages))

    def test_rejected_path_substitution_log_does_not_include_secret_value(self) -> None:
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "path-secret",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET"],
                        "paths": ["/tenants/*"],
                    },
                    "substitutions": [
                        {
                            "placeholder": "__tenant_id__",
                            "value": "tenant/secret",
                            "in": ["path"],
                        },
                    ],
                }
            ],
            ["__tenant_id__", "tenant/secret", "tenant%2Fsecret"],
        )
        flow = _Flow()
        flow.request.path = "/tenants/__tenant_id__/items"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        logs = "\n".join(system.ctx.log.messages)
        self.assertIn("path=[REDACTED]", logs)
        self.assertNotIn("tenant/secret", logs)
        self.assertNotIn("tenant%2Fsecret", logs)

    def test_path_substitution_rejects_nested_encoded_separator(self) -> None:
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "path-secret",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET"],
                        "paths": ["/tenants/*"],
                    },
                    "substitutions": [
                        {
                            "placeholder": "__tenant_id__",
                            "value": "tenant%2Fsecret",
                            "in": ["path"],
                        },
                    ],
                }
            ],
            ["__tenant_id__", "tenant%2Fsecret", "tenant%252Fsecret"],
        )
        flow = _Flow()
        flow.request.path = "/tenants/__tenant_id__/items"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)


class SystemAddonPathTraversalTest(unittest.TestCase):
    """Regression tests for CVE-like path traversal credential injection bypass."""

    def _make_system_with_vault(self):
        system = _load_system_module()
        system._load_active_vault = lambda: system.ActiveVault(
            1,
            [
                {
                    "name": "gitlab-api",
                    "match": {
                        "hosts": ["code.example.com"],
                        "methods": ["GET"],
                        "paths": ["/api/v8/projects/123/*"],
                    },
                    "headers": [{"name": "Private-Token", "value": "secret-token"}],
                }
            ],
            ["secret-token"],
        )
        return system

    def test_dot_dot_traversal_rejected(self) -> None:
        """Raw .. traversal escaping path scope must be rejected with 403."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/../456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_encoded_dot_dot_traversal_rejected(self) -> None:
        """%2e%2e encoded traversal must be rejected with 403."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/%2e%2e/456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_mixed_case_encoded_dot_dot_rejected(self) -> None:
        """%2E%2e mixed-case encoded traversal must be rejected."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/%2E%2e/456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_encoded_slash_rejected(self) -> None:
        """%2f encoded path separator must be rejected."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123%2f..%2f456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_dot_dot_in_query_string_not_rejected(self) -> None:
        """.. in query string is harmless and should not trigger rejection."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/variables?ref=../../main"

        system.request(flow)

        # Should inject credential normally (path matches the binding).
        self.assertEqual("secret-token", flow.request.headers.get("Private-Token"))

    def test_normal_path_within_scope_injects_credential(self) -> None:
        """Normal path within scope still receives credential injection."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/variables"

        system.request(flow)

        self.assertEqual("secret-token", flow.request.headers.get("Private-Token"))

    def test_normal_path_outside_scope_no_injection(self) -> None:
        """Normal path outside scope does not receive credential injection."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/456/variables"

        system.request(flow)

        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_dot_dot_substring_not_rejected(self) -> None:
        """'..' not as a complete segment (e.g. '/.../') must NOT be blocked."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/.../data"
        system.request(flow)
        self.assertEqual("secret-token", flow.request.headers.get("Private-Token"))

    def test_dot_dot_metadata_path_not_rejected(self) -> None:
        """``/..metadata`` is not a traversal segment."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/..metadata"
        system.request(flow)
        self.assertEqual("secret-token", flow.request.headers.get("Private-Token"))

    def test_encoded_backslash_rejected(self) -> None:
        """%5c encoded backslash must be rejected."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/%5c..%5c456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_raw_backslash_rejected(self) -> None:
        """Raw backslash in path must be rejected."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123\\..\\456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_double_encoded_dot_dot_rejected(self) -> None:
        """Double percent-encoded traversal (%252e%252e) must be rejected."""
        system = self._make_system_with_vault()
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/%252e%252e/456/variables"

        system.request(flow)

        self.assertIsNotNone(flow.response)
        self.assertEqual(403, flow.response.status_code)
        self.assertNotIn("Private-Token", flow.request.headers._values)

    def test_no_vault_active_allows_dot_dot_through(self) -> None:
        """When no vault is active, ambiguous paths are not blocked (no credential risk)."""
        system = _load_system_module()
        system._load_active_vault = lambda: None
        flow = _Flow()
        flow.request.path = "/api/v8/projects/123/../456/variables"

        system.request(flow)

        # No response set means the request passes through unmodified.
        # (flow.response is the pre-initialized _Response, not a 403)
        self.assertNotIn("Private-Token", flow.request.headers._values)


if __name__ == "__main__":
    unittest.main()
