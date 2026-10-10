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

"""Raw-wire ECH coverage for the live credential-bound TLS decision.

Existing unit tests load ``system.py`` with a *fake* ``mitmproxy`` package in
``sys.modules``; real-library parsing therefore runs in an isolated
subprocess (``fixtures/real_wire_worker.py``), because unittest discovery
executes every ``test_*`` module inside one shared process.

Layers covered:

- ``mitmproxy.tls.ClientHello`` parses real wire bytes; the addon inspects
  the actual ``extensions`` list and detects ECH (0xfe0d) before outer-SNI
  matching. A bound outer SNI with an ECH extension passes through opaquely
  (``ignore_connection``, no admission token), while the identical hello
  without the extension is decrypted and tracked.
- A real ``mitmdump`` running the addon under the live admission bundle
  proves the opaque boundary end to end: an ECH-bearing ClientHello with a
  bound outer SNI is answered by the *origin's own certificate* (not the
  mitmproxy CA), while the same hello without ECH is intercepted.

Requires the real ``mitmproxy`` package (this file cannot run under the
fake-module harness); the mitmdump segment skips when the binary is absent.
"""

from __future__ import annotations

import hashlib
import http.client
import http.server
import ipaddress
import json
import shutil
import socket
import socketserver
import ssl
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"
TESTS = Path(__file__).resolve().parent
MITMDUMP = shutil.which("mitmdump")
CONTROL = "ech-control"
SUBJECT = "ech-subject"
TOKEN = "c" * 43
BOUND = "bound.example.com"
ECH_EXTENSION_TYPE = 0xFE0D


def _build_client_hello(sni: str, extra_extensions: tuple = ()) -> bytes:
    """Minimal TLS 1.2 ClientHello handshake body (no record/header wrapper).

    TLS 1.2 keeps the Certificate handshake message plaintext so the test
    can compare the peer's leaf DER with the origin's own certificate. The
    ECDHE/signature extensions make the hello acceptable to a stock OpenSSL
    server; unknown extensions (ECH 0xfe0d) are ignored by the origin.
    """
    name = sni.encode("ascii")
    entry = b"\x00" + len(name).to_bytes(2, "big") + name
    sni_body = len(entry).to_bytes(2, "big") + entry
    extensions = (0).to_bytes(2, "big") + len(sni_body).to_bytes(2, "big") + sni_body
    # supported_groups: secp256r1, secp384r1
    groups = b"\x00\x04\x00\x17\x00\x18"
    extensions += (0x000A).to_bytes(2, "big") + len(groups).to_bytes(2, "big") + groups
    # ec_point_formats: uncompressed
    formats = b"\x01\x00"
    extensions += (0x000B).to_bytes(2, "big") + len(formats).to_bytes(2, "big") + formats
    # signature_algorithms: rsa_pkcs1_sha256, rsa_pkcs1_sha384
    sigalgs = b"\x00\x04\x04\x01\x05\x01"
    extensions += (0x000D).to_bytes(2, "big") + len(sigalgs).to_bytes(2, "big") + sigalgs
    for extension_type, payload in extra_extensions:
        extensions += (
            extension_type.to_bytes(2, "big")
            + len(payload).to_bytes(2, "big")
            + payload
        )
    return (
        b"\x03\x03"  # legacy_version (TLS 1.2)
        + b"\x11" * 32  # random
        + b"\x00"  # empty session_id
        + (2).to_bytes(2, "big") + b"\xc0\x2f"  # TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
        + b"\x01\x00"  # null compression
        + len(extensions).to_bytes(2, "big") + extensions
    )


WORKER = TESTS / "fixtures" / "real_wire_worker.py"


