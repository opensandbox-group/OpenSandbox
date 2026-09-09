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

"""Exercise shadow imports and logging inside a real mitmdump TLS connection."""

import http.client
import json
import os
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

from test_mitmproxy_runtime import _free_port, _VaultUnixServer

MITMDUMP = shutil.which("mitmdump")


@unittest.skipUnless(MITMDUMP, "mitmdump is not installed")
class TLSShadowRuntimeTest(unittest.TestCase):
    def test_real_tls_request_preserves_injection_and_emits_shadow(self):
        payload = json.dumps(
            {
                "revision": 1,
                "bindings": [
                    {
                        "name": "local-test",
                        "match": {
                            "hosts": ["api.example.com"],
                            "schemes": ["https"],
                            "methods": ["GET"],
                            "paths": ["/"],
                        },
                        "headers": [{"name": "x-key", "value": "never-log-secret"}],
                    }
                ],
                "redactions": ["never-log-secret"],
            }
        ).encode()
        # Short path stays within macOS Unix-socket path limits.
        with tempfile.TemporaryDirectory(prefix="tls-shadow-", dir="/tmp") as tmp:
            root = Path(tmp)
            vault = _VaultUnixServer(str(root / "vault.sock"), payload)
            vault.start()
            self.addCleanup(vault.stop)
            responder = root / "responder.py"
            responder.write_text(
                "from mitmproxy import http\n"
                "def requestheaders(flow):\n"
                "    body = b'injected' if flow.request.headers.get('x-key') == 'never-log-secret' else b'missing'\n"
                "    flow.response = http.Response.make(200, body)\n"
            )
            system = Path(__file__).resolve().parents[1] / "mitmscripts" / "system.py"
            port = _free_port()
            with (root / "proxy.log").open("w+") as log:
                proc = subprocess.Popen(
                    [
                        MITMDUMP,
                        "--listen-host",
                        "127.0.0.1",
                        "--listen-port",
                        str(port),
                        "--set",
                        "confdir=" + str(root / "ca"),
                        "--set",
                        "upstream_cert=false",
                        "--set",
                        "connection_strategy=lazy",
                        "--set",
                        "flow_detail=0",
                        "-s",
                        str(system),
                        "-s",
                        str(responder),
                    ],
                    env={
                        **os.environ,
                        "OPENSANDBOX_EGRESS_MITMPROXY_SHADOW": "true",
                        "OPENSANDBOX_EGRESS_PROFILE": "sidecar",
                        "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET": vault.socket_path,
                    },
                    stdout=log,
                    stderr=subprocess.STDOUT,
                )
                try:
                    ca = root / "ca" / "mitmproxy-ca-cert.pem"
                    deadline = time.monotonic() + 10
                    while time.monotonic() < deadline:
                        if proc.poll() is not None:
                            self.fail("mitmdump exited before readiness")
                        try:
                            with socket.create_connection(
                                ("127.0.0.1", port), timeout=0.2
                            ):
                                if ca.exists():
                                    break
                        except OSError:
                            pass
                        time.sleep(0.05)
                    else:
                        self.fail("mitmdump did not become ready")
                    conn = http.client.HTTPSConnection(
                        "127.0.0.1",
                        port,
                        context=ssl.create_default_context(cafile=str(ca)),
                        timeout=5,
                    )
                    try:
                        conn.set_tunnel("api.example.com", 443)
                        conn.request("GET", "/")
                        response = conn.getresponse()
                        self.assertEqual(response.status, 200)
                        self.assertEqual(response.read(), b"injected")
                    finally:
                        conn.close()
                finally:
                    proc.terminate()
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=5)
                log.seek(0)
                output = log.read()
                self.assertIn("credential proxy: tls-shadow binding_host", output)
                self.assertNotIn("observer_error", output)
                self.assertNotIn("never-log-secret", output)
