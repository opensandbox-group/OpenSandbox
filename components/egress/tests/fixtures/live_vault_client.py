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

"""TLS client driver for the OSEP-0023 live vault image test.

Runs inside the egress container's network namespace via ``docker exec`` so
the transparent interception rules apply. Takes a JSON plan on argv:

    {"requests": [
        {"host": "...", "path": "...", "method": "GET",
         "trust": "mitm" | "origin" | "none",
         "host_header": optional override,
         "sni": optional SNI override,
         "no_sni": bool,
         "keepalive_with": index of a previous request to reuse
        }
    ]}

Prints one JSON result per request: status, error, and the origin's echo
body. Never prints credential values directly; the origin echoes them, so
the driver masks well-known authorization headers in its own output.
"""

from __future__ import annotations

import hashlib
import http.client
import json
import socket
import ssl
import sys
import time

MITM_CA = "/opt/opensandbox/mitmproxy-ca-cert.pem"
ORIGIN_CA = "/run/live-origin/ca.pem"


def trust_context(trust: str, hostname: str | None) -> ssl.SSLContext:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    if trust == "mitm":
        context.load_verify_locations(MITM_CA)
    elif trust == "origin":
        context.load_verify_locations(ORIGIN_CA)
    else:
        context.check_hostname = False
        context.verify_mode = ssl.CERT_NONE
        return context
    # Hostname checking is off for SNI-less dials; chain verification stays on.
    context.check_hostname = bool(hostname)
    return context


class Result:
    def __init__(self) -> None:
        self.connection: http.client.HTTPSConnection | None = None

    def close(self) -> None:
        if self.connection is not None:
            with_suppressed(self.connection.close)
            self.connection = None


def with_suppressed(fn, *args) -> None:
    try:
        fn(*args)
    except Exception:
        pass


def perform(plan: dict) -> list[dict]:
    results: list[dict] = []
    connections: list[http.client.HTTPSConnection | None] = []
    for index, request in enumerate(plan["requests"]):
        if request.get("vault_api"):
            results.append(vault_api(request["vault_api"]))
            connections.append(None)
            continue
        if request.get("delay_ms"):
            import time

            time.sleep(request["delay_ms"] / 1000.0)
        result = perform_one(request, connections)
        results.append(result)
        connections.append(result.pop("_connection", None))
    for connection in connections:
        if connection is not None:
            with_suppressed(connection.close)
    return results


def vault_api(command: dict) -> dict:
    """Call the egress policy server from inside the container."""
    connection = http.client.HTTPConnection("127.0.0.1", 18080, timeout=10)
    body = command.get("body")
    headers = {"OPENSANDBOX-EGRESS-AUTH": command.get("token", "")}
    if body is not None:
        headers["content-type"] = "application/json"
    try:
        connection.request(command.get("method", "GET"), command["path"], body=body, headers=headers)
        response = connection.getresponse()
        return {
            "vault_api": command["path"],
            "status": response.status,
            "body": response.read().decode("utf-8", "replace")[:400],
        }
    except Exception as exc:  # noqa: BLE001
        return {"vault_api": command["path"], "error": type(exc).__name__ + ": " + str(exc)[:160]}
    finally:
        with_suppressed(connection.close)


def perform_one(request: dict, connections: list) -> dict:
    if request.get("ech_hello"):
        return perform_ech_hello(request)
    if request.get("raw") is not None:
        return perform_raw(request)
    if request.get("idle_read"):
        return perform_idle_read(request, connections)
    host = request["host"]
    path = request.get("path", "/echo")
    method = request.get("method", "GET")
    trust = request.get("trust", "none")
    host_header = request.get("host_header", host)
    no_sni = request.get("no_sni", False)
    reuse = request.get("keepalive_with")
    output = {"host": host, "path": path, "method": method, "trust": trust}
    connection = connections[reuse] if reuse is not None else None
    # Reuse is measured on the TCP socket, not the Python object: http.client
    # silently opens a new socket when the previous response said Connection:
    # close, and that new transport is a new interception decision.
    if connection is not None:
        try:
            current_socket = connection.sock.getsockname() if connection.sock else None
        except OSError:
            current_socket = None
        output["reused_connection"] = current_socket is not None and (
            current_socket == getattr(connection, "_live_socket", None)
        )
    else:
        output["reused_connection"] = False
    try:
        if connection is None:
            context = trust_context(trust, None if no_sni else host)
            if no_sni:
                # A connection without SNI: dial the resolved address with the
                # hostname kept only for the Host header.
                address = socket.gethostbyname(host)
                connection = http.client.HTTPSConnection(
                    address, 443, context=context, timeout=10
                )
            else:
                connection = http.client.HTTPSConnection(
                    host, 443, context=context, timeout=10
                )
        connection.request(method, path, headers={"Host": host_header})
        response = connection.getresponse()
        body = response.read().decode("utf-8", "replace")
        output["status"] = response.status
        output["connection_close"] = "close" in response.getheader("Connection", "").lower()
        try:
            echoed = json.loads(body)
            echoed["authorization"] = mask(echoed.get("authorization", ""))
            echoed["private_token"] = mask(echoed.get("private_token", ""))
            output["echo"] = echoed
        except ValueError:
            output["body"] = body[:200]
    except Exception as exc:  # noqa: BLE001 - every failure mode is evidence
        output["error"] = type(exc).__name__ + ": " + str(exc)[:160]
        if connection is not None:
            with_suppressed(connection.close)
            connection = None
    if connection is not None and no_sni:
        # Verify the actual TLS peer certificate against the origin CA: with
        # no SNI the connection must have passed through untouched, so the
        # peer certificate is the origin's own, not a mitmproxy one.
        with_suppressed(lambda: output.update(peer_matches_origin=peer_is_origin(connection)))
    if connection is not None:
        try:
            connection._live_socket = connection.sock.getsockname() if connection.sock else None
        except OSError:
            connection._live_socket = None
    output["_connection"] = connection
    return output


def peer_is_origin(connection: http.client.HTTPSConnection) -> bool | None:
    try:
        import hashlib

        peer_der = connection.sock.getpeercert(binary_form=True)
        origin_pem = open(ORIGIN_CA, "rb").read()
        origin_der = ssl.PEM_cert_to_DER_cert(origin_pem.decode("ascii"))
        return hashlib.sha256(peer_der).hexdigest() == hashlib.sha256(origin_der).hexdigest()
    except Exception:  # noqa: BLE001 - failure to inspect is reported as unknown
        return None


def mask(value: str) -> str:
    if not value:
        return ""
    return "present(" + str(len(value)) + " chars)"


def _build_client_hello(sni: str, extra: tuple = ()) -> bytes:
    """Minimal TLS 1.2 ClientHello body; unknown extensions ride along."""
    name = sni.encode("ascii")
    entry = b"\x00" + len(name).to_bytes(2, "big") + name
    sni_body = len(entry).to_bytes(2, "big") + entry
    extensions = (0).to_bytes(2, "big") + len(sni_body).to_bytes(2, "big") + sni_body
    for ext_type, payload in (
        (0x000A, b"\x00\x04\x00\x17\x00\x18"),   # supported_groups
        (0x000B, b"\x01\x00"),                    # ec_point_formats
        (0x000D, b"\x00\x04\x04\x01\x05\x01"),   # signature_algorithms
    ) + tuple(extra):
        extensions += ext_type.to_bytes(2, "big") + len(payload).to_bytes(2, "big") + payload
    return (
        b"\x03\x03" + b"\x11" * 32 + b"\x00"
        + (2).to_bytes(2, "big") + b"\xc0\x2f"
        + b"\x01\x00"
        + len(extensions).to_bytes(2, "big") + extensions
    )


def _read_leaf_der(sock: socket.socket) -> bytes:
    """Read TLS records until the Certificate handshake message; return leaf."""
    buffered = b""
    handshake = b""
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        try:
            chunk = sock.recv(65536)
        except socket.timeout:
            continue
        if not chunk:
            raise ConnectionError("peer closed before the Certificate message")
        buffered += chunk
        offset = 0
        while offset + 5 <= len(buffered):
            record_type = buffered[offset]
            record_len = int.from_bytes(buffered[offset + 3:offset + 5], "big")
            if offset + 5 + record_len > len(buffered):
                break
            payload = buffered[offset + 5:offset + 5 + record_len]
            offset += 5 + record_len
            if record_type != 0x16:
                continue
            # Handshake messages can span multiple TLS records: parse from
            # the concatenated handshake stream, not a single record body.
            handshake += payload
            inner = 0
            while inner + 4 <= len(handshake):
                hs_type = handshake[inner]
                hs_len = int.from_bytes(handshake[inner + 1:inner + 4], "big")
                if inner + 4 + hs_len > len(handshake):
                    break
                body = handshake[inner + 4:inner + 4 + hs_len]
                inner += 4 + hs_len
                if hs_type == 0x0B:
                    cert_len = int.from_bytes(body[3:6], "big")
                    return body[6:6 + cert_len]
            handshake = handshake[inner:]
    raise TimeoutError("no Certificate message arrived")


