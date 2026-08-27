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

# Fleet profile (OSEP-0021) smoke test — real dns+nft end to end, no Docker,
# no external network. The fleet profile is inherently dns+nft (there is no
# dns-only mode), so this is the only fleet smoke variant.
#
# Topology (all on the host, requires root):
#
#   sandbox netns osb-sandbox-a   Pod/host netns        "ext" netns osb-ext
#   10.10.0.5/24 ──veth-a── veth-a-p 10.10.0.1/24       (external world)
#   DNS -> 10.10.0.1:53 ────────────► fleet dnsproxy    osb-ext 10.99.0.2/24
#   TCP  -> 10.99.0.2:8080 ─────────► forward hook ── veth-ext-p 10.99.0.1/24
#                                    (nft opensandbox-fleet)         └─ HTTP :8080
#
# The slot store is a real temp dir; the egress binary runs directly with the
# fleet profile. Every assertion below touches the real kernel (nft) or real
# packets (netns-to-netns).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
EGRESS_BIN="/tmp/osb-egress-fleet"

SLOT_DIR="$(mktemp -d -t fleet-slot.XXXXXX)"
RESOLV_A="$(mktemp -t fleet-resolv-a.XXXXXX)"
EGRESS_LOG="/tmp/fleet-egress.log"
POLICY_PORT=18080
UPSTREAM_ADDR="127.0.0.1:5300"

ALWAYS_RULES_DIR="/var/egress/rules"
SAVED_IP_FORWARD=""

# pids of helpers + egress
UPSTREAM_PID=""
EXT_PID=""
EGRESS_PID=""

info() { echo "[$(date +%H:%M:%S)] $*"; }
pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

require() {
  [ "$(id -u)" = "0" ] || fail "fleet smoke requires root (ip netns + nft)"
  for c in go nft ip python3 curl; do
    command -v "${c}" >/dev/null || fail "missing required command: ${c}"
  done
}

cleanup() {
  set +e
  [ -n "${EGRESS_PID}" ] && kill "${EGRESS_PID}" 2>/dev/null
  [ -n "${UPSTREAM_PID}" ] && kill "${UPSTREAM_PID}" 2>/dev/null
  [ -n "${EXT_PID}" ] && kill "${EXT_PID}" 2>/dev/null
  ip link del veth-a 2>/dev/null
  ip link del veth-ext 2>/dev/null
  ip netns del osb-sandbox-a 2>/dev/null
  ip netns del osb-ext 2>/dev/null
  if [ -n "${SAVED_IP_FORWARD}" ]; then
    sysctl -w net.ipv4.ip_forward="${SAVED_IP_FORWARD}" >/dev/null 2>&1
  fi
  if [ -n "${SAVED_FORWARD_POLICY}" ]; then
    iptables -t filter -P FORWARD ACCEPT >/dev/null 2>&1 || true
    [ "${SAVED_FORWARD_POLICY}" = "-P FORWARD ACCEPT" ] || iptables -t filter -P FORWARD DROP >/dev/null 2>&1 || true
  fi
  rm -rf "${SLOT_DIR}" "${RESOLV_A}" 2>/dev/null
  nft delete table inet opensandbox-fleet 2>/dev/null
}
trap cleanup EXIT

# wait_for <timeout_sec> <label> <cmd...>
wait_for() {
  local timeout_sec="$1" label="$2"
  shift 2
  local elapsed=0
  while [ "${elapsed}" -lt "${timeout_sec}" ]; do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  fail "timed out waiting for: ${label}"
}

write_slot() {
  # write_slot <id> <uid> <gen> <ip> <veth> <resolv>
  local id="$1" uid="$2" gen="$3" ip="$4" veth="$5" resolv="$6"
  cat > "${SLOT_DIR}/${id}.json" <<EOF
{"id":"${id}","phase":"Bound","owner":{"sandboxUid":"${uid}","instanceGeneration":${gen},"assignmentAttempt":1},"ip":"${ip}","hostNetnsPath":"/var/run/netns/osb-sandbox-${uid}","hostVeth":"${veth}","gateway":"10.10.0.1","privateCidr":"10.10.0.0/24","dnsPath":"${resolv}"}
EOF
}

