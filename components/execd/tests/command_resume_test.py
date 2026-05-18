#!/usr/bin/env python3
# Copyright 2026 Alibaba Group Holding Ltd.
#
# Manual local test: POST /command (streaming output) -> disconnect -> GET /resume (catch-up + live tail),
# repeat at least 3 disconnect/resume rounds, then read until execution_complete on the last connection.
#
# Configure EXECD_URL (and optional EXECD_TOKEN). Examples:
#   EXECD_URL=localhost:44772
#   EXECD_URL=https://remote.example
#   python3 components/execd/tests/command_resume_test.py

from __future__ import annotations

import http.client
import json
import os
import ssl
import sys
import urllib.parse
from typing import Any

API_ACCESS_TOKEN_HEADER = "X-EXECD-ACCESS-TOKEN"


class RunCollector:
    """Aggregates stdout and execution_complete across connections for final assertions."""

    def __init__(self) -> None:
        self.stdout_by_eid: dict[int, str] = {}
        self.primary_stdout_lines = 0
        self.resume_stdout_lines = 0
        self.saw_complete = False

    def record(self, tag: str, ev: dict[str, Any]) -> None:
        t = ev.get("type")
        if t == "execution_complete":
            self.saw_complete = True
            return
        if t != "stdout":
            return
        eid = int(ev.get("eid") or 0)
        txt = (ev.get("text") or "").strip()
        if eid in self.stdout_by_eid:
            assert self.stdout_by_eid[eid] == txt, (
                f"duplicate eid {eid} with different text: {self.stdout_by_eid[eid]!r} vs {txt!r}"
            )
        else:
            self.stdout_by_eid[eid] = txt
        if tag == "primary":
            self.primary_stdout_lines += 1
        elif tag.startswith("resume"):
            self.resume_stdout_lines += 1

    def assert_ok(self) -> None:
        assert self.saw_complete, "expected execution_complete"
        assert self.resume_stdout_lines > 0, (
            "resume delivered no stdout lines; disconnect resume may not be working (check 409, STDOUT_PER_CHOP)"
        )
        assert len(self.stdout_by_eid) == OUTPUT_LINES, (
            f"expected {OUTPUT_LINES} stdout lines, got distinct eid count={len(self.stdout_by_eid)}"
        )
        for n in range(1, OUTPUT_LINES + 1):
            assert n in self.stdout_by_eid, f"missing eid={n}"
            assert self.stdout_by_eid[n] == f"tick{n}", (
                f"eid={n} text should be tick{n}, got {self.stdout_by_eid[n]!r}"
            )
        assert self.primary_stdout_lines >= 1, "primary connection should receive at least one stdout line"
        assert self.primary_stdout_lines + self.resume_stdout_lines == OUTPUT_LINES, (
            "primary + resume stdout line counts should equal total stdout lines (each line counted once): "
            f"primary={self.primary_stdout_lines} resume={self.resume_stdout_lines} expected={OUTPUT_LINES}"
        )
        print(
            "ASSERT ok: execution_complete + resume delivered output + tick1..tick"
            + str(OUTPUT_LINES)
            + " with eid 1.."
            + str(OUTPUT_LINES)
            + " complete",
            flush=True,
        )

# Execd base URL (host:port is ok; http:// is prepended if missing).
EXECD_URL = os.environ.get("EXECD_URL", "http://127.0.0.1:44772")
if "://" not in EXECD_URL:
    EXECD_URL = "http://" + EXECD_URL

TOKEN = os.environ.get("EXECD_TOKEN", "")

# Close each connection after this many stdout lines (three disconnect/resume rounds before the final read).
STDOUT_PER_CHOP = 15

# One primary disconnect plus (RESUME_CHOPS - 1) partial resume disconnects; last resume reads until complete.
RESUME_CHOPS = 3

# Bounded output: sleep 0.1s between lines, OUTPUT_LINES total; wall time ~ OUTPUT_LINES * 0.1s.
OUTPUT_LINES = 200

TIMEOUT_MS = 300_000

COMMAND = (
    "sh -c 'n=0; while [ \"$n\" -lt "
    + str(OUTPUT_LINES)
    + " ]; do n=$((n+1)); echo tick$n; sleep 0.1; done'"
)


def parse_frames(buf: bytes) -> tuple[list[dict[str, Any]], bytes]:
    out: list[dict[str, Any]] = []
    while True:
        i = buf.find(b"\n\n")
        if i < 0:
            return out, buf
        raw = buf[:i].strip()
        buf = buf[i + 2 :]
        if not raw:
            continue
        try:
            out.append(json.loads(raw.decode("utf-8")))
        except (json.JSONDecodeError, UnicodeDecodeError):
            pass


def connect(scheme: str, host: str, port: int) -> http.client.HTTPConnection:
    if scheme == "https":
        return http.client.HTTPSConnection(
            host, port, timeout=600, context=ssl.create_default_context()
        )
    return http.client.HTTPConnection(host, port, timeout=600)


def parse_url(base: str) -> tuple[str, str, int, str]:
    u = urllib.parse.urlparse(base.rstrip("/"))
    scheme = (u.scheme or "http").lower()
    host = u.hostname or "127.0.0.1"
    port = u.port or (443 if scheme == "https" else 80)
    return scheme, host, port, u.path or ""


def path_join(prefix: str, p: str) -> str:
    if not prefix:
        return p if p.startswith("/") else "/" + p
    return prefix.rstrip("/") + (p if p.startswith("/") else "/" + p)


def headers() -> dict[str, str]:
    h: dict[str, str] = {
        "Content-Type": "application/json",
        "Accept": "text/event-stream",
    }
    if TOKEN:
        h[API_ACCESS_TOKEN_HEADER] = TOKEN
    return h


def pump(
    resp: http.client.HTTPResponse,
    tag: str,
    max_eid: int,
    *,
    stop_after_stdout: int | None,
    collector: RunCollector | None = None,
) -> tuple[int, bool, str | None]:
    """Read SSE; update max_eid. If stop_after_stdout is a number, stop after that many stdout lines; if None, read until execution_complete."""
    buf = b""
    cmd_id: str | None = None
    stdout_n = 0
    complete = False
    while True:
        chunk = resp.read(8192)
        if not chunk:
            break
        buf += chunk
        frames, buf = parse_frames(buf)
        for ev in frames:
            if collector is not None:
                collector.record(tag, ev)
            t = ev.get("type")
            if t == "init":
                cmd_id = ev.get("text")
                print(f"[{tag}] init id={cmd_id}")
            elif t in ("stdout", "stderr"):
                eid = int(ev.get("eid") or 0)
                max_eid = max(max_eid, eid)
                txt = ev.get("text", "")
                print(f"[{tag}] {t} eid={eid} {txt!r}")
                if t == "stdout":
                    stdout_n += 1
            elif t == "execution_complete":
                complete = True
                print(f"[{tag}] execution_complete ms={ev.get('execution_time')}")
            elif t != "ping":
                print(f"[{tag}] {t}")

            if complete:
                return max_eid, True, cmd_id
            if stop_after_stdout is not None and stdout_n >= stop_after_stdout:
                return max_eid, False, cmd_id
    return max_eid, complete, cmd_id


def main() -> int:
    scheme, host, port, prefix = parse_url(EXECD_URL)
    h = headers()
    cmd_path = path_join(prefix, "/command")
    resume_tmpl = path_join(prefix, "/command/{id}/resume")
    collector = RunCollector()

    body = json.dumps({"command": COMMAND, "timeout": TIMEOUT_MS})
    conn = connect(scheme, host, port)
    conn.request("POST", cmd_path, body.encode("utf-8"), h)
    r = conn.getresponse()
    if r.status != 200:
        print(f"POST /command HTTP {r.status}", r.read().decode("utf-8", "replace"), file=sys.stderr)
        conn.close()
        return 1

    max_eid = 0
    max_eid, done, cid = pump(
        r,
        "primary",
        max_eid,
        stop_after_stdout=STDOUT_PER_CHOP,
        collector=collector,
    )
    conn.close()
    if not cid:
        print("no init", file=sys.stderr)
        return 1
    if done:
        print("command finished on primary connection (unexpected)", file=sys.stderr)
        return 0

    for round_i in range(RESUME_CHOPS):
        path = resume_tmpl.format(id=cid) + f"?after_eid={max_eid}"
        tag = f"resume{round_i + 1}"
        c2 = connect(scheme, host, port)
        c2.request("GET", path, headers=h)
        r2 = c2.getresponse()
        if r2.status == 409:
            print(
                f"{tag} HTTP 409: primary SSE still active; retry later or increase STDOUT_PER_CHOP",
                file=sys.stderr,
            )
            print(r2.read().decode("utf-8", "replace"), file=sys.stderr)
            c2.close()
            return 1
        if r2.status != 200:
            print(f"{tag} HTTP {r2.status}", r2.read().decode("utf-8", "replace"), file=sys.stderr)
            c2.close()
            return 1

        last = round_i == RESUME_CHOPS - 1
        max_eid, done, _ = pump(
            r2,
            tag,
            max_eid,
            stop_after_stdout=None if last else STDOUT_PER_CHOP,
            collector=collector,
        )
        c2.close()
        if done:
            try:
                collector.assert_ok()
            except AssertionError as e:
                print(f"ASSERT failed: {e}", file=sys.stderr)
                return 1
            print("done.")
            return 0

    print("done (unexpected: should have completed in last resume)", file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
