# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real mitmdump + subject IPC + loopback TLS; no host network changes."""

import base64
import hashlib
import os
import socket
import ssl
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import test_credential_bound_runtime as h


class SubjectHarness(h.LiveRuntimeHarness):
    sequence = 0
    http2 = False
    untracked = False

    def env(self, drain_seconds=1):
        # Mirror buildMitmdumpChildEnv instead of inheriting the sidecar bundle:
        # production strips the inherited passthrough value and, with admission
        # on, ships an empty policy explicitly. Inheriting it would hide a
        # missing handoff field; omitting it is a combination production never
        # produces. The Go emission is guarded by
        # TestSubjectAdmissionStaticPassthroughHandoff.
        env = {k: v for k, v in super().env().items()
               if not k.startswith("OPENSANDBOX_EGRESS_REVISION_")
               and k != "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"}
        values = dict(MODE="subject-v2", SOCKET=self.socket_path, TOKEN=h.TOKEN,
                      CONTROL_GENERATION=h.CONTROL, MAX_SUBJECTS="2",
                      MAX_SNAPSHOT_BYTES="1048576", RETAINED_PAYLOAD_BUDGET="4194304",
                      TLS_ADMISSION="true", TLS_CAPACITY="2", PROCESS_TLS_CAPACITY="2")
        env.update({"OPENSANDBOX_EGRESS_SUBJECT_REVISION_" + k: v for k, v in values.items()})
        env["OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"] = "[]"
        return env

    def start(self, **kwargs):
        popen = subprocess.Popen
        def launch(args, **options):
            extras = ["--set", "http2=" + str(self.http2).lower(),
                      "-s", str(h.MITMSCRIPTS / "subject_runtime.py")]
            if self.untracked:
                extras += ["-s", str(h.TESTS / "fixtures" / "subject_untracked.py")]
            return popen([*args, *extras], **options)
        with patch.object(subprocess, "Popen", side_effect=launch):
            super().start(**kwargs)

    def _ipc_ready(self):
        return self.command("/v2/subjects/lifecycle-status", {"controlGeneration": h.CONTROL})[0] == 200

    def lifecycle(self, operation, subject="a"):
        self.sequence += 1
        value = dict(controlGeneration=h.CONTROL, lifecycleSequence=self.sequence,
                     target=dict(subjectKey="subject-" + subject, subjectGeneration=subject))
        if operation == "register":
            value["sourceAddresses"] = ["127.0.0.1"]
        status, result = self.command("/v2/subjects/" + operation, value)
        if status != 200:
            raise AssertionError((status, result))

    def install_subject(self, subject="a", epoch=1, bound=True, secret=h.SECRET_ONE):
        payload = h.decision_payload([h.binding(secret)] if bound else [],
                                     [secret] if bound else [], epoch)
        revision = dict(controlGeneration=h.CONTROL, subjectGeneration=subject,
                        decisionEpoch=epoch, vaultRevision=epoch, policyEpoch=0,
                        digest=hashlib.sha256(payload).hexdigest())
        target = dict(subjectKey="subject-" + subject, subjectGeneration=subject)
        for operation in ("prepare", "commit"):
            value = dict(target=target, revision=revision)
            if operation == "prepare":
                value["payload"] = base64.b64encode(payload).decode()
            status, result = self.command("/v2/revisions/" + operation, value)
            if status != 200:
                raise AssertionError((status, result))


