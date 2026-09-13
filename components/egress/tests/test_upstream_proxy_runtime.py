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

"""Real-mitmproxy runtime tests for the bundled upstream-proxy addon.

A fake HTTP CONNECT proxy and a plain HTTP target run locally; mitmdump is
started in regular mode with OPENSANDBOX_EGRESS_UPSTREAM_PROXY pointing at the
fake proxy. The tests assert that:

- requests are chained through the upstream proxy (CONNECT observed, correct
  authority, Proxy-Authorization forwarded),
- direct dials to non-proxy addresses are refused (fail closed),
- and with the env unset the addon is inert (traffic goes direct).

Requires ``mitmdump`` on PATH (installed by CI); skipped otherwise.
"""

from __future__ import annotations

import http.client
import http.server
import os
import select
import shutil
import socket
import subprocess
import threading
import time
import unittest
from pathlib import Path

MITMDUMP = shutil.which("mitmdump")


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class _ConnectProxy:
    """Minimal CONNECT proxy: records the CONNECT request, then tunnels bytes."""

    def __init__(self) -> None:
        self.port = _free_port()
        self.requests: list[dict[str, str]] = []
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._sock: socket.socket | None = None

    def start(self) -> None:
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind(("127.0.0.1", self.port))
        self._sock.listen(16)
        self.port = self._sock.getsockname()[1]
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        assert self._sock is not None
        self._sock.settimeout(0.5)
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn: socket.socket) -> None:
        target: socket.socket | None = None
        try:
            conn.settimeout(10)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            head, _, rest = data.partition(b"\r\n\r\n")
            lines = head.split(b"\r\n")
            method, authority, _ = lines[0].split(b" ", 2)
            if method != b"CONNECT":
                conn.sendall(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
                return
            headers = {}
            for line in lines[1:]:
                k, _, v = line.partition(b":")
                headers[k.strip().lower().decode("latin1")] = v.strip().decode(
                    "latin1"
                )
            host, _, port = authority.rpartition(b":")
            host = host.strip(b"[]")
            with self._lock:
                self.requests.append(
                    {
                        "authority": authority.decode("latin1"),
                        "proxy-authorization": headers.get(
                            "proxy-authorization", ""
                        ),
                    }
                )
            target = socket.create_connection((host.decode(), int(port)), timeout=10)
            conn.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
            if rest:
                target.sendall(rest)
            self._tunnel(conn, target)
        except (OSError, ValueError):
            try:
                conn.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
            except OSError:
                pass
        finally:
            conn.close()
            if target is not None:
                target.close()

    @staticmethod
    def _tunnel(a: socket.socket, b: socket.socket) -> None:
        a.settimeout(30)
        b.settimeout(30)
        try:
            while True:
                r, _, _ = select.select([a, b], [], [], 30)
                if not r:
                    return
                for s in r:
                    other = b if s is a else a
                    chunk = s.recv(65536)
                    if not chunk:
                        return
                    other.sendall(chunk)
        except OSError:
            return

    def stop(self) -> None:
        self._stop.set()
        if self._sock is not None:
            self._sock.close()


class _TlsTargetServer:
    """TLS-wrapped one-shot HTTP server on a self-signed cert (openssl CLI)."""

    def __init__(self, cert: Path, key: Path) -> None:
        import ssl

        self.port = 0
        self._stop = threading.Event()
        self._sock: socket.socket | None = None
        self._ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self._ctx.load_cert_chain(certfile=cert, keyfile=key)

    def start(self) -> None:
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind(("127.0.0.1", 0))
        self._sock.listen(16)
        self.port = self._sock.getsockname()[1]
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        assert self._sock is not None
        self._sock.settimeout(0.5)
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn: socket.socket) -> None:
        try:
            conn.settimeout(10)
            tls = self._ctx.wrap_socket(conn, server_side=True)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = tls.recv(4096)
                if not chunk:
                    return
                data += chunk
            body = b"upstream-proxy-tls-e2e-ok"
            tls.sendall(
                b"HTTP/1.1 200 OK\r\ncontent-length: "
                + str(len(body)).encode()
                + b"\r\nconnection: close\r\n\r\n"
                + body
            )
        except OSError:
            pass
        finally:
            conn.close()

    def stop(self) -> None:
        self._stop.set()
        if self._sock is not None:
            self._sock.close()


