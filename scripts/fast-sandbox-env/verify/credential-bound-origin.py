#!/usr/bin/env python3
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

"""Persistent HTTP/1.1 TLS origin for the credential-bound verify (OSEP-0023).

Unlike the environment's header-echo endpoint (HTTP/1.0, closes after each
response), every response here carries Content-Length and the connection
stays open, so a guest can prove what happens to an existing connection.

Each response reports, as JSON:
  conn  an origin-side identifier of the TCP connection (one per accept)
  req   the request ordinal on that connection
  cred  "absent", or "sha256:<hex>" of the X-Api-Key value it received
  credential_headers  names (closed set) of credential-shaped headers seen
Credential values are never echoed or logged: the caller compares digests,
and the egress response redaction cannot distort the comparison.

Standard library only (runs on the egress image's python3).
"""

import argparse
import hashlib
import itertools
import json
import ssl
import sys
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

PROTOCOL_VERSION = 1
CREDENTIAL_HEADERS = ("authorization", "proxy-authorization", "x-api-key")
_COUNTER = itertools.count(1)
_LOCK = threading.Lock()
_RUN = uuid.uuid4().hex[:8]


def credential_code(value):
    if value is None:
        return "absent"
    return "sha256:" + hashlib.sha256(value.encode("utf-8")).hexdigest()


def _tag(path):
    values = parse_qs(urlsplit(path).query).get("tag", [""])
    tag = values[0]
    return tag if tag and all(c.isalnum() or c in "-_." for c in tag) and len(tag) <= 64 else ""


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    # Idle keep-alive budget: the verify holds connections across vault
    # commits and egress/mitmdump restarts.
    timeout = 1800

    def setup(self):
        BaseHTTPRequestHandler.setup(self)
        with _LOCK:
            self.conn_id = "%s-%d" % (_RUN, next(_COUNTER))
        self.requests_served = 0

    def _reply(self):
        self.requests_served += 1
        length = int(self.headers.get("Content-Length") or 0)
        if length:
            self.rfile.read(min(length, 65536))
        present = sorted(name for name in CREDENTIAL_HEADERS if self.headers.get(name) is not None)
        body = json.dumps(
            {
                "v": PROTOCOL_VERSION,
                "conn": self.conn_id,
                "req": self.requests_served,
                "proto": self.request_version,
                "tag": _tag(self.path),
                "cred": credential_code(self.headers.get("X-Api-Key")),
                "credential_headers": present,
            },
            sort_keys=True,
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "keep-alive")
        self.end_headers()
        self.wfile.write(body)

    do_GET = _reply
    do_POST = _reply

    def log_message(self, *args):  # never log request lines or headers
        pass


class OriginServer(ThreadingHTTPServer):
    # The bench (credential-bound-bench.sh) opens connections from several
    # guests at once while the accept loop runs each TLS handshake; the
    # socketserver default backlog of 5 would turn that into SYN retries.
    request_queue_size = 128


def build_server(port, cert, key, host="0.0.0.0"):
    server = OriginServer((host, port), Handler)
    server.daemon_threads = True
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cert, key)
    try:
        ctx.set_alpn_protocols(["http/1.1"])
    except (AttributeError, NotImplementedError):
        pass
    server.socket = ctx.wrap_socket(server.socket, server_side=True)
    return server


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--port", type=int, default=443)
    parser.add_argument("--cert", required=True)
    parser.add_argument("--key", required=True)
    parser.add_argument("--host", default="0.0.0.0")
    args = parser.parse_args(sys.argv[1:] if argv is None else argv)
    server = build_server(args.port, args.cert, args.key, args.host)
    sys.stdout.write("credential-bound origin ready\n")
    sys.stdout.flush()
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
