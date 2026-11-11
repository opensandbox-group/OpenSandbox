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
import json
import os
import select
import shutil
import socket
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path

MITMDUMP = shutil.which("mitmdump")

# Synthetic, non-sensitive credentials: they only prove that the vault-injected
# business credential and the upstream proxy credential stay separated.
VAULT_AUTH = "Bearer synthetic-vault-token"
PROXY_AUTH = "Basic cHJveHktdGVzdDp0b2tlbg=="
VAULT_PAYLOAD = json.dumps(
    {
        "revision": 1,
        "bindings": [
            {
                "name": "compat-api",
                "match": {
                    "schemes": ["http"],
                    "hosts": ["code.example.com"],
                    "methods": ["GET"],
                    "paths": ["/v1/secure"],
                },
                "headers": [{"name": "Authorization", "value": VAULT_AUTH}],
            }
        ],
        "redactions": [VAULT_AUTH],
    }
).encode("utf-8")


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class _ConnectProxy:
    """Minimal CONNECT proxy: records the CONNECT request, then tunnels bytes."""

    def __init__(
        self,
        routes: dict[tuple[str, int], tuple[str, int]] | None = None,
    ) -> None:
        self.port = _free_port()
        self.requests: list[dict[str, str]] = []
        self._routes = routes or {}
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
                        "authorization": headers.get("authorization", ""),
                    }
                )
            dial_host, dial_port = self._routes.get(
                (host.decode(), int(port)), (host.decode(), int(port))
            )
            target = socket.create_connection((dial_host, dial_port), timeout=10)
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
        self.requests: list[dict[str, object]] = []
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
            head = data.split(b"\r\n\r\n", 1)[0]
            lines = head.split(b"\r\n")
            headers: dict[str, str] = {}
            for line in lines[1:]:
                k, _, v = line.partition(b":")
                headers[k.strip().lower().decode("latin1")] = (
                    v.strip().decode("latin1")
                )
            with self._lock:
                self.hits += 1
                self.requests.append(
                    {
                        "request-line": lines[0].decode("latin1"),
                        "headers": headers,
                    }
                )
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