def _decision_payload(hosts: list[str]) -> bytes:
    bindings = [
        {
            "name": f"api-{index}",
            "match": {
                "schemes": ["https"],
                "hosts": [host],
                "methods": ["GET"],
                "paths": ["/v1/*"],
            },
            "headers": [{"name": "Private-Token", "value": "ech-secret"}],
        }
        for index, host in enumerate(hosts)
    ]
    selectors = sorted(set(hosts))
    return json.dumps(
        {
            "version": 1,
            "vaultRevision": 1,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active" if bindings else "active-empty",
            "tlsBindingHostSelectors": selectors,
            "fullRenderedBindings": bindings,
            "redactions": ["ech-secret"],
        },
        separators=(",", ":"),
    ).encode()


class ECHWireParseTest(unittest.TestCase):
    """Real ``mitmproxy.tls.ClientHello`` parsing of crafted wire bytes.

    Runs in an isolated subprocess: ``unittest`` discovery loads this file
    in the same process as the fake-module unit tests, so real-library
    parsing must happen where fake ``sys.modules`` entries cannot leak.
    """

    def test_wire_parse_modes(self) -> None:
        import subprocess

        result = subprocess.run(
            [sys.executable, str(WORKER), "ech-parse"],
            capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(
            result.returncode, 0,
            f"worker failed:\n{result.stdout}\n{result.stderr}",
        )
        self.assertIn("ech-opaque ok", result.stdout)
        self.assertIn("ech-absent-decrypt ok", result.stdout)
        self.assertIn("ech-empty-list ok", result.stdout)


# -- Real-mitmdump opaque boundary evidence --------------------------------


class _EchoHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args) -> None:
        pass

    def do_GET(self) -> None:
        body = json.dumps({"private_token": ""}).encode()
        self.send_response(200)
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class _TLSOriginServer(socketserver.ThreadingTCPServer):
    """Wrap accepted sockets inside get_request so a failed TLS handshake
    closes the raw socket instead of leaking it from ``accept()``."""

    daemon_threads = True

    def __init__(self, address, handler, context: ssl.SSLContext) -> None:
        self.tls_context = context
        super().__init__(address, handler)

    def get_request(self):
        sock, address = self.socket.accept()
        try:
            wrapped = self.tls_context.wrap_socket(sock, server_side=True)
        except Exception:
            sock.close()
            raise
        wrapped.settimeout(60)
        return wrapped, address


def _generate_origin_certificate(directory: Path) -> None:
    import datetime

    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import rsa
    from cryptography.x509.oid import NameOID

    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, BOUND)])
    certificate = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(datetime.datetime(2020, 1, 1))
        .not_valid_after(datetime.datetime(2050, 1, 1))
        .add_extension(
            x509.SubjectAlternativeName(
                [x509.DNSName(BOUND), x509.IPAddress(ipaddress.ip_address("127.0.0.1"))]
            ),
            critical=False,
        )
        .sign(key, hashes.SHA256())
    )
    (directory / "origin.pem").write_bytes(
        certificate.public_bytes(serialization.Encoding.PEM)
        + key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.TraditionalOpenSSL,
            serialization.NoEncryption(),
        )
    )


def _read_server_certificate_der(tls_bytes_sock: socket.socket) -> bytes:
    """Read TLS records until the Certificate handshake message; return leaf DER."""
    buffered = b""
    handshake = b""
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            chunk = tls_bytes_sock.recv(65536)
        except socket.timeout:
            continue
        if not chunk:
            raise ConnectionError("proxy closed the connection before the Certificate")
        buffered += chunk
        # Walk TLS records; handshake messages may span records, so parse
        # them from the concatenated handshake stream.
        offset = 0
        while offset + 5 <= len(buffered):
            record_type = buffered[offset]
            record_len = int.from_bytes(buffered[offset + 3:offset + 5], "big")
            if offset + 5 + record_len > len(buffered):
                break  # incomplete record; wait for more bytes
            payload = buffered[offset + 5:offset + 5 + record_len]
            offset += 5 + record_len
            if record_type != 0x16:
                continue
            handshake += payload
            inner = 0
            while inner + 4 <= len(handshake):
                hs_type = handshake[inner]
                hs_len = int.from_bytes(handshake[inner + 1:inner + 4], "big")
                if inner + 4 + hs_len > len(handshake):
                    break  # incomplete handshake; more record bytes may follow
                body = handshake[inner + 4:inner + 4 + hs_len]
                inner += 4 + hs_len
                if hs_type == 0x0B:  # Certificate
                    total = int.from_bytes(body[0:3], "big")
                    cert_len = int.from_bytes(body[3:6], "big")
                    assert 3 + 3 + cert_len <= 3 + total, "bad Certificate body"
                    return body[6:6 + cert_len]
            handshake = handshake[inner:]
    raise TimeoutError("no Certificate message arrived")


