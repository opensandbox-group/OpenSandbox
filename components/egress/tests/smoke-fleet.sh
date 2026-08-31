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
  for c in go nft ip python3 curl openssl; do
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
  [ -n "${SSL_DIR:-}" ] && rm -rf "${SSL_DIR}" 2>/dev/null
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
  local mitm="${EGRESS_MITM-}"
  local mitm_env=()
  if [ "${mitm}" = "1" ]; then
    # ssl_insecure: the ext upstream serves a self-signed cert (curl -k on
    # the client side skips its own validation; the trust stack itself is a
    # fast-sandbox CA delivery, issue #19)
    mitm_env=(
      OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
      OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true
    )
  fi
  # env(1) is required: words produced by "${mitm_env[@]}" expansion are NOT
  # treated as environment assignments by bash (they'd be executed as commands).
  env \
  OPENSANDBOX_EGRESS_PROFILE=fleet \
  OPENSANDBOX_EGRESS_SLOT_STORE_DIR="${SLOT_DIR}" \
  OPENSANDBOX_EGRESS_SLOT_POLL_INTERVAL=1 \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM="${UPSTREAM_ADDR}" \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE=allow.test \
  OPENSANDBOX_EGRESS_HTTP_ADDR="127.0.0.1:${POLICY_PORT}" \
  OPENSANDBOX_EGRESS_BLOCK_DOH_443="${block_doh}" \
  OPENSANDBOX_EGRESS_DOH_BLOCKLIST="${doh_blocklist}" \
  "${mitm_env[@]}" \
  "${EGRESS_BIN}" >"${EGRESS_LOG}" 2>&1 &
  EGRESS_PID=$!
  wait_for 30 "egress healthz" curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz"
}

push_policy() {
  # push_policy <uid> <json>
  curl -sSf -H "X-Fast-Sandbox-Uid: $1" -XPUT \
    "http://127.0.0.1:${POLICY_PORT}/policy" -d "$2" >/dev/null
}

