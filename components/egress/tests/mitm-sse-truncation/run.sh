#!/usr/bin/env bash

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

# Repro/regression test: upstream closes with unread request data -> kernel
# sends TCP RST -> RST flushes the receiver's kernel buffer -> the SSE tail is
# lost and the client stream is truncated. Standard TCP behavior (RFC 793),
# not a mitmproxy bug. See docs/components/egress-mitmproxy-sse-truncation.md.
#
# Default mode (TLS upstream, "speaks first", immediate close) reproduces the
# truncation. Controls that must NOT truncate: --plain, --delay-close 1,
# --read-request (clean FIN).
#
# Usage:
#   ./tests/mitm-sse-truncation/run.sh
#
# Options:
#   --size BYTES          SSE event payload size (default 4194304; truncation
#                         reliable at >= 4 MiB, smaller is timing-dependent)
#   --iterations N        client runs per mode (default 4)
#   --delay-close SECONDS keep the upstream open SECONDS after the body
#                         (default 0; positive value is a control)
#   --plain               plain-TCP control instead of the TLS repro
#   --read-request        upstream reads the request first (clean-FIN control)
#   --probe               load probe.py to surface the error hook in the log
#   --mitmdump PATH       path to the mitmdump binary (default: from PATH)
#   --port N              mitmdump listen port (default 19080)
#   --upstream-port N     upstream SSE server port (default 19011)
#   --workdir DIR         scratch dir for certs/logs (default: mktemp -d)
#   --keep-workdir        do not remove the scratch dir on exit
#
# Requires: python3, openssl, mitmdump.
#
# Exit status: 0 if every client run matched the expected outcome
# (TRUNCATED for the TLS repro, OK for the controls), 1 otherwise.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYSTEM_ADDON="$(cd "${SCRIPT_DIR}/../.." && pwd)/mitmscripts/system.py"
PYTHON="${PYTHON:-python3}"

SIZE=4194304
ITERATIONS=4
DELAY_CLOSE=0
PLAIN=0
READ_REQUEST=0
USE_PROBE=0
MITMDUMP="${MITMDUMP:-mitmdump}"
PORT=19080
UPSTREAM_PORT=19011
WORKDIR=""
KEEP_WORKDIR=0

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | grep -E '^\s*#' | sed 's/^\s*# \{0,1\}//'
  exit "${1:-1}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --size) SIZE="$2"; shift 2 ;;
    --iterations) ITERATIONS="$2"; shift 2 ;;
    --delay-close) DELAY_CLOSE="$2"; shift 2 ;;
    --plain) PLAIN=1; shift ;;
    --read-request) READ_REQUEST=1; shift ;;
    --probe) USE_PROBE=1; shift ;;
    --mitmdump) MITMDUMP="$2"; shift 2 ;;
    --port) PORT="$2"; shift 2 ;;
    --upstream-port) UPSTREAM_PORT="$2"; shift 2 ;;
    --workdir) WORKDIR="$2"; shift 2 ;;
    --keep-workdir) KEEP_WORKDIR=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown option: $1" >&2; usage 1 ;;
  esac
done

info() { echo "[$(date +%H:%M:%S)] $*"; }

if [[ -z "$WORKDIR" ]]; then
  WORKDIR="$(mktemp -d /tmp/mitm-sse-truncation.XXXXXX)"
fi
mkdir -p "$WORKDIR"
CERT="$WORKDIR/cert.pem"
KEY="$WORKDIR/key.pem"
MITM_LOG="$WORKDIR/mitm.log"
cleanup() {
  [[ "$KEEP_WORKDIR" -eq 1 ]] && info "workdir kept: $WORKDIR" || rm -rf "$WORKDIR"
}
trap cleanup EXIT

if [[ "$PLAIN" -eq 1 ]]; then
  MODE_LABEL="plain TCP upstream + immediate close (control)"
  EXPECT="OK"
  # The "speaks first" variant only works over TLS; plain TCP must wait for
  # the request or mitmproxy rejects the premature response as a 502.
  UPSTREAM_ARGS=(--plain --read-request)
else
  MODE_LABEL="TLS HTTP/1.1 upstream, close after ${DELAY_CLOSE}s (immediate)"
  # awk performs the float comparison bash cannot do (e.g. --delay-close 0.5)
  if awk -v v="$DELAY_CLOSE" 'BEGIN{exit !(v>0)}'; then EXPECT="OK"; else EXPECT="TRUNCATED"; fi
  UPSTREAM_ARGS=(--delay-close "$DELAY_CLOSE" --cert "$CERT" --key "$KEY")
  if [[ "$READ_REQUEST" -eq 1 ]]; then
    # clean-FIN control: must not truncate
    UPSTREAM_ARGS+=(--read-request)
    EXPECT="OK"
    MODE_LABEL="TLS HTTP/1.1 upstream, reads request first (clean-FIN control)"
  fi
