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

"""Hermetic peers for tests/smoke-forward.sh (OSEP-0030 forward mode).

    forward_smoke.py gateway <port>   CONNECT gateway; logs one JSON line per request
    forward_smoke.py origin           TCP 80/81/8443 + UDP 9999; logs every peer
    forward_smoke.py client tcp|udp <ip> <port>

Stdlib only, so it runs in a stock python:alpine image.
"""

import asyncio
import json
import os
import socket
import socketserver
import sys
import threading

ORIGIN_TCP_PORTS = (80, 81, 8443)
ORIGIN_UDP_PORT = 9999


def log(**fields):
    print(json.dumps(fields, sort_keys=True), flush=True)


# --- gateway ---------------------------------------------------------------


async def pipe(reader, writer):
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, OSError):
        pass
    finally:
        writer.close()


async def handle_gateway(reader, writer):
    expected_auth = os.environ["AUTH"]
    deny_ports = {p for p in os.environ.get("DENY_PORTS", "").split(",") if p}
    try:
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 10)
    except (asyncio.IncompleteReadError, asyncio.LimitOverrunError, asyncio.TimeoutError):
        writer.close()
        return
    lines = head.decode("latin-1").split("\r\n")
    method, target, _ = (lines[0].split(" ") + ["", "", ""])[:3]
    headers = {}
    for line in lines[1:]:
        if ":" in line:
            k, v = line.split(":", 1)
            headers[k.strip().lower()] = v.strip()
    entry = {
        "event": "request",
        "method": method,
        "target": target,
        "auth": headers.get("proxy-authorization") == expected_auth,
        "sandbox_id": headers.get("opensandbox-sandbox-id"),
        "namespace": headers.get("opensandbox-sandbox-namespace"),
        "tenant": headers.get("opensandbox-tenant-name"),
    }

    def reply(status, reason):
        log(**entry, status=status)
        writer.write(f"HTTP/1.1 {status} {reason}\r\nContent-Length: 0\r\n\r\n".encode())

    if method != "CONNECT":
        reply(405, "Method Not Allowed")
        writer.close()
        return
    if not entry["auth"]:
        reply(407, "Proxy Authentication Required")
        writer.close()
        return
    host, _, port = target.rpartition(":")
    if port in deny_ports:
        reply(403, "Forbidden")
        writer.close()
        return
    try:
        up_reader, up_writer = await asyncio.wait_for(asyncio.open_connection(host.strip("[]"), int(port)), 5)
    except (OSError, ValueError, asyncio.TimeoutError):
        reply(502, "Bad Gateway")
        writer.close()
        return
    reply(200, "Connection Established")
    await writer.drain()
    await asyncio.gather(pipe(reader, up_writer), pipe(up_reader, writer))


async def run_gateway(port):
    server = await asyncio.start_server(handle_gateway, "0.0.0.0", port)
    log(event="listening", port=port)
    async with server:
        await server.serve_forever()


# --- origin ----------------------------------------------------------------


def make_tcp_handler(port):
    class Handler(socketserver.StreamRequestHandler):
        def handle(self):
            self.rfile.readline()
            log(event="tcp", port=port, peer=self.client_address[0])
            body = f"origin-{port}".encode()
            self.wfile.write(b"HTTP/1.0 200 OK\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body))

    return Handler


class UDPHandler(socketserver.BaseRequestHandler):
    def handle(self):
        data, sock = self.request
        log(event="udp", port=ORIGIN_UDP_PORT, peer=self.client_address[0])
        sock.sendto(data, self.client_address)


def run_origin():
    socketserver.ThreadingTCPServer.allow_reuse_address = True
    servers = [socketserver.ThreadingTCPServer(("0.0.0.0", p), make_tcp_handler(p)) for p in ORIGIN_TCP_PORTS]
    servers.append(socketserver.UDPServer(("0.0.0.0", ORIGIN_UDP_PORT), UDPHandler))
    for srv in servers[1:]:
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    log(event="listening", tcp=list(ORIGIN_TCP_PORTS), udp=ORIGIN_UDP_PORT)
    servers[0].serve_forever()


# --- client ----------------------------------------------------------------


def run_client(proto, ip, port):
    port = int(port)
    try:
        if proto == "udp":
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            s.settimeout(3)
            s.sendto(b"ping", (ip, port))
            data, _ = s.recvfrom(1024)
            print(data.decode())
            return 0
        with socket.create_connection((ip, port), timeout=5) as s:
            s.settimeout(5)
            s.sendall(f"GET / HTTP/1.0\r\nHost: {ip}\r\n\r\n".encode())
            chunks = []
            while chunk := s.recv(65536):
                chunks.append(chunk)
    except OSError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    response = b"".join(chunks).decode("latin-1")
    print(response.split("\r\n\r\n", 1)[-1])
    return 0 if response.startswith(("HTTP/1.0 200", "HTTP/1.1 200")) else 1


def main(argv):
    if argv[:1] == ["gateway"]:
        asyncio.run(run_gateway(int(argv[1])))
    elif argv[:1] == ["origin"]:
        run_origin()
    elif argv[:1] == ["client"] and len(argv) == 4:
        return run_client(*argv[1:])
    else:
        print(__doc__, file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
