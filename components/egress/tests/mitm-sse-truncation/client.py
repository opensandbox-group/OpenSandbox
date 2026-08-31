#!/usr/bin/env python3
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

"""Client for the mitmproxy truncation repro (tests/mitm-sse-truncation).

Requests the SSE stream from upstream_server.py through a local mitmdump
(regular mode), de-chunks the response and verifies the full body arrived.

Prints one line: `<encoded-bytes> bytes, <chunks> chunks, <data-bytes> data,
terminated=<yes|no>, expected=<n> -> OK|TRUNCATED` and exits 0 on OK.
"""

from __future__ import annotations

import argparse
import os
import socket
import ssl
import time


def dechunk(body: bytes) -> tuple[bool | None, int, int]:
    i = 0
    chunks = 0
    total = 0
    while i < len(body):
        nl = body.find(b"\r\n", i)
        if nl < 0:
            return None, chunks, total
        line = body[i:nl]
        try:
            size = int(line, 16)
        except ValueError:
            return None, chunks, total
        if size == 0:
            return True, chunks, total
        if nl + 2 + size + 2 > len(body):
            return None, chunks, total
        total += size
        i = nl + 2 + size + 2
        chunks += 1
    return None, chunks, total


def read_all(sock: socket.socket) -> bytes:
    received = bytearray()
    while True:
        data = sock.recv(65535)
        if not data:
            return bytes(received)
        received += data


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--proxy-host", default="127.0.0.1")
    parser.add_argument("--proxy-port", type=int, default=19080)
    parser.add_argument("--upstream-port", type=int, default=19011)
    parser.add_argument("--payload-size", type=int, default=2 * 1024 * 1024)
    parser.add_argument("--plain", action="store_true", help="use plain HTTP (absolute-form) instead of CONNECT+TLS")
    args = parser.parse_args()

    time.sleep(0.2)
    raw = socket.create_connection((args.proxy_host, args.proxy_port), timeout=15)
    raw.settimeout(15)

    if args.plain:
        raw.sendall(
            b"GET http://127.0.0.1:%d/stream HTTP/1.1\r\n"
            b"Host: 127.0.0.1:%d\r\nAccept: text/event-stream\r\n"
            b"Connection: close\r\n\r\n" % (args.upstream_port, args.upstream_port)
        )
        received = read_all(raw)
        raw.close()
    else:
        raw.sendall(
            b"CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n\r\n"
            % (args.upstream_port, args.upstream_port)
        )
        resp = b""
        while b"\r\n\r\n" not in resp:
            resp += raw.recv(4096)
        if b"200" not in resp.split(b"\r\n")[0]:
            print("CONNECT failed:", resp.split(b"\r\n")[0].decode(errors="replace"))
            os._exit(2)
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        ctx.set_alpn_protocols(["http/1.1"])
        tls = ctx.wrap_socket(raw, server_hostname="127.0.0.1")
        tls.sendall(
            b"GET /stream HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n"
            b"Accept: text/event-stream\r\nConnection: close\r\n\r\n" % args.upstream_port
        )
        received = read_all(tls)
        tls.close()

    head_end = received.find(b"\r\n\r\n")
    body = received[head_end + 4 :]
    terminated, chunks, total = dechunk(body)
    expected = 6 + args.payload_size + 2 + len(b"data: [DONE]\n\n")
    ok = terminated is True and total == expected
    print(
        f"{len(body)} bytes, {chunks} chunks, {total} data, "
        f"terminated={'yes' if terminated else 'no'}, expected={expected} -> {'OK' if ok else 'TRUNCATED'}",
        flush=True,
    )
    os._exit(0 if ok else 1)


if __name__ == "__main__":
    main()
