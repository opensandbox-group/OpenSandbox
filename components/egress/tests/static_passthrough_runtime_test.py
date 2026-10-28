# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Opt-in real mitmdump/IPC evidence, without destination-port rewriting.

Run with OPENSANDBOX_STATIC_RUNTIME_TESTS=1 and mitmdump on PATH:
  python -m unittest discover -s tests -p 'static_passthrough_runtime_test.py' -v

The TLS test requires permission to bind loopback TCP/443. It uses explicit
CONNECT, not transparent interception, and cannot establish nft deny behavior.
Missing port permission is reported as a skip, never a high-port substitution.
Set OPENSANDBOX_STATIC_RUNTIME_REQUIRE=1 in CI to fail on any missing prerequisite
instead of skipping; this flag also enables the tests without the opt-in flag.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest

from test_credential_bound_runtime import (
    BOUND, CONTROL, SUBJECT, UNBOUND, SECRET_ONE, MITMDUMP, MITMSCRIPTS,
    LiveRuntimeHarness, binding, decision_payload,
)

ENV = "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"
REQUIRED = os.environ.get("OPENSANDBOX_STATIC_RUNTIME_REQUIRE") == "1"


class StaticHarness(LiveRuntimeHarness):
    def env(self, drain_seconds=1):
        return {**super().env(drain_seconds), ENV: json.dumps([UNBOUND])}

    def start(self, drain_seconds=1):
        # Deliberately never load the canonical_port_443 remapping fixture.
        self.process = subprocess.Popen([
            MITMDUMP, "--listen-host", "127.0.0.1",
            "--listen-port", str(self.proxy_port),
            "--set", "confdir=" + str(self.root / "ca"),
            "--set", "connection_strategy=lazy",
            "--set", "flow_detail=0",
            "--set", "ssl_verify_upstream_trusted_ca=" + str(self.root / "origin.pem"),
            "-s", str(MITMSCRIPTS / "system.py"),
        ], env=self.env(drain_seconds), stdout=self.log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise AssertionError("mitmdump exited: " + self.read_log()[-2000:])
            if os.path.exists(self.socket_path) and self._ipc_ready():
                return
            time.sleep(0.05)
        raise AssertionError("mitmdump IPC readiness timeout: " + self.read_log()[-2000:])


@unittest.skipUnless(REQUIRED or os.environ.get("OPENSANDBOX_STATIC_RUNTIME_TESTS") == "1",
                     "opt-in real mitmdump runtime test")
class StaticPassthroughRuntimeTest(unittest.TestCase):
    def unavailable(self, reason):
        if REQUIRED:
            self.fail(reason)
        self.skipTest(reason)

    def setUp(self):
        if not MITMDUMP:
            self.unavailable("mitmdump is not installed")

    def harness(self, port):
        try:
            probe = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        except OSError as error:
            self.unavailable("Unix IPC unavailable: " + str(error))
        else:
            probe.close()
        temp = tempfile.TemporaryDirectory(prefix="static-tls-")
        self.addCleanup(temp.cleanup)
        harness = StaticHarness(Path(temp.name), origin_port=port)
        self.addCleanup(harness.stop)
        harness.start()
        return harness

    def test_real_ipc_overlap_rejection_preserves_active_readback(self):
        # Ephemeral origin port is unused: this case exercises only real IPC.
        harness = self.harness(0)
        active = harness.install(1, [binding(SECRET_ONE)], [SECRET_ONE], 1)
        payload = decision_payload([binding(SECRET_ONE, UNBOUND)], [SECRET_ONE], 2)
        candidate = {
            "controlGeneration": CONTROL, "subjectGeneration": SUBJECT,
            "decisionEpoch": 2, "vaultRevision": 2, "policyEpoch": 0,
            "digest": hashlib.sha256(payload).hexdigest(),
        }
        status, response = harness.command("/v1/revisions/prepare", {
            "revision": candidate, "payload": base64.b64encode(payload).decode(),
        })
        self.assertNotEqual(200, status, response)
        self.assertEqual(active, harness.active())

    def test_canonical443_static_bootstrap_and_bound_injection(self):
        probe = socket.socket()
        try:
            probe.bind(("127.0.0.1", 443))
        except OSError as error:
            self.unavailable("canonical TCP/443 unavailable: " + str(error))
        finally:
            probe.close()
        harness = self.harness(443)
        static = harness.connect(sni=UNBOUND, trust="origin")
        self.addCleanup(static.close)
        status, payload = static.request("GET", "/v1/data", UNBOUND)
        self.assertEqual(200, status)
        self.assertEqual("", payload["private_token"])
        origin_certificate = static.peer_certificate()
        harness.install(1, [binding(SECRET_ONE)], [SECRET_ONE], 1)
        bound = harness.connect(sni=BOUND, trust="mitm")
        self.addCleanup(bound.close)
        status, payload = bound.request("GET", "/v1/data", BOUND)
        self.assertEqual(200, status)
        self.assertEqual(SECRET_ONE, payload["private_token"])
        self.assertNotEqual(origin_certificate, bound.peer_certificate())
        static_after = harness.connect(sni=UNBOUND, trust="origin")
        self.addCleanup(static_after.close)
        self.assertEqual(origin_certificate, static_after.peer_certificate())
        status, payload = static_after.request("GET", "/v1/data", UNBOUND)
        self.assertEqual(200, status)
        self.assertEqual("", payload["private_token"])


if __name__ == "__main__":
    unittest.main()
