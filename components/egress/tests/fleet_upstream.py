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

"""Helper servers + DNS query tool for the fleet smoke test (tests/smoke-fleet.sh).

Modes:
  dns         UDP DNS upstream on 127.0.0.1:5300. Authoritative for *.test:
              allow.test -> 1.1.1.1, other.test -> 1.1.1.2, everything else
              NXDOMAIN. Deterministic, no external network.
  ext         HTTP server on 0.0.0.0:8080 (run inside the "ext" netns,
              plays the role of the external network).
  query ARGS  Send a DNS A query (python3 fleet_upstream.py query <server> <name>)
              and print "rcode=N [answers=...]".
"""

import http.server
import os
import socket
import ssl
import struct
import sys
import threading
import time

TEST_ALLOW_IP = "1.1.1.1"
TEST_OTHER_IP = "1.1.1.2"


def build_query(name: str) -> bytes:
    qid = 0x1234
    header = struct.pack(">HHHHHH", qid, 0x0100, 1, 0, 0, 0)
    qname = b"".join(bytes([len(label)]) + label.encode() for label in name.split(".")) + b"\x00"
    return header + qname + struct.pack(">HH", 1, 1)


def parse_response(pkt: bytes):
    if len(pkt) < 12:
        return None
    qid, flags, qd, an, ns, ar = struct.unpack(">HHHHHH", pkt[:12])
    rcode = flags & 0x000F
    answers = []
    # skip question
    idx = 12
    while idx < len(pkt) and pkt[idx] != 0:
        idx += 1
    idx += 5  # null byte + qtype + qclass
    for _ in range(an):
        while idx < len(pkt) and pkt[idx] != 0:
            if pkt[idx] & 0xC0 == 0xC0:
                idx += 2  # compression pointer, two bytes total
                break
            idx += 1 + pkt[idx]
        else:
            idx += 1  # plain name: null terminator
        if idx + 10 > len(pkt):
            break
        rtype, rclass, ttl, rdlen = struct.unpack(">HHIH", pkt[idx : idx + 10])
        idx += 10
        if rtype == 1 and rdlen == 4:
            answers.append(socket.inet_ntoa(pkt[idx : idx + 4]))
        idx += rdlen
    return rcode, answers


def extract_query_name(data: bytes) -> str:
    labels = []
    i = 12
    while i < len(data) and data[i] != 0:
        ln = data[i]
        if ln & 0xC0 == 0xC0:
            break
        i += 1
        if i + ln > len(data):
            return ""
        labels.append(data[i : i + ln].decode("ascii", "replace"))
        i += ln
    return ".".join(labels)


def run_dns_upstream():
    # EXT_ANSWER: comma-separated name=ip extra answers (e.g.
    # "ext.test=10.99.0.2") for the firecracker e2e; base answers below.
    extra: dict[str, str] = {}
    for entry in (os.environ.get("EXT_ANSWER", "") or "").split(","):
        entry = entry.strip()
        if not entry:
            continue
        name, _, ip = entry.partition("=")
        if name and ip:
            extra[name.strip().rstrip(".")] = ip.strip()

    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    # 0.0.0.0: the firecracker e2e reaches this upstream from the Pod netns
    # via the docker bridge gateway IP (the Pod loopback is Pod-private and
    # nsenter-based launches proved unreliable there); the sidecar smoke
    # connects to 127.0.0.1 which 0.0.0.0 also covers.
    sock.bind(("0.0.0.0", 5300))
    while True:
        data, addr = sock.recvfrom(512)
        try:
            name = extract_query_name(data)
            # Echo ONLY the question section (qname + qtype + qclass): the
            # client (dnsproxy) may send EDNS0/OPT in the additional section,
            # which must not be echoed back inside the answer area.
            qend = data.index(b"\x00", 12) + 1
            question = data[12 : qend + 4]
            if name in extra:
                ip, rcode = extra[name], 0
            elif name == "allow.test":
                ip, rcode = TEST_ALLOW_IP, 0
            elif name == "other.test":
                ip, rcode = TEST_OTHER_IP, 0
            else:
                ip, rcode = None, 3  # NXDOMAIN
            resp = bytearray(data[:2]) + struct.pack(">H", 0x8180 | rcode)
            resp += struct.pack(">HHHH", 1, 1 if rcode == 0 else 0, 0, 0)
            resp += question
            if ip:
                resp += b"\xc0\x0c" + struct.pack(">HHIH", 1, 1, 60, 4) + socket.inet_aton(ip)
            sock.sendto(bytes(resp), addr)
        except Exception:  # noqa: BLE001 - upstream must never die
            pass


def run_ext_http():
    # Echo the request headers as the response body: the MITM smoke phase
    # asserts the credential-proxy-injected header arrived at the upstream
    # (proving interception + subject-aware vault dispatch). Served on both
    # :80 (inside the interception set {80,443}) and :8080 (direct-traffic
    # tests); the egress DNAT requires port 80 for the MITM phase.
    class EchoHeadersHandler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            # The client address is the interception oracle: the sandbox's
            # direct path arrives with src=10.10.0.5 (the sandbox IP), while a
            # request proxied by the shared mitm arrives with the host-side
            # veth src (10.99.0.1).
            lines = [f"client={self.client_address[0]}"]
            lines += [f"{k}: {v}" for k, v in sorted(self.headers.items())]
            body = ("\n".join(lines) + "\n").encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):  # keep the server quiet
            pass

    for port in (80, 8080):
        srv = http.server.HTTPServer(("0.0.0.0", port), EchoHeadersHandler)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
    # HTTPS on :443 (the other interception port): self-signed cert from the
    # environment; the smoke asserts TLS interception + credential injection
    # with curl -k (the sandbox trust stack is a fast-sandbox delivery).
    cert = os.environ.get("EXT_SSL_CERT", "")
    key = os.environ.get("EXT_SSL_KEY", "")
    if cert and key:
        https_srv = http.server.HTTPServer(("0.0.0.0", 443), EchoHeadersHandler)
        tls_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls_ctx.load_cert_chain(cert, key)
        https_srv.socket = tls_ctx.wrap_socket(https_srv.socket, server_side=True)
        threading.Thread(target=https_srv.serve_forever, daemon=True).start()
    while True:  # keep the main thread alive
        time.sleep(3600)


def do_query(server: str, name: str, port: int = 53) -> int:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.settimeout(3)
    sock.sendto(build_query(name), (server, port))
    pkt, _ = sock.recvfrom(512)
    parsed = parse_response(pkt)
    if parsed is None:
        print("parse-error")
        return 2
    rcode, answers = parsed
    print(f"rcode={rcode} answers={','.join(answers)}")
    return 0


def main() -> int:
    mode = sys.argv[1]
    if mode == "dns":
        run_dns_upstream()
    elif mode == "ext":
        run_ext_http()
    elif mode == "query":
        port = int(sys.argv[4]) if len(sys.argv) > 4 else 53
        return do_query(sys.argv[2], sys.argv[3], port)
    return 0


if __name__ == "__main__":
    sys.exit(main())
