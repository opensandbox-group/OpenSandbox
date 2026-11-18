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

"""HTTP/1.1-over-TLS probe for the credential-bound verify stage (OSEP-0023).

Runs inside the guest (and, for the origin self-check, in a container on the
kind network). Standard library only; compatible with Python 3.6+.

The client never carries a credential itself and never reconnects: every
mode opens exactly one TCP connection, verifies the server certificate
(CERT_REQUIRED + hostname check, no insecure switch exists), and reports the
presented leaf's DER SHA-256 so the caller can tell the origin leaf from a
MITM leaf even when both chain to the same CA.

Modes:
  once  one connection, --count sequential requests on it, one result line.
  hold  one connection: request 1 -> <dir>/r1.json, wait for <dir>/go, then
        request 2 on the SAME socket -> <dir>/r2.json. A failure on request 2
        is classified (eof/reset/tls_closed/timeout/...) instead of retried.

Bench modes (credential-bound-bench.sh only; the single-connection guard
above still applies to once/hold):
  handshake-bench  --samples new connections, one after another: TCP connect,
        TLS handshake, one request, close. Reports TCP and TCP+TLS connect
        times and first-response times (client-side, milliseconds), plus
        tallies of presented leaf digests, origin credential codes and
        failure kinds.
  hold-many  --conns connections, each verified by one request and then kept
        open: <dir>/status.json once all are opened, wait for <dir>/go, then
        one more request per held socket and close -> <dir>/done.json.

Output: a single line ``CBV-RESULT <json>`` (once), ``CBV-BENCH <json>``
(handshake-bench, also written to --out when given) or the JSON files
(hold, hold-many). Bench output carries only digests, counts and timings.
Exit status: 0 when every request succeeded, 3 otherwise, 2 on usage errors.
"""

import argparse
import errno
import hashlib
import json
import os
import socket
import ssl
import sys
import time
import uuid

PROTOCOL_VERSION = 1
RESULT_PREFIX = "CBV-RESULT "
BENCH_PREFIX = "CBV-BENCH "
MAX_BENCH_CONNECTIONS = 5000
CLOSED_KINDS = ("eof", "reset", "broken_pipe", "tls_closed")
MAX_HEADER_BYTES = 16384
MAX_BODY_BYTES = 65536


class ProbeError(Exception):
    def __init__(self, phase, kind, detail=""):
        Exception.__init__(self, "%s/%s" % (phase, kind))
        self.phase = phase
        self.kind = kind
        self.detail = detail


def classify(phase, exc):
    """Map a socket/TLS exception to a closed vocabulary (never message text
    that could carry peer data)."""
    if isinstance(exc, ProbeError):
        return exc
    if isinstance(exc, socket.timeout):
        return ProbeError(phase, "timeout")
    cert_error = getattr(ssl, "SSLCertVerificationError", None)
    if cert_error is not None and isinstance(exc, cert_error):
        return ProbeError(phase, "verify_failed", str(getattr(exc, "verify_message", "")))
    if isinstance(exc, ssl.CertificateError):
        return ProbeError(phase, "verify_failed")
    if isinstance(exc, (ssl.SSLZeroReturnError, ssl.SSLEOFError)):
        return ProbeError(phase, "tls_closed")
    if isinstance(exc, ssl.SSLError):
        return ProbeError(phase, "tls_error", str(getattr(exc, "reason", "") or ""))
    if isinstance(exc, ConnectionRefusedError):
        return ProbeError(phase, "refused")
    if isinstance(exc, ConnectionResetError):
        return ProbeError(phase, "reset")
    if isinstance(exc, BrokenPipeError):
        return ProbeError(phase, "broken_pipe")
    if isinstance(exc, OSError):
        if exc.errno in (errno.ETIMEDOUT,):
            return ProbeError(phase, "timeout")
        if exc.errno in (errno.EHOSTUNREACH, errno.ENETUNREACH):
            return ProbeError(phase, "unreachable")
        return ProbeError(phase, "os_error", errno.errorcode.get(exc.errno or 0, "unknown"))
    return ProbeError(phase, "unexpected", type(exc).__name__)