def perform_ech_hello(request: dict) -> dict:
    """Send a crafted TLS 1.2 ClientHello and report which cert answered.

    With an ECH extension present the connection must be opaque: the peer
    certificate is the origin's own. Without it a bound host is intercepted.
    """
    host = request["host"]
    output = {"host": host, "ech_hello": True}
    ech = request.get("ech", True)
    extra = [(0xFE0D, b"\x01\x02\x03\x04")] if ech else []
    sock = None
    try:
        sock = socket.create_connection((host, 443), timeout=10)
        sock.settimeout(10)
        hello = _build_client_hello(host, extra)
        record = (
            b"\x16\x03\x01" + (len(hello) + 4).to_bytes(2, "big")
            + b"\x01" + len(hello).to_bytes(3, "big") + hello
        )
        sock.sendall(record)
        leaf = _read_leaf_der(sock)
        origin_der = ssl.PEM_cert_to_DER_cert(open(ORIGIN_CA, "rb").read().decode("ascii"))
        output["peer_matches_origin"] = (
            hashlib.sha256(leaf).hexdigest() == hashlib.sha256(origin_der).hexdigest()
        )
    except Exception as exc:  # noqa: BLE001 - every failure mode is evidence
        output["error"] = type(exc).__name__ + ": " + str(exc)[:160]
    finally:
        if sock is not None:
            with_suppressed(sock.close)
    output["_connection"] = None
    return output


def _read_http_head(stream) -> tuple[str, dict, bytes]:
    head = b""
    while b"\r\n\r\n" not in head:
        chunk = stream.recv(65536)
        if not chunk:
            raise ConnectionError("connection closed before the response head")
        head += chunk
    header_section, _, body = head.partition(b"\r\n\r\n")
    lines = header_section.split(b"\r\n")
    status = lines[0].decode("latin1").split(" ", 2)
    headers = {}
    for line in lines[1:]:
        if b":" in line:
            name, _, value = line.partition(b":")
            headers[name.decode("latin1").strip().lower()] = value.decode("latin1").strip()
    length = int(headers.get("content-length", "0") or "0")
    while len(body) < length:
        chunk = stream.recv(length - len(body))
        if not chunk:
            break
        body += chunk
    return status[1] if len(status) > 1 else "0", headers, body[:length]


def perform_raw(request: dict) -> dict:
    """Send verbatim request bytes over a TLS connection (raw H1 inputs)."""
    host = request["host"]
    sni = request.get("sni", host)
    trust = request.get("trust", "none")
    output = {"host": host, "raw": True, "trust": trust}
    connection = None
    sock = None
    try:
        context = trust_context(trust, sni)
        address = socket.gethostbyname(host)
        sock = socket.create_connection((address, 443), timeout=10)
        connection = context.wrap_socket(sock, server_hostname=sni)
        connection.sendall(request["raw"].encode("latin1"))
        status, headers, body = _read_http_head(connection)
        output["status"] = int(status)
        try:
            echoed = json.loads(body)
            echoed["authorization"] = mask(echoed.get("authorization", ""))
            echoed["private_token"] = mask(echoed.get("private_token", ""))
            output["echo"] = echoed
        except ValueError:
            output["body"] = body.decode("utf-8", "replace")[:200]
    except Exception as exc:  # noqa: BLE001 - every failure mode is evidence
        output["error"] = type(exc).__name__ + ": " + str(exc)[:160]
    if connection is not None:
        with_suppressed(connection.close)
    elif sock is not None:
        # The TLS wrap failed: the raw socket must not leak.
        with_suppressed(sock.close)
    output["_connection"] = None
    return output


def perform_idle_read(request: dict, connections: list) -> dict:
    """Read-only recv on an existing TLS socket; sends nothing.

    Used for drain-deadline evidence: the transport must already exist, and
    only EOF/TLS EOF/reset (or a timeout) counts — never an HTTP response.
    """
    reuse = request.get("keepalive_with")
    timeout_s = request.get("timeout_s", 10)
    output = {"idle_read": True, "keepalive_with": reuse}
    connection = connections[reuse] if reuse is not None else None
    if connection is None:
        output["error"] = "no connection to read"
        output["_connection"] = None
        return output
    sock = getattr(connection, "sock", None)
    if sock is None:
        output["error"] = "connection socket is gone"
        output["_connection"] = None
        return output
    started = time.monotonic()
    try:
        previous_timeout = sock.gettimeout()
        sock.settimeout(timeout_s)
        try:
            data = sock.recv(65536)
        finally:
            # Never leave the shared transport with a changed timeout.
            with_suppressed(sock.settimeout, previous_timeout)
        output["elapsed_s"] = round(time.monotonic() - started, 3)
        if data:
            # Any bytes mean the transport was still serving: not a closure.
            output["outcome"] = "data"
            output["bytes"] = len(data)
        else:
            output["outcome"] = "eof"
    except socket.timeout:
        output["outcome"] = "timeout"
        output["elapsed_s"] = round(time.monotonic() - started, 3)
    except (ssl.SSLError, OSError, ConnectionError) as exc:
        output["outcome"] = "reset"
        output["elapsed_s"] = round(time.monotonic() - started, 3)
        output["error"] = type(exc).__name__ + ": " + str(exc)[:160]
    output["_connection"] = connection
    return output


if __name__ == "__main__":
    plan = json.loads(sys.argv[1])
    print(json.dumps(perform(plan)))
