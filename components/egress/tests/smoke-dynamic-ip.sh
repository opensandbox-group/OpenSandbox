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

# Smoke test: default deny + domain allow in dns+nft mode.
# Verifies that allowing a domain causes its resolved IP to be added to nft (dynamic IP),
# so that curl to that domain succeeds without static IP/CIDR in policy.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# tests/ is two levels under repo root: components/egress/tests -> climb 3 levels.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
containerName="egress-smoke-dynamic-ip"
POLICY_PORT=18080

info() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  docker rm -f "${containerName}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

info "Building image ${IMG}"
docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"

info "Starting sidecar (dns+nft)"
docker run -d --name "${containerName}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns+nft \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM=8.8.8.8,8.8.4.4 \
  -p ${POLICY_PORT}:18080 \
  "${IMG}"

info "Waiting for policy server..."
for i in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.5
done

info "Pushing policy (default deny; allow google.com only)"
curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"google.com"}]}'

run_in_app() {
  docker run --rm --network container:"${containerName}" curlimages/curl "$@"
}

pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

info "Test: allowed domain (google.com) should succeed via dynamic IP"
run_in_app -I https://google.com --max-time 20 >/dev/null 2>&1 || fail "google.com should succeed (DNS allow + dynamic IP in nft)"
pass "google.com allowed"

info "Test: denied domain (api.github.com) should fail"
if run_in_app -I https://api.github.com --max-time 8 >/dev/null 2>&1; then
  fail "api.github.com should be blocked"
else
  pass "api.github.com blocked"
fi

info "Test: denied IP (1.1.1.1) should fail"
if run_in_app -I 1.1.1.1 --max-time 8 >/dev/null 2>&1; then
  fail "1.1.1.1 should be blocked"
else
  pass "1.1.1.1 blocked"
fi

info "All smoke tests (dynamic IP) passed."