class _VaultUnixServer:
    """Minimal HTTP server on a Unix socket serving the active vault JSON."""

    def __init__(self, socket_path: str) -> None:
        self.socket_path = socket_path
        self._sock: socket.socket | None = None
        self._stop = threading.Event()
        self._lock = threading.Lock()
        self._mode = "normal"

    def start(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.bind(self.socket_path)
        sock.listen(16)
        self._sock = sock
        threading.Thread(target=self._serve, daemon=True).start()

    def _serve(self) -> None:
        assert self._sock is not None
        while not self._stop.is_set():
            try:
                conn, _ = self._sock.accept()
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
            with self._lock:
                mode = self._mode
            if mode == "server-error":
                body = b"vault-private-diagnostic"
                conn.sendall(
                    b"HTTP/1.1 503 Service Unavailable\r\n"
                    b"content-length: "
                    + str(len(body)).encode("ascii")
                    + b"\r\n\r\n"
                    + body
                )
                return
            if b'\r\nif-none-match: "compat-v1"\r\n' in data.lower():
                conn.sendall(
                    b"HTTP/1.1 304 Not Modified\r\n"
                    b'etag: "compat-v1"\r\n'
                    b"content-length: 0\r\n\r\n"
                )
                return
            conn.sendall(
                b"HTTP/1.1 200 OK\r\n"
                b'etag: "compat-v1"\r\n'
                b"content-type: application/json\r\n"
                b"content-length: "
                + str(len(VAULT_PAYLOAD)).encode("ascii")
                + b"\r\n\r\n"
                + VAULT_PAYLOAD
            )
        except OSError:
            pass
        finally:
            conn.close()

    def set_mode(self, mode: str) -> None:
        with self._lock:
            self._mode = mode

    def stop(self) -> None:
        self._stop.set()
        try:
            os.unlink(self.socket_path)
        except OSError:
            pass
        if self._sock is not None:
            self._sock.close()


def _start_mitmdump(
    port: int,
    env_extra: dict[str, str],
    *extra_args: str,
    load_system_addon: bool = False,
) -> tuple[subprocess.Popen, list[str]]:
    scripts_dir = Path(__file__).parents[1] / "mitmscripts"
    script_args: list[str] = []
    if load_system_addon:
        script_args.extend(["-s", str(scripts_dir / "system.py")])
    script_args.extend(["-s", str(scripts_dir / "upstream_proxy.py")])
    proc = subprocess.Popen(
        [
            MITMDUMP,
            "--listen-host",
            "127.0.0.1",
            "--listen-port",
            str(port),
            *script_args,
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
    if proc.stdout is not None:
        proc.stdout.close()


def _proxy_get_host(
    port: int, url: str, host: str
) -> tuple[int, bytes]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    try:
        conn.request("GET", url, headers={"Host": host})
        resp = conn.getresponse()
        return resp.status, resp.read()
    finally:
        conn.close()


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
        cls._target = _TargetServer()
        cls._target.start()
        # The CONNECT dial is routed: lets tests keep a business FQDN in the
        # CONNECT authority while dialing the local target.
        cls._proxy = _ConnectProxy(
            routes={("code.example.com", 80): ("127.0.0.1", cls._target.port)}
        )
        cls._proxy.start()
        cls._tmp = tempfile.TemporaryDirectory(prefix="egress-upstream-test-")
        cls._vault_path = str(Path(cls._tmp.name) / "vault.sock")
        cls._vault = _VaultUnixServer(cls._vault_path)
        cls._vault.start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls._vault.stop()
        cls._proxy.stop()
        cls._target.stop()
        cls._tmp.cleanup()

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

    def test_hostname_upstream_proxy_chained(self) -> None:
        # A hostname proxy endpoint must stay a hostname in server.address
        # while the dial resolves it: the server_connect guard compares
        # address[0] to the configured host, so this only passes if mitmproxy
        # keeps the configured name rather than the resolved IP. The endpoint
        # must be a dotted domain — dotless names (localhost included) are
        # rejected at load because they resolve differently through resolver
        # search lists — and it must dial the local relay, so use the public
        # sslip.io wildcard that maps back to 127.0.0.1.
        port = _free_port()
        proc, log = _start_mitmdump(
            port,
            {
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1.sslip.io:{self._proxy.port}",
            },
        )
        try:
            before = len(self._proxy.requests)
            status, body = _proxy_get(port, self._target_url())
            self.assertEqual(200, status, log)
            self.assertEqual(b"upstream-proxy-e2e-ok", body)
            new = self._proxy.requests[before:]
            self.assertEqual(1, len(new))
            self.assertEqual(
                f"127.0.0.1:{self._target.port}", new[0]["authority"]
            )
        finally:
            _stop(proc)

    def test_via_cleared_fails_closed(self) -> None:
        # A later-loaded addon clearing server_conn.via must not fall back to
        # a direct dial: server_connect refuses it, so the request fails and
        # neither the proxy nor the target is contacted.
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            breaker = Path(tmp) / "break_via.py"
            breaker.write_text(
                "def requestheaders(flow):\n"
                "    flow.server_conn.via = None\n"
            )
            port = _free_port()
            proc, log = _start_mitmdump(
                port,
                {
                    "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
                },
                "-s",
                str(breaker),
            )
            try:
                proxy_before = len(self._proxy.requests)
                with self._target._lock:
                    hits_before = self._target.hits
                status, _ = _proxy_get(port, self._target_url())
                self.assertEqual(502, status, log)
                self.assertEqual(proxy_before, len(self._proxy.requests), log)
                with self._target._lock:
                    self.assertEqual(hits_before, self._target.hits)
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

        openssl = shutil.which("openssl")
        if openssl is None:
            self.skipTest("openssl is not installed")
        with tempfile.TemporaryDirectory() as tmp:
            cert, key = Path(tmp) / "c.pem", Path(tmp) / "k.pem"
            gen = subprocess.run(
                [
                    openssl, "req", "-x509", "-newkey", "rsa:2048",
                    "-keyout", str(key), "-out", str(cert),
                    "-days", "1", "-nodes", "-subj", "/CN=example.test",
                    "-addext", "subjectAltName=DNS:example.test",
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
                    # Strict upstream verification: the self-signed target cert
                    # is trusted only via the extra CA bundle, not ssl_insecure.
                    "--set", f"ssl_verify_upstream_trusted_ca={cert}",
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

    def test_credential_vault_and_proxy_auth_remain_separated(self) -> None:
        # system.py (credential vault) runs before upstream_proxy.py: the
        # vault-injected business Authorization reaches the target, while the
        # upstream Proxy-Authorization stays on the outer CONNECT only.
        port = _free_port()
        proc, log = _start_mitmdump(
            port,
            {
                "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET": self._vault_path,
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH": PROXY_AUTH,
            },
            load_system_addon=True,
        )
        try:
            proxy_before = len(self._proxy.requests)
            with self._target._lock:
                target_before = len(self._target.requests)
            status, body = _proxy_get_host(
                port, "http://code.example.com/v1/secure", "code.example.com"
            )
            self.assertEqual(200, status, log)
            self.assertEqual(b"upstream-proxy-e2e-ok", body)

            new_connects = self._proxy.requests[proxy_before:]
            self.assertEqual(1, len(new_connects), log)
            self.assertEqual("code.example.com:80", new_connects[0]["authority"])
            self.assertEqual(PROXY_AUTH, new_connects[0]["proxy-authorization"])
            self.assertEqual("", new_connects[0]["authorization"])

            with self._target._lock:
                new_requests = self._target.requests[target_before:]
            self.assertEqual(1, len(new_requests), log)
            headers = new_requests[0]["headers"]
            assert isinstance(headers, dict)
            self.assertEqual(VAULT_AUTH, headers.get("authorization"))
            self.assertNotIn("proxy-authorization", headers)

            merged = "\n".join(log)
            self.assertNotIn(VAULT_AUTH, merged)
            self.assertNotIn(PROXY_AUTH, merged)
        finally:
            _stop(proc)

    def test_credential_vault_lookup_failure_stays_fail_closed_before_connect(
        self,
    ) -> None:
        # A vault lookup failure must deny the request in system.py before any
        # upstream CONNECT is attempted.
        self._vault.set_mode("server-error")
        port = _free_port()
        proc, log = _start_mitmdump(
            port,
            {
                "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET": self._vault_path,
                "OPENSANDBOX_EGRESS_UPSTREAM_PROXY": f"http://127.0.0.1:{self._proxy.port}",
            },
            load_system_addon=True,
        )
        try:
            proxy_before = len(self._proxy.requests)
            with self._target._lock:
                target_before = self._target.hits
            status, body = _proxy_get_host(
                port, "http://code.example.com/v1/secure", "code.example.com"
            )
            self.assertEqual(503, status, log)
            self.assertEqual(b"credential proxy unavailable\n", body)
            self.assertEqual(proxy_before, len(self._proxy.requests), log)
            with self._target._lock:
                self.assertEqual(target_before, self._target.hits)
            merged = "\n".join(log)
            self.assertNotIn("vault-private-diagnostic", merged)
            self.assertNotIn(VAULT_AUTH, merged)
            self.assertNotIn(PROXY_AUTH, merged)
        finally:
            self._vault.set_mode("normal")
            _stop(proc)


if __name__ == "__main__":
    unittest.main()
