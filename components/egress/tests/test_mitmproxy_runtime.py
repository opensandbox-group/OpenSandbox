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

"""Real-mitmproxy runtime regression tests for the credential proxy addon.

Unit tests in test_mitmscripts_system.py call the addon hooks directly, which
cannot exercise mitmproxy's pre-hook body-size check: with
``stream_large_bodies=1m``, a request body above 1 MiB is marked for
streaming *before* the ``requestheaders`` hook runs, and mitmproxy 11.0.2
raises ``NotImplementedError`` if the hook then sets a local response. These
tests run a real ``mitmdump`` with the addon and a fake credential vault over
a Unix socket, and verify that rejecting a >1 MiB request terminates it
without crashing the proxy.

Requires ``mitmdump`` on PATH (installed by CI); skipped otherwise.
"""

from __future__ import annotations

import http.client
import json
import os
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path

MITMDUMP = shutil.which("mitmdump")

LARGE_BODY_SIZE = 2 * 1024 * 1024
SMALL_BODY = b"{" + b"a" * 100 + b"}"

VAULT_PAYLOAD = json.dumps(
    {
        "revision": 1,
        "bindings": [
            {
                "name": "llm-api",
                "match": {
                    "schemes": ["http"],
                    "hosts": ["code.example.com"],
                    "methods": ["POST"],
                    "paths": ["/v1/chat/*"],
                },
                "headers": [{"name": "x-api-key", "value": "secret-api-key"}],
            }
        ],
        "redactions": ["secret-api-key"],
    }
).encode("utf-8")