dns_query() {
  # dns_query <netns-or-host> <name>
  local where="$1" name="$2"
  if [ "${where}" = "host" ]; then
    python3 "${SCRIPT_DIR}/fleet_upstream.py" query 10.10.0.1 "${name}"
  else
    ip netns exec "${where}" python3 "${SCRIPT_DIR}/fleet_upstream.py" query 10.10.0.1 "${name}"
  fi
}

expect_rcode() {
  # expect_rcode <netns-or-host> <name> <rcode>
  local out rcode
  out="$(dns_query "$1" "$2")"
  rcode="$(echo "${out}" | sed -n 's/^rcode=\([0-9]*\).*/\1/p')"
  [ "${rcode}" = "$3" ] || fail "dns ${2}: expected rcode ${3}, got '${out}'"
}

expect_answers() {
  local out
  out="$(dns_query "$1" "$2")"
  echo "${out}" | grep -q "answers=$3" || fail "dns ${2}: expected answer $3, got '${out}'"
}

nft_has() { nft list table inet opensandbox-fleet 2>/dev/null | grep -q "$1"; }

# ns_nft_has <netns> <pattern>: assert inside a sandbox's OWN netns (the
# per-sandbox netns OUTPUT defense-in-depth table opensandbox-fleet-ns).
ns_nft_has() {
  ip netns exec "$1" nft list table inet opensandbox-fleet-ns 2>/dev/null | grep -q "$2"
}

start_egress() {
  info "Starting fleet egress"
  # DoH env overridable per-test (Test 10 exercises strict mode with an
  # empty blocklist). `${VAR-default}` (no colon) keeps a set-but-empty
  # value empty, so Test 10's `EGRESS_DOH_BLOCKLIST=""` really disables the
  # blocklist instead of falling back to the default.
  local block_doh="${EGRESS_BLOCK_DOH_443-true}"
  local doh_blocklist="${EGRESS_DOH_BLOCKLIST-203.0.113.1}"
  OPENSANDBOX_EGRESS_PROFILE=fleet \
  OPENSANDBOX_EGRESS_SLOT_STORE_DIR="${SLOT_DIR}" \
  OPENSANDBOX_EGRESS_SLOT_POLL_INTERVAL=1 \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM="${UPSTREAM_ADDR}" \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE=allow.test \
  OPENSANDBOX_EGRESS_HTTP_ADDR="127.0.0.1:${POLICY_PORT}" \
  OPENSANDBOX_EGRESS_BLOCK_DOH_443="${block_doh}" \
  OPENSANDBOX_EGRESS_DOH_BLOCKLIST="${doh_blocklist}" \
  "${EGRESS_BIN}" >"${EGRESS_LOG}" 2>&1 &
  EGRESS_PID=$!
  wait_for 30 "egress healthz" curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz"
}

push_policy() {
  # push_policy <uid> <json>
  curl -sSf -H "X-Fast-Sandbox-Uid: $1" -XPUT \
    "http://127.0.0.1:${POLICY_PORT}/policy" -d "$2" >/dev/null
}

