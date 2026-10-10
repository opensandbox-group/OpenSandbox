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

"""Real HTTPS origin for the OSEP-0023 live vault image test.

Generates its own CA and leaf certificate at startup (SANs from argv), serves
TLS on :443, echoes the exact received request metadata back as JSON, and
appends one sanitized record per request to /tmp/origin-log.jsonl. Secret
values are never written to the log: only header NAMES and the presence of
credential headers, so the evidence file can be attached as-is.
"""

from __future__ import annotations

import datetime
import json
import ssl
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from cryptography.x509.oid import NameOID

LOG_PATH = Path("/tmp/origin-log.jsonl")
CA_PATH = Path("/run/live-origin/ca.pem")


def generate_certificate(sans: list[str]) -> None:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    subject = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, sans[0])])
    builder = (
        x509.CertificateBuilder()
        .subject_name(subject)
        .issuer_name(subject)
        .public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(datetime.datetime(2020, 1, 1))
        .not_valid_after(datetime.datetime(2050, 1, 1))
    )
    builder = builder.add_extension(
        x509.SubjectAlternativeName([x509.DNSName(name) for name in sans]),
        critical=False,
    )
    certificate = builder.sign(key, hashes.SHA256())
    CA_PATH.parent.mkdir(parents=True, exist_ok=True)
    CA_PATH.write_bytes(certificate.public_bytes(serialization.Encoding.PEM))
    Path("/run/live-origin/server.pem").write_bytes(
        certificate.public_bytes(serialization.Encoding.PEM)
        + key.private_bytes(
            serialization.Encoding.PEM,
            serialization.PrivateFormat.TraditionalOpenSSL,
            serialization.NoEncryption(),
        )
    )


def record(request: BaseHTTPRequestHandler) -> None:
    credential_headers = sorted(
        name.lower()
        for name in request.headers
        if name.lower() in ("authorization", "private-token", "x-api-key")
    )
    entry = {
        "time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "method": request.command,
        "host": request.headers.get("Host", ""),
        "path": request.path,
        "credential_headers": credential_headers,
    }
    with LOG_PATH.open("a") as stream:
        stream.write(json.dumps(entry) + "\n")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, _format: str, *_args: object) -> None:
        pass

    def _serve(self) -> None:
        record(self)
        body = json.dumps(
            {
                "method": self.command,
                "host": self.headers.get("Host", ""),
                "path": self.path,
                "authorization": self.headers.get("Authorization", ""),
                "private_token": self.headers.get("Private-Token", ""),
            }
        ).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = _serve
    do_POST = _serve
    do_DELETE = _serve
    do_PUT = _serve
    do_PATCH = _serve


def main() -> None:
    sans = sys.argv[1:] or ["origin.example.com"]
    generate_certificate(sans)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain("/run/live-origin/server.pem")
    server = ThreadingHTTPServer(("0.0.0.0", 443), Handler)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    print(json.dumps({"ready": True, "sans": sans}), flush=True)
    threading.Event().wait()


if __name__ == "__main__":
    main()