class _VaultUnixServer:
    """Minimal HTTP server on a Unix socket serving the active vault JSON."""

    def __init__(self, socket_path: str, payload: bytes) -> None:
        self.socket_path = socket_path
        self.payload = payload
        self._sock: socket.socket | None = None
        self._stop = threading.Event()

    def start(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.bind(self.socket_path)
        sock.listen(16)
        self._sock = sock
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()  # type: ignore[union-attr]
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn: socket.socket) -> None:
        try:
            conn.settimeout(5)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            conn.sendall(
                b"HTTP/1.1 200 OK\r\n"
                b"content-type: application/json\r\n"
                b"content-length: "
                + str(len(self.payload)).encode("ascii")
                + b"\r\n\r\n"
                + self.payload
            )
        finally:
            conn.close()

    def stop(self) -> None:
        self._stop.set()
        try:
            os.unlink(self.socket_path)
        except OSError:
            pass
        if self._sock is not None:
            self._sock.close()


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@unittest.skipUnless(MITMDUMP, "mitmdump is not installed (pip install mitmproxy==11.0.2)")
class MitmproxyRuntimeRegressionTest(unittest.TestCase):
    """A real mitmdump must not crash when the credential proxy rejects a
    request whose body exceeds stream_large_bodies (mitmproxy 11.0.2 bug)."""

    @classmethod
    def setUpClass(cls) -> None:
        cls._tmp = tempfile.TemporaryDirectory(prefix="egress-mitm-test-")
        cls._vault_path = str(Path(cls._tmp.name) / "vault.sock")
        cls._vault = _VaultUnixServer(cls._vault_path, VAULT_PAYLOAD)
        cls._vault.start()

        script = Path(__file__).parents[1] / "mitmscripts" / "system.py"
        cls._port = _free_port()
        cls._proc = subprocess.Popen(
            [
                MITMDUMP,
                "--listen-host",
                "127.0.0.1",
                "--listen-port",
                str(cls._port),
                "-s",
                str(script),
                "--set",
                "stream_large_bodies=1m",
                "--set",
                "termlog_verbosity=info",
            ],
            env={
                **os.environ,
                "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET": cls._vault_path,
            },
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        cls._log: list[str] = []
        threading.Thread(target=cls._drain, daemon=True).start()

        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if cls._proc.poll() is not None:
                raise RuntimeError(f"mitmdump exited early: {cls._log}")
            try:
                with socket.create_connection(("127.0.0.1", cls._port), timeout=0.25):
                    return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError(f"mitmdump did not start listening: {cls._log}")

    @classmethod
    def _drain(cls) -> None:
        assert cls._proc.stdout is not None
        for line in cls._proc.stdout:
            cls._log.append(line.rstrip())

    @classmethod
    def tearDownClass(cls) -> None:
        if cls._proc.poll() is None:
            cls._proc.terminate()
            try:
                cls._proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                cls._proc.kill()
        cls._vault.stop()
        cls._tmp.cleanup()

    def _conn(self) -> http.client.HTTPConnection:
        return http.client.HTTPConnection("127.0.0.1", self._port, timeout=30)

    def _request(self, path: str, body: bytes) -> tuple[int | None, bytes]:
        conn = self._conn()
        try:
            conn.request(
                "POST",
                f"http://code.example.com{path}",
                body=body,
                headers={"Host": "code.example.com", "content-type": "application/json"},
            )
            response = conn.getresponse()
            return response.status, response.read()
        finally:
            conn.close()

    def _send_expect_continue(self, path: str, body_size: int) -> tuple[int | None, bytes]:
        """Send request headers with ``Expect: 100-continue`` and wait for the
        proxy's decision before uploading the body.

        A killed flow is closed without any response (never sends the 100),
        so this deterministically distinguishes kill from 403 even for bodies
        larger than the client send buffer.
        """
        with socket.create_connection(("127.0.0.1", self._port), timeout=30) as sock:
            sock.settimeout(15)
            sock.sendall(
                f"POST http://code.example.com{path} HTTP/1.1\r\n"
                f"Host: code.example.com\r\n"
                f"Content-Type: application/json\r\n"
                f"Content-Length: {body_size}\r\n"
                f"Expect: 100-continue\r\n"
                f"Connection: close\r\n\r\n".encode("ascii")
            )
            response = b""
            try:
                while b"\r\n\r\n" not in response:
                    chunk = sock.recv(4096)
                    if not chunk:
                        break
                    response += chunk
            except ConnectionError:
                return None, b""
            if not response:
                return None, b""
            status_line = response.split(b"\r\n", 1)[0]
            status_code = int(status_line.split(b" ")[1])
            return status_code, response

    def _assert_proxy_alive(self) -> None:
        status, _ = self._request("/v1/chat/completions/../admin", SMALL_BODY)
        self.assertEqual(403, status)

    def _wait_for_log(self, needle: str, timeout: float = 10.0) -> bool:
        """Poll the drained mitmdump log; termlog writes are asynchronous."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if any(needle in line for line in self._log):
                return True
            time.sleep(0.05)
        return False

    def _assert_no_crash(self) -> None:
        time.sleep(0.2)
        merged = "\n".join(self._log)
        self.assertNotIn("NotImplementedError", merged)
        self.assertNotIn("Traceback", merged)

    def test_small_rejected_request_gets_403(self) -> None:
        status, body = self._request("/v1/chat/completions/../admin", SMALL_BODY)
        self.assertEqual(403, status)
        self.assertIn(b"ambiguous", body)

    def test_large_rejected_request_kills_without_crash(self) -> None:
        """The regression: a >1 MiB body is streamed before the requestheaders
        hook, so rejecting it must kill the flow instead of setting a 403
        (which crashes mitmproxy 11.0.2). A killed flow never answers the
        Expect: 100-continue handshake, so no body is uploaded."""
        status, _ = self._send_expect_continue(
            "/v1/chat/completions/../admin", LARGE_BODY_SIZE
        )
        self.assertIsNone(status)
        self.assertTrue(
            self._wait_for_log("credential proxy: rejected request with ambiguous path")
        )

        # The proxy process must survive and keep serving.
        self._assert_proxy_alive()
        self._assert_no_crash()

    def test_large_encoded_slash_rejection_does_not_crash(self) -> None:
        status, _ = self._send_expect_continue(
            "/v1/chat/completions/123%2f..%2f456", LARGE_BODY_SIZE
        )
        self.assertIsNone(status)
        self._assert_proxy_alive()
        self._assert_no_crash()

    def test_large_normal_request_injects_header_at_requestheaders(self) -> None:
        """Header injection must happen before the streamed body is forwarded;
        the requestheaders hook fires before the 100-continue is sent, so the
        addon log line is observable without uploading the body."""
        status, _ = self._send_expect_continue("/v1/chat/completions", LARGE_BODY_SIZE)
        self.assertEqual(100, status)
        self.assertTrue(self._wait_for_log("credential proxy: applied binding="))
        merged = "\n".join(self._log)
        self.assertIn("headers=x-api-key", merged)
        self.assertNotIn("secret-api-key", merged)
        self._assert_no_crash()


if __name__ == "__main__":
    unittest.main()
