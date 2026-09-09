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

"""Shadow observations must not affect traffic or perform another vault lookup."""

import sys
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from test_mitmscripts_system import _Flow, _load_system_module


class TLSShadowTest(unittest.TestCase):
    def setUp(self):
        paths = patch.object(
            sys,
            "path",
            [str(Path(__file__).resolve().parents[1] / "mitmscripts"), *sys.path],
        )
        paths.start()
        self.addCleanup(paths.stop)
        self.system = _load_system_module()
        self.system._tls_shadow_enabled = True

    def flow(self, sni="api.example.com"):
        f = _Flow()
        f.request.scheme, f.request.port = "https", 443
        f.request.pretty_host = f.request.host = "api.example.com"
        f.request.method, f.request.path = "GET", "/"
        f.client_conn = types.SimpleNamespace(sni=sni, peername=("10.0.0.1", 1234))
        return f

    def vault(self, host="api.example.com", scheme="https"):
        return self.system.ActiveVault(
            1,
            [
                {
                    "name": "private-binding",
                    "match": {
                        "hosts": [host],
                        "schemes": [scheme],
                        "methods": ["GET"],
                        "paths": ["/"],
                    },
                    "headers": [{"name": "x-key", "value": "never-log-secret"}],
                }
            ],
            ["never-log-secret"],
        )

    def test_projection(self):
        for sni, vault, failed, want in [
            ("API.EXAMPLE.COM.", self.vault(), False, "binding_host"),
            ("api.example.com", self.vault("*.example.com"), False, "binding_host"),
            ("example.com", self.vault("*.example.com"), False, "no_binding_host"),
            ("other.example.com", self.vault(), False, "no_binding_host"),
            ("api.example.com", self.vault(scheme="http"), False, "no_binding_host"),
            ("api.example.com", None, False, "no_vault"),
            ("api.example.com", None, True, "lookup_failed"),
            (None, self.vault(), False, "missing_sni"),
            ("bad host", self.vault(), False, "invalid_sni"),
            ("api.example.com", self.vault("*.invalid.*"), False, "invalid_snapshot"),
        ]:
            with self.subTest(want=want):
                flow = self.flow(sni)
                flow.request.method, flow.request.path = "POST", "/unmatched"
                self.system._observe_tls_shadow(flow, vault, lookup_failed=failed)
                self.assertEqual(
                    self.system.ctx.log.messages[-1],
                    "credential proxy: tls-shadow " + want,
                )
                self.assertFalse(flow.killed)

    def test_disabled_and_noncanonical_ports(self):
        self.system._tls_shadow_enabled = False
        self.system._observe_tls_shadow(self.flow(), self.vault())
        self.system._tls_shadow_enabled = True
        for scheme, port in [("https", 8443), ("http", 80)]:
            f = self.flow()
            f.request.scheme, f.request.port = scheme, port
            self.system._observe_tls_shadow(f, self.vault())
        self.assertEqual(self.system.ctx.log.messages, [])

    def test_accepted_empty_wildcard_language_does_not_poison_vault(self):
        import tls_shadow

        for size in (252, 253):
            base = ".".join(["a" * 63, "b" * 63, "c" * 63, "d" * (size - 192)])
            wildcard = "*." + base
            self.assertEqual(
                self.system._normalize_active_vault_host(wildcard, "test"), wildcard
            )
            impossible = self.vault(wildcard).bindings
            self.assertEqual(
                tls_shadow.project("api.example.com", impossible), "no_binding_host"
            )
            for bindings in (
                impossible + self.vault().bindings,
                self.vault().bindings + impossible,
            ):
                self.assertEqual(
                    tls_shadow.project("api.example.com", bindings), "binding_host"
                )
        base = ".".join(["a" * 63, "b" * 63, "c" * 63, "d" * 59])
        self.assertEqual(
            tls_shadow.project("x." + base, self.vault("*." + base).bindings),
            "binding_host",
        )
        for malformed in (
            "*.*." + base[:-1],
            "*." + base + "dddd",
            "*." + "a" * 64 + "." + "b" * 63 + "." + "c" * 63 + "." + "d" * 59,
        ):
            self.assertEqual(
                tls_shadow.project("api.example.com", self.vault(malformed).bindings),
                "invalid_snapshot",
            )

    def test_fleet_absence_does_not_claim_passthrough(self):
        self.system._set_fleet_mode(True)
        self.system._observe_tls_shadow(self.flow(), None)
        self.assertEqual(
            self.system.ctx.log.messages[-1],
            "credential proxy: tls-shadow unknown_subject_or_vault",
        )

    def test_hook_reuses_lookup_and_preserves_injection(self):
        f = self.flow()
        with patch.object(
            self.system, "_load_active_vault", return_value=self.vault()
        ) as lookup:
            self.system.requestheaders(f)
            lookup.assert_called_once()
        self.assertEqual(f.request.headers.get("x-key"), "never-log-secret")
        self.assertFalse(f.killed)
        self.assertEqual(
            self.system.ctx.log.messages[0], "credential proxy: tls-shadow binding_host"
        )
        self.assertNotIn("never-log-secret", "\n".join(self.system.ctx.log.messages))

    def test_observer_error_cannot_change_request(self):
        import tls_shadow

        f = self.flow()
        with patch.object(
            tls_shadow, "project", side_effect=RuntimeError("secret-error")
        ):
            self.system._observe_tls_shadow(f, self.vault())
        self.assertFalse(f.killed)
        self.assertNotIn("secret-error", "\n".join(self.system.ctx.log.messages))

    def test_lookup_failure_preserves_rejection(self):
        f = self.flow()
        with patch.object(
            self.system,
            "_load_active_vault",
            side_effect=self.system.ActiveVaultLookupError("unavailable"),
        ) as lookup:
            self.system.requestheaders(f)
            lookup.assert_called_once()
        self.assertIn(
            "credential proxy: tls-shadow lookup_failed", self.system.ctx.log.messages
        )
        self.assertEqual(f.response.status_code, 503)
