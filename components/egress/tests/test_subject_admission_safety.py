# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""I6: an enabled handshake experiment cannot enter the IP-based request path."""

import sys
import types
import unittest
from unittest.mock import patch

import test_subject_revision as subject
import test_tls_registry as tls

import test_mitmscripts_system as f


class SubjectRequestSafetyTest(unittest.TestCase):
    def test_h1_h2_ip_reuse_suspend_unload_and_generation_windows(self):
        addon = f._load_system_module()
        # The old transport still carries A's TLS identity after the source IP
        # has been reassigned to B. Missing connection membership must never authorize HTTP.
        for version in ("HTTP/1.1", "HTTP/2.0"):
            for state in ("active-a", "suspended-a", "unloaded-a", "generation-b"):
                with self.subTest(version=version, state=state):
                    flow = f._Flow()
                    flow.response = None
                    flow.request.http_version = version
                    flow.client_conn = types.SimpleNamespace(peername=("192.0.2.1", 1234),
                                                             tls=True, alpn=b"h2" if version == "HTTP/2.0" else None)
                    runtime = types.SimpleNamespace(state=state, token_for=lambda client: None,
                                                    h2_for=lambda client: None)
                    module = types.SimpleNamespace(runtime=lambda: runtime)
                    with patch.dict(sys.modules, {"subject_admission": module}), \
                            patch.object(addon, "_requestheaders_pull", return_value=None) as pull, \
                            patch.object(addon, "_flow_client_ip") as ip, \
                            patch.object(addon, "_load_active_vault_for_ip") as load, \
                            patch.object(addon, "_fetch_active_vault") as fetch, \
                            patch.object(addon, "_apply_vault_binding") as inject:
                        addon.requestheaders(flow)
                        self.assertTrue(flow.killed, "I6 must reject before origin or IP lookup")
                        for call in (pull, ip, load, fetch, inject):
                            call.assert_not_called()
                        self.assertNotIn("Private-Token", flow.request.headers)
                        self.assertIsNone(flow.response)

    def admitted_flow(self):
        from subject_admission import SubjectAdmissionRuntime
        addon = f._load_system_module()
        owner = subject.make_owner(tls_capacity=2, process_tls_capacity=2, static_passthrough=())
        self.addCleanup(owner.close)
        owner.lifecycle("register", subject.command(1, sourceAddresses=["192.0.2.1"]))
        snapshot = tls.TLSRegistryTest().snapshot(
            identity=(subject.CONTROL, subject.A.subject_generation), secret="test-secret")
        owner.prepare(subject.A, snapshot.revision, snapshot.payload)
        owner.commit(subject.A, snapshot.revision)
        runtime = SubjectAdmissionRuntime(owner)
        client = types.SimpleNamespace(peername=("192.0.2.1", 1234),
                                       sni="api.example.com", tls=True, alpn=None)
        runtime.admit_tls(client, client.sni, destination_port=443)
        self.addCleanup(runtime.client_disconnected, client)
        flow = f._Flow()
        flow.response = None
        flow.client_conn = client
        flow.request.host = flow.request.pretty_host = client.sni
        flow.request.headers = f._Headers({"Host": client.sni})
        flow.request.headers.get_all = lambda name: [value for key, value in flow.request.headers.items()
                                                   if key.lower() == name.lower()]
        return addon, runtime, owner.lookup_source("192.0.2.1").registry, flow

    def test_admitted_requests_never_use_pull_and_finish_response_or_error(self):
        for terminal in ("response", "error"):
            with self.subTest(terminal=terminal):
                addon, runtime, registry, flow = self.admitted_flow()
                with patch.dict(sys.modules, {"subject_admission": types.SimpleNamespace(runtime=lambda: runtime)}), \
                        patch.object(addon, "_load_active_vault_for_ip", side_effect=AssertionError("pull forbidden")):
                    addon.requestheaders(flow)
                    self.assertFalse(flow.killed)
                    self.assertEqual(flow.request.headers.get("Private-Token"), "test-secret")
                    self.assertEqual(len(registry.pending_requests()), 1)
                    getattr(addon, terminal)(flow)
                    getattr(addon, terminal)(flow)
                    self.assertEqual(registry.pending_requests(), ())
                self.assertNotIn("api.example.com", " ".join(addon.ctx.log.messages))
                self.assertNotIn("test-secret", " ".join(addon.ctx.log.messages))

    def test_subject_reuses_authority_ssl_insecure_path_and_method_gates(self):
        for case in ("missing", "duplicate", "wrong", "port", "sni", "insecure", "path", "method"):
            with self.subTest(case=case):
                addon, runtime, registry, flow = self.admitted_flow()
                if case == "missing":
                    flow.request.headers.get_all = lambda name: []
                elif case == "duplicate":
                    flow.request.headers.get_all = lambda name: ["api.example.com", "api.example.com"]
                elif case == "wrong":
                    flow.request.headers["Host"] = "other.example.com"
                elif case == "port":
                    flow.request.headers["Host"] = "api.example.com:8443"
                elif case == "sni":
                    flow.client_conn.sni = "other.example.com"
                elif case == "insecure":
                    addon.ctx.options.ssl_insecure = True
                elif case == "path":
                    flow.request.path = "/outside"
                else:
                    flow.request.method = "POST"
                with patch.dict(sys.modules, {"subject_admission": types.SimpleNamespace(runtime=lambda: runtime)}), \
                        patch.object(addon, "_load_active_vault_for_ip", side_effect=AssertionError("pull forbidden")):
                    addon.requestheaders(flow)
                    self.assertNotIn("Private-Token", flow.request.headers)
                    if case in ("path", "method"):
                        self.assertFalse(flow.killed)
                        addon.error(flow)
                    else:
                        self.assertTrue(flow.killed)
                    self.assertEqual(registry.pending_requests(), ())

    def test_plain_http_matches_sidecar_live_and_never_pulls(self):
        addon, runtime, registry, flow = self.admitted_flow()
        flow.request.scheme = "http"
        flow.request.port = 80
        flow.client_conn.tls = False
        with patch.dict(sys.modules, {"subject_admission": types.SimpleNamespace(runtime=lambda: runtime)}), \
                patch.object(addon, "_load_active_vault_for_ip", side_effect=AssertionError("pull forbidden")):
            addon.requestheaders(flow)
        self.assertFalse(flow.killed)
        self.assertNotIn("Private-Token", flow.request.headers)
        self.assertEqual(registry.pending_requests(), ())

    def test_h2_version_and_alpn_are_independent_deny_gates(self):
        for version, alpn in (("HTTP/1.1", b"h2"), ("HTTP/2", None),
                              ("HTTP/2.0", None), ("HTTP/1.0", None), ("", None)):
            addon, runtime, registry, flow = self.admitted_flow()
            flow.request.http_version, flow.client_conn.alpn = version, alpn
            with patch.dict(sys.modules, {"subject_admission": types.SimpleNamespace(runtime=lambda: runtime)}):
                addon.requestheaders(flow)
            self.assertTrue(flow.killed)
            self.assertNotIn("Private-Token", flow.request.headers)
            self.assertEqual(registry.pending_requests(), ())

    def test_disabled_keeps_existing_request_path(self):
        addon = f._load_system_module()
        module = types.SimpleNamespace(runtime=lambda: None)
        flow = f._Flow()
        with patch.dict(sys.modules, {"subject_admission": module}), \
                patch.object(addon, "_requestheaders_pull", return_value=None) as pull:
            addon.requestheaders(flow)
        pull.assert_called_once_with(flow)
        self.assertFalse(flow.killed)

    def test_disabled_does_not_require_admission_module(self):
        # Deployments that ship only system.py (for example the Fast Sandbox
        # smoke) must keep the existing hooks when admission is disabled.
        addon = f._load_system_module()
        flow = f._Flow()
        client = types.SimpleNamespace(peername=("192.0.2.1", 1234), tls=False, alpn=None)
        with patch.dict(sys.modules, {"subject_admission": None}), \
                patch.object(addon, "_requestheaders_pull", return_value=None) as pull:
            addon.requestheaders(flow)
            addon.client_disconnected(client)
        pull.assert_called_once_with(flow)
        self.assertFalse(flow.killed)


if __name__ == "__main__":
    unittest.main()