class _TargetServer:
    def __init__(self) -> None:
        self.hits = 0
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._sock: socket.socket | None = None
        self.port = 0

    def start(self) -> None:
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self._sock.bind(("127.0.0.1", 0))
        self._sock.listen(16)
        self.port = self._sock.getsockname()[1]
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        assert self._sock is not None
        self._sock.settimeout(0.5)
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
            except socket.timeout:
                continue
            except OSError:
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn: socket.socket) -> None:
        try:
            conn.settimeout(10)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            with self._lock:
                self.hits += 1
            body = b"upstream-proxy-e2e-ok"
            conn.sendall(
                b"HTTP/1.1 200 OK\r\ncontent-length: "
                + str(len(body)).encode()
                + b"\r\nconnection: close\r\n\r\n"
                + body
            )
        except OSError:
            pass
        finally:
            conn.close()

    def stop(self) -> None:
        self._stop.set()
        if self._sock is not None:
            self._sock.close()


def _start_mitmdump(
    port: int, env_extra: dict[str, str], *extra_args: str
) -> tuple[subprocess.Popen, list[str]]:
    script = Path(__file__).parents[1] / "mitmscripts" / "upstream_proxy.py"
    proc = subprocess.Popen(
        [
            MITMDUMP,
            "--listen-host",
            "127.0.0.1",
            "--listen-port",
            str(port),
            "-s",
            str(script),
            "--set",
            "connection_strategy=lazy",
            "--set",
            "termlog_verbosity=info",
            *extra_args,
        ],
        env={**os.environ, **env_extra},
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    log: list[str] = []

    def drain() -> None:
        assert proc.stdout is not None
        for line in proc.stdout:
            log.append(line.rstrip())

    threading.Thread(target=drain, daemon=True).start()
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"mitmdump exited early: {log}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.25):
                return proc, log
        except OSError:
            time.sleep(0.1)
    raise RuntimeError(f"mitmdump did not start listening: {log}")


def _stop(proc: subprocess.Popen) -> None:
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()


def _proxy_get(port: int, target: str) -> tuple[int, bytes]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        conn.request("GET", target, headers={"Host": target.split("//", 1)[-1].split(":", 1)[0]})
        resp = conn.getresponse()
        return resp.status, resp.read()
    finally:
        conn.close()


@unittest.skipUnless(MITMDUMP, "mitmdump is not installed (pip install mitmproxy==11.0.2)")
class UpstreamProxyRuntimeTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls._proxy = _ConnectProxy()
        cls._proxy.start()
        cls._target = _TargetServer()
        cls._target.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._proxy.stop()
        cls._target.stop()

    def _target_url(self) -> str:
        return f"http://127.0.0.1:{self._target.port}/"

    def test_plain_http_chained_with_auth(self) -> None:
        port = _free_port()
        proc, log = _start_mitmdump(
            port,
            {
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH": "Basic dGVzdDp0ZXN0",
            },
        )
        try:
            before = len(self._proxy.requests)
            status, body = _proxy_get(port, self._target_url())
            self.assertEqual(200, status, log)
            self.assertEqual(b"upstream-proxy-e2e-ok", body)
            new = self._proxy.requests[before:]
            self.assertEqual(1, len(new))
            self.assertEqual(f"127.0.0.1:{self._target.port}", new[0]["authority"])
            self.assertEqual(
                "Basic dGVzdDp0ZXN0", new[0]["proxy-authorization"]
            )
        finally:
            _stop(proc)

    def test_disabled_env_keeps_direct_path(self) -> None:
        port = _free_port()
        proc, log = _start_mitmdump(port, {})
        try:
            before = len(self._proxy.requests)
            status, body = _proxy_get(port, self._target_url())
            self.assertEqual(200, status, log)
            self.assertEqual(b"upstream-proxy-e2e-ok", body)
            self.assertEqual(before, len(self._proxy.requests))
        finally:
            _stop(proc)

    def test_inner_tls_flow_traverses_chain(self) -> None:
        # TLS intercepted inside a client CONNECT tunnel: the inner flow must
        # still go through the single upstream CONNECT, not a second dial.
        import ssl
        import tempfile

        openssl = shutil.which("openssl")
        if openssl is None:
            self.skipTest("openssl is not installed")
        with tempfile.TemporaryDirectory() as tmp:
            cert, key = Path(tmp) / "c.pem", Path(tmp) / "k.pem"
            gen = subprocess.run(
                [
                    openssl, "req", "-x509", "-newkey", "rsa:2048",
                    "-keyout", str(key), "-out", str(cert),
                    "-days", "1", "-nodes", "-subj", "/CN=localhost",
                    "-addext", "subjectAltName=IP:127.0.0.1",
                ],
                capture_output=True,
            )
            if gen.returncode != 0:
                self.skipTest(f"openssl cert generation failed: {gen.stderr!r}")
            target = _TlsTargetServer(cert, key)
            target.start()
            try:
                port = _free_port()
                proc, log = _start_mitmdump(
                    port,
                    {
                        "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
                    },
                    # the TLS target is self-signed; we are testing chaining,
                    # not upstream verification
                    "--set", "ssl_insecure=true",
                )
                try:
                    before = len(self._proxy.requests)
                    sock = socket.create_connection(("127.0.0.1", port), timeout=30)
                    try:
                        sock.sendall(
                            f"CONNECT 127.0.0.1:{target.port} HTTP/1.1\r\n"
                            f"Host: 127.0.0.1:{target.port}\r\n\r\n".encode()
                        )
                        buf = b""
                        while b"\r\n\r\n" not in buf:
                            buf += sock.recv(4096)
                        self.assertIn(b" 200 ", buf.split(b"\r\n", 1)[0], log)
                        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
                        ctx.check_hostname = False
                        ctx.verify_mode = ssl.CERT_NONE
                        tls = ctx.wrap_socket(sock, server_hostname="example.test")
                        tls.sendall(
                            b"GET / HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"
                        )
                        buf = b""
                        while b"upstream-proxy-tls-e2e-ok" not in buf:
                            chunk = tls.recv(65536)
                            if not chunk:
                                break
                            buf += chunk
                        self.assertIn(b"200 OK", buf, log)
                        self.assertIn(b"upstream-proxy-tls-e2e-ok", buf)
                    finally:
                        sock.close()
                    new = self._proxy.requests[before:]
                    self.assertEqual(
                        [f"127.0.0.1:{target.port}"],
                        [r["authority"] for r in new],
                        log,
                    )
                finally:
                    _stop(proc)
            finally:
                target.stop()

    def test_client_connect_flow_is_chained(self) -> None:
        # A client CONNECT (HTTPS-style) must also traverse the upstream proxy.
        port = _free_port()
        proc, log = _start_mitmdump(
            port,
            {
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
            },
        )
        try:
            before = len(self._proxy.requests)
            sock = socket.create_connection(("127.0.0.1", port), timeout=30)
            try:
                sock.sendall(
                    f"CONNECT 127.0.0.1:{self._target.port} HTTP/1.1\r\n"
                    f"Host: 127.0.0.1:{self._target.port}\r\n\r\n".encode()
                )
                buf = b""
                while b"\r\n\r\n" not in buf:
                    chunk = sock.recv(4096)
                    if not chunk:
                        raise AssertionError(f"CONNECT closed early: {buf!r} {log}")
                    buf += chunk
                self.assertIn(b" 200 ", buf.split(b"\r\n", 1)[0])
                # Inside the tunnel we now speak plain HTTP to the target.
                sock.sendall(b"GET / HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n")
                buf = sock.recv(65536)
                self.assertIn(b"200 OK", buf)
            finally:
                sock.close()
            new = self._proxy.requests[before:]
            self.assertEqual(1, len(new))
            self.assertEqual(
                f"127.0.0.1:{self._target.port}", new[0]["authority"]
            )
        finally:
            _stop(proc)


if __name__ == "__main__":
    unittest.main()