@unittest.skipUnless(h.MITMDUMP, "mitmdump is not installed")
class SubjectAdmissionRuntimeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="b3b-", dir=os.environ.get("TMPDIR"))
        self.addCleanup(directory.cleanup)
        self.h = SubjectHarness(Path(directory.name), origin_port=h.free_port())
        self.addCleanup(self.h.stop)
        # Exercise the defensive H2 hook with launcher protection deliberately
        # bypassed; production and all H1 cases disable H2 at launch.
        self.h.http2 = self._testMethodName == "test_h2_ip_reuse_never_reaches_origin_or_injects_credentials"
        self.h.untracked = self._testMethodName == "test_decrypted_without_membership_is_denied"
        self.h.start()
        self.requests = []
        requests = self.requests
        class RecordingOrigin(h.EchoHandler):
            def do_GET(self):
                requests.append(tuple(self.headers.items()))
                self._serve()

            def send_header(self, name, value):
                if name.lower() == "content-type":
                    super().send_header("X-Token-Echo", self.headers.get("Private-Token", ""))
                super().send_header(name, value)
        self.h.origin.RequestHandlerClass = RecordingOrigin

    def assert_denied(self):
        with self.assertRaises((ssl.SSLError, h.ClientError, OSError, ConnectionError)):
            connection = self.h.connect(sni=h.BOUND, trust="mitm")
            connection.close()

    def test_unknown_uninstalled_suspend_and_same_sni_subject_isolation(self):
        self.assert_denied()
        self.h.lifecycle("register")
        self.assert_denied()
        connection = self.h.connect(sni=None, trust="origin")
        connection.close()
        self.h.install_subject()
        bound = self.h.connect(sni=h.BOUND, trust="mitm")
        self.h.lifecycle("suspend")
        self.assert_denied()
        with self.assertRaises((h.ClientError, OSError, ConnectionError)):
            bound.request("GET", "/v1/check", h.BOUND)
        bound.close()
        self.h.lifecycle("unload")
        self.h.lifecycle("register", "b")
        self.h.install_subject("b", bound=False)
        opaque = self.h.connect(sni=h.BOUND, trust="origin")
        opaque.close()
        self.assertIsNone(self.h.process.poll())

    def test_budget_exhaustion_closes_new_bound_but_opaque_still_connects(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connections = [self.h.connect(sni=h.BOUND, trust="mitm") for _ in range(2)]
        try:
            self.assert_denied()
            opaque = self.h.connect(sni=h.UNBOUND, trust="origin")
            opaque.close()
            for connection in connections:
                self.assertTrue(connection.peer_certificate())
        finally:
            for connection in connections:
                connection.close()

    def test_unload_ip_reuse_old_decrypted_request_is_killed(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connection = self.h.connect(sni=h.BOUND, trust="mitm")
        self.h.lifecycle("unload")
        self.h.lifecycle("register", "b")
        self.h.install_subject("b")
        try:
            with self.assertRaises((h.ClientError, OSError, ConnectionError)):
                connection.request("GET", "/v1/check", h.BOUND)
        finally:
            connection.close()
        self.assertEqual(self.requests, [])

    def test_suspend_reinstall_old_connection_never_reaches_origin(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connection = self.h.connect(sni=h.BOUND, trust="mitm")
        self.h.lifecycle("suspend")
        self.h.install_subject(epoch=2, secret=h.SECRET_TWO)
        try:
            with self.assertRaises((h.ClientError, OSError, ConnectionError)):
                connection.request("GET", "/v1/check", h.BOUND)
        finally:
            connection.close()
        self.assertEqual(self.requests, [])

    def test_same_sni_subject_a_injects_b_passes_through_and_rotation(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connection = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(connection.close)
        status, payload = connection.request("GET", "/v1/check", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))
        self.h.install_subject(epoch=2, secret=h.SECRET_TWO)
        status, payload = connection.request("GET", "/v1/check", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_TWO))
        self.h.lifecycle("unload")
        self.h.lifecycle("register", "b")
        self.h.install_subject("b", bound=False)
        opaque = self.h.connect(sni=h.BOUND, trust="origin")
        self.addCleanup(opaque.close)
        status, payload = opaque.request("GET", "/v1/check", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, ""))
        log = self.h.read_log()
        for forbidden in (h.SECRET_ONE, h.SECRET_TWO, h.BOUND, "subject-a", "subject-b"):
            self.assertNotIn(forbidden, log)

    def test_wrong_host_is_denied_before_origin(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connection = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(connection.close)
        with self.assertRaises((h.ClientError, OSError, ConnectionError)):
            connection.request("GET", "/v1/check", h.UNBOUND)
        self.assertEqual(self.requests, [])

    def test_decrypted_without_membership_is_denied(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        connection = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(connection.close)
        with self.assertRaises((h.ClientError, OSError, ConnectionError)):
            connection.request("GET", "/v1/check", h.BOUND)
        self.assertEqual(self.requests, [])

    def test_h2_offer_negotiates_http11_and_injects(self):
        self.h.lifecycle("register")
        self.h.install_subject()
        raw = socket.create_connection(("127.0.0.1", self.h.proxy_port), timeout=5)
        self.addCleanup(raw.close)
        raw.sendall(f"CONNECT 127.0.0.1:{self.h.origin_port} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n".encode())
        head, surplus = h.TunneledConnection._read_head(raw)
        self.assertIn(b"200", head)
        self.assertFalse(surplus)
        context = ssl.create_default_context(cafile=self.h.mitm_ca())
        context.set_alpn_protocols(["h2", "http/1.1"])
        tls = context.wrap_socket(raw, server_hostname=h.BOUND)
        self.addCleanup(tls.close)
        self.assertEqual(tls.selected_alpn_protocol(), "http/1.1")
        tls.sendall(f"GET /v1/check HTTP/1.1\r\nHost: {h.BOUND}\r\nConnection: close\r\n\r\n".encode())
        head, body = h.TunneledConnection._read_head(tls)
        self.assertIn(b"200", head)
        self.assertIn(b"X-Token-Echo: [REDACTED]", head)
        self.assertNotIn(h.SECRET_ONE.encode(), head)
        self.assertEqual(dict(self.requests[0])["Private-Token"], h.SECRET_ONE)

    def test_h2_ip_reuse_never_reaches_origin_or_injects_credentials(self):
        import h2.config
        import h2.connection
        import h2.events

        self.h.lifecycle("register")
        self.h.install_subject()
        raw = socket.create_connection(("127.0.0.1", self.h.proxy_port), timeout=5)
        self.addCleanup(raw.close)
        raw.sendall(f"CONNECT 127.0.0.1:{self.h.origin_port} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n".encode())
        head, surplus = h.TunneledConnection._read_head(raw)
        self.assertIn(b"200", head)
        self.assertFalse(surplus)
        context = ssl.create_default_context(cafile=self.h.mitm_ca())
        context.set_alpn_protocols(["h2"])
        tls = context.wrap_socket(raw, server_hostname=h.BOUND)
        self.addCleanup(tls.close)
        self.assertEqual(tls.selected_alpn_protocol(), "h2")
        self.h.lifecycle("unload")
        self.h.lifecycle("register", "b")
        self.h.install_subject("b")
        client = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=True))
        client.initiate_connection()
        client.send_headers(1, [(":method", "GET"), (":scheme", "https"),
                                (":authority", h.BOUND), (":path", "/v1/check")], end_stream=True)
        tls.sendall(client.data_to_send())
        terminal = False
        while not terminal:
            data = tls.recv(65535)
            if not data:
                break
            for event in client.receive_data(data):
                self.assertNotIsInstance(event, h2.events.ResponseReceived)
                self.assertNotIsInstance(event, h2.events.DataReceived)
                terminal |= isinstance(event, (h2.events.StreamReset, h2.events.ConnectionTerminated))
        self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
