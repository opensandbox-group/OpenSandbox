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

"""Caller-credential tests for the OSEP-0025 test resolver stub (W3.3).

Exercises ``--require-credential`` mode end to end: the stub verifies
``OpenSandbox-Egress-Credential`` JWTs through the resolver-auth SDK
against a local HTTPS JWKS endpoint built from the same fixture
certificate the HTTPS transport tests use. Off-mode compatibility (no
credential required, byte-identical behavior) is asserted here too —
that is the guarantee the egress Go integration suite relies on.

Skipped entirely when ``cryptography`` or the SDK is unavailable, the
same way Go tests skip without python3: the default stub path never
needs either.
"""

from __future__ import annotations

import base64
import hashlib
import http.client
import http.server
import json
import os
import socket
import ssl
import tempfile
import threading
import time
import unittest
from pathlib import Path
from typing import Any

from resolver import resolver

try:
    REPO_ROOT = Path(__file__).resolve().parents[4]
    import sys as _sys

    _sys.path.insert(
        0, str(REPO_ROOT / "sdks" / "resolver-auth" / "python" / "src")
    )
    from cryptography.hazmat.primitives.asymmetric.ed25519 import (  # noqa: F401
        Ed25519PrivateKey,
    )

    HAS_CRYPTO = True
except ImportError:  # pragma: no cover - exercised only on bare python3
    HAS_CRYPTO = False

FIXTURES = Path(__file__).resolve().parent / "fixtures"
CERT = FIXTURES / "test-localhost-cert.pem"
KEY = FIXTURES / "test-localhost-key.pem"

ISSUER = "opensandbox"
AUDIENCE = "credential-resolver@resolver.internal"
KID = "pool-a:v2"
SUBJECT = "sbx_01HZYEXAMPLE"
CREDENTIAL_HEADER = "OpenSandbox-Egress-Credential"

VALID_REQUEST: dict[str, Any] = {
    "subject": SUBJECT,
    "credential": "spi-signer",
    "binding": "internal-api",
    "destination": {
        "scheme": "https",
        "authority": "api.internal.example.com:443",
        "sni": "api.internal.example.com",
    },
    "request": {
        "method": "GET",
        "host": "api.internal.example.com",
        "path": "/v1/foo",
        "headers": {},
    },
}


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


if HAS_CRYPTO:

    def mint(
        key: "Ed25519PrivateKey",
        *,
        kid: str | None = KID,
        iss: str = ISSUER,
        aud: str = AUDIENCE,
        sub: str = SUBJECT,
        iat: int | None = None,
        exp: int | None = None,
        header_extra: dict[str, Any] | None = None,
    ) -> str:
        """Mint one compact-JWS credential (mirror of the SDK tests'
        vectors.mint — keep the two in sync)."""
        now = int(time.time())
        header: dict[str, Any] = {"alg": "EdDSA", "typ": "JWT"}
        if kid is not None:
            header["kid"] = kid
        if header_extra is not None:
            header.update(header_extra)
        payload = {
            "iss": iss,
            "aud": aud,
            "sub": sub,
            "iat": now if iat is None else iat,
            "exp": (now + 3600) if exp is None else exp,
            "jti": "kX9sT2vZ8qLm4NpR6wYb3A",
        }
        header_b64 = _b64url(json.dumps(header).encode())
        payload_b64 = _b64url(json.dumps(payload).encode())
        signature = key.sign(f"{header_b64}.{payload_b64}".encode("ascii"))
        return f"{header_b64}.{payload_b64}.{_b64url(signature)}"

    def public_jwk(key: "Ed25519PrivateKey", kid: str = KID) -> dict[str, Any]:
        return {
            "kty": "OKP",
            "crv": "Ed25519",
            "x": _b64url(key.public_key().public_bytes_raw()),
            "kid": kid,
        }


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


class _JwksHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format: str, *args: object) -> None:
        pass

    def do_GET(self) -> None:
        kid = self.path.rsplit("/", 1)[-1]
        self.server.requests.append(kid)
        document = self.server.keys.get(kid)
        if document is None:
            body = b'{"error": "not_found"}'
            status = 404
        else:
            body = json.dumps(document).encode()
            status = 200
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class _JwksServer:
    """Local HTTPS JWKS endpoint backed by the shared fixture key pair."""

    def __init__(self, keys: dict[str, dict[str, Any]]) -> None:
        port = free_port()
        self.server = http.server.ThreadingHTTPServer(
            ("127.0.0.1", port), _JwksHandler
        )
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(CERT), str(KEY))
        self.server.socket = context.wrap_socket(self.server.socket, server_side=True)
        self.server.keys = keys
        self.server.requests: list[str] = []
        self._thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self._thread.start()
        self.base = f"https://127.0.0.1:{port}"

    @property
    def requests(self) -> list[str]:
        return list(self.server.requests)

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()