def _connect_through(proxy_port: int, target_port: int) -> socket.socket:
    sock = socket.create_connection(("127.0.0.1", proxy_port), timeout=10)
    try:
        sock.settimeout(10)
        sock.sendall(
            f"CONNECT 127.0.0.1:{target_port} HTTP/1.1\r\n"
            f"Host: 127.0.0.1:{target_port}\r\n\r\n".encode()
        )
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = sock.recv(4096)
            if not chunk:
                raise ConnectionError("proxy closed while establishing the tunnel")
            head += chunk
        assert head.startswith(b"HTTP/1.1 200"), head[:80]
        return sock
    except Exception:
        sock.close()
        raise


@unittest.skipUnless(MITMDUMP, "mitmdump is not installed")
class ECHOpaqueRuntimeTest(unittest.TestCase):
    """End-to-end opacity: real mitmdump + real ClientHello wire bytes."""

    def setUp(self) -> None:
        import subprocess

        self._subprocess = subprocess
        self.temporary = tempfile.TemporaryDirectory(prefix="osch-", dir="/tmp")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        _generate_origin_certificate(self.root)
        self.origin_port = self._free_port()
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(self.root / "origin.pem"))
        self.origin = _TLSOriginServer(
            ("127.0.0.1", self.origin_port), _EchoHandler, context
        )
        self.addCleanup(self.origin.server_close)
        self.addCleanup(self.origin.shutdown)
        threading.Thread(target=self.origin.serve_forever, daemon=True).start()

        self.proxy_port = self._free_port()
        self.socket_path = str(self.root / "receiver.sock")
        self.log = (self.root / "proxy.log").open("w+")
        self.addCleanup(self.log.close)
        env = {
            **{
                key: value
                for key, value in __import__("os").environ.items()
                if not key.startswith("OPENSANDBOX_EGRESS_REVISION_")
            },
            "OPENSANDBOX_EGRESS_REVISION_IPC_SOCKET": self.socket_path,
            "OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN": TOKEN,
            "OPENSANDBOX_EGRESS_REVISION_CONTROL_GENERATION": CONTROL,
            "OPENSANDBOX_EGRESS_REVISION_SUBJECT_GENERATION": SUBJECT,
            "OPENSANDBOX_EGRESS_REVISION_MAX_SNAPSHOT_BYTES": "1048576",
            "OPENSANDBOX_EGRESS_REVISION_TLS_CAPACITY": "64",
            "OPENSANDBOX_EGRESS_REVISION_REQUEST_CAPACITY": "256",
            "OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS": "1",
        }
        self.process = self._subprocess.Popen(
            [
                MITMDUMP,
                "--listen-host", "127.0.0.1",
                "--listen-port", str(self.proxy_port),
                "--set", "confdir=" + str(self.root / "ca"),
                "--set", "connection_strategy=lazy",
                "--set", "flow_detail=0",
                "--set", "ssl_verify_upstream_trusted_ca=" + str(self.root / "origin.pem"),
                "-s", str(MITMSCRIPTS / "system.py"),
            ],
            env=env,
            stdout=self.log,
            stderr=self._subprocess.STDOUT,
        )
        self.addCleanup(self._stop_process)
        self._wait_ready()
        self._install_snapshot()

    def _stop_process(self) -> None:
        if self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except self._subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)

    @staticmethod
    def _free_port() -> int:
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        return port

    def _ipc(self, path: str, value: dict | None = None) -> tuple[int, dict]:
        import base64  # noqa: F401 - parity with sibling harness style

        body = None if value is None else json.dumps(value).encode()
        headers = {"Authorization": "Bearer " + TOKEN}
        if body is not None:
            headers["Content-Type"] = "application/json"
        connection = http.client.HTTPConnection("localhost", timeout=5)
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sock.settimeout(5)
            sock.connect(self.socket_path)
        except Exception:
            sock.close()
            raise
        connection.sock = sock
        try:
            connection.request(
                "POST" if body is not None else "GET", path, body=body, headers=headers
            )
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def _wait_ready(self) -> None:
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                self.log.flush()
                raise AssertionError(
                    "mitmdump exited before readiness: "
                    + (self.root / "proxy.log").read_text(errors="replace")[-2000:]
                )
            if (self.root / "ca" / "mitmproxy-ca-cert.pem").exists() and Path(
                self.socket_path
            ).exists():
                try:
                    status, _ = self._ipc("/v1/revisions/active")
                    if status == 200:
                        return
                except OSError:
                    pass
            time.sleep(0.05)
        self.log.flush()
        raise AssertionError(
            "mitmdump did not become ready: "
            + (self.root / "proxy.log").read_text(errors="replace")[-2000:]
        )

    def _install_snapshot(self) -> None:
        import base64

        payload = _decision_payload([BOUND])
        envelope = {
            "controlGeneration": CONTROL,
            "subjectGeneration": SUBJECT,
            "decisionEpoch": 1,
            "vaultRevision": 1,
            "policyEpoch": 0,
            "digest": hashlib.sha256(payload).hexdigest(),
        }
        status, value = self._ipc(
            "/v1/revisions/prepare",
            {"revision": envelope, "payload": base64.b64encode(payload).decode()},
        )
        assert status == 200, f"prepare failed: {status} {value}"
        status, value = self._ipc("/v1/revisions/commit", {"revision": envelope})
        assert status == 200, f"commit failed: {status} {value}"

    def _origin_leaf_der(self) -> bytes:
        pem = (self.root / "origin.pem").read_text()
        marker = "-----END CERTIFICATE-----"
        return ssl.PEM_cert_to_DER_cert(pem.split(marker)[0] + marker)

    def test_ech_hello_is_opaque_but_same_hello_without_ech_is_mitmd(self) -> None:
        origin_leaf = self._origin_leaf_der()

        # Bound outer SNI + ECH extension: the addon must ignore the
        # connection; the byte stream reaches the origin untouched, so the
        # Certificate message carries the origin's own certificate.
        sock = _connect_through(self.proxy_port, self.origin_port)
        try:
            hello = _build_client_hello(BOUND, [(ECH_EXTENSION_TYPE, b"\x01\x02\x03\x04")])
            record = b"\x16\x03\x01" + (len(hello) + 4).to_bytes(2, "big") + (
                b"\x01" + len(hello).to_bytes(3, "big") + hello
            )
            sock.sendall(record)
            leaf = _read_server_certificate_der(sock)
        finally:
            sock.close()
        self.assertEqual(
            hashlib.sha256(leaf).hexdigest(),
            hashlib.sha256(origin_leaf).hexdigest(),
            "ECH-bearing hello was not opaque: the peer was not the origin",
        )

        # Contrast: the identical hello without the ECH extension is
        # intercepted, so the presented leaf is the mitmproxy CA-signed one.
        sock = _connect_through(self.proxy_port, self.origin_port)
        try:
            hello = _build_client_hello(BOUND)
            record = b"\x16\x03\x01" + (len(hello) + 4).to_bytes(2, "big") + (
                b"\x01" + len(hello).to_bytes(3, "big") + hello
            )
            sock.sendall(record)
            leaf = _read_server_certificate_der(sock)
        finally:
            sock.close()
        self.assertNotEqual(
            hashlib.sha256(leaf).hexdigest(),
            hashlib.sha256(origin_leaf).hexdigest(),
            "bound hello without ECH was not intercepted as expected",
        )


if __name__ == "__main__":
    unittest.main()
