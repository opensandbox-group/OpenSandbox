# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Socket-free checks of the production H2 harness child environment."""

import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import test_credential_bound_h2_runtime as runner
from test_credential_bound_runtime import LiveRuntimeHarness, TOKEN


class StaticHandoffTests(unittest.TestCase):
    def test_start_preserves_controlled_static_handoff_and_filters_inherited_values(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            # Exercise start() and the real IPC env() without binding sockets,
            # generating certificates, or launching mitmdump.
            harness = object.__new__(runner.RuntimeHarness)
            harness.root = root
            harness.protocol = "h2"
            harness.scenario = "normal"
            harness.drain_seconds = 8
            harness.barrier_release_path = root / "release-pre-admission"
            harness.ipc = object.__new__(LiveRuntimeHarness)
            harness.ipc.socket_path = str(root / "receiver.sock")
            harness.socket_path = harness.ipc.socket_path
            Path(harness.socket_path).touch()
            (root / "ca").mkdir()
            (root / "ca/mitmproxy-ca-cert.pem").write_bytes(b"proxy-ca\n")
            (root / "origin-ca.pem").write_bytes(b"origin-ca\n")
            inherited = {
                "PATH": "/synthetic/bin",
                "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS": '["untrusted.example"]',
                "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS_EXTRA": "untrusted",
                "OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN": "untrusted",
                "OPENSANDBOX_EGRESS_REVISION_UNRECOGNIZED": "untrusted",
                "OPENSANDBOX_EGRESS_PROXY_URL": "http://untrusted.example",
                "OPENSANDBOX_UNRELATED": "untrusted",
                "H2_PRODUCTION_OBSERVATIONS": "/untrusted",
                "H2_PRODUCTION_SCENARIO": "untrusted",
                "H2_PRODUCTION_BARRIER_RELEASE": "/untrusted/release",
                "H2_PRODUCTION_UNRECOGNIZED": "untrusted",
                "SSLKEYLOGFILE": "/untrusted/keys",
                "MITMPROXY_SSLKEYLOGFILE": "/untrusted/keys",
            }
            with (
                mock.patch.dict(os.environ, inherited, clear=True),
                mock.patch.object(runner, "OriginServer") as origin,
                mock.patch.object(runner.socket, "socket") as socket_factory,
                mock.patch.object(runner.subprocess, "Popen") as popen,
                mock.patch.object(harness.ipc, "active", return_value=None),
                mock.patch.object(harness, "events", return_value=[{"kind": "observer_ready"}]),
            ):
                socket_factory.return_value.__enter__.return_value.getsockname.return_value = ("127.0.0.1", 12345)
                popen.return_value.poll.return_value = None
                try:
                    harness.start()
                finally:
                    harness.log.close()
                origin.assert_called_once_with(root, "h2", "normal")
                popen.assert_called_once()
                environment = popen.call_args.kwargs["env"]
                self.assertEqual(environment.get("OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS"), "[]")
                self.assertEqual(environment["OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN"], TOKEN)
                self.assertEqual(environment["OPENSANDBOX_EGRESS_REVISION_IPC_SOCKET"], harness.socket_path)
                self.assertEqual(environment["OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS"], "8")
                self.assertEqual(environment["H2_PRODUCTION_OBSERVATIONS"], str(root))
                self.assertEqual(environment["H2_PRODUCTION_SCENARIO"], "normal")
                self.assertEqual(environment["H2_PRODUCTION_BARRIER_RELEASE"],
                                 str(root / "release-pre-admission"))
                self.assertEqual(environment["PATH"], "/synthetic/bin")
                for key in inherited.keys() - {
                    "PATH", "OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS",
                    "OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN",
                    "H2_PRODUCTION_OBSERVATIONS", "H2_PRODUCTION_SCENARIO",
                    "H2_PRODUCTION_BARRIER_RELEASE",
                }:
                    self.assertNotIn(key, environment)
                self.assertEqual(harness.combined_ca.read_bytes(), b"proxy-ca\norigin-ca\n")


if __name__ == "__main__":
    unittest.main()