class _UnixConnection(http.client.HTTPConnection):
    def __init__(self, path: str, timeout: float = 10) -> None:
        super().__init__("localhost", timeout=timeout)
        self._socket_path = path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self._socket_path)
        self.sock = sock


class _ResolverProcess:
    def __init__(self, server: Any) -> None:
        self.server = server
        self._thread = threading.Thread(target=server.serve_forever, daemon=True)

    def __enter__(self) -> "_ResolverProcess":
        self._thread.start()
        return self

    def __exit__(self, *_exc: object) -> None:
        self.server.shutdown()
        self.server.server_close()
        records = list(getattr(self.server, "exceptions", []))
        if records:
            raise AssertionError(
                "resolver handler raised unexpectedly:\n" + "\n".join(records)
            )


@unittest.skipUnless(
    HAS_CRYPTO, "cryptography + resolver-auth SDK required for credential tests"
)
class ResolverCredentialModeTest(unittest.TestCase):
    """``--require-credential``: 401 reasons, opt-in scope, JWKS edge."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.socket_path = os.path.join(self._tmp.name, "resolver.sock")
        self.key = Ed25519PrivateKey.generate()
        self.wrong_key = Ed25519PrivateKey.generate()
        self.jwks = _JwksServer({KID: public_jwk(self.key)})
        self.addCleanup(self.jwks.stop)

    def build_server(self, argv: list[str] | None = None) -> Any:
        options = resolver.parse_args(
            [
                "--uds",
                self.socket_path,
                "--require-credential",
                "--jwks-base",
                self.jwks.base,
                "--jwks-cafile",
                str(CERT),
                "--expected-issuer",
                ISSUER,
                "--expected-aud",
                AUDIENCE,
                *(argv or []),
            ]
        )
        return resolver.build_server(options, transport="uds")

    def post(
        self,
        body: bytes,
        headers: dict[str, list[str] | str] | None = None,
    ) -> tuple[int, dict[str, Any]]:
        merged = {"Content-Type": "application/json", **(headers or {})}
        repeated = {
            name: value
            for name, value in merged.items()
            if isinstance(value, list)
        }
        for name in repeated:
            del merged[name]
        connection = _UnixConnection(self.socket_path)
        try:
            if repeated:
                # Duplicate headers need putheader's multi-value form.
                connection.putrequest("POST", "/resolve")
                merged["Content-Length"] = str(len(body))
                for name, value in merged.items():
                    connection.putheader(name, value)
                for name, values in repeated.items():
                    connection.putheader(name, *values)
                connection.endheaders(body)
            else:
                connection.request("POST", "/resolve", body=body, headers=merged)
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def test_missing_credential_answers_401_credential_missing(self) -> None:
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(json.dumps(VALID_REQUEST).encode())
        self.assertEqual(401, status)
        self.assertEqual("credential_missing", payload["error"])

    def test_valid_credential_passes_and_signature_chain_unchanged(self) -> None:
        token = mint(self.key)
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(),
                {CREDENTIAL_HEADER: token},
            )
        self.assertEqual(200, status)
        # The frozen x-test-signature chain (D15) is untouched by auth.
        source = "|".join(
            (
                VALID_REQUEST["subject"],
                VALID_REQUEST["credential"],
                VALID_REQUEST["binding"],
                VALID_REQUEST["destination"]["authority"],
                VALID_REQUEST["request"]["path"],
            )
        )
        self.assertEqual(
            hashlib.sha256(source.encode()).hexdigest(),
            payload["headers"]["x-test-signature"],
        )
        self.assertEqual(SUBJECT, payload["headers"]["x-test-subject"])

    def test_wrong_signature_answers_401_credential_invalid(self) -> None:
        token = mint(self.wrong_key)
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
            )
        self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])

    def test_expired_credential_answers_401_credential_invalid(self) -> None:
        now = int(time.time())
        token = mint(self.key, iat=now - 7200, exp=now - 3600)
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
            )
        self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])

    def test_subject_mismatch_answers_401_subject_mismatch(self) -> None:
        token = mint(self.key, sub="sbx-someone-else")
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
            )
        self.assertEqual(401, status)
        self.assertEqual("subject_mismatch", payload["error"])

    def test_cross_resolver_replay_answers_401(self) -> None:
        token = mint(self.key, aud="other-resolver@other.internal")
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
            )
        self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])

    def test_off_grammar_kid_answers_401_without_touching_jwks(self) -> None:
        token = mint(self.key, kid="../escape")
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
            )
        self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])
        self.assertEqual([], self.jwks.requests)

    def test_unknown_kid_fetches_then_negatively_cached(self) -> None:
        token = mint(self.key, kid="pool-unknown:v9")
        with _ResolverProcess(self.build_server()):
            for _ in range(2):
                status, payload = self.post(
                    json.dumps(VALID_REQUEST).encode(), {CREDENTIAL_HEADER: token}
                )
                self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])
        # First contact fetched; the negative cache absorbed the retry.
        self.assertEqual(["pool-unknown:v9"], self.jwks.requests)

    def test_duplicate_credential_header_answers_401(self) -> None:
        token = mint(self.key)
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(
                json.dumps(VALID_REQUEST).encode(),
                {CREDENTIAL_HEADER: [token, token]},
            )
        self.assertEqual(401, status)
        self.assertEqual("credential_invalid", payload["error"])

    def test_malformed_body_still_answers_400_with_valid_credential(self) -> None:
        token = mint(self.key)
        with _ResolverProcess(self.build_server()):
            status, payload = self.post(b"{not json", {CREDENTIAL_HEADER: token})
        self.assertEqual(400, status)
        self.assertEqual("malformed_request", payload["error"])

    def test_credential_mode_over_https_happy_path(self) -> None:
        port = free_port()
        options = resolver.parse_args(
            [
                "--https",
                f"127.0.0.1:{port}",
                "--tls-cert",
                str(CERT),
                "--tls-key",
                str(KEY),
                "--require-credential",
                "--jwks-base",
                self.jwks.base,
                "--jwks-cafile",
                str(CERT),
                "--expected-issuer",
                ISSUER,
                "--expected-aud",
                AUDIENCE,
            ]
        )
        server = resolver.build_server(options, transport="https")
        token = mint(self.key)
        context = ssl.create_default_context(cafile=str(CERT))
        with _ResolverProcess(server):
            connection = http.client.HTTPSConnection(
                "127.0.0.1", port, context=context, timeout=10
            )
            try:
                connection.request(
                    "POST",
                    "/resolve",
                    body=json.dumps(VALID_REQUEST).encode(),
                    headers={
                        "Content-Type": "application/json",
                        CREDENTIAL_HEADER: token,
                    },
                )
                response = connection.getresponse()
                status = response.status
                body = json.loads(response.read())
            finally:
                connection.close()
        self.assertEqual(200, status)
        self.assertEqual(SUBJECT, body["headers"]["x-test-subject"])


@unittest.skipUnless(
    HAS_CRYPTO, "cryptography + resolver-auth SDK required for credential tests"
)
class ResolverCredentialOptOutTest(unittest.TestCase):
    """Without ``--require-credential`` the stub behaves exactly as before."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.socket_path = os.path.join(self._tmp.name, "resolver.sock")

    def test_no_credential_header_is_served_as_before(self) -> None:
        options = resolver.parse_args(["--uds", self.socket_path])
        server = resolver.build_server(options, transport="uds")
        self.assertIsNone(server.credential_verifier)
        connection = _UnixConnection(self.socket_path)
        with _ResolverProcess(server):
            connection.request(
                "POST",
                "/resolve",
                body=json.dumps(VALID_REQUEST).encode(),
                headers={"Content-Type": "application/json"},
            )
            response = connection.getresponse()
            status = response.status
            body = json.loads(response.read())
        connection.close()
        self.assertEqual(200, status)
        self.assertEqual(SUBJECT, body["headers"]["x-test-subject"])

    def test_auth_options_without_flag_are_rejected(self) -> None:
        with self.assertRaises(SystemExit):
            resolver.parse_args(
                ["--uds", self.socket_path, "--jwks-base", "https://x.example"]
            )

    def test_require_credential_missing_options_are_rejected(self) -> None:
        with self.assertRaises(SystemExit):
            resolver.parse_args(
                ["--uds", self.socket_path, "--require-credential"]
            )


if __name__ == "__main__":
    unittest.main()