push_vault() {
  # push_vault <uid> <json>: POST the vault; on failure print the server's
  # rejection body (the fleet handler reports validation errors there).
  local uid="$1" json="$2" resp code body
  resp="$(curl -s -w '\n%{http_code}' -H "X-Fast-Sandbox-Uid: ${uid}" \
    -XPOST "http://127.0.0.1:${POLICY_PORT}/credential-vault" -d "${json}")"
  code="$(printf '%s' "${resp}" | tail -1)"
  body="$(printf '%s' "${resp}" | sed '$d')"
  [ "${code}" = "201" ] || fail "vault push for ${uid}: HTTP ${code}: ${body}"
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
# Self-signed cert for the ext HTTPS :443 echo (TLS interception smoke).
SSL_DIR="$(mktemp -d -t fleet-ssl.XXXXXX)"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "${SSL_DIR}/key.pem" \
  -out "${SSL_DIR}/cert.pem" -days 1 -subj "/CN=ext.test" >/dev/null 2>&1
python3 "${SCRIPT_DIR}/fleet_upstream.py" dns >/dev/null 2>&1 &
UPSTREAM_PID=$!
EXT_SSL_CERT="${SSL_DIR}/cert.pem" EXT_SSL_KEY="${SSL_DIR}/key.pem" \
  ip netns exec osb-ext python3 "${SCRIPT_DIR}/fleet_upstream.py" ext >/dev/null 2>&1 &
EXT_PID=$!
wait_for 5 "ext http server up" ip netns exec osb-ext curl -s -m 2 -o /dev/null http://127.0.0.1:8080/
wait_for 5 "ext https server up" ip netns exec osb-ext curl -sk -m 2 -o /dev/null https://127.0.0.1:443/

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
info "Test 11: MITM data plane — shared mitmdump + per-subject DNAT + subject-aware active vault"
if ! command -v mitmdump >/dev/null 2>&1 || ! id mitmproxy >/dev/null 2>&1; then
  info "SKIP MITM phase: mitmdump and/or the 'mitmproxy' user are not installed."
  info "  Install to match the egress image (CI does this in .github/workflows/egress-test.yml):"
  info "    apt-get install -y python3-venv"
  info "    useradd -r -u 10042 -d /var/lib/mitmproxy -s /usr/sbin/nologin mitmproxy"
  info "    python3 -m venv /opt/mitmproxy && /opt/mitmproxy/bin/pip install 'mitmproxy==11.0.2'"
  info "    ln -s /opt/mitmproxy/bin/mitmdump /usr/local/bin/mitmdump"
  info "  then re-run ./tests/smoke-fleet.sh (Test 11 runs last)."
else
  # Ship the baked-in mitm config AND the system addon (the egress image
  # carries both under /var/lib/mitmproxy and /var/egress; a bare host may
  # not — the addon path is a hard requirement of the mitmdump launch).
  mkdir -p /var/lib/mitmproxy/.mitmproxy
  cp -f "${SCRIPT_DIR}/../mitmproxy/config.yaml" /var/lib/mitmproxy/.mitmproxy/config.yaml
  mkdir -p /var/egress/mitmscripts
  cp -f "${SCRIPT_DIR}/../mitmscripts/system.py" /var/egress/mitmscripts/system.py
  chown -R mitmproxy:mitmproxy /var/lib/mitmproxy

  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress

  # CA exported into the dedicated fleet subdir (the fastlet mount point).
  wait_for 20 "fleet CA export" test -s /opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem
  pass "CA exported to /opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem"

  # Per-subject Pod-netns prerouting DNAT + management-dst exception.
  # (nft's canonical `list` output renders port sets with spaces { 80, 443 }
  # and prefixes the dnat address family: `dnat ip to`.)
  # the rules are veth-bound (iifname) so a forged source IP from another
  # sandbox's veth is not intercepted
  wait_for 15 "per-subject DNAT" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 iifname \"veth-a-p\" tcp dport { 80, 443 } dnat'"
  nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 iifname \"veth-a-p\" ip daddr 10.10.0.1 tcp dport { 80, 443 } return' \
    || fail "management-dst exception missing"
  pass "per-subject DNAT installed (10.10.0.5 -> 10.10.0.1:18081; gateway dst excluded)"

  # Subject-aware active vault API over the shared unix socket.
  ACTIVE_SOCK="/run/opensandbox/credential-proxy/active.sock"
  code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "${ACTIVE_SOCK}" 'http://localhost/credential-vault/_active?clientIp=10.10.0.5')"
  [ "${code}" = "404" ] || fail "active API: subject without vault must 404, got ${code}"
  code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "${ACTIVE_SOCK}" 'http://localhost/credential-vault/_active?clientIp=10.10.0.99')"
  [ "${code}" = "404" ] || fail "active API: unknown IP must 404, got ${code}"
  pass "active API dispatch (unknown IP 404, no-vault 404)"

  # Deny-first: 80/443 is DNATed to the shared mitm and delivered locally,
  # so the Pod-netns INPUT enforcement chain (ct-original policy) is the
  # authoritative layer even before any policy is pushed.
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null -H 'Host: ext.test' http://10.99.0.2/ 2>/dev/null; then
    fail "deny-first must block MITM 80/443 via the Pod INPUT chain"
  fi
  pass "deny-first blocks MITM 80/443 at the Pod INPUT chain"

  # Subject a: policy + vault, then a real HTTP request through the shared
  # mitm; the upstream must see the injected credential (proves interception
  # AND clientIp -> subject -> vault dispatch). Binding hosts must be FQDNs
  # (IP binding hosts are rejected), so the sandbox curls the ext netns with
  # an explicit Host header; the policy keeps allowing the real daddr
  # 10.99.0.2 so the nft layers pass the packet.
  push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  push_vault a '{"credentials":[{"name":"k","source":{"type":"inline","value":"secret-v1"}}],"bindings":[{"name":"b","match":{"schemes":["http","https"],"hosts":["ext.test"]},"auth":{"type":"apiKey","name":"X-Api-Key","credential":"k"}}]}'
  code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "${ACTIVE_SOCK}" 'http://localhost/credential-vault/_active?clientIp=10.10.0.5')"
  [ "${code}" = "200" ] || fail "active API: subject a vault must resolve, got ${code}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "request did not traverse the shared mitm (direct path?); got: ${out}"
  echo "${out}" | grep -qi "x-api-key: secret-v1" || fail "mitm must inject subject a's credential; got: ${out}"
  pass "real HTTP through shared mitm with credential injection (clientIp dispatch)"

  # Subject b (re-observed after Test 8's unload): a second sandbox with a
  # DIFFERENT policy (allows alt.test, not ext.test) and its own vault; each
  # sandbox's rules and credentials stay strictly isolated.
  write_slot b b 1 10.10.0.6 veth-b-p "${RESOLV_A}"
  wait_for 15 "subject b re-registered with DNAT" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.6 '"
  push_policy b '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"alt.test"}]}'

  # b's vault must reject a binding for a host its own policy does not allow
  # (ext.test belongs to subject a's policy) — per-subject rule isolation at
  # the vault validation layer.
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Fast-Sandbox-Uid: b" -XPOST \
    "http://127.0.0.1:${POLICY_PORT}/credential-vault" \
    -d '{"credentials":[{"name":"k","source":{"type":"inline","value":"leak"}}],"bindings":[{"name":"b","match":{"schemes":["http","https"],"hosts":["ext.test"]},"auth":{"type":"apiKey","name":"X-Api-Key","credential":"k"}}]}')"
  [ "${code}" = "400" ] || fail "b's vault must reject a binding outside its policy (ext.test), got ${code}"

  push_vault b '{"credentials":[{"name":"k","source":{"type":"inline","value":"secret-v2"}}],"bindings":[{"name":"b","match":{"schemes":["http","https"],"hosts":["alt.test"]},"auth":{"type":"apiKey","name":"X-Api-Key","credential":"k"}}]}'
  out="$(ip netns exec osb-sandbox-b curl -s -m 5 -H 'Host: alt.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key: secret-v2" || fail "mitm must inject subject b's credential; got: ${out}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject a request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key: secret-v1" || fail "subject a's credential must survive subject b's registration; got: ${out}"

  # Cross-policy isolation: b has no binding for ext.test and a has none for
  # alt.test — neither sandbox may receive the other's credential, even
  # though both can reach the same upstream IP.
  out="$(ip netns exec osb-sandbox-b curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key" && fail "b must NOT receive a credential for ext.test (a's host); got: ${out}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: alt.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject a request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key" && fail "a must NOT receive a credential for alt.test (b's host); got: ${out}"
  pass "per-subject policy + vault isolation (two sandboxes, distinct rules and credentials)"

  # HTTPS data plane: TLS interception + SO_ORIGINAL_DST(443) + credential
  # injection on the encrypted path. curl -k skips the sandbox-side trust
  # check (the CA delivery into sandboxes is fast-sandbox issue #19); the
  # mitm's own upstream validation is disabled via ssl_insecure (self-signed
  # ext cert). The client=10.99.0.1 oracle still proves the mitm proxied it.
  out="$(ip netns exec osb-sandbox-a curl -sk -m 5 -H 'Host: ext.test' https://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "https request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key: secret-v1" || fail "mitm must inject subject a's credential over TLS; got: ${out}"
  out="$(ip netns exec osb-sandbox-b curl -sk -m 5 -H 'Host: alt.test' https://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b https request did not traverse the shared mitm; got: ${out}"
  echo "${out}" | grep -qi "x-api-key: secret-v2" || fail "mitm must inject subject b's credential over TLS; got: ${out}"
  pass "HTTPS data plane (TLS interception + per-subject injection)"

  # Vault revision update reaches the data plane: PATCH the credential, wait
  # out the addon's 0.5s cache TTL, and assert the NEW value is injected.
  code="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Fast-Sandbox-Uid: a" -XPATCH \
    "http://127.0.0.1:${POLICY_PORT}/credential-vault" \
    -d '{"credentials":{"replace":[{"name":"k","source":{"type":"inline","value":"secret-v1-new"}}]}}')"
  [ "${code}" = "200" ] || fail "vault PATCH must succeed, got ${code}"
  sleep 1  # the addon vault cache TTL is 0.5s
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "x-api-key: secret-v1-new" || fail "PATCHed credential must be injected after TTL; got: ${out}"
  pass "vault revision update reaches the data plane (PATCH -> injected)"

  # Interception-set precision: :8080 is outside {80,443}, so it must go
  # DIRECT (client=10.10.0.5, the sandbox IP) with no credential injection.
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2:8080/)"
  echo "${out}" | grep -qi "client=10.10.0.5" || fail ":8080 must NOT be intercepted (direct path); got: ${out}"
  echo "${out}" | grep -qi "x-api-key" && fail ":8080 must not receive credentials; got: ${out}"
  pass "interception set precision (:8080 direct, no injection)"

  # Direct proxy-protocol connection to the mitm port: a default-allow
  # sandbox must not bypass the transparent interception by talking to the
  # gateway:18081 itself (the input chain drops non-DNATed traffic to it).
  if ip netns exec osb-sandbox-a curl -s -m 3 -x http://10.10.0.1:18081 -H 'Host: ext.test' http://ext.test/ 2>/dev/null; then
    fail "direct connection to the mitm port must be dropped"
  fi
  pass "direct mitm-port connection rejected"

  # DoH-443 blocking under MITM: the DNAT happens in the Pod netns prerouting
  # (AFTER the sandbox OUTPUT hook), so the sandbox-layer DoH rules still see
  # the real daddr/dport and keep dropping blocklisted 443 endpoints before
  # they can even reach the interceptor — while non-blocklisted 443 (the
  # HTTPS assertions above) is intercepted normally. Assert both rule layers
  # are present and a blocklisted endpoint stays unreachable.
  nft list table inet opensandbox-fleet 2>/dev/null | grep -q 'doh_block_v4 tcp dport 443 drop' \
    || fail "Pod-layer DoH-443 drop missing under MITM"
  ip netns exec osb-sandbox-a nft list table inet opensandbox-fleet-ns 2>/dev/null | grep -q 'doh_block_v4 tcp dport 443 drop' \
    || fail "sandbox-layer DoH-443 drop missing under MITM"
  ip netns exec osb-sandbox-a nft list table inet opensandbox-fleet-ns 2>/dev/null | grep -q '203.0.113.1' \
    || fail "doh blocklist element missing in the sandbox layer under MITM"
  if ip netns exec osb-sandbox-a curl -sk -m 2 -o /dev/null https://203.0.113.1/ 2>/dev/null; then
    fail "blocklisted DoH endpoint 203.0.113.1 must be unreachable under MITM"
  fi
  pass "DoH-443 blocking under MITM (both layers; blocklist still enforced)"
  # Compromised-sandbox scenario: DELETE the sandbox's own OUTPUT table
  # (not flush — a flushed hook chain keeps its drop policy, so flushing
  # would block everything and prove nothing). With the table gone the
  # sandbox has zero local enforcement; the Pod-netns authoritative layers
  # must still hold — the forward hook for non-MITM traffic, the INPUT
  # enforcement chain (ct-original policy) for the DNATed 80/443.
  ip netns exec osb-sandbox-a nft delete table inet opensandbox-fleet-ns
  if ! out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"; then
    echo "--- Pod netns fleet table (input chain + subject a sets) ---"
    nft list table inet opensandbox-fleet 2>&1 | grep -E "input|subj_s_a" | head -30
    echo "--- Pod netns conntrack (subject a) ---"
    conntrack -L 2>/dev/null | grep "10.10.0.5" | head -10 || true
    echo "--- sandbox a nft tables after flush ---"
    ip netns exec osb-sandbox-a nft list tables 2>&1 | head -5
    fail "allow must still inject via the Pod INPUT chain with the sandbox layer flushed"
  fi
  echo "${out}" | grep -qi "x-api-key: secret-v1-new" \
    || fail "allow must still inject via the Pod INPUT chain with the sandbox layer flushed; got: ${out}"
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null -H 'Host: ext.test' http://10.99.0.9/ 2>/dev/null; then
    fail "always-deny (MITM 80) must hold via the Pod INPUT chain with the sandbox layer flushed"
  fi
  if ip netns exec osb-sandbox-a curl -sk -m 3 -o /dev/null https://203.0.113.1/ 2>/dev/null; then
    fail "DoH-443 blocklist must hold via the Pod INPUT chain with the sandbox layer flushed"
  fi
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null http://10.99.0.9:8080/ 2>/dev/null; then
    fail "always-deny (non-MITM :8080) must hold via the Pod forward hook with the sandbox layer flushed"
  fi
  pass "authoritative enforcement survives a sandbox-layer flush (forward hook + MITM input chain)"

  # Spoofed-source-IP attack: sandbox b forges sandbox a's IP. The DNAT is
  # iifname-bound, so the packet is NOT intercepted — it hits the forward
  # master drop (iifname mismatch) and never reaches any vault dispatch. A
  # permissive DNAT (saddr only) would have let the mitm dispatch on the
  # forged clientIp and injected subject a's credential into b.
  ip netns exec osb-sandbox-b ip addr add 10.10.0.5/32 dev veth-b 2>/dev/null || true
  if ip netns exec osb-sandbox-b curl -s -m 3 -o /dev/null --interface 10.10.0.5 -H 'Host: ext.test' http://10.99.0.2/ 2>/dev/null; then
    fail "spoofed source IP must not reach the mitm (DNAT iifname binding)"
  fi
  pass "spoofed source IP rejected (DNAT iifname binding)"


  # Unload removes the subject's DNAT rule (the rebuild drops it).
  rm -f "${SLOT_DIR}/b.json"
  wait_for 15 "subject b DNAT removed on unload" bash -c "! nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.6'"
  pass "unload removed subject b's DNAT rule"

  # mitmdump death -> healthz 503 -> auto-restart (watchMitmproxy + backoff)
  # -> injection resumes. The 503 window can be sub-second, so poll fast.
  pkill -TERM -x mitmdump 2>/dev/null || true
  deadline=$((SECONDS + 15))
  saw_503=0
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${POLICY_PORT}/healthz" 2>/dev/null || true)"
    if [ "${code}" = "503" ]; then saw_503=1; break; fi
    sleep 0.2
  done
  [ "${saw_503}" = "1" ] || fail "healthz must 503 while mitmdump is down"
  deadline=$((SECONDS + 30))
  while [ "${SECONDS}" -lt "${deadline}" ]; do
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${POLICY_PORT}/healthz" 2>/dev/null || true)"
    if [ "${code}" = "200" ]; then break; fi
    sleep 0.5
  done
  [ "${code}" = "200" ] || fail "healthz must recover after mitmdump auto-restart"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "x-api-key: secret-v1-new" || fail "injection must resume after mitmdump restart; got: ${out}"
  pass "mitmdump death recovery (503 -> auto-restart -> injection resumes)"

###############################################################################
  info "Test 12: MITM-mode restart recovery (DNAT rebuild + CA re-export + vault re-push)"
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress
  wait_for 20 "CA re-exported after restart" test -s /opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem
  wait_for 15 "DNAT rebuilt after restart" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 iifname '"
  wait_for 15 "subject a re-registered denying" bash -c "curl -s -H 'X-Fast-Sandbox-Uid: a' http://127.0.0.1:${POLICY_PORT}/policy | grep -q denying"
  # the in-memory vault died with the old process; re-push (idempotent)
  push_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  push_vault a '{"credentials":[{"name":"k","source":{"type":"inline","value":"secret-v1-new"}}],"bindings":[{"name":"b","match":{"schemes":["http","https"],"hosts":["ext.test"]},"auth":{"type":"apiKey","name":"X-Api-Key","credential":"k"}}]}'
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "x-api-key: secret-v1-new" || fail "injection must work after MITM-mode restart; got: ${out}"
  pass "MITM-mode restart recovery (DNAT + CA + vault re-push + injection)"
fi

###############################################################################
info "All fleet smoke tests passed."