class Connection(object):
    """One TCP+TLS connection. In the verify modes connect() is callable once
    per process; the bench modes (multi=True) open many on purpose."""

    connects = 0

    def __init__(self, args, multi=False, ctx=None):
        self.args = args
        self.multi = multi
        self.ctx = ctx  # shared context (hold-many); None = one per connect
        self.sock = None
        self.socket_id = uuid.uuid4().hex
        self.buf = b""
        self.info = {}
        self.timing = {}

    def connect(self):
        if Connection.connects and not self.multi:
            raise ProbeError("connect", "reconnect_forbidden")
        Connection.connects += 1
        ctx = self.ctx or client_context(self.args)
        start = time.perf_counter()
        try:
            raw = socket.create_connection((self.args.ip, self.args.port), timeout=self.args.connect_timeout)
        except Exception as exc:  # noqa: BLE001 - classified below
            raise classify("connect", exc)
        tcp_done = time.perf_counter()
        try:
            self.sock = ctx.wrap_socket(raw, server_hostname=self.args.host)
        except Exception as exc:  # noqa: BLE001
            raw.close()
            raise classify("handshake", exc)
        self.timing = {
            "tcp_ms": (tcp_done - start) * 1000.0,
            "handshake_ms": (time.perf_counter() - start) * 1000.0,
        }
        der = self.sock.getpeercert(binary_form=True) or b""
        local = self.sock.getsockname()
        self.info = {
            "verified": bool(der) and ctx.verify_mode == ssl.CERT_REQUIRED and ctx.check_hostname,
            "leaf_sha256": hashlib.sha256(der).hexdigest() if der else "",
            "tls_version": self.sock.version() or "",
            "alpn": self.sock.selected_alpn_protocol() or "",
            "local_ip": local[0],
            "local_port": local[1],
            "socket_id": self.socket_id,
        }

    def request(self, tag, seq):
        target = "/cbv?tag=%s&seq=%d" % (tag, seq)
        head = (
            "GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: osep0023-cbv/%d\r\n"
            "Accept: application/json\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n"
            % (target, self.args.host, PROTOCOL_VERSION)
        )
        self.sock.settimeout(self.args.request_timeout)
        try:
            self.sock.sendall(head.encode("ascii"))
        except Exception as exc:  # noqa: BLE001
            raise classify("send", exc)
        try:
            return self._read_response()
        except Exception as exc:  # noqa: BLE001
            raise classify("recv", exc)

    def _fill(self):
        chunk = self.sock.recv(4096)
        if not chunk:
            raise ProbeError("recv", "eof")
        self.buf += chunk

    def _read_response(self):
        while b"\r\n\r\n" not in self.buf:
            if len(self.buf) > MAX_HEADER_BYTES:
                raise ProbeError("recv", "header_too_large")
            self._fill()
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        lines = head.decode("iso-8859-1").split("\r\n")
        parts = lines[0].split(" ", 2)
        if len(parts) < 2 or not parts[0].startswith("HTTP/"):
            raise ProbeError("recv", "bad_status_line")
        headers = {}
        for line in lines[1:]:
            name, _, value = line.partition(":")
            headers[name.strip().lower()] = value.strip()
        if "content-length" not in headers:
            raise ProbeError("recv", "no_content_length")
        length = int(headers["content-length"])
        if length < 0 or length > MAX_BODY_BYTES:
            raise ProbeError("recv", "bad_content_length")
        while len(self.buf) < length:
            self._fill()
        body, self.buf = self.buf[:length], self.buf[length:]
        try:
            origin = json.loads(body.decode("utf-8"))
        except ValueError:
            origin = None
        return {
            "http_version": parts[0],
            "status": int(parts[1]),
            "connection": headers.get("connection", ""),
            "origin": origin,
        }

    def close(self):
        if self.sock is not None:
            try:
                self.sock.close()
            except OSError:
                pass


def result(conn, responses, error):
    out = {"v": PROTOCOL_VERSION, "ok": error is None, "connects": Connection.connects}
    out.update(conn.info)
    out["responses"] = responses
    if responses:
        out["last"] = responses[-1]
    if error is not None:
        out["error"] = {"phase": error.phase, "kind": error.kind, "detail": error.detail[:120]}
        out["closed_by_peer"] = error.kind in CLOSED_KINDS
    return out


def write_json(path, payload):
    tmp = path + ".tmp"
    with open(tmp, "w") as handle:
        json.dump(payload, handle, sort_keys=True)
    os.rename(tmp, path)


def run_once(args):
    conn = Connection(args)
    responses, error = [], None
    try:
        conn.connect()
        for seq in range(1, args.count + 1):
            responses.append(conn.request(args.tag, seq))
    except ProbeError as exc:
        error = exc
    finally:
        conn.close()
    payload = result(conn, responses, error)
    sys.stdout.write(RESULT_PREFIX + json.dumps(payload, sort_keys=True) + "\n")
    sys.stdout.flush()
    return 0 if error is None else 3


