# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Real-mitmdump proof that a subject TLS deny closes the exact transport.

A denied ClientHello in the Fast Sandbox subject admission runtime must end
the handshake with an explicit EOF, TCP reset or TLS alert. The probe below
runs one explicit CONNECT per trust root (the mitmproxy CA and the origin's
own CA): a handshake timeout is a failure, and so is a certificate
verification error, which instead indicates that the deny degraded into an
erroneous opaque pass-through (origin cert under the MITM root) or an
erroneous decrypt (proxy cert under the origin root). A successfully
completed handshake means the deny never happened. The positive control in
the same file (bound SNI decrypted after install) keeps an unavailable
listener distinguishable from a real deny close.
"""

import os
import socket
import ssl
import tempfile
import unittest
from pathlib import Path

import test_credential_bound_runtime as h
from test_subject_admission_runtime import SubjectHarness

# Bounded failure ceiling only: closure must be explicit and far faster.
DENY_HANDSHAKE_TIMEOUT_SECONDS = 8


@unittest.skipUnless(h.MITMDUMP, "mitmdump is not installed")
class SubjectHandshakeCloseRuntimeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="fsb-deny-", dir=os.environ.get("TMPDIR"))
        self.addCleanup(directory.cleanup)
        self.h = SubjectHarness(Path(directory.name), origin_port=h.free_port())
        self.addCleanup(self.h.stop)

    def start_harness(self):
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

    def denied_handshake(self, trust):
        """Assert one denied handshake ends in an explicit transport close.

        TimeoutError means the deny never closed the transport; a
        certificate verification error means it degraded into opaque
        pass-through or an unwanted decrypt instead of closing. Any other
        TLS/transport failure (EOF, reset, TLS alert) is closure evidence.
        """
        raw = socket.create_connection(("127.0.0.1", self.h.proxy_port), timeout=10)
        try:
            raw.sendall(
                f"CONNECT 127.0.0.1:{self.h.origin_port} HTTP/1.1\r\n"
                f"Host: 127.0.0.1:{self.h.origin_port}\r\n\r\n".encode()
            )
            head, surplus = h.TunneledConnection._read_head(raw)
            self.assertTrue(head.startswith(b"HTTP/1.1 200"), head[:80])
            self.assertFalse(surplus)
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            context.load_verify_locations(
                self.h.mitm_ca() if trust == "mitm" else self.h.origin_ca())
            context.check_hostname = True
            raw.settimeout(DENY_HANDSHAKE_TIMEOUT_SECONDS)
            try:
                tls = context.wrap_socket(raw, server_hostname=h.BOUND)
            except TimeoutError:
                self.fail("denied handshake timed out instead of closing")
            except ssl.SSLCertVerificationError:
                self.fail("certificate verification error is not a deny close")
            except (ssl.SSLError, ConnectionError, OSError):
                return
            else:
                tls.close()
                self.fail("denied handshake completed successfully")
        finally:
            raw.close()

    def finish_log(self):
        """Terminate mitmdump first: process exit flushes its redirected stdout."""
        self.h.process.terminate()
        self.h.process.wait(timeout=5)
        return self.h.read_log()

    # -- F1: registered without a snapshot, fresh child, both trust roots ---

    def test_registered_without_snapshot_denied_handshake_closes_explicitly(self):
        self.start_harness()
        self.h.lifecycle("register")
        self.denied_handshake("mitm")
        self.denied_handshake("origin")
        self.assertEqual(self.requests, [])
        # Denied handshakes leak no decrypted-connection budget: the full
        # subject capacity (2) stays available for admitted clients.
        self.h.install_subject()
        admitted = [self.h.connect(sni=h.BOUND, trust="mitm") for _ in range(2)]
        try:
            for connection in admitted:
                status, payload = connection.request("GET", "/v1/check", h.BOUND)
                self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))
        finally:
            for connection in admitted:
                connection.close()
        self.assertIsNone(self.h.process.poll())
        log = self.finish_log()
        self.assertEqual(log.count("credential proxy: tls-admission-v1 deny snapshot_missing"), 2)
        for forbidden in (h.SECRET_ONE, h.SECRET_TWO, h.BOUND, "subject-a", "subject-b"):
            self.assertNotIn(forbidden, log)

    # -- F2 + F5: deny does not poison opaque exceptions or other clients ---

    def test_deny_leaves_no_sni_opaque_and_later_admission_intact(self):
        self.start_harness()
        self.h.lifecycle("register")
        self.denied_handshake("mitm")
        # Same source address, no SNI: the opaque exception is untouched.
        no_sni = self.h.connect(sni=None, trust="origin")
        no_sni.close()
        self.h.install_subject()
        bound = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(bound.close)
        status, payload = bound.request("GET", "/v1/check", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))
        unbound = self.h.connect(sni=h.UNBOUND, trust="origin")
        self.addCleanup(unbound.close)
        status, payload = unbound.request("GET", "/v1/check", h.UNBOUND)
        self.assertEqual((status, payload["private_token"]), (200, ""))
        self.assertEqual(self.requests and dict(self.requests[0]).get("Private-Token"),
                         h.SECRET_ONE)
        self.assertIsNone(self.h.process.poll())

    # -- F3: unknown identity and budget exhaustion close explicitly -------

    def test_unknown_identity_after_unload_closes_explicitly(self):
        self.start_harness()
        self.h.lifecycle("register")
        self.h.install_subject()
        established = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(established.close)
        self.h.lifecycle("unload")
        self.denied_handshake("mitm")
        self.denied_handshake("origin")
        self.assertEqual(self.requests, [])
        log = self.finish_log()
        self.assertEqual(log.count("credential proxy: tls-admission-v1 deny unknown_identity"), 2)
        for forbidden in (h.SECRET_ONE, h.SECRET_TWO, h.BOUND, "subject-a", "subject-b"):
            self.assertNotIn(forbidden, log)

    def test_budget_exhaustion_closes_new_bound_but_keeps_established(self):
        self.start_harness()
        self.h.lifecycle("register")
        self.h.install_subject()
        established = [self.h.connect(sni=h.BOUND, trust="mitm") for _ in range(2)]
        try:
            self.denied_handshake("mitm")
            self.assertEqual(self.requests, [])
            # Closing the denied third client must not touch the two
            # established clients sharing the same source address.
            for connection in established:
                status, payload = connection.request("GET", "/v1/check", h.BOUND)
                self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))
            opaque = self.h.connect(sni=h.UNBOUND, trust="origin")
            opaque.close()
        finally:
            for connection in established:
                connection.close()
        self.assertEqual(len(self.requests), 2)
        self.assertIsNone(self.h.process.poll())
        log = self.finish_log()
        self.assertEqual(log.count("credential proxy: tls-admission-v1 deny registry_exhausted"), 1)
        for forbidden in (h.SECRET_ONE, h.SECRET_TWO, h.BOUND, "subject-a", "subject-b"):
            self.assertNotIn(forbidden, log)


if __name__ == "__main__":
    unittest.main()