set_up_netns() {
  # set_up_netns <uid> <ip>: first sandbox owns the gateway subnet on its
  # pod-side veth; later sandboxes get a /32 route back to their pod-side
  # veth (the gateway addr is shared, so return traffic must pick the right
  # interface). Both veth ends must be UP before adding routes (veth carrier
  # requires the peer up, and a route on a carrier-less device is rejected).
  local uid="$1" ip="$2"
  local ns="osb-sandbox-${uid}"
  ip netns add "${ns}"
  ip link add "veth-${uid}" type veth peer name "veth-${uid}-p"
  ip link set "veth-${uid}" netns "${ns}"
  ip -n "${ns}" link set lo up
  ip -n "${ns}" link set "veth-${uid}" up
  ip link set "veth-${uid}-p" up
  ip addr add 10.10.0.1/24 dev "veth-${uid}-p" 2>/dev/null || true
  ip -n "${ns}" addr add "${ip}/24" dev "veth-${uid}"
  ip -n "${ns}" route add default via 10.10.0.1
  ip route add "${ip}/32" dev "veth-${uid}-p"
}

###############################################################################
info "== Fleet profile smoke (dns+nft) =="
require

info "Preparing environment"
SAVED_IP_FORWARD="$(sysctl -n net.ipv4.ip_forward)"
sysctl -w net.ipv4.ip_forward=1 >/dev/null

# Hosts with Docker installed have iptables FORWARD policy DROP (docker's
# filter chains), which silently drops our veth-to-veth forwarding. The CI
# runner is an ephemeral VM, so flip it to ACCEPT (restored in cleanup).
SAVED_FORWARD_POLICY="$(iptables -t filter -S FORWARD 2>/dev/null | head -1)"
iptables -t filter -P FORWARD ACCEPT || true

# always-deny CIDR file must exist before egress starts (loaded once)
mkdir -p "${ALWAYS_RULES_DIR}"
printf '%s\n' '10.99.0.9' > "${ALWAYS_RULES_DIR}/deny.always"

info "Building egress binary"
(cd "${REPO_ROOT}/components/egress" && go build -o "${EGRESS_BIN}" .)

info "Setting up network namespaces"
set_up_netns a 10.10.0.5

ip netns add osb-ext
ip link add veth-ext type veth peer name veth-ext-p
ip link set veth-ext netns osb-ext
ip -n osb-ext link set lo up
ip -n osb-ext link set veth-ext up
ip link set veth-ext-p up
ip -n osb-ext addr add 10.99.0.2/24 dev veth-ext
ip addr add 10.99.0.1/24 dev veth-ext-p
ip -n osb-ext route add default via 10.99.0.1

info "Starting helper servers"
python3 "${SCRIPT_DIR}/fleet_upstream.py" dns >/dev/null 2>&1 &
UPSTREAM_PID=$!
ip netns exec osb-ext python3 "${SCRIPT_DIR}/fleet_upstream.py" ext >/dev/null 2>&1 &
EXT_PID=$!
wait_for 5 "ext http server up" ip netns exec osb-ext curl -s -m 2 -o /dev/null http://127.0.0.1:8080/

write_slot a a 1 10.10.0.5 veth-a-p "${RESOLV_A}"
start_egress

###############################################################################
info "Test 0: DoH-443 blocking installed globally (master chain)"
nft_has 'doh_block_v4' || fail "DoH blocklist set missing"
nft_has '203.0.113.1' || fail "DoH blocklist element missing"
nft_has 'doh_block_v4 tcp dport 443 drop' || fail "DoH 443 block rule missing"
pass "DoH-443 blocking installed (doh_block_v4 set + element + drop rule)"

###############################################################################
info "Test 1: deny-first registration (fail closed before any policy)"
wait_for 15 "subject a deny-first installed" nft_has 'subj_s_a'
nft_has 'ip saddr 10.10.0.5 iifname "veth-a-p" jump subj_s_a' || fail "dispatch rule missing"
nft_has 'subj_s_a_allow_v4 {' || fail "subject a static sets missing"
grep -q '^nameserver 10.10.0.1$' "${RESOLV_A}" || fail "resolv.conf not rewritten to gateway"
expect_rcode osb-sandbox-a allow.test 3
pass "deny-first registered (nft + resolv + NXDOMAIN)"