fi

if [[ ! "$DELAY_CLOSE" =~ ^[0-9]*\.?[0-9]+$ ]]; then
  echo "invalid --delay-close: $DELAY_CLOSE (expected a non-negative number)" >&2
  exit 1
fi

if ! command -v "$MITMDUMP" >/dev/null 2>&1; then
  echo "mitmdump not found: $MITMDUMP (set --mitmdump or MITMDUMP=)" >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl not found" >&2
  exit 1
fi
if ! "$PYTHON" -c "import ssl, socket" >/dev/null 2>&1; then
  echo "python3 with ssl/socket not found" >&2
  exit 1
fi

if [[ "$PLAIN" -eq 0 ]]; then
  if [[ ! -f "$CERT" ]]; then
    openssl req -x509 -newkey rsa:2048 -keyout "$KEY" -out "$CERT" \
      -days 2 -nodes -subj "/CN=127.0.0.1" \
      -addext "subjectAltName=IP:127.0.0.1,DNS:127.0.0.1" >/dev/null 2>&1
  fi
  if [[ ! -f "$SYSTEM_ADDON" ]]; then
    echo "system addon not found: $SYSTEM_ADDON" >&2
    exit 1
  fi
fi

info "mode: $MODE_LABEL"
info "payload size: $SIZE bytes, iterations: $ITERATIONS"
info "mitmdump: $MITMDUMP (port $PORT), upstream: 127.0.0.1:$UPSTREAM_PORT"

pids=()
stop_all() {
  for pid in "${pids[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  wait 2>/dev/null || true
}
trap 'stop_all; cleanup' EXIT

MITM_ARGS=(--mode regular --listen-port "$PORT" --set flow_detail=0)
MITM_ARGS+=(--set connection_strategy=lazy --set ssl_insecure=true)
MITM_ARGS+=(-s "$SYSTEM_ADDON")
[[ "$USE_PROBE" -eq 1 ]] && MITM_ARGS+=(-s "$SCRIPT_DIR/probe.py")

"$MITMDUMP" "${MITM_ARGS[@]}" > "$MITM_LOG" 2>&1 &
pids+=("$!")
"$PYTHON" "$SCRIPT_DIR/upstream_server.py" --listen-port "$UPSTREAM_PORT" \
  --payload-size "$SIZE" "${UPSTREAM_ARGS[@]}" > "$WORKDIR/upstream.log" 2>&1 &
pids+=("$!")

sleep 3
if ! curl -s -o /dev/null -m 3 "http://127.0.0.1:$PORT/" >/dev/null 2>&1; then
  echo "mitmdump did not start; see $MITM_LOG" >&2
  stop_all
  exit 1
fi

ok=0
truncated=0
fails=0
for i in $(seq 1 "$ITERATIONS"); do
  if [[ "$PLAIN" -eq 1 ]]; then
    out="$("$PYTHON" "$SCRIPT_DIR/client.py" --proxy-port "$PORT" \
      --upstream-port "$UPSTREAM_PORT" --payload-size "$SIZE" --plain 2>&1 || true)"
  else
    out="$("$PYTHON" "$SCRIPT_DIR/client.py" --proxy-port "$PORT" \
      --upstream-port "$UPSTREAM_PORT" --payload-size "$SIZE" 2>&1 || true)"
  fi
  echo "  run $i: $out"
  case "$out" in
    *"-> OK"*) ok=$((ok + 1)) ;;
    *"-> TRUNCATED"*) truncated=$((truncated + 1)) ;;
    *) fails=$((fails + 1)) ;;
  esac
done

info "result: ok=$ok truncated=$truncated failed=$fails (expected: $EXPECT)"
if [[ "$fails" -gt 0 ]]; then
  info "client failures; tail of $MITM_LOG:"
  tail -20 "$MITM_LOG"
  exit 1
fi
if [[ "$EXPECT" == "TRUNCATED" && "$truncated" -ge 1 ]]; then
  info "bug reproduced: $truncated/$ITERATIONS runs truncated"
  exit 0
elif [[ "$EXPECT" == "TRUNCATED" ]]; then
  info "expected truncation but the race did not hit this time (try --iterations 8)"
  exit 1
fi
if [[ "$EXPECT" == "OK" && "$ok" -eq "$ITERATIONS" ]]; then
  info "control passed: no truncation"
  exit 0
fi
info "control failed: some runs truncated unexpectedly"
exit 1
