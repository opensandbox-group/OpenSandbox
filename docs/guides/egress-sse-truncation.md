---
title: Egress Mitmproxy SSE Truncation
description: Why large SSE bodies can be truncated when an HTTP/1.1 upstream closes its connection with a TCP RST, how to confirm it, and how to fix it on the gateway side.
---

# Egress Mitmproxy: Large SSE Chunks Truncated at the Tail

## Symptom

Sandboxes calling an LLM API through the egress sidecar sometimes receive a **truncated SSE stream**: a `data:` event larger than ~1 MB is cut off near the end, and the SDK reports malformed JSON or a dropped connection. The lost amount varies (a few bytes to hundreds of KB).

## Root cause

Byte-level instrumentation of the full data path shows this is **standard TCP RST semantics**, not a mitmproxy or OpenSandbox bug:

1. The upstream closes the connection while data is still unread in its receive buffer (e.g. a gateway that streams a response without consuming the client's request body). The kernel sends **TCP RST** instead of a clean FIN.
2. Per [RFC 793](https://www.rfc-editor.org/rfc/rfc793), a RST discards the receiver's unread kernel receive buffer — the unread tail of the response is lost before any application code sees it.
3. mitmproxy's h11 parser then hard-errors (`incomplete chunked read`), the flow is killed and the client connection closed.

Confirmed by a controlled A/B/C experiment (same server code, payload, proxy and client; only the close behavior differs):

| Close behavior | mitmproxy TCP signature | Client result |
|---|---|---|
| A. Unread request data on close → kernel RST | `ConnectionResetError` | Truncated, 5/5 |
| B. Request consumed, close → clean FIN | clean EOF | Complete, 5/5 |
| C. Request consumed + `SO_LINGER=0` forced RST (empty receive buffer) | `ConnectionResetError` | Truncated, 4/5 |

Cell C is the smoking gun: **RST alone reproduces the truncation even with no unread data; FIN never does**. A direct slow-reading client loses the whole body against server A and nothing against server B, so the loss is purely a function of how much of the tail is still unread when the RST lands. HTTP/2 upstreams and keep-alive connections are unaffected.

## Reproduce

Scripts in [`components/egress/tests/mitm-sse-truncation/`](https://github.com/opensandbox-group/OpenSandbox/blob/main/components/egress/tests/mitm-sse-truncation/run.sh) drive a local `mitmdump` (same options as egress transparent mode) against a synthetic RST-closing SSE upstream:

```bash
cd components/egress
./tests/mitm-sse-truncation/run.sh            # reproduce the truncation (TLS, 4 MiB, RST close)
./tests/mitm-sse-truncation/run.sh --plain     # controls that must pass
./tests/mitm-sse-truncation/run.sh --delay-close 1
./tests/mitm-sse-truncation/run.sh --read-request
./tests/mitm-sse-truncation/run.sh --probe --keep-workdir   # show the error hook in the mitmdump log
```

Requires `python3`, `openssl`, `mitmdump` (pass `--mitmdump PATH` if not on `PATH`). Options: `--size`, `--iterations`, `--delay-close`, `--plain`, `--read-request`, `--probe`, `--mitmdump`, `--port`, `--upstream-port`, `--workdir`, `--keep-workdir`.

## Fix (gateway side)

There is no code fix in mitmproxy or OpenSandbox — the bytes are gone from the kernel. The fix belongs to the upstream server.

**Confirm it is a RST first** (run on/in front of the gateway while reproducing):

```bash
tcpdump -ni any 'tcp port 443 and tcp[tcpflags] & (tcp-rst) != 0'   # RST right after the last response byte, no FIN
netstat -s | grep -i reset
# from a client: "Recv failure: Connection reset by peer" / curl: (56) instead of a clean EOF
```

**Then fix the close behavior** — the rule: consume the request before closing; for SSE, prefer not closing at all:

- **nginx**: keep `lingering_close on` (default) with `lingering_time 30s` / `lingering_timeout 5s`; check this especially if `proxy_request_buffering off` was set.
- **FastAPI / uvicorn / Starlette**: read the request body before streaming (`await request.body()`), otherwise a streaming response followed by close can leave the body unread → RST.
- **Go net/http**: drain the body explicitly (`io.Copy(io.Discard, r.Body)`); Go does not drain for you. Never use `SO_LINGER` 0 (`SetLinger(0)`) on streaming responses.
- **Generic**: prefer keep-alive (end chunked bodies with `0\r\n\r\n`); if you must close, drain the receive buffer first; HTTP/2 connections avoid this whole class of failure.

If the gateway cannot be changed: route the sandbox to the gateway over HTTP/2, or front it with a proxy that consumes the request body and forwards keep-alive.

## Status

- mitmproxy upstream: [mitmproxy/mitmproxy#8364](https://github.com/mitmproxy/mitmproxy/issues/8364) — closed as not-a-bug (TCP RST semantics).
- OpenSandbox tracking: [opensandbox-group/OpenSandbox#1461](https://github.com/opensandbox-group/OpenSandbox/issues/1461) — awaiting field confirmation.

A bigger egress-side read buffer was considered and measured: it shrinks the loss window at burst rate but never changes the user-visible outcome (the trailing SSE event is lost either way), so no patch is shipped.
