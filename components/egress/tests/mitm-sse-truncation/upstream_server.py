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

"""SSE upstream for the mitmproxy truncation repro (tests/mitm-sse-truncation).

Serves a single large SSE event over HTTP/1.1 and closes the connection.
By default the response is sent without reading the client's request, so the
close happens with unread data in the receive buffer and the kernel sends TCP
RST — which flushes the receiver's kernel receive buffer and truncates the
SSE stream (standard TCP semantics, not a mitmproxy bug). --read-request
consumes the request first, making the close a clean FIN that never truncates.

See docs/components/egress-mitmproxy-sse-truncation.md.
"""

from __future__ import annotations

import argparse
import socket
import ssl
import threading
import time

CHUNKED_RESPONSE_HEAD = (
    "HTTP/1.1 200 OK\r\n"
    "Content-Type: text/event-stream\r\n"
    "Transfer-Encoding: chunked\r\n"
    "Connection: {conn}\r\n"
    "\r\n"
)


def build_body(payload_size: int) -> bytes:
    payload = b"data: " + b"x" * payload_size + b"\n\n"
    tail = b"data: [DONE]\n\n"
    body = bytearray()
    for part in (payload, tail):
        body += b"%x\r\n" % len(part)
        body += part
        body += b"\r\n"
    body += b"0\r\n\r\n"
    return bytes(body)


def serve(conn, args: argparse.Namespace) -> None:
    if args.read_request:
        buf = b""
        while b"\r\n\r\n" not in buf:
            data = conn.recv(4096)
            if not data:
                conn.close()
                return
            buf += data

    conn_header = b"keep-alive" if args.keep_alive else b"close"
    conn.sendall(CHUNKED_RESPONSE_HEAD.format(conn=conn_header.decode()).encode())
    conn.sendall(build_body(args.payload_size))

    if args.delay_close:
        time.sleep(args.delay_close)
    if args.close_notify:
        try:
            conn.unwrap()
        except Exception:
            pass
    conn.close()


def accept_loop(raw_sock: socket.socket, args: argparse.Namespace) -> None:
    while True:
        conn, _ = raw_sock.accept()
        if args.tls:
            tls_conn = args.tls_ctx.wrap_socket(conn, server_side=True)
            if tls_conn.selected_alpn_protocol() != "http/1.1":
                tls_conn.close()
                continue
            conn = tls_conn
        threading.Thread(target=serve, args=(conn, args), daemon=True).start()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--listen-port", type=int, default=19011)
    parser.add_argument("--payload-size", type=int, default=2 * 1024 * 1024)
    parser.add_argument("--plain", action="store_true", help="serve plain TCP instead of TLS")
    parser.add_argument("--read-request", action="store_true", help="wait for the full request before responding (realistic mode, race triggers only under load)")
    parser.add_argument("--delay-close", type=float, default=0.0, help="seconds to wait before closing the connection")
    parser.add_argument("--keep-alive", action="store_true", help="send Connection: keep-alive")
    parser.add_argument("--close-notify", action="store_true", help="send TLS close_notify before closing")
    parser.add_argument("--cert", default=None)
    parser.add_argument("--key", default=None)
    args = parser.parse_args()

    if not args.plain:
        args.tls = True
        if not args.cert or not args.key:
            parser.error("--cert and --key are required unless --plain is used")
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(args.cert, args.key)
        ctx.set_alpn_protocols(["http/1.1"])
        args.tls_ctx = ctx
    else:
        args.tls = False

    srv = socket.socket()
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(("127.0.0.1", args.listen_port))
    srv.listen(16)
    accept_loop(srv, args)


if __name__ == "__main__":
    main()
