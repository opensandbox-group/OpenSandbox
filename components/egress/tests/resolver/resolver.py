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

"""Standalone credential header-resolver stub for OSEP-0025 tests.

Serves the egress-to-resolver contract (one POSTed JSON document answered
with a header map plus optional incremental redactions) as the conformance
peer for egress tests and a development stub that runs before the egress
client exists.

Transports:

- ``--uds PATH``: HTTP over a Unix domain socket (bound 0660); no caller
  authentication — the socket permissions are the trust boundary.
- ``--https HOST:PORT --tls-cert PEM --tls-key PEM``: TLS with Bearer caller
  authentication; the token is ``--auth`` or is looked up by origin in
  ``OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_AUTH``. Without a configured
  token every request is rejected with 401 (fail closed).

Framing (D15): fixed at ``POST /resolve``; other paths answer 404; UDS mode
never inspects Host. ``subject`` is the egress-authoritative sandboxId, and
``x-test-signature`` is the SHA-256 hex of
``subject|credential|binding|authority|path`` so tests can assert both.

Fault injection: ``--delay MS`` sleeps before every response; ``--fail
once|RATE`` answers ``503 resolution_refused``; ``--incomplete-redactions``
returns redactions for only one header (egress must auto-derive the rest,
R9).
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import hmac
import http.client
import http.server
import json
import os
import random
import socket
import socketserver
import ssl
import stat
import sys
import time
import traceback
from typing import Any

MAX_REQUEST_BYTES = 64 * 1024
# Bodies over the cap are drained up to this bound before the 413, so
# clients finish sending instead of dying on a broken pipe.
MAX_DRAIN_BYTES = 1024 * 1024

_REQUEST_FIELDS = {"subject", "credential", "binding", "destination", "request"}
_DESTINATION_FIELDS = {"scheme", "authority", "sni"}
_REQUEST_REQUEST_FIELDS = {"method", "host", "path", "headers"}
_AUTH_ENV = "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_AUTH"


class _BadRequest(Exception):
    pass


class _TooLarge(Exception):
    pass


def _unique_object(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise _BadRequest
        result[key] = value
    return result


def _non_empty_string(value: Any) -> bool:
    return type(value) is str and bool(value)


def _string_map(value: Any) -> bool:
    return (
        type(value) is dict
        and all(type(key) is str for key in value)
        and all(type(item) is str for item in value.values())
    )


def parse_resolve_request(payload: dict[str, Any]) -> dict[str, str]:
    """Validate one resolver request (``subject`` is the egress sandboxId).

    Raises ``_BadRequest`` for anything off the OSEP-0025 contract.
    """
    if set(payload) != _REQUEST_FIELDS:
        raise _BadRequest
    subject = payload["subject"]
    credential = payload["credential"]
    binding = payload["binding"]
    if not all(_non_empty_string(part) for part in (subject, credential, binding)):
        raise _BadRequest
    destination = payload["destination"]
    if type(destination) is not dict or set(destination) != _DESTINATION_FIELDS:
        raise _BadRequest
    scheme = destination["scheme"]
    authority = destination["authority"]
    sni = destination["sni"]
    if (
        scheme not in ("http", "https")
        or not _non_empty_string(authority)
        or not _non_empty_string(sni)
    ):
        raise _BadRequest
    request = payload["request"]
    if type(request) is not dict or set(request) != _REQUEST_REQUEST_FIELDS:
        raise _BadRequest
    method = request["method"]
    host = request["host"]
    path = request["path"]
    if (
        not _non_empty_string(method)
        or not _non_empty_string(host)
        or not _non_empty_string(path)
        or not path.startswith("/")
        or not _string_map(request["headers"])
    ):
        raise _BadRequest
    return {
        "subject": subject,
        "credential": credential,
        "binding": binding,
        "scheme": scheme,
        "authority": authority,
        "sni": sni,
        "method": method,
        "host": host,
        "path": path,
    }


def resolve_headers(parts: dict[str, str]) -> dict[str, str]:
    """Deterministic header set derived from the verified request parts."""
    signature_source = "|".join(
        (
            parts["subject"],
            parts["credential"],
            parts["binding"],
            parts["authority"],
            parts["path"],
        )
    )
    return {
        "authorization": "Bearer test-" + parts["credential"],
        "x-test-signature": hashlib.sha256(signature_source.encode()).hexdigest(),
        "x-test-subject": parts["subject"],
    }


def resolve_response(
    parts: dict[str, str], incomplete_redactions: bool
) -> dict[str, Any]:
    headers = resolve_headers(parts)
    redactions = {name: [value] for name, value in headers.items()}
    if incomplete_redactions:
        first = sorted(redactions)[0]
        redactions = {first: redactions[first]}
    return {"headers": headers, "redactions": redactions}


def expected_token_for_origin(origin: str, auth_override: str | None) -> str | None:
    """Resolve the caller token for this server's origin.

    ``auth_override`` (``--auth``) wins; otherwise the token table in
    ``OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_AUTH`` is searched for
    ``origin=token`` entries. ``None`` means no token is configured and the
    HTTPS listener must reject every caller.
    """
    if auth_override:
        return auth_override
    raw = os.environ.get(_AUTH_ENV, "")
    for entry in raw.split(","):
        entry = entry.strip()
        if not entry:
            continue
        configured_origin, separator, token = entry.partition("=")
        if separator and configured_origin.strip() == origin:
            return token.strip() or None
    return None


class _UnixHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    address_family = socket.AF_UNIX

    def server_bind(self) -> None:
        # HTTPServer.server_bind expects an AF_INET address tuple.
        socketserver.TCPServer.server_bind(self)
        self.server_name = "resolver.uds"
        self.server_port = 0

    def handle_error(self, _request: object, _address: object) -> None:
        # Recorded, not swallowed: tests assert this stays empty.
        self.exceptions.append(traceback.format_exc())


class _TCPHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def handle_error(self, _request: object, _address: object) -> None:
        self.exceptions.append(traceback.format_exc())


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "OpenSandboxTestResolver/1"
    sys_version = ""

    def setup(self) -> None:
        super().setup()
        self.connection.settimeout(self.server.request_timeout)

    def log_message(self, _format: str, *args: object) -> None:
        pass

    def _reply(self, status: int, value: dict[str, Any]) -> None:
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True

    def _authenticate(self) -> bool:
        """Bearer caller authentication; HTTPS only, fail closed."""
        expected = self.server.expected_token
        if expected is not None:
            values = self.headers.get_all("Authorization", [])
            supplied = values[0] if len(values) == 1 else ""
            if hmac.compare_digest(supplied, "Bearer " + expected):
                return True
        self._drain_request()
        self._reply(401, {"error": "unauthorized"})
        return False

    def _json_body(self) -> dict[str, Any]:
        # Consume the body per its declared length before rejecting the
        # request: replying and closing with the body unread races the
        # client's send on Unix sockets (immediate EPIPE).
        if self.headers.get_all("Transfer-Encoding", []):
            raise _BadRequest
        lengths = self.headers.get_all("Content-Length", [])
        if len(lengths) != 1 or not lengths[0].isdigit():
            raise _BadRequest
        length = int(lengths[0])
        if length > MAX_REQUEST_BYTES:
            self._drain(length)
            raise _TooLarge
        raw = self.rfile.read(length)
        if len(raw) != length:
            raise _BadRequest
        if self.headers.get_all("Content-Type", []) != ["application/json"]:
            raise _BadRequest
        try:
            value = json.loads(
                raw,
                object_pairs_hook=_unique_object,
                parse_constant=lambda _value: (_ for _ in ()).throw(_BadRequest()),
            )
        except (json.JSONDecodeError, UnicodeDecodeError, RecursionError, _BadRequest):
            raise _BadRequest from None
        if type(value) is not dict:
            raise _BadRequest
        return value

    def _drain(self, length: int) -> None:
        remaining = min(length, MAX_DRAIN_BYTES)
        while remaining > 0:
            chunk = self.rfile.read(min(remaining, 65536))
            if not chunk:
                break
            remaining -= len(chunk)

    def _drain_request(self) -> None:
        # Early replies must consume the request body first: replying and
        # closing with the body unread races the client's send, and a Unix
        # socket raises EPIPE the moment the peer has closed.
        lengths = self.headers.get_all("Content-Length", [])
        if len(lengths) == 1 and lengths[0].isdigit():
            self._drain(int(lengths[0]))

    def do_POST(self) -> None:
        if self.server.transport == "https" and not self._authenticate():
            return
        if self.path != "/resolve":
            self._drain_request()
            self._reply(404, {"error": "not_found"})
            return
        options = self.server.options
        if options.delay > 0:
            time.sleep(options.delay / 1000.0)
        if options.fail_once:
            options.fail_once = False
            self._drain_request()
            self._reply(503, {"error": "resolution_refused"})
            return
        if options.fail_rate > 0 and random.random() < options.fail_rate:
            self._drain_request()
            self._reply(503, {"error": "resolution_refused"})
            return
        try:
            parts = parse_resolve_request(self._json_body())
        except _TooLarge:
            self._reply(413, {"error": "request_too_large"})
            return
        except _BadRequest:
            self._reply(400, {"error": "malformed_request"})
            return
        self._reply(
            200, resolve_response(parts, options.incomplete_redactions)
        )

    def do_GET(self) -> None:
        if self.server.transport == "https" and not self._authenticate():
            return
        self._drain_request()
        self._reply(405, {"error": "method_not_allowed"})

    def _unsupported(self) -> None:
        if self.server.transport == "https" and not self._authenticate():
            return
        self._drain_request()
        self._reply(405, {"error": "method_not_allowed"})

    do_CONNECT = do_DELETE = do_HEAD = do_OPTIONS = do_PATCH = do_PUT = do_TRACE = (
        _unsupported
    )


def build_server(
    options: argparse.Namespace,
    *,
    transport: str,
    expected_token: str | None,
) -> http.server.HTTPServer:
    """Construct (but do not start) the resolver server for one transport."""
    if transport == "uds":
        if os.path.exists(options.uds):
            # Only a stale socket may be removed; never unlink an arbitrary
            # filesystem object that happens to sit on the requested path.
            if not stat.S_ISSOCK(os.stat(options.uds).st_mode):
                raise SystemExit(
                    f"--uds path exists and is not a socket: {options.uds}"
                )
            os.unlink(options.uds)
        server = _UnixHTTPServer(options.uds, _Handler)
        os.chmod(options.uds, 0o660)
    else:
        host, separator, port_text = options.https.rpartition(":")
        if not separator or not host or not port_text.isdigit():
            raise SystemExit("--https requires HOST:PORT")
        server = _TCPHTTPServer((host, int(port_text)), _Handler)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(options.tls_cert, options.tls_key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    server.transport = transport
    server.expected_token = expected_token
    server.options = options
    server.exceptions = []
    server.request_timeout = max(30.0, options.delay / 1000.0 * 2 + 5.0)
    return server


def serve(server: http.server.HTTPServer) -> None:
    if server.transport == "uds":
        endpoint = f"uds://{server.server_address}"
    else:
        host, port = server.server_address[:2]
        endpoint = f"https://{host}:{port}"
    print(f"test resolver ready: {endpoint}", file=sys.stderr, flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass  # Ctrl+C is the expected shutdown signal; cleanup runs below.
    finally:
        server.server_close()
        if server.transport == "uds":
            with contextlib.suppress(OSError):
                os.unlink(server.server_address)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    transport = parser.add_mutually_exclusive_group(required=True)
    transport.add_argument("--uds", metavar="PATH", help="serve HTTP over this Unix socket")
    transport.add_argument(
        "--https", metavar="HOST:PORT", help="serve HTTPS on this address"
    )
    parser.add_argument("--tls-cert", metavar="PEM", help="TLS certificate (required with --https)")
    parser.add_argument("--tls-key", metavar="PEM", help="TLS private key (required with --https)")
    parser.add_argument(
        "--auth",
        metavar="TOKEN",
        help="expected caller bearer token; defaults to the entry for this "
        f"origin in {_AUTH_ENV}",
    )
    parser.add_argument("--delay", type=int, default=0, metavar="MS", help="sleep before every response")
    parser.add_argument(
        "--fail",
        default="",
        metavar="once|RATE",
        help="refuse resolutions once or at a random rate in [0,1]",
    )
    parser.add_argument(
        "--incomplete-redactions",
        action="store_true",
        help="return redactions for only one header (R9 auto-derivation tests)",
    )
    args = parser.parse_args(argv)
    if args.https and (not args.tls_cert or not args.tls_key):
        parser.error("--https requires --tls-cert and --tls-key")
    if args.fail:
        if args.fail == "once":
            args.fail_once = True
            args.fail_rate = 0.0
        else:
            try:
                rate = float(args.fail)
            except ValueError:
                parser.error("--fail accepts 'once' or a number in [0,1]")
            if not 0 <= rate <= 1:
                parser.error("--fail accepts 'once' or a number in [0,1]")
            args.fail_once = False
            args.fail_rate = rate
    else:
        args.fail_once = False
        args.fail_rate = 0.0
    return args


def main(argv: list[str] | None = None) -> int:
    options = parse_args(argv)
    transport = "uds" if options.uds else "https"
    origin = f"https://{options.https}" if transport == "https" else None
    server = build_server(
        options,
        transport=transport,
        expected_token=(
            expected_token_for_origin(origin, options.auth)
            if transport == "https"
            else None
        ),
    )
    serve(server)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
