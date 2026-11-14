# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Lifecycle teardown through real mitmdump, subject IPC and loopback sockets."""

import os
import socket
import ssl
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import test_credential_bound_runtime as h
from test_subject_admission_runtime import SubjectHarness


def _second_source():
    """A second loopback source: 127.0.0.2 on Linux, ::1 where only lo0 exists."""
    for family, address in ((socket.AF_INET, "127.0.0.2"), (socket.AF_INET6, "::1")):
        try:
            with socket.socket(family, socket.SOCK_STREAM) as probe:
                probe.bind((address, 0))
            return address
        except OSError:
            continue
    return None


SECOND = _second_source()


class TeardownHarness(SubjectHarness):
    tls_capacity = 4

    def start(self, **kwargs):
        popen = subprocess.Popen

        def launch(args, **options):
            if SECOND == "::1":
                # Listen on both loopback families; the default stays IPv4-only.
                index = args.index("--listen-host") + 1
                args = [*args[:index], "", *args[index + 1:]]
            return popen(args, **options)
        with patch.object(subprocess, "Popen", side_effect=launch):
            super().start(**kwargs)

    def receipt(self, operation, subject, source=None):
        self.sequence += 1
        value = dict(controlGeneration=h.CONTROL, lifecycleSequence=self.sequence,
                     target=dict(subjectKey="subject-" + subject, subjectGeneration=subject))
        if operation == "register":
            value["sourceAddresses"] = [source]
        return self.command("/v2/subjects/" + operation, value)

    def connect_from(self, source, *, sni, trust):
        real = socket.create_connection
        proxy = "::1" if source == "::1" else "127.0.0.1"

        def bound(address, timeout=None, *args, **kwargs):
            return real((proxy, address[1]), timeout, source_address=(source, 0))
        with patch.object(h.socket, "create_connection", side_effect=bound):
            return self.connect(sni=sni, trust=trust)

    def tunnel_only(self):
        raw = socket.create_connection(("127.0.0.1", self.proxy_port), timeout=10)
        raw.sendall(f"CONNECT 127.0.0.1:{self.origin_port} HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n".encode())
        head, _ = h.TunneledConnection._read_head(raw)
        if b" 200" not in head:
            raise AssertionError(head)
        return raw


@unittest.skipUnless(h.MITMDUMP, "mitmdump is not installed")
class SubjectTeardownRuntimeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="r3-", dir=os.environ.get("TMPDIR"))
        self.addCleanup(directory.cleanup)
        self.h = TeardownHarness(Path(directory.name), origin_port=h.free_port())
        self.addCleanup(self.h.stop)
        self.h.start()
        self.paths = paths = []

        class RecordingOrigin(h.EchoHandler):
            def do_GET(self):
                paths.append(self.path)
                self._serve()
        self.h.origin.RequestHandlerClass = RecordingOrigin

    def enroll(self, subject, source="127.0.0.1", secret=h.SECRET_ONE, epoch=1):
        status, receipt = self.h.receipt("register", subject, source)
        self.assertEqual((status, receipt["result"]["outcome"]), (200, "registered"))
        self.h.install_subject(subject, epoch=epoch, secret=secret)

    def open(self, source="127.0.0.1", *, opaque=False):
        trust, sni = ("origin", None) if opaque else ("mitm", h.BOUND)
        connection = self.h.connect_from(source, sni=sni, trust=trust)
        self.addCleanup(connection.close)
        status, payload = connection.request("GET", "/v1/live", h.BOUND)
        self.assertEqual(status, 200)
        return connection, payload["private_token"]

    def assert_closed(self, sock):
        """The receipt follows confirmed disconnect, so EOF/reset is already due."""
        sock.settimeout(2)
        try:
            data = sock.recv(4096)
        except socket.timeout:
            self.fail("proxied transport stayed open after the lifecycle receipt")
        except (ssl.SSLError, OSError):
            data = b""
        self.assertEqual(data, b"")

    def assert_clean_log(self):
        log = self.h.read_log()
        for forbidden in (h.SECRET_ONE, h.SECRET_TWO, h.BOUND, "subject-a", "subject-b"):
            self.assertNotIn(forbidden, log)

    @unittest.skipUnless(SECOND, "no second loopback source address")
    def test_unload_closes_decrypted_and_opaque_and_spares_other_subject(self):
        self.enroll("a")
        self.enroll("b", SECOND, h.SECRET_TWO)
        decrypted, token = self.open()
        self.assertEqual(token, h.SECRET_ONE)
        opaque, token = self.open(opaque=True)
        self.assertEqual(token, "")
        other, token = self.open(SECOND)
        self.assertEqual(token, h.SECRET_TWO)
        status, receipt = self.h.receipt("unload", "a")
        self.assertEqual(status, 200)
        self.assertEqual(receipt["result"]["unload"], {
            "routingRevoked": True, "transportObligations": "none", "completion": "terminal",
            "closedConnections": 2, "remainingConnections": 0})
        self.assert_closed(decrypted.tls)
        self.assert_closed(opaque.tls)
        status, payload = other.request("GET", "/v1/after", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_TWO))
        self.assertIsNone(self.h.process.poll())
        self.assert_clean_log()

    def test_source_reuse_closes_residuals_before_register_and_never_inherits(self):
        early = self.h.tunnel_only()  # proxied before any owner of this source
        self.addCleanup(early.close)
        self.enroll("a")
        self.assert_closed(early)
        decrypted, _ = self.open()
        opaque, _ = self.open(opaque=True)
        status, receipt = self.h.receipt("unload", "a")
        self.assertEqual((status, receipt["result"]["unload"]["remainingConnections"]), (200, 0))
        self.enroll("b", secret=h.SECRET_TWO)
        self.assert_closed(decrypted.tls)
        self.assert_closed(opaque.tls)
        with self.assertRaises((h.ClientError, OSError, ConnectionError)):
            decrypted.request("GET", "/v1/old-identity", h.BOUND)
        fresh, token = self.open()
        self.assertEqual(token, h.SECRET_TWO)
        self.assertNotIn("/v1/old-identity", self.paths)
        self.assert_clean_log()

    def test_suspend_closes_old_and_new_snapshot_serves_new_connections(self):
        self.enroll("a")
        decrypted, _ = self.open()
        opaque, _ = self.open(opaque=True)
        status, receipt = self.h.receipt("suspend", "a")
        self.assertEqual((status, receipt["result"]["outcome"]), (200, "suspended"))
        self.assert_closed(decrypted.tls)
        self.assert_closed(opaque.tls)
        self.h.install_subject("a", epoch=2, secret=h.SECRET_TWO)
        fresh, token = self.open()
        self.assertEqual(token, h.SECRET_TWO)
        with self.assertRaises((h.ClientError, OSError, ConnectionError)):
            decrypted.request("GET", "/v1/revived", h.BOUND)
        self.assertNotIn("/v1/revived", self.paths)
        status, payload = fresh.request("GET", "/v1/again", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_TWO))


if __name__ == "__main__":
    unittest.main()
