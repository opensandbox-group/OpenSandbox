#!/bin/bash

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

# Smoke: dead upstreams are dropped from the active resolver list after health probes,
# so DNS forwarding does not wait on a black-holed address first.
#
# Requires Docker with --cap-add=NET_ADMIN. The container must reach the "good" resolver
# (default 8.8.8.8). Override with GOOD_DNS if needed.
#
# Example:
#   ./smoke-dns-upstream-probe.sh
#   GOOD_DNS=1.1.1.1 ./smoke-dns-upstream-probe.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
containerName="egress-smoke-dns-upstream-probe"
POLICY_PORT=18080
# Overwritten each run; inspect locally after failure.
EGRESS_LOG_FILE="${SCRIPT_DIR}/egress-smoke-dns-upstream-probe.egress.log"

# Nothing listens here; probes and forwards should skip it quickly once marked dead.
DEAD_PORT="${DEAD_PORT:-59123}"
GOOD_DNS="${GOOD_DNS:-8.8.8.8}"

info() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  docker rm -f "${containerName}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

info "Building image ${IMG}"
docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"

info "Starting ${containerName} (dead upstream 127.0.0.1:${DEAD_PORT} then ${GOOD_DNS}:53)"
docker run -d --name "${containerName}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns \
  -e OPENSANDBOX_EGRESS_RULES='{"defaultAction":"allow"}' \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM="127.0.0.1:${DEAD_PORT},${GOOD_DNS}:53" \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM_TIMEOUT=10 \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE_INTERVAL_SEC=30 \
  -e OPENSANDBOX_EGRESS_LOG_LEVEL=info \
  -p "${POLICY_PORT}:18080" \
  "${IMG}"

info "Waiting for policy server..."
for _ in {1..50}; do
  if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.5
done

pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

UPSTREAM_ENV="127.0.0.1:${DEAD_PORT},${GOOD_DNS}:53"
info "Configured OPENSANDBOX_EGRESS_DNS_UPSTREAM=${UPSTREAM_ENV}"
info "(dig only shows 127.0.0.1:15353; unreachable upstreams are logged per probe as \"[dns] upstream unreachable:\".)"

info "Resolving via local DNS proxy (127.0.0.1:15353); if dead upstream were still first, this would take ~upstream-timeout (10s)."
# First probe runs right after Start; wait so active list is pruned before dig.
sleep 3

out="$(docker exec "${containerName}" dig @127.0.0.1 -p 15353 +tries=1 +time=25 example.com. 2>&1)" || true
echo "${out}" | tail -n 5

qt="$(echo "${out}" | sed -n 's/^;; Query time: \([0-9]*\) msec/\1/p' | head -1)"
if [[ -z "${qt}" ]]; then
  fail "could not parse dig Query time (dig failed? output above)"
fi

# Without active-list pruning, the proxy would try 127.0.0.1:DEAD_PORT first and block ~10s.
if [[ "${qt}" -ge 8000 ]]; then
  fail "query took ${qt} msec (expected well under 8000 msec if dead upstream was removed from active list)"
fi

pass "dig completed in ${qt} msec (dead upstream not blocking)"

info "Saving egress container logs to ${EGRESS_LOG_FILE}"
docker logs "${containerName}"
docker logs "${containerName}" >"${EGRESS_LOG_FILE}" 2>&1

if ! grep -q '\[dns\] upstream probe' "${EGRESS_LOG_FILE}"; then
  fail "expected log line containing \"[dns] upstream probe\" (dead upstream probe failure); see ${EGRESS_LOG_FILE}"
fi
pass "egress log contains \"[dns] upstream probe\" (saved in ${EGRESS_LOG_FILE})"

info "All smoke tests passed."