###############################################################################
info "Test 1b: per-sandbox netns OUTPUT defense-in-depth installed"
wait_for 15 "sandbox netns OUTPUT deny-first" ns_nft_has osb-sandbox-a 'hook output'
ns_nft_has osb-sandbox-a 'policy drop' || fail "sandbox OUTPUT chain must be drop-policy"
pass "sandbox netns OUTPUT chain installed (drop policy)"

###############################################################################
info "Test 2: policy push activates the subject (dns+nft)"
push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"},{"action":"allow","target":"10.99.0.2"}]}'
expect_answers osb-sandbox-a allow.test 1.1.1.1
expect_answers osb-sandbox-a other.test 1.1.1.2
expect_rcode osb-sandbox-a nope.test 3
pass "DNS per-subject policy (allow *.test, deny others)"

nft_has '10.99.0.2' || fail "static allow element missing from nft"
wait_for 10 "dns-learned dynamic allow" nft_has '1.1.1.1'
pass "nft static allow + DNS-learned dynamic lease"

wait_for 10 "sandbox netns static allow mirror" ns_nft_has osb-sandbox-a '10.99.0.2'
pass "sandbox netns OUTPUT mirrors policy (static allow element)"

###############################################################################
info "Test 3: real data path through the forward hook"
if ! ip netns exec osb-sandbox-a curl -s -m 5 -o /dev/null http://10.99.0.2:8080/; then
  echo "--- diagnostics (data path failure) ---"
  nft list table inet opensandbox-fleet 2>&1 | head -30
  echo "--- pod routes ---"; ip route
  echo "--- sandbox routes ---"; ip netns exec osb-sandbox-a ip route
  echo "--- ext routes ---"; ip netns exec osb-ext ip route
  echo "--- ext listener ---"; ip netns exec osb-ext ss -ltn 2>/dev/null || true
  echo "--- iptables FORWARD ---"; iptables -S FORWARD 2>/dev/null | head -5 || true
  echo "--- iptables FORWARD counters ---"; iptables -L FORWARD -v -n 2>/dev/null | head -5 || true
  echo "--- ping gateway ---"; ip netns exec osb-sandbox-a ping -c 1 -W 1 10.10.0.1 2>&1 || true
  echo "--- egress log tail ---"; tail -5 "${EGRESS_LOG}" 2>/dev/null || true
  fail "allowed destination must be reachable"
fi
pass "forward allow (dispatch -> subject chain -> ext netns)"

if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null http://10.99.0.9:8080/ 2>/dev/null; then
  fail "default-deny destination 10.99.0.9 must be dropped at forward"
fi
pass "forward drop (default deny)"

###############################################################################
info "Test 4: deny CIDR overrides allow (nft layer)"
push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"},{"action":"deny","target":"10.99.0.0/24"}]}'
if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null http://10.99.0.2:8080/ 2>/dev/null; then
  fail "deny CIDR must block 10.99.0.2"
fi
pass "deny CIDR enforced (atomic swap)"

###############################################################################
info "Test 5: unknown source is fail-closed"
# Query from the "ext" netns (10.99.0.2): a real external unknown source that
# traverses the gateway REDIRECT — must be denied (NXDOMAIN), never served.
expect_rcode osb-ext allow.test 3
pass "DNS from unknown source -> NXDOMAIN"

###############################################################################
info "Test 6: pending push flushed on registration"
http_code="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Fast-Sandbox-Uid: b" -XPUT \
  "http://127.0.0.1:${POLICY_PORT}/policy" \
  -d '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}')"
[ "${http_code}" = "202" ] || fail "push before slot must be 202 pending, got ${http_code}"
pass "push before slot cached as pending (202)"

set_up_netns b 10.10.0.6
write_slot b b 1 10.10.0.6 veth-b-p "${RESOLV_A}"
wait_for 15 "subject b active after pending flush" bash -c "curl -s -H 'X-Fast-Sandbox-Uid: b' http://127.0.0.1:${POLICY_PORT}/policy | grep -q active"
expect_answers osb-sandbox-b allow.test 1.1.1.1
pass "pending push applied on registration (subject b active, DNS works)"

