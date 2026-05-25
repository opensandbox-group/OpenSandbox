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
# Optional upstream failover check: tests/smoke-dns-upstream-failover.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# tests/ is two levels under repo root: components/egress/tests -> climb 3 levels.
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="opensandbox/egress:local"
containerName="egress-smoke-nft"
POLICY_PORT=18080
ALWAYS_RULES_DIR_HOST="$(mktemp -d -t egress-always-rules.XXXXXX)"

info() { echo "[$(date +%H:%M:%S)] $*"; }

cleanup() {
  docker rm -f "${containerName}" >/dev/null 2>&1 || true
  rm -rf "${ALWAYS_RULES_DIR_HOST}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

info "Building image ${IMG}"
docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"

info "Starting containerName"
docker run -d --name "${containerName}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=dns+nft \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM=8.8.8.8,8.8.4.4 \
  -p ${POLICY_PORT}:18080 \
  -v "${ALWAYS_RULES_DIR_HOST}:/var/egress/rules" \
  "${IMG}"

info "Waiting for policy server..."
for i in {1..50}; do
  if curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null; then
    break
  fi
  sleep 0.5
done

info "Pushing policy (allow by default; deny github.com & 10.0.0.0/8)"
curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"allow","egress":[{"action":"deny","target":"*.github.com"},{"action":"deny","target":"10.0.0.0/8"}]}'

run_in_app() {
  docker run --rm --network container:"${containerName}" curlimages/curl "$@"
}

pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

wait_until_always_flip() {
  local timeout_sec="${1:-90}"
  local elapsed=0
  local step_sec=2
  while [ "${elapsed}" -lt "${timeout_sec}" ]; do
    # Expected after refresh:
    # - deny.always blocks api.github.com
    # - allow.always allows www.mozilla.org
    if ! run_in_app -I https://api.github.com --max-time 8 >/dev/null 2>&1 \
      && run_in_app -I https://www.mozilla.org --max-time 8 >/dev/null 2>&1; then
      return 0
    fi
    sleep "${step_sec}"
    elapsed=$((elapsed + step_sec))
  done
  return 1
}

info "Test: allowed domain should succeed (google.com)"
run_in_app -I https://google.com --max-time 20 >/dev/null 2>&1 || fail "google.com should succeed"
pass "google.com allowed"

info "Test: denied domain should fail (api.github.com)"
if run_in_app -I https://api.github.com --max-time 8 >/dev/null 2>&1; then
  fail "api.github.com should be blocked"
else
  pass "api.github.com blocked"
fi

info "Test: allowed IP should succeed (1.1.1.1)"
run_in_app -I https://1.1.1.1 --max-time 10 >/dev/null 2>&1 || fail "1.1.1.1 should succeed"
pass "1.1.1.1 allowed"

info "Test: denied CIDR should fail (10.0.0.1)"
if run_in_app -I http://10.0.0.1 --max-time 5 >/dev/null 2>&1; then
  fail "10.0.0.1 should be blocked"
else
  pass "10.0.0.1 blocked"
fi

info "Test: DoT (853) should be blocked"
if run_in_app -k https://1.1.1.1:853 --max-time 5 >/dev/null 2>&1; then
  fail "DoT 853 should be blocked"
else
  pass "DoT 853 blocked"
fi

info "Rules update: wildcard deny -> patch allow specific (dns+nft)"
curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"allow","egress":[{"action":"deny","target":"*.cloudflare.com"}]}'

info "Test: www.cloudflare.com should be blocked initially (deny via wildcard)"
if run_in_app -I https://www.cloudflare.com --max-time 8 >/dev/null 2>&1; then
  fail "www.cloudflare.com should be blocked before patch"
else
  pass "www.cloudflare.com blocked before patch"
fi

info "Patching allow for www.cloudflare.com (specific should override earlier deny)"
curl -sSf -XPATCH "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '[{"action":"allow","target":"www.cloudflare.com"}]'

info "Test: www.cloudflare.com should be allowed after patch"
run_in_app -I https://www.cloudflare.com --max-time 20 >/dev/null 2>&1 || fail "www.cloudflare.com should succeed after patch"
pass "www.cloudflare.com allowed after patch"

info "Rules update: wildcard allow -> patch deny specific (dns+nft)"
curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.mozilla.org"}]}'

info "Test: www.mozilla.org should be allowed initially (allow via wildcard)"
run_in_app -I https://www.mozilla.org --max-time 20 >/dev/null 2>&1 || fail "www.mozilla.org should succeed before patch"
pass "www.mozilla.org allowed before patch"

info "Patching deny for www.mozilla.org (specific should override earlier allow)"
curl -sSf -XPATCH "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '[{"action":"deny","target":"www.mozilla.org"}]'

info "Test: www.mozilla.org should be blocked after patch"
if run_in_app -I https://www.mozilla.org --max-time 8 >/dev/null 2>&1; then
  fail "www.mozilla.org should be blocked after patch"
else
  pass "www.mozilla.org blocked after patch"
fi

info "Always-rule dynamic check (single transition)"
curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"api.github.com"}]}'

info "Baseline before file update: github allowed, mozilla blocked"
run_in_app -I https://api.github.com --max-time 20 >/dev/null 2>&1 || fail "api.github.com should be allowed before always file update"
if run_in_app -I https://www.mozilla.org --max-time 8 >/dev/null 2>&1; then
  fail "www.mozilla.org should be blocked before always file update"
fi
pass "baseline verified"

printf '%s\n' '*.github.com' > "${ALWAYS_RULES_DIR_HOST}/deny.always"
printf '%s\n' 'www.mozilla.org' > "${ALWAYS_RULES_DIR_HOST}/allow.always"
if wait_until_always_flip 90; then
  pass "always files reloaded (github blocked, mozilla allowed)"
else
  fail "always file update did not take effect in time"
fi

info "All smoke tests passed."