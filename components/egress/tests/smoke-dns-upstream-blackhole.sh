#!/bin/bash

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

# Smoke: an upstream that goes BLACK mid-run (IP vanished behind a silent drop) must not
# fail client queries: the proxy fails over to the next upstream after one exchange
# timeout, and the periodic probe then drops the dead upstream from the active list.
#
# Hermetic: two local DNS responders (tests/blackhole_upstream.py) play the upstreams,
# so no public resolver is needed. The black hole is a BOUND-BUT-SILENT socket: the
# first upstream answers at startup, then stops replying mid-run (socket stays bound,
# so no ICMP is ever sent and clients observe a full silent timeout — the same
# signature as an IP vanishing behind a routed black hole).
#
# Note on rejected alternatives: a closed port fails fast (ICMP refused), and an
# iptables OUTPUT DROP fails fast too — the kernel reports EPERM to a connected UDP
# socket for locally-dropped packets, which the proxy treats as an instant error and
# fails over within milliseconds. Neither reproduces a routed black hole; only the
# silent socket does.
#
# The 1500-4000ms two-sided bound on the window dig proves the dead upstream actually
# absorbed the full exchange timeout inside the ejection window (failover without the
# burned timeout fails the lower bound; the old 5s default fails the upper bound).
#
# Example:
#   ./smoke-dns-upstream-blackhole.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
containerName="egress-smoke-dns-upstream-blackhole"
POLICY_PORT=18080
# Overwritten each run; inspect locally after failure.
EGRESS_LOG_FILE="${SCRIPT_DIR}/egress-smoke-dns-upstream-blackhole.egress.log"

# Local upstreams: distinct loopback IPs (whole 127/8 is local on lo).
UPSTREAM_A="127.0.0.2"
UPSTREAM_A_PORT=5321
UPSTREAM_B="127.0.0.3"
UPSTREAM_B_PORT=5322
# Exchange timeout (sec): the window dig below must take ~this long (one burned
# timeout on the black-holed upstream) plus one cheap local round trip.
UPSTREAM_TIMEOUT="${UPSTREAM_TIMEOUT:-2}"
# Probe interval (sec). Rounds land at ~start+15s and ~start+30s; see the timeline
# comments below for why both the silence switch and the ejection wait straddle them.
PROBE_INTERVAL="${PROBE_INTERVAL:-15}"
# Seconds after responder start when upstream A goes silent. 18 keeps it healthy for
# the startup probe (~0s) and the second round (~15s), so the window phase still has
# A in the active list; silence then lands before the third round ejects it.
SILENT_AFTER="${SILENT_AFTER:-18}"

info() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { info "PASS: $*"; }

cleanup() {
  docker rm -f "${containerName}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# dig via the in-container proxy; fail unless NOERROR and min_ms <= Query time < max_ms.
run_dig() {
  local label="$1" min_ms="$2" max_ms="$3" out qt
  out="$(docker exec "${containerName}" dig @127.0.0.1 -p 15353 +tries=1 +time=25 example.com. 2>&1)" || true
  echo "${out}" | tail -n 4
  qt="$(echo "${out}" | sed -n 's/^;; Query time: \([0-9]*\) msec/\1/p' | head -1)"
  if [[ -z "${qt}" ]]; then
    fail "${label}: could not parse dig Query time (dig failed? output above)"
  fi
  if ! grep -q 'status: NOERROR' <<<"${out}"; then
    fail "${label}: expected NOERROR, got: $(grep -m1 -o 'status: [A-Z]*' <<<"${out}" || echo 'no header')"
  fi
  if [[ "${qt}" -lt "${min_ms}" || "${qt}" -ge "${max_ms}" ]]; then
    fail "${label}: query took ${qt} msec (expected ${min_ms}-${max_ms} msec)"
  fi
  info "${label}: NOERROR in ${qt} msec (${min_ms}-${max_ms})"
}

info "Building image ${IMG}"
docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"

info "Starting ${containerName} (local upstreams ${UPSTREAM_A}:${UPSTREAM_A_PORT}, ${UPSTREAM_B}:${UPSTREAM_B_PORT}, timeout=${UPSTREAM_TIMEOUT}s, probe=${PROBE_INTERVAL}s, A silent after ${SILENT_AFTER}s)"
docker run -d --name "${containerName}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns \
  -e OPENSANDBOX_EGRESS_RULES='{"defaultAction":"allow"}' \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM="${UPSTREAM_A}:${UPSTREAM_A_PORT},${UPSTREAM_B}:${UPSTREAM_B_PORT}" \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT="${UPSTREAM_TIMEOUT}" \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE_INTERVAL_SEC="${PROBE_INTERVAL}" \
  -e OPENSANDBOX_EGRESS_LOG_LEVEL=info \
  -p "${POLICY_PORT}:18080" \
  "${IMG}"

info "Starting local DNS responders (A answers for ${SILENT_AFTER}s, then stays bound-but-silent; B always answers)"
docker cp "${SCRIPT_DIR}/blackhole_upstream.py" "${containerName}:/tmp/blackhole_upstream.py"
docker exec -d "${containerName}" python3 /tmp/blackhole_upstream.py "${UPSTREAM_A}" "${UPSTREAM_A_PORT}" --silent-after "${SILENT_AFTER}"
docker exec -d "${containerName}" python3 /tmp/blackhole_upstream.py "${UPSTREAM_B}" "${UPSTREAM_B_PORT}"

info "Waiting for policy server..."
for _ in {1..50}; do
  if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.5
done

# Startup probe round marks both upstreams healthy while A is still answering.
sleep 3

info "Baseline: both upstreams healthy, first one must answer directly"
run_dig "baseline" 0 1000

# A goes silent ~${SILENT_AFTER}s after responder start; the second probe round
# (~15s) still sees it healthy, so it stays in the active list for the window.
info "Waiting for upstream A to go silently black"
sleep 15

silence=""
for _ in {1..3}; do
  out="$(docker exec "${containerName}" dig "@${UPSTREAM_A}" -p "${UPSTREAM_A_PORT}" +tries=1 +time=2 example.com. 2>&1)" || true
  if ! grep -q 'status: NOERROR' <<<"${out}"; then
    silence=1
    break
  fi
  info "upstream A still answering, waiting"
  sleep 1
done
if [[ -z "${silence}" ]]; then
  fail "upstream A never went silent (responder bug?)"
fi
pass "upstream A is silently black (bound socket, no reply, no ICMP)"

info "Ejection window: dead upstream still active; failover to ${UPSTREAM_B} must burn ~${UPSTREAM_TIMEOUT}s on it"
run_dig "window-failover" "$((UPSTREAM_TIMEOUT * 1000 - 500))" "$((UPSTREAM_TIMEOUT * 1000 + 2000))"

docker logs "${containerName}" >"${EGRESS_LOG_FILE}" 2>&1
if ! grep -q "upstream ${UPSTREAM_A}:${UPSTREAM_A_PORT} exchange error" "${EGRESS_LOG_FILE}"; then
  fail "expected log line \"upstream ${UPSTREAM_A}:${UPSTREAM_A_PORT} exchange error\" (failover attempt); see ${EGRESS_LOG_FILE}"
fi
pass "log shows exchange error on ${UPSTREAM_A} before failover (saved in ${EGRESS_LOG_FILE})"

# Third probe round (~30s) probes the silent A, times out, and ejects it (~32s);
# waiting 10s past the window dig (~22s worst case) clears that ejection.
info "Waiting for the probe to eject the black-holed upstream"
sleep 10

info "After ejection: queries must go straight to ${UPSTREAM_B} (no burned timeout)"
run_dig "after-ejection" 0 1000

docker logs "${containerName}" >"${EGRESS_LOG_FILE}" 2>&1
if ! grep -q "upstream probe ${UPSTREAM_A}:${UPSTREAM_A_PORT} failed" "${EGRESS_LOG_FILE}"; then
  fail "expected log line \"upstream probe ${UPSTREAM_A}:${UPSTREAM_A_PORT} failed\" (ejection); see ${EGRESS_LOG_FILE}"
fi
pass "log shows probe failure and ejection of ${UPSTREAM_A} (saved in ${EGRESS_LOG_FILE})"

info "All smoke tests passed."