###############################################################################
info "Test 7: rebind discards policy (fail closed until re-push)"
write_slot a a 2 10.10.0.5 veth-a-p "${RESOLV_A}"
wait_for 15 "rebind back to denying" bash -c "curl -s -H 'X-Fast-Sandbox-Uid: a' http://127.0.0.1:${POLICY_PORT}/policy | grep -q denying"
nft_has '10.99.0.2' && fail "stale policy must not survive a rebind"
ns_nft_has osb-sandbox-a '10.99.0.0/24' && fail "stale sandbox policy must not survive a rebind"
expect_rcode osb-sandbox-a allow.test 3
push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
expect_answers osb-sandbox-a allow.test 1.1.1.1
pass "rebind reset + re-push reactivates"

###############################################################################
info "Test 8: unload removes enforcement"
rm -f "${SLOT_DIR}/b.json"
wait_for 15 "subject b unloaded" bash -c "! nft list table inet opensandbox-fleet | grep -q subj_s_b"
if ip netns exec osb-sandbox-b nft list table inet opensandbox-fleet-ns 2>/dev/null | grep -q 'opensandbox-fleet-ns'; then
  fail "sandbox b OUTPUT table must be removed on unload"
fi
pass "unload removed chain/map element/sets"

###############################################################################
info "Test 9: restart recovery (reset -> rescan -> denying -> re-push)"
# Refresh the DNS-learned dyn lease (1.1.1.1) in BOTH layers right before the
# restart, so the post-restart "stale wiped" assertions are meaningful (the
# static allow 10.99.0.2 is already gone since Test 7's re-push).
expect_answers osb-sandbox-a allow.test 1.1.1.1
wait_for 10 "dyn lease present in sandbox netns before restart" ns_nft_has osb-sandbox-a '1.1.1.1'
kill "${EGRESS_PID}" 2>/dev/null
wait "${EGRESS_PID}" 2>/dev/null || true
EGRESS_PID=""
start_egress
wait_for 15 "subject a re-registered denying after restart" bash -c "curl -s -H 'X-Fast-Sandbox-Uid: a' http://127.0.0.1:${POLICY_PORT}/policy | grep -q denying"
nft_has '1.1.1.1' && fail "stale dyn leases must be wiped on restart (Pod table)"
wait_for 15 "sandbox netns re-installed deny-first after restart" ns_nft_has osb-sandbox-a 'hook output'
ns_nft_has osb-sandbox-a '1.1.1.1' && fail "stale dyn leases must be wiped on restart (sandbox netns)"
expect_rcode osb-sandbox-a allow.test 3
push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
expect_answers osb-sandbox-a allow.test 1.1.1.1
pass "restart recovery (stale wiped, re-push reactivates)"

###############################################################################
info "Test 10: strict DoH mode (no blocklist) drops all tcp 443 globally"
kill "${EGRESS_PID}" 2>/dev/null
wait "${EGRESS_PID}" 2>/dev/null || true
EGRESS_PID=""
EGRESS_DOH_BLOCKLIST="" start_egress
wait_for 15 "subject a re-registered after strict-mode restart" bash -c "curl -s -H 'X-Fast-Sandbox-Uid: a' http://127.0.0.1:${POLICY_PORT}/policy | grep -q denying"
nft_has 'tcp dport 443 drop' || fail "strict mode must install a bare tcp 443 drop"
nft_has 'doh_block_v4' && fail "strict mode must not create blocklist sets"
ns_nft_has osb-sandbox-a 'tcp dport 443 drop' || fail "strict mode must mirror the bare 443 drop into the sandbox netns"
pass "strict DoH mode enforced (bare 443 drop, no blocklist sets)"

###############################################################################
info "All fleet smoke tests passed."
