# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""R6 restart-recovery evidence: a real mitmdump child generation change.

The harness terminates the real mitmdump child and starts a fresh receiver
session — new socket directory, session token, and control generation —
against the SAME loopback TLS origin and mitm CA. This is a child
generation change, not a whole-harness restart: the origin stays live, so
what is recovered is exactly the receiver-side subject state. It is not a
full Linux/KVM egress process restart; that remains CI evidence.
"""

import contextlib
import hashlib
import os
import re
import shutil
import socket
import ssl
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import test_credential_bound_runtime as h
import test_subject_admission_runtime as adm
import test_ech_wire as ech


class RecoveryHarness(adm.SubjectHarness):
    """adm.SubjectHarness plus a real mitmdump child restart.

    ``command``, ``lifecycle`` and ``install_subject`` stay the production
    test helpers; they are bound to the live session values (``self.token``,
    ``self.control``) so a restart immediately uses the new session without
    duplicating the helper bodies.
    """

    def __init__(self, root: Path, origin_port: int) -> None:
        super().__init__(root, origin_port)
        self.control = h.CONTROL
        self.token = h.TOKEN
        self.generation = 1
        self._logs = [self.log]

    def env(self, drain_seconds=1):
        # Same shape as SubjectHarness.env, bound to the live session values.
        env = {k: v for k, v in h.LiveRuntimeHarness.env(self, drain_seconds).items()
               if not k.startswith("OPENSANDBOX_EGRESS_REVISION_")
               and k != "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"}
        values = dict(MODE="subject-v2", SOCKET=self.socket_path, TOKEN=self.token,
                      CONTROL_GENERATION=self.control, MAX_SUBJECTS="2",
                      MAX_SNAPSHOT_BYTES="1048576", RETAINED_PAYLOAD_BUDGET="4194304",
                      TLS_ADMISSION="true", TLS_CAPACITY=str(self.tls_capacity),
                      PROCESS_TLS_CAPACITY=str(self.tls_capacity),
                      DRAIN_TIMEOUT_SECONDS=str(drain_seconds))
        env.update({"OPENSANDBOX_EGRESS_SUBJECT_REVISION_" + k: v for k, v in values.items()})
        env["OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"] = "[]"
        return env

    def command(self, path, value=None):
        with patch.object(h, "TOKEN", self.token):
            return super().command(path, value)

    def _ipc_ready(self):
        return self.command("/v2/subjects/lifecycle-status",
                            {"controlGeneration": self.control})[0] == 200

    def lifecycle(self, operation, subject="a"):
        with patch.object(h, "CONTROL", self.control):
            super().lifecycle(operation, subject)

    def install_subject(self, subject="a", epoch=1, bound=True, secret=h.SECRET_ONE,
                        vault=None, policy=0):
        with patch.object(h, "CONTROL", self.control):
            super().install_subject(subject, epoch, bound=bound, secret=secret,
                                    vault=vault, policy=policy)

    def restart_child(self, drain_seconds=1):
        """Terminate the mitmdump child and start a fresh receiver session.

        Only the child is terminated and waited for; the origin, its
        certificate, and the mitm CA directory survive (production restarts
        reuse the same egress CA directory). The old session directory is
        removed, mirroring the production session close.
        """
        previous = self.process
        assert previous is not None
        previous.terminate()
        try:
            previous.wait(timeout=5)
        except subprocess.TimeoutExpired:
            previous.kill()
            previous.wait(timeout=5)
        self.log.flush()
        self.log.close()
        self.generation += 1
        with contextlib.suppress(OSError):
            shutil.rmtree(Path(self.socket_path).parent)
        directory = self.root / f"receiver-{self.generation}"
        directory.mkdir(mode=0o700, exist_ok=False)
        self.socket_path = str(directory / "receiver.sock")
        # Distinct, obviously fake values per generation.
        self.token = chr(ord("b") + self.generation - 1) * 43
        self.control = f"runtime-control-{self.generation}"
        self.sequence = 0
        self.log_path = self.root / f"proxy-{self.generation}.log"
        self.log = self.log_path.open("w")
        self._logs.append(self.log)
        self.start(drain_seconds=drain_seconds)

    def finish_logs(self):
        """Terminate the current child, then read every generation's log.

        mitmdump's redirected stdout is only reliably flushed at process
        exit, so a log read from a live child can miss lines.
        """
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            self.process.wait(timeout=5)
        self.log.flush()
        return [Path(log.name).read_text(errors="replace") for log in self._logs]

    def stop(self):
        try:
            super().stop()
        finally:
            for log in self._logs:
                with contextlib.suppress(OSError, ValueError):
                    log.close()


@unittest.skipUnless(h.MITMDUMP, "mitmdump is not installed")
class SubjectRecoveryRuntimeTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="r6-", dir=os.environ.get("TMPDIR"))
        self.addCleanup(directory.cleanup)
        self.h = RecoveryHarness(Path(directory.name), origin_port=h.free_port())
        self.h.tls_capacity = 8
        self.addCleanup(self.h.stop)
        self.h.start()
        self.requests = []
        requests = self.requests

        class RecordingOrigin(h.EchoHandler):
            def do_GET(self):
                # Sanitized at capture: path plus whether a credential was
                # injected, never the credential value itself.
                requests.append((self.path, bool(self.headers.get("Private-Token"))))
                self._serve()

        self.h.origin.RequestHandlerClass = RecordingOrigin

    def raw_probe(self, token, path, value):
        """Run one real IPC command with an explicit session token."""
        original = self.h.token
        self.h.token = token
        try:
            return self.h.command(path, value)
        finally:
            self.h.token = original

    def assert_closed(self, connection, timeout=5.0):
        """Explicit closure only: EOF or a transport error. A timeout means
        the old transport stayed open, which is not closure evidence."""
        connection.tls.settimeout(timeout)
        try:
            try:
                data = connection.tls.recv(4096)
            except socket.timeout:
                self.fail("retired connection stayed open past its deadline")
            except (ssl.SSLError, OSError):
                data = b""
        finally:
            connection.tls.settimeout(10)
        self.assertEqual(data, b"", "retired transport sent bytes")

    def assert_denied_handshake(self, trust):
        """A denied bound-SNI handshake must end in explicit EOF, reset or alert.

        None of these is closure evidence: a timeout (the deny never closed
        the transport), a certificate verification error (the deny degraded
        into opaque pass-through or an unwanted decrypt), a failed CONNECT
        tunnel (no TLS handshake was attempted), or a completed handshake.
        """
        try:
            connection = self.h.connect(sni=h.BOUND, trust=trust)
        except TimeoutError:
            self.fail("denied handshake timed out instead of closing")
        except ssl.SSLCertVerificationError:
            self.fail("certificate verification error is not a deny close")
        except h.ClientError:
            self.fail("the CONNECT tunnel failed before any TLS handshake")
        except (ssl.SSLError, ConnectionError, OSError):
            return
        connection.close()
        self.fail("denied handshake completed successfully")

    def test_restart_window_denies_sni_and_restores_isolation(self):
        # Generation 1: A is bound and injects its own obviously fake value.
        self.h.lifecycle("register")
        self.h.install_subject()
        conn_a = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(conn_a.close)
        status, payload = conn_a.request("GET", "/v1/one", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))
        served = list(self.requests)

        old_control, old_token = self.h.control, self.h.token
        self.h.restart_child()
        self.assertNotEqual(old_control, self.h.control)
        self.assertNotEqual(old_token, self.h.token)

        # The old session token cannot reach the new child at all.
        status, _ = self.raw_probe(old_token, "/v2/subjects/lifecycle-status",
                                   {"controlGeneration": self.h.control})
        self.assertEqual(status, 401)
        # The old control generation is a confirmed mismatch, so an old
        # generation's install can never affect the new child.
        target = dict(subjectKey="subject-a", subjectGeneration="a")
        status, body = self.raw_probe(self.h.token, "/v2/subjects/register", dict(
            controlGeneration=old_control, lifecycleSequence=1, target=target))
        self.assertEqual((status, body.get("error")), (409, "control_generation_mismatch"))

        # The old transport is closed explicitly, never timed out.
        self.assert_closed(conn_a)

        # Window: A is registered in the new child but its snapshot is not
        # installed. Bound-SNI handshakes are denied and never reach the
        # origin; the old ACK from generation 1 does not carry over.
        self.h.lifecycle("register")
        # Probe both trust roots: opaque fallback must not masquerade as a
        # deny merely because its origin certificate fails MITM-root validation.
        for trust in ("mitm", "origin"):
            with self.subTest(window_trust=trust):
                self.assert_denied_handshake(trust)
        self.assertEqual(served, self.requests, "denied handshakes never reached the origin")

        # AC8: no-SNI keeps its opaque exception across the restart, with a
        # real connection and no injected credential.
        opaque = self.h.connect(sni=None, trust="origin")
        self.addCleanup(opaque.close)
        status, payload = opaque.request("GET", "/v1/opaque", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, ""))

        # Installing the snapshot completes the recovery: A again injects
        # its own fake value on a fresh handshake.
        self.h.install_subject()
        conn_a2 = self.h.connect(sni=h.BOUND, trust="mitm")
        self.addCleanup(conn_a2.close)
        status, payload = conn_a2.request("GET", "/v1/two", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, h.SECRET_ONE))

        # B takes over the source address with no binding for the same SNI:
        # A is unloaded (its connection closes explicitly), B passes the same
        # SNI through opaquely with no credential.
        self.h.lifecycle("unload")
        self.assert_closed(conn_a2)
        self.h.lifecycle("register", "b")
        self.h.install_subject("b", bound=False)
        conn_b = self.h.connect(sni=h.BOUND, trust="origin")
        self.addCleanup(conn_b.close)
        status, payload = conn_b.request("GET", "/v1/b", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, ""))
        # A's retired membership is not revived for B.
        self.assert_closed(conn_a2)
        self.assertIsNone(self.h.process.poll())
        logs = self.h.finish_logs()
        # Exactly the two window probes were denied by the new child, for
        # the fixed snapshot-missing reason.
        self.assertEqual(logs[-1].count("credential proxy: tls-admission-v1 deny snapshot_missing"), 2)
        for log in logs:
            for forbidden in (h.SECRET_ONE, h.BOUND, "subject-a", "subject-b"):
                self.assertNotIn(forbidden, log)


@unittest.skipUnless(h.MITMDUMP, "mitmdump is not installed")
class SubjectRecoveryOpaqueBoundaryTest(unittest.TestCase):
    """AC8 across a child restart, on the real wire.

    The ECH evidence reuses the real ClientHello builder from
    test_ech_wire: the bytes carry a genuine 0xfe0d extension, but this is
    NOT a full ECH client — the inner-name encryption is not executed. The
    asserted boundary is the addon's admission decision: an ECH-bearing
    hello with a bound outer SNI stays opaque before and after recovery,
    while the identical hello without ECH is intercepted once installed.
    """

    def setUp(self):
        directory = tempfile.TemporaryDirectory(prefix="r6o-", dir=os.environ.get("TMPDIR"))
        self.addCleanup(directory.cleanup)
        self.h = RecoveryHarness(Path(directory.name), origin_port=h.free_port())
        self.h.tls_capacity = 8
        self.addCleanup(self.h.stop)
        self.h.start()
        self.h.lifecycle("register")
        self.h.install_subject()

    def _leaf_digest(self, der: bytes) -> str:
        return hashlib.sha256(der).hexdigest()

    def _origin_leaf(self) -> bytes:
        pem = (self.h.root / "origin.pem").read_text()
        marker = "-----END CERTIFICATE-----"
        return ssl.PEM_cert_to_DER_cert(pem.split(marker)[0] + marker)

    def _send_hello(self, extra_extensions=()):
        sock = ech._connect_through(self.h.proxy_port, self.h.origin_port)
        try:
            hello = ech._build_client_hello(h.BOUND, extra_extensions)
            record = b"\x16\x03\x01" + (len(hello) + 4).to_bytes(2, "big") + (
                b"\x01" + len(hello).to_bytes(3, "big") + hello)
            sock.sendall(record)
            return ech._read_server_certificate_der(sock)
        finally:
            sock.close()

    def test_ech_and_no_sni_stay_opaque_across_restart(self):
        origin_digest = self._leaf_digest(self._origin_leaf())
        # Installed: ECH-bearing hello with a bound outer SNI is opaque.
        self.assertEqual(self._leaf_digest(self._send_hello(
            [(ech.ECH_EXTENSION_TYPE, b"\x01\x02\x03\x04")])), origin_digest)

        self.h.restart_child()
        # Window (registered, not installed): ECH still opaque.
        self.h.lifecycle("register")
        self.assertEqual(self._leaf_digest(self._send_hello(
            [(ech.ECH_EXTENSION_TYPE, b"\x01\x02\x03\x04")])), origin_digest)
        # After reinstall: ECH still opaque, while the identical hello
        # WITHOUT the ECH extension is intercepted — recovery changed
        # nothing about the classifier.
        self.h.install_subject()
        self.assertEqual(self._leaf_digest(self._send_hello(
            [(ech.ECH_EXTENSION_TYPE, b"\x01\x02\x03\x04")])), origin_digest)
        self.assertNotEqual(self._leaf_digest(self._send_hello()), origin_digest)

        # A real no-SNI connection stays opaque with no injected credential.
        opaque = self.h.connect(sni=None, trust="origin")
        self.addCleanup(opaque.close)
        status, payload = opaque.request("GET", "/v1/none", h.BOUND)
        self.assertEqual((status, payload["private_token"]), (200, ""))
        # Read after process exit so every line is flushed. The credential
        # never appears anywhere. The hostname may appear only in mitmproxy's
        # own core handshake-failure line, caused by the intercepted
        # positive-control hello above disconnecting after the certificate;
        # the addon's own decision lines carry closed reasons, never a host.
        hostname_lines = []
        for log in self.h.finish_logs():
            self.assertNotIn(h.SECRET_ONE, log)
            self.assertIn("credential proxy:", log)
            hostname_lines.extend(line for line in log.splitlines() if h.BOUND in line)
        self.assertLessEqual(len(hostname_lines), 1)
        for line in hostname_lines:
            self.assertRegex(
                line,
                r"^\[\d{2}:\d{2}:\d{2}\.\d{3}\]\[127\.0\.0\.1:\d+\] "
                r"Client TLS handshake failed\. The client disconnected during the handshake\. "
                rf"If this happens consistently for {re.escape(h.BOUND)}, this may indicate "
                r"that the client does not trust the proxy's certificate\.$",
            )


if __name__ == "__main__":
    unittest.main()