def run_hold(args):
    if not os.path.isdir(args.dir):
        os.makedirs(args.dir)
    conn = Connection(args)
    error = None
    try:
        conn.connect()
        first = conn.request(args.tag, 1)
    except ProbeError as exc:
        write_json(os.path.join(args.dir, "r1.json"), result(conn, [], exc))
        conn.close()
        return 3
    write_json(os.path.join(args.dir, "r1.json"), result(conn, [first], None))
    deadline = time.time() + args.wait_timeout
    go = os.path.join(args.dir, "go")
    while not os.path.exists(go):
        if time.time() >= deadline:
            error = ProbeError("wait", "go_timeout")
            break
        time.sleep(0.2)
    responses = []
    if error is None:
        try:
            responses.append(conn.request(args.tag, 2))
        except ProbeError as exc:
            error = exc
    conn.close()
    write_json(os.path.join(args.dir, "r2.json"), result(conn, responses, error))
    return 0 if error is None else 3

# --- bench helpers (pure; unit-tested)

def nearest_rank(ordered, pct):
    """Nearest-rank percentile of an ascending list (None when empty)."""
    if not ordered:
        return None
    rank = int(-(-pct * len(ordered) // 100))  # ceil without floats
    return ordered[max(0, rank - 1)]


def latency_summary(values):
    """n/mean/p50/p90/p99/min/max in the unit of ``values`` (3 decimals)."""
    if not values:
        return {"n": 0}
    ordered = sorted(values)
    out = {"n": len(ordered), "mean": sum(ordered) / len(ordered), "min": ordered[0], "max": ordered[-1]}
    for pct in (50, 90, 99):
        out["p%d" % pct] = nearest_rank(ordered, pct)
    return dict((k, round(v, 3) if isinstance(v, float) else v) for k, v in out.items())


def tally(counts, key):
    counts[key] = counts.get(key, 0) + 1


def error_key(exc):
    return "%s/%s" % (exc.phase, exc.kind)


def origin_cred(response):
    origin = response.get("origin") if response else None
    if not isinstance(origin, dict):
        return "no_origin_body"
    return origin.get("cred") or "absent"


def origin_response(response):
    """Bench modes count only a 200 from the origin fixture. A proxy-local
    403/503 or a body that is not origin evidence is a failed sample."""
    if response.get("status") != 200:
        raise ProbeError("response", "http_%d" % response.get("status", 0))
    if not isinstance(response.get("origin"), dict):
        raise ProbeError("response", "no_origin_body")
    return response


def client_context(args):
    ctx = ssl.create_default_context(cafile=args.cafile)
    ctx.check_hostname = True
    ctx.verify_mode = ssl.CERT_REQUIRED
    try:
        ctx.set_alpn_protocols(["http/1.1"])
    except (AttributeError, NotImplementedError):
        pass
    return ctx


def raise_nofile(want):
    """Best effort: lift the soft fd limit for hold-many (never above hard)."""
    try:
        import resource  # noqa: PLC0415 - POSIX only
        soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
        target = want if hard == resource.RLIM_INFINITY else min(want, hard)
        if soft != resource.RLIM_INFINITY and soft < target:
            resource.setrlimit(resource.RLIMIT_NOFILE, (target, hard))
    except (ImportError, ValueError, OSError):
        pass


def run_handshake_bench(args):
    tcp, handshake, request = [], [], []
    leaves, creds, errors = {}, {}, {}
    started = time.time()
    for seq in range(1, args.samples + 1):
        conn = Connection(args, multi=True)
        try:
            conn.connect()
            tally(leaves, conn.info.get("leaf_sha256") or "none")
            sent = time.perf_counter()
            response = origin_response(conn.request(args.tag, seq))
            request.append((time.perf_counter() - sent) * 1000.0)
            tcp.append(conn.timing["tcp_ms"])
            handshake.append(conn.timing["handshake_ms"])
            tally(creds, origin_cred(response))
        except ProbeError as exc:
            tally(errors, error_key(exc))
        finally:
            conn.close()
        if args.interval_ms:
            time.sleep(args.interval_ms / 1000.0)
    payload = {
        "v": PROTOCOL_VERSION,
        "mode": "handshake-bench",
        "samples": args.samples,
        "ok": len(handshake),
        "failed": args.samples - len(handshake),
        "elapsed_s": round(time.time() - started, 3),
        "tcp_ms": latency_summary(tcp),
        "handshake_ms": latency_summary(handshake),
        "request_ms": latency_summary(request),
        "handshake_ms_raw": [round(v, 3) for v in handshake],
        "leaf_sha256": leaves,
        "origin_cred": creds,
        "errors": errors,
    }
    if args.out:
        write_json(args.out, payload)
    sys.stdout.write(BENCH_PREFIX + json.dumps(payload, sort_keys=True) + "\n")
    sys.stdout.flush()
    return 0 if not errors else 3


def run_hold_many(args):
    if not os.path.isdir(args.dir):
        os.makedirs(args.dir)
    raise_nofile(args.conns + 64)
    held, leaves, creds, errors = [], {}, {}, {}
    started = time.time()
    # One context for every held socket: a context per connection keeps its
    # CA store alive per socket and inflates guest memory at large K.
    ctx = client_context(args)
    phase = "holding"
    try:
        for seq in range(1, args.conns + 1):
            conn = Connection(args, multi=True, ctx=ctx)
            try:
                conn.connect()
                tally(leaves, conn.info.get("leaf_sha256") or "none")
                tally(creds, origin_cred(origin_response(conn.request(args.tag, 1))))
                held.append(conn)
            except ProbeError as exc:
                tally(errors, error_key(exc))
                conn.close()
            except OSError as exc:  # e.g. EMFILE outside connect/request
                tally(errors, error_key(classify("hold", exc)))
                conn.close()
    except MemoryError:
        # Report what is held instead of dying without a status file.
        tally(errors, "hold/memory_error")
        phase = "aborted"
    status = {
        "v": PROTOCOL_VERSION,
        "mode": "hold-many",
        "phase": phase,
        "target": args.conns,
        "held": len(held),
        "failed": args.conns - len(held),
        "open_s": round(time.time() - started, 3),
        "leaf_sha256": leaves,
        "origin_cred": creds,
        "errors": errors,
    }
    write_json(os.path.join(args.dir, "status.json"), status)
    deadline = time.time() + args.wait_timeout
    go = os.path.join(args.dir, "go")
    released = True
    while not os.path.exists(go):
        if time.time() >= deadline:
            released = False
            break
        time.sleep(0.5)
    alive, closed = 0, {}
    for conn in held:
        try:
            origin_response(conn.request(args.tag, 2))
            alive += 1
        except ProbeError as exc:
            tally(closed, error_key(exc))
        conn.close()
    done = dict(status, phase="done", released=released, alive_at_release=alive, release_errors=closed)
    write_json(os.path.join(args.dir, "done.json"), done)
    return 0 if not errors and not closed and released else 3


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("mode", choices=("once", "hold", "handshake-bench", "hold-many", "version"))
    parser.add_argument("--host", default="")
    parser.add_argument("--ip", default="")
    parser.add_argument("--port", type=int, default=443)
    parser.add_argument("--cafile", default=None)
    parser.add_argument("--tag", default="probe")
    parser.add_argument("--count", type=int, default=1)
    parser.add_argument("--dir", default="")
    parser.add_argument("--connect-timeout", type=float, default=8.0)
    parser.add_argument("--request-timeout", type=float, default=20.0)
    parser.add_argument("--wait-timeout", type=float, default=1200.0)
    parser.add_argument("--samples", type=int, default=200)
    parser.add_argument("--interval-ms", type=int, default=0)
    parser.add_argument("--conns", type=int, default=100)
    parser.add_argument("--out", default="")
    args = parser.parse_args(argv)
    if args.mode != "version":
        if not args.host or not args.ip:
            parser.error("--host and --ip are required")
        if not all(c.isalnum() or c in "-_." for c in args.tag):
            parser.error("--tag must be [A-Za-z0-9._-]")
        if args.count < 1 or args.count > 10:
            parser.error("--count must be 1..10")
        if args.mode in ("hold", "hold-many") and not args.dir:
            parser.error("%s requires --dir" % args.mode)
        if not 1 <= args.samples <= MAX_BENCH_CONNECTIONS:
            parser.error("--samples must be 1..%d" % MAX_BENCH_CONNECTIONS)
        if not 1 <= args.conns <= MAX_BENCH_CONNECTIONS:
            parser.error("--conns must be 1..%d" % MAX_BENCH_CONNECTIONS)
        if not 0 <= args.interval_ms <= 10000:
            parser.error("--interval-ms must be 0..10000")
        if args.cafile is not None and not os.path.isfile(args.cafile):
            parser.error("--cafile does not exist")
    return args


def main(argv=None):
    args = parse_args(sys.argv[1:] if argv is None else argv)
    if args.mode == "version":
        sys.stdout.write("CBV-CLIENT %d\n" % PROTOCOL_VERSION)
        return 0
    if args.mode == "once":
        return run_once(args)
    if args.mode == "handshake-bench":
        return run_handshake_bench(args)
    if args.mode == "hold-many":
        return run_hold_many(args)
    return run_hold(args)


if __name__ == "__main__":
    sys.exit(main())
