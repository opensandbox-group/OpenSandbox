# Copyright 2026 The OpenSandbox Authors
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

    @unittest.skipUnless(shutil.which("openssl"), "openssl is not installed")
    def test_upstream_certificate_failure_logs_safe_reason(self):
        system = Path(__file__).resolve().parents[1] / "mitmscripts" / "system.py"
        with tempfile.TemporaryDirectory(prefix="upstream-tls-") as tmp:
            root = Path(tmp)
            vault = _VaultUnixServer(str(root / "vault.sock"), b'{"revision":1,"bindings":[]}')
            vault.start()
            self.addCleanup(vault.stop)
            cert, key = root / "cert.pem", root / "key.pem"
            subprocess.run(
                ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
                 "-days", "1", "-subj", "/CN=untrusted-fixture",
                 "-addext", "subjectAltName=IP:127.0.0.1",
                 "-out", str(cert), "-keyout", str(key)],
                check=True, capture_output=True, timeout=10,
            )
            upstream_port, proxy_port = _free_port(), _free_port()
            with subprocess.Popen(
                ["openssl", "s_server", "-accept", f"127.0.0.1:{upstream_port}",
                 "-cert", str(cert), "-key", str(key), "-www", "-quiet"],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            ) as upstream, (root / "proxy.log").open("w+") as log:
                try:
                    with subprocess.Popen(
                        [MITMDUMP, "--mode", f"reverse:https://127.0.0.1:{upstream_port}",
                         "--listen-host", "127.0.0.1", "--listen-port", str(proxy_port),
                         "--set", f"confdir={root / 'proxy-ca'}",
                         "--set", "ssl_insecure=false", "-s", str(system)],
                        env={
                            **os.environ,
                            "OPENSANDBOX_EGRESS_PROFILE": "sidecar",
                            "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET": vault.socket_path,
                        },
                        stdout=log, stderr=subprocess.STDOUT,
                    ) as proxy:
                        try:
                            deadline = time.monotonic() + 10
                            for port in (upstream_port, proxy_port):
                                while time.monotonic() < deadline:
                                    try:
                                        with socket.create_connection(("127.0.0.1", port), timeout=.2):
                                            break
                                    except OSError:
                                        time.sleep(.05)
                                else:
                                    self.fail("TLS fixture did not become ready")
                            conn = http.client.HTTPConnection("127.0.0.1", proxy_port, timeout=5)
                            try:
                                conn.request("GET", "/")
                                response = conn.getresponse()
                                self.assertEqual(502, response.status)
                                response.read()
                            finally:
                                conn.close()
                        finally:
                            proxy.terminate()
                    log.seek(0)
                    prefix = "credential proxy: upstream-tls-failure "
                    output = log.read()
                    events = [json.loads(line.split(prefix, 1)[1])
                              for line in output.splitlines() if prefix in line]
                    self.assertEqual(1, len(events), output)
                    event = events[0]
                    self.assertRegex(event.pop("connection_id"), r"^[0-9a-f-]{36}$")
                    self.assertEqual(
                        {"level": "warn", "component": "egress", "event": "upstream_tls_failure",
                         "reason": "self_signed"}, event,
                    )
                finally:
                    upstream.terminate()
