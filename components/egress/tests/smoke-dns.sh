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

# Simple smoke test using local image.
# Requires Docker with --cap-add=NET_ADMIN available.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# tests/ is two levels under repo root: components/egress/tests -> climb 3 levels.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
containerName="egress-smoke-dns"
POLICY_PORT=18080
POLICY_FILE_HOST="$(mktemp)"

info() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  docker rm -f "${containerName}" >/dev/null 2>&1 || true
  rm -f "${POLICY_FILE_HOST}" 2>/dev/null || true
}
trap cleanup EXIT

info "Building image ${IMG}"
docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"

info "Writing policy JSON to ${POLICY_FILE_HOST} (default deny; allow *.github.com)"
printf '%s\n' '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.github.com"}]}' >"${POLICY_FILE_HOST}"

info "Starting containerName (policy from file; env rules intentionally wrong to assert file wins)"
docker run -d --name "${containerName}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns \
  -e OPENSANDBOX_EGRESS_POLICY_FILE=/etc/opensandbox/egress-policy.json \
  -e OPENSANDBOX_EGRESS_RULES='{"defaultAction":"allow"}' \
  -v "${POLICY_FILE_HOST}:/etc/opensandbox/egress-policy.json" \
  -p ${POLICY_PORT}:18080 \
  "${IMG}"

info "Waiting for policy server..."
for i in {1..50}; do
  if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.5
done

run_in_app() {
  docker run --rm --network container:"${containerName}" curlimages/curl "$@"
}

pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

info "Test: denied domain should fail (google.com)"
if run_in_app -I https://google.com --max-time 5 >/dev/null 2>&1; then
  fail "google.com should be blocked"
else
  pass "google.com blocked"
fi

info "Test: allowed domain should succeed (api.github.com)"
run_in_app -I https://api.github.com --max-time 10 >/dev/null 2>&1 || fail "api.github.com should succeed"
pass "api.github.com allowed"

info "All smoke tests passed."