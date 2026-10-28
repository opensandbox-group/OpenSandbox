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

# Fast Sandbox profile (OSEP-0022) smoke test — real dns+nft end to end, no Docker,
# no external network. The fast-sandbox profile is inherently dns+nft (there is no
# dns-only mode), so this is the only fast-sandbox smoke variant.
#
# Topology (all on the host, requires root):
#
#   sandbox netns osb-sandbox-a   Pod/host netns        "ext" netns osb-ext
#   10.10.0.5/24 ──veth-a── veth-a-p 10.10.0.1/24       (external world)
#   DNS -> 10.10.0.1:53 ────────────► fast-sandbox dnsproxy    osb-ext 10.99.0.2/24
#   TCP  -> 10.99.0.2:8080 ─────────► forward hook ── veth-ext-p 10.99.0.1/24
#                                    (nft opensandbox-fast-sandbox)         └─ HTTP :8080
#
# Subject lifecycle is driven by the fast-sandbox Sandbox Actions Handler
# protocol (SET_BINDING / LIFECYCLE_HOOK / REMOVE_BINDING over
# /_fastlet/v1/actions); the egress binary runs directly with the fast-sandbox
# profile. Every assertion below touches the real kernel (nft) or real
# packets (netns-to-netns).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
EGRESS_BIN="/tmp/osb-egress-fast-sandbox"

EGRESS_LOG="/tmp/fast-sandbox-egress.log"
POLICY_PORT=18080
UPSTREAM_ADDR="127.0.0.1:5300"
ACTIONS_URL="http://127.0.0.1:${POLICY_PORT}/_fastlet/v1/actions"

ALWAYS_RULES_DIR="/var/egress/rules"
SAVED_IP_FORWARD=""

# Pinned MITM CA seed mount (OSEP-0022): provisioned at the MITM phase,
# `tls.crt` + `tls.key` layout, handed to egress via
# OPENSANDBOX_EGRESS_MITM_CA_SEED_DIR. Empty = no seed (the addon-down case).
CA_SEED_DIR=""
SEED_FP=""

# pids of helpers + egress
UPSTREAM_PID=""
EXT_PID=""
PROXY_PID=""
EGRESS_PID=""
HOSTS_BACKUP=""

# Per-subject credential vault files (OSEP-0022 stage 2), wired through
# OPENSANDBOX_EGRESS_VAULT_DIR for the whole run: a fresh directory per run
# keeps the "no vault yet" assertions meaningful across re-runs, while the
# same directory must survive the in-run restarts (Test 14 durability).
VAULT_DIR=""
VAULT_BODY=""

# Per-subject policy files (egress-held policy): REQUIRED by the fast-sandbox
# profile — the recorded policy is egress-held state, so a fresh directory
# per run keeps the seeding assertions meaningful while surviving the in-run
# restarts (Test 9/14 durability).
POLICY_DIR=""

info() { echo "[$(date +%H:%M:%S)] $*"; }
pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

require() {
  [ "$(id -u)" = "0" ] || fail "fast-sandbox smoke requires root (ip netns + nft)"
  for c in go nft ip python3 curl openssl; do
    command -v "${c}" >/dev/null || fail "missing required command: ${c}"
  done
}

cleanup() {
  set +e
  [ -n "${EGRESS_PID}" ] && kill "${EGRESS_PID}" 2>/dev/null
  [ -n "${UPSTREAM_PID}" ] && kill "${UPSTREAM_PID}" 2>/dev/null
  [ -n "${EXT_PID}" ] && kill "${EXT_PID}" 2>/dev/null
  [ -n "${PROXY_PID}" ] && kill "${PROXY_PID}" 2>/dev/null
  # Test 13 maps proxy.test in /etc/hosts (the Pod-resolver-only authority);
  # restore the original file even on failure.
  if [ -n "${HOSTS_BACKUP}" ] && [ -f "${HOSTS_BACKUP}" ]; then
    cp -f "${HOSTS_BACKUP}" /etc/hosts
    rm -f "${HOSTS_BACKUP}"
  fi
  ip link del veth-a 2>/dev/null
  ip link del veth-ext 2>/dev/null
  ip netns del osb-sandbox-a 2>/dev/null
  ip netns del osb-sandbox-b 2>/dev/null
  ip netns del osb-ext 2>/dev/null
  if [ -n "${SAVED_IP_FORWARD}" ]; then
    sysctl -w net.ipv4.ip_forward="${SAVED_IP_FORWARD}" >/dev/null 2>&1
  fi
  if [ -n "${SAVED_FORWARD_POLICY}" ]; then
    iptables -t filter -P FORWARD ACCEPT >/dev/null 2>&1 || true
    [ "${SAVED_FORWARD_POLICY}" = "-P FORWARD ACCEPT" ] || iptables -t filter -P FORWARD DROP >/dev/null 2>&1 || true
  fi
  [ -n "${SSL_DIR:-}" ] && rm -rf "${SSL_DIR}" 2>/dev/null
  [ -n "${CA_SEED_DIR}" ] && rm -rf "${CA_SEED_DIR}" 2>/dev/null
  [ -n "${VAULT_DIR}" ] && rm -rf "${VAULT_DIR}" 2>/dev/null
  [ -n "${POLICY_DIR}" ] && rm -rf "${POLICY_DIR}" 2>/dev/null
  [ -n "${VAULT_BODY}" ] && rm -f "${VAULT_BODY}" 2>/dev/null
  nft delete table inet opensandbox-fast-sandbox 2>/dev/null
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

# action <method> <body>: POST one action envelope; fail on non-200.
action() {
  curl -sSf -XPOST "${ACTIONS_URL}" -d "$1" >/dev/null
}

# json_input <raw>: encodes a value as the JSON string the binding input
# carries (the literal null stays null = binding removal).
json_input() {
  if [ "$1" = "null" ]; then
    printf '%s' "null"
  else
    python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "$1"
  fi
}

# set_binding <uid> <ip> <spec_gen> <runtime> <att> <input>
#   input: the policy (JSON string) or the literal null (removal).
set_binding() {
  local uid="$1" ip="$2" spec_gen="$3" runtime="$4" att="$5" input="$6"
  local body
  body="$(cat <<EOF
{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","invocationId":"smoke-${uid}-${spec_gen}","sandbox":{"uid":"${uid}"},"revision":{"specGeneration":${spec_gen},"runtimeInstanceId":"${runtime}","attachmentId":"${att}","routeGeneration":1},"attachment":{"network":{"ip":"${ip}","gateway":"10.10.0.1","privateCidr":"10.10.0.0/24","hostVeth":"veth-${uid}-p"}},"binding":{"input":$(json_input "${input}")}}
EOF
)"
  action "${body}"
}

# lifecycle_hook <uid> <runtime> <att> <hook_name>
lifecycle_hook() {
  local uid="$1" runtime="$2" att="$3" hook="$4"
  local body
  body="$(cat <<EOF
{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","invocationId":"smoke-${uid}-${hook}","sandbox":{"uid":"${uid}"},"revision":{"specGeneration":1,"runtimeInstanceId":"${runtime}","attachmentId":"${att}","routeGeneration":1},"hook":{"name":"${hook}","sequence":1}}
EOF
)"
  action "${body}"
}

# remove_binding <uid> <runtime> <att>
remove_binding() {
  local uid="$1" runtime="$2" att="$3"
  local body
  body="$(cat <<EOF
{"apiVersion":"sandbox.fast.io/actions/v1","operation":"REMOVE_BINDING","invocationId":"smoke-${uid}-remove","sandbox":{"uid":"${uid}"},"revision":{"specGeneration":1,"runtimeInstanceId":"${runtime}","attachmentId":"${att}","routeGeneration":1}}
EOF
)"
  action "${body}"
}

# bind_and_ready <uid> <ip> <spec_gen> <runtime> <att> <input>: the standard
# creation spine — SET_BINDING (deny-first) -> runtime-ready ->
# data-plane-ready (active).
bind_and_ready() {
  local uid="$1" ip="$2" spec_gen="$3" runtime="$4" att="$5" input="$6"
  set_binding "${uid}" "${ip}" "${spec_gen}" "${runtime}" "${att}" "${input}"
  lifecycle_hook "${uid}" "${runtime}" "${att}" sandbox.runtime-ready
  lifecycle_hook "${uid}" "${runtime}" "${att}" sandbox.data-plane-ready
}

# relay_put_policy <uid> <input>: a runtime policy change through the
# handler-relay surface (/policy/{uid}). The binding input only ever seeds a
# fresh subject's policy — every later change rides this surface, which
# applies in place on an active subject and holds pending on a denying one.
relay_put_policy() {
  local uid="$1" input="$2" code
  code="$(curl -s -o /dev/null -w '%{http_code}' -XPUT \
    "http://127.0.0.1:${POLICY_PORT}/policy/${uid}" -d "{\"policy\":${input}}")"
  case "${code}" in
    200|201) ;;
    *) fail "relay PUT /policy/${uid} failed: ${code}" ;;
  esac
}

# relay_get_policy <uid> <name>: echoes the recorded policy document.
relay_get_policy() {
  curl -s "http://127.0.0.1:${POLICY_PORT}/policy/${1}"
}

# relay_delete_policy <uid>: DELETE lands an authoritative default-deny
# tombstone at the successor revision (204).
relay_delete_policy() {
  local uid="$1" code
  code="$(curl -s -o /dev/null -w '%{http_code}' -XDELETE \
    "http://127.0.0.1:${POLICY_PORT}/policy/${uid}")"
  [ "${code}" = "204" ] || fail "relay DELETE /policy/${uid} failed: ${code}"
}

dns_query() {
  # dns_query <netns-or-host> <name>
  local where="$1" name="$2"
  if [ "${where}" = "host" ]; then
    python3 "${SCRIPT_DIR}/fast_sandbox_upstream.py" query 10.10.0.1 "${name}"
  else
    ip netns exec "${where}" python3 "${SCRIPT_DIR}/fast_sandbox_upstream.py" query 10.10.0.1 "${name}"
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

nft_has() { nft list table inet opensandbox-fast-sandbox 2>/dev/null | grep -q "$1"; }

# dns_rcode_is <netns-or-host> <name> <rcode>: probe variant of expect_rcode
# that returns non-zero instead of failing (usable inside wait_for).
dns_rcode_is() {
  local out rcode
  out="$(dns_query "$1" "$2")"
  rcode="$(echo "${out}" | sed -n 's/^rcode=\([0-9]*\).*/\1/p')"
  [ "${rcode}" = "$3" ]
}

start_egress() {
  info "Starting fast-sandbox egress"
  # DoH env overridable per-test (Test 10 exercises strict mode with an
  # empty blocklist). `${VAR-default}` (no colon) keeps a set-but-empty
  # value empty, so Test 10's `EGRESS_DOH_BLOCKLIST=""` really disables the
  # blocklist instead of falling back to the default.
  local block_doh="${EGRESS_BLOCK_DOH_443-true}"
  local doh_blocklist="${EGRESS_DOH_BLOCKLIST-203.0.113.1}"
  local mitm="${EGRESS_MITM-}"
  local expect_down="${EXPECT_MITM_DOWN-}"
  local mitm_env=()
  if [ "${mitm}" = "1" ]; then
    # ssl_insecure: the ext upstream serves a self-signed cert (curl -k on
    # the client side skips its own validation; the trust stack itself is a
    # fast-sandbox CA delivery, issue #19)
    mitm_env=(
      OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true
      OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true
    )
    if [ -n "${CA_SEED_DIR}" ]; then
      mitm_env+=(OPENSANDBOX_EGRESS_MITM_CA_SEED_DIR="${CA_SEED_DIR}")
    fi
  fi
  # Optional chained upstream proxy (Test 13): EGRESS_UPSTREAM_PROXY is only
  # meaningful together with EGRESS_MITM=1 (validation requires transparent).
  local upstream="${EGRESS_UPSTREAM_PROXY-}"
  local upstream_env=()
  if [ -n "${upstream}" ]; then
    upstream_env=(OPENSANDBOX_EGRESS_UPSTREAM_PROXY="${upstream}")
  fi
  # env(1) is required: words produced by "${mitm_env[@]}" expansion are NOT
  # treated as environment assignments by bash (they'd be executed as commands).
  env \
  OPENSANDBOX_EGRESS_PROFILE=fast-sandbox \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM="${UPSTREAM_ADDR}" \
  OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE=allow.test \
  OPENSANDBOX_EGRESS_HTTP_ADDR="127.0.0.1:${POLICY_PORT}" \
  OPENSANDBOX_EGRESS_BLOCK_DOH_443="${block_doh}" \
  OPENSANDBOX_EGRESS_DOH_BLOCKLIST="${doh_blocklist}" \
  OPENSANDBOX_EGRESS_VAULT_DIR="${VAULT_DIR}" \
  OPENSANDBOX_EGRESS_POLICY_DIR="${POLICY_DIR}" \
  "${mitm_env[@]}" \
  "${upstream_env[@]}" \
  "${EGRESS_BIN}" >"${EGRESS_LOG}" 2>&1 &
  EGRESS_PID=$!
  if [ "${expect_down}" = "1" ]; then
    # Addon-down startup (pinned CA missing): /healthz must settle at 503
    # with the explicit addon-down signal — not a transient not-ready —
    # while the listener still comes up for policy actions.
    wait_for 30 "egress listener (addon-down)" curl -s "http://127.0.0.1:${POLICY_PORT}/healthz"
    local code body
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${POLICY_PORT}/healthz")"
    [ "${code}" = "503" ] || fail "addon-down healthz must be 503, got ${code}"
    body="$(curl -s "http://127.0.0.1:${POLICY_PORT}/healthz")"
    echo "${body}" | grep -q '"status":"mitm-addon-down"' || fail "addon-down healthz body must carry mitm-addon-down, got: ${body}"
    return
  fi
  wait_for 30 "egress healthz" curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz"
}

# healthz_fingerprint: extracts the pinned CA fingerprint from /healthz.
healthz_fingerprint() {
  curl -s "http://127.0.0.1:${POLICY_PORT}/healthz" | sed -n 's/.*"mitmCaFingerprint":"\([0-9a-f]*\)".*/\1/p'
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
info "== Fast Sandbox profile smoke (dns+nft) =="
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

# Vault persistence directory: enabled for every start_egress below, so relay
# writes run the commit protocol and the ack means durable (exercised by the
# Test 14 restart reload).
VAULT_DIR="$(mktemp -d -t fast-sandbox-vault.XXXXXX)"
POLICY_DIR="$(mktemp -d -t fast-sandbox-policy.XXXXXX)"
VAULT_BODY="$(mktemp -t fast-sandbox-vault-body.XXXXXX)"

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
SSL_DIR="$(mktemp -d -t fast-sandbox-ssl.XXXXXX)"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "${SSL_DIR}/key.pem" \
  -out "${SSL_DIR}/cert.pem" -days 1 -subj "/CN=ext.test" >/dev/null 2>&1
python3 "${SCRIPT_DIR}/fast_sandbox_upstream.py" dns >/dev/null 2>&1 &
UPSTREAM_PID=$!
EXT_SSL_CERT="${SSL_DIR}/cert.pem" EXT_SSL_KEY="${SSL_DIR}/key.pem" \
  ip netns exec osb-ext python3 "${SCRIPT_DIR}/fast_sandbox_upstream.py" ext >/dev/null 2>&1 &
EXT_PID=$!
wait_for 5 "ext http server up" ip netns exec osb-ext curl -s -m 2 -o /dev/null http://127.0.0.1:8080/
wait_for 5 "ext https server up" ip netns exec osb-ext curl -sk -m 2 -o /dev/null https://127.0.0.1:443/

start_egress

###############################################################################
info "Test 0: DoH-443 blocking installed globally (master chain)"
nft_has 'doh_block_v4' || fail "DoH blocklist set missing"
nft_has '203.0.113.1' || fail "DoH blocklist element missing"
nft_has 'doh_block_v4 tcp dport 443 drop' || fail "DoH 443 block rule missing"
pass "DoH-443 blocking installed (doh_block_v4 set + element + drop rule)"

###############################################################################
info "Test 1: SET_BINDING registers deny-first (fail closed before any policy)"
set_binding a 10.10.0.5 1 runtime-a att-a '{"defaultAction":"deny"}'
wait_for 15 "subject a deny-first installed" nft_has 'subj_s_a'
nft_has 'ip saddr 10.10.0.5 jump subj_s_a' || fail "dispatch rule missing"
nft_has 'subj_s_a_allow_v4 {' || fail "subject a static sets missing"
expect_rcode osb-sandbox-a allow.test 3
pass "deny-first registered (nft + NXDOMAIN, policy pending)"

###############################################################################
info "Test 2: data-plane-ready activates the subject; the relay owns runtime changes"
lifecycle_hook a runtime-a att-a sandbox.runtime-ready
lifecycle_hook a runtime-a att-a sandbox.data-plane-ready
# The binding input seeded deny at spec 1; the allow policy is a RUNTIME
# change through the handler-relay surface, applied in place on the active
# subject. A same-fence SET_BINDING with a new input would be superseded.
relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"},{"action":"allow","target":"10.99.0.2"}]}'
expect_answers osb-sandbox-a allow.test 1.1.1.1
expect_answers osb-sandbox-a other.test 1.1.1.2
expect_rcode osb-sandbox-a nope.test 3
pass "DNS per-subject policy (allow *.test, deny others)"

nft_has '10.99.0.2' || fail "static allow element missing from nft"
wait_for 10 "dns-learned dynamic allow" nft_has '1.1.1.1'
pass "nft static allow + DNS-learned dynamic lease"

###############################################################################
info "Test 3: real data path through the forward hook"
if ! ip netns exec osb-sandbox-a curl -s -m 5 -o /dev/null http://10.99.0.2:8080/; then
  echo "--- diagnostics (data path failure) ---"
  nft list table inet opensandbox-fast-sandbox 2>&1 | head -30
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
info "Test 4: deny CIDR overrides allow (nft layer, relay update)"
relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"},{"action":"deny","target":"10.99.0.0/24"}]}'
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

# ---------------------------------------------------------------------------
info "Test 6: superseded proxy-route surfaces are gone (no UID-header channel)"
http_code="$(curl -s -o /dev/null -w '%{http_code}' -XPOST \
  "http://127.0.0.1:${POLICY_PORT}/credential-vault" -d '{}')"
[ "${http_code}" = "404" ] || fail "superseded /credential-vault must be gone (404), got ${http_code}"
http_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${POLICY_PORT}/policy")"
case "${http_code}" in
  301|307|308) ;; # redirects into the /policy/{uid} relay subtree
  *) fail "bare /policy must redirect into the relay subtree, got ${http_code}" ;;
esac
location="$(curl -s -D - -o /dev/null "http://127.0.0.1:${POLICY_PORT}/policy" | tr -d '\r' | sed -n 's/^[Ll]ocation: //p')"
[ "${location}" = "/policy/" ] || fail "bare /policy redirect target must be /policy/, got ${location:-none}"
pass "superseded proxy-route surfaces removed (relay surfaces only)"

set_up_netns b 10.10.0.6
bind_and_ready b 10.10.0.6 1 runtime-b att-b '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
expect_answers osb-sandbox-b allow.test 1.1.1.1
pass "subject b registered + active through the action protocol"

###############################################################################
info "Test 7: rebind discards policy (fail closed until re-activation)"
set_binding a 10.10.0.5 4 runtime-a-2 att-a-2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
wait_for 15 "rebind back to denying" dns_rcode_is osb-sandbox-a allow.test 3
nft_has '10.99.0.2' && fail "stale policy must not survive a rebind"
expect_rcode osb-sandbox-a allow.test 3
lifecycle_hook a runtime-a-2 att-a-2 sandbox.data-plane-ready
expect_answers osb-sandbox-a allow.test 1.1.1.1
pass "rebind reset + data-plane-ready reactivates"

###############################################################################
info "Test 8: REMOVE_BINDING removes enforcement"
remove_binding b runtime-b att-b
wait_for 15 "subject b unloaded" bash -c "! nft list table inet opensandbox-fast-sandbox | grep -q subj_s_b"
pass "unload removed chain/sets"

###############################################################################
info "Test 9: restart recovery (reset -> action replay -> active)"
# Refresh the DNS-learned dyn lease (1.1.1.1) right before the restart, so
# the post-restart "stale wiped" assertions are meaningful.
expect_answers osb-sandbox-a allow.test 1.1.1.1
# Durability probe: relay a policy DISTINCT from the replayed binding input;
# after the restart + SET_BINDING replay the recorded file must supersede
# the input — asserted by the marker target surviving the GET below
# (vault parity, Test 14e).
relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"},{"action":"allow","target":"durability.test"},{"action":"allow","target":"10.99.0.0/24"}]}'
wait_for 10 "dyn lease present before restart" nft_has '1.1.1.1'
kill "${EGRESS_PID}" 2>/dev/null
wait "${EGRESS_PID}" 2>/dev/null || true
EGRESS_PID=""
start_egress
wait_for 15 "stale dyn leases wiped on restart" bash -c "! nft list table inet opensandbox-fast-sandbox | grep -q '1.1.1.1'"
expect_rcode osb-sandbox-a allow.test 3
# The Fastlet's instanceId replay: SET_BINDING + reached Hooks.
bind_and_ready a 10.10.0.5 5 runtime-a-2 att-a-2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
expect_answers osb-sandbox-a allow.test 1.1.1.1
# The durability probe's relay write survived the restart: the recorded
# file superseded the replayed binding input.
recorded="$(curl -s "http://127.0.0.1:${POLICY_PORT}/policy/a")"
echo "${recorded}" | grep -q 'durability.test' || fail "the relayed policy must survive the restart (file supersedes the replayed binding input), got: ${recorded}"
# Enforcement, not just the record: the probe's static allow must be live
# in nft (the replayed input does not contain it — a regression where the
# replay overrode the reload would lose this element).
nft_has '10.99.0.0/24' || fail "the reloaded policy must be ENFORCED (10.99.0.0/24 missing from nft)"
pass "restart durability (recorded policy supersedes the replayed binding input, enforced)"
# Restore the lineage: under file-supersedes-input, every later section
# starts from the recorded policy — put back what the binding inputs intend.
relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"*.test"}]}'
# DELETE tombstone: an authoritative default-deny at the successor
# revision (204); the stale expectedRevision precondition is a 412 on the
# handler contract (the server remaps it to 409 on the public API).
http_code="$(curl -s -o /dev/null -w '%{http_code}' -XPUT "http://127.0.0.1:${POLICY_PORT}/policy/a" -d '{"policy":{"defaultAction":"deny","egress":[]},"expectedRevision":99}')"
[ "${http_code}" = "412" ] || fail "stale expectedRevision must be 412, got ${http_code}"
relay_delete_policy a
recorded="$(relay_get_policy a)"
echo "${recorded}" | grep -q '"defaultAction":"deny"' \
  || fail "the DELETE tombstone must be an authoritative default-deny record, got: ${recorded}"
pass "DELETE tombstone (default-deny record persisted, stale revision 412)"

# The tombstone survives the Test 10 restart + binding replay: the
# replayed bootstrap binding (deny) is superseded by the identical tombstone
# at the SAME revision — no resurrection of the pre-delete allow, no
# revision reset.
relay_get_policy a | grep -q '"defaultAction":"deny"' \
  || fail "the tombstone must survive the restart, got: $(relay_get_policy a)"

###############################################################################
info "Test 10: strict DoH mode (no blocklist) drops all tcp 443 globally"
kill "${EGRESS_PID}" 2>/dev/null
wait "${EGRESS_PID}" 2>/dev/null || true
EGRESS_PID=""
EGRESS_DOH_BLOCKLIST="" start_egress
set_binding a 10.10.0.5 6 runtime-a-2 att-a-2 '{"defaultAction":"deny"}'
wait_for 15 "subject a re-registered after strict-mode restart" dns_rcode_is osb-sandbox-a allow.test 3
nft_has 'tcp dport 443 drop' || fail "strict mode must install a bare tcp 443 drop"
nft_has 'doh_block_v4' && fail "strict mode must not create blocklist sets"
# The tombstone revision is preserved across the restart + replay — the
# binding input was superseded, not re-seeded.
relay_get_policy a | grep -q '"defaultAction":"deny"' \
  || fail "the tombstone must survive the strict-mode restart + replay"
pass "strict DoH mode enforced (bare 443 drop, no blocklist sets); tombstone survives replay"

###############################################################################
info "Test 11: MITM data plane — shared mitmdump + per-subject DNAT + subject-aware active vault"
if ! command -v mitmdump >/dev/null 2>&1 || ! id mitmproxy >/dev/null 2>&1; then
  info "SKIP MITM phase: mitmdump and/or the 'mitmproxy' user are not installed."
  info "  Install to match the egress image (CI does this in .github/workflows/egress-test.yml):"
  info "    apt-get install -y python3-venv"
  info "    useradd -r -u 10042 -d /var/lib/mitmproxy -s /usr/sbin/nologin mitmproxy"
  info "    python3 -m venv /opt/mitmproxy && /opt/mitmproxy/bin/pip install 'mitmproxy==11.0.2'"
  info "    ln -s /opt/mitmproxy/bin/mitmdump /usr/local/bin/mitmdump"
  info "  then re-run ./tests/smoke-fast-sandbox.sh (Test 11 runs last)."
else
  # Ship the baked-in mitm config AND the system addon (the egress image
  # carries both under /var/lib/mitmproxy and /var/egress; a bare host may
  # not — the addon path is a hard requirement of the mitmdump launch).
  mkdir -p /var/lib/mitmproxy/.mitmproxy
  cp -f "${SCRIPT_DIR}/../mitmproxy/config.yaml" /var/lib/mitmproxy/.mitmproxy/config.yaml
  mkdir -p /var/egress/mitmscripts
  cp -f "${SCRIPT_DIR}/../mitmscripts/system.py" /var/egress/mitmscripts/system.py
  # Test 13 chains through an upstream proxy: the egress passes the bundled
  # upstream addon to mitmdump by absolute path when
  # OPENSANDBOX_EGRESS_UPSTREAM_PROXY is set, so it must exist here too.
  cp -f "${SCRIPT_DIR}/../mitmscripts/upstream_proxy.py" /var/egress/mitmscripts/upstream_proxy.py
  chown -R mitmproxy:mitmproxy /var/lib/mitmproxy

  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""

  ###############################################################################
  info "Test 11a: pinned CA absent + MITM enabled -> addon fails closed, policy actions keep serving"
  EGRESS_MITM=1 EXPECT_MITM_DOWN=1 start_egress
  # Actions status payload mirrors the down state with the explicit signal.
  status="$(curl -s "http://127.0.0.1:${POLICY_PORT}/_fastlet/v1/actions/status")"
  echo "${status}" | grep -q '"mitmCaDown":true' || fail "actions status must carry mitmCaDown when the pinned CA is missing, got: ${status}"
  # Policy actions keep serving: SET_BINDING is accepted (the action handler
  # 200s; since #2247 there is no proxy-route /policy readback — the actions
  # endpoint IS the policy surface, deny-first on registration).
  set_binding a 10.10.0.5 7 runtime-a-2 att-a-2 '{"defaultAction":"deny"}'
  # No interception may be installed: the DNAT table stays absent.
  if nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '; then
    fail "addon-down must not install per-subject interception rules"
  fi
  pass "addon-down fail-closed (healthz 503 + status signal, policy serving, no interception)"

  ###############################################################################
  info "Test 11b: pinned CA seeding -> confdir bundle + fingerprint observability"
  # Provision the pool-pinned CA exactly as the platform Secret mount would:
  # `tls.crt` + `tls.key` on a read-only seed mount.
  CA_SEED_DIR="$(mktemp -d /tmp/osb-mitm-ca-seed.XXXXXX)"
  openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
    -keyout "${CA_SEED_DIR}/tls.key" -out "${CA_SEED_DIR}/tls.crt" \
    -subj "/CN=smoke-pinned-ca" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,digitalSignature" >/dev/null 2>&1
  SEED_FP="$(openssl x509 -in "${CA_SEED_DIR}/tls.crt" -noout -fingerprint -sha256 | sed 's/^.*=//' | tr -d ':' | tr 'A-Z' 'a-z')"
  [ -n "${SEED_FP}" ] || fail "could not fingerprint the pinned CA (openssl failed)"

  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  # Drop any confdir CA left by a previous run so the materialization
  # assertion below is meaningful (config.yaml must survive: mitmdump
  # auto-loads it).
  rm -f /var/lib/mitmproxy/.mitmproxy/mitmproxy-ca.pem \
        /var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem
  EGRESS_MITM=1 start_egress

  # The seed materialized the confdir CA bundle mitmdump loads (one file
  # holding certificate + private key); the certificate copy's fingerprint
  # must equal the pinned seed, and healthz mirrors it.
  wait_for 20 "pinned CA seeded into confdir" test -s /var/lib/mitmproxy/.mitmproxy/mitmproxy-ca.pem
  [ -s /var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem ] || fail "confdir cert missing after seeding"
  mat_fp="$(openssl x509 -in /var/lib/mitmproxy/.mitmproxy/mitmproxy-ca-cert.pem -noout -fingerprint -sha256 | sed 's/^.*=//' | tr -d ':' | tr 'A-Z' 'a-z')"
  [ "${mat_fp}" = "${SEED_FP}" ] || fail "materialized confdir cert ${mat_fp} must equal the pinned cert ${SEED_FP}"
  got_fp="$(healthz_fingerprint)"
  [ "${got_fp}" = "${SEED_FP}" ] || fail "healthz fingerprint ${got_fp} must equal the pinned CA fingerprint ${SEED_FP}"
  pass "pinned CA seeded (confdir bundle + fingerprint ${SEED_FP:0:12}...)"

  # Subject a registers (deny-first): the per-subject DNAT is installed at
  # registration.
  set_binding a 10.10.0.5 7 runtime-a-2 att-a-2 '{"defaultAction":"deny"}'
  wait_for 15 "per-subject DNAT" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 tcp dport { 80, 443 } dnat'"
  nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 ip daddr 10.10.0.1 tcp dport { 80, 443 } return' \
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
  # authoritative layer even before any policy is applied.
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null -H 'Host: ext.test' http://10.99.0.2/ 2>/dev/null; then
    fail "deny-first must block MITM 80/443 via the Pod INPUT chain"
  fi
  pass "deny-first blocks MITM 80/443 at the Pod INPUT chain"

  # Subject a: policy via the binding, then a real HTTP request through the
  # shared mitm (proves interception; credential delivery returns with the
  # OSEP-0022 relay surface — no vault exists here, so flows proceed
  # uncredentialed). Binding hosts must be FQDNs (IP binding hosts are
  # rejected), so the sandbox curls the ext netns with an explicit Host
  # header; the policy keeps allowing the real daddr 10.99.0.2 so the nft
  # layers pass the packet.
  lifecycle_hook a runtime-a-2 att-a-2 sandbox.runtime-ready
  # A relay write while denying is held to the data-plane-ready barrier
  # (pending-policy validation semantics, same as the vault).
  relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  lifecycle_hook a runtime-a-2 att-a-2 sandbox.data-plane-ready
  code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "${ACTIVE_SOCK}" 'http://localhost/credential-vault/_active?clientIp=10.10.0.5')"
  [ "${code}" = "404" ] || fail "active API: subject a has no vault yet (relay not wired), got ${code}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "request did not traverse the shared mitm (direct path?); got: ${out}"
  pass "real HTTP through shared mitm (clientIp -> subject dispatch, no vault -> plain flow)"

  # Subject b (re-registered after Test 8's unload): a second sandbox with a
  # DIFFERENT policy (allows alt.test, not ext.test); each sandbox's rules
  # stay strictly isolated.
  set_binding b 10.10.0.6 2 runtime-b att-b '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"alt.test"}]}'
  lifecycle_hook b runtime-b att-b sandbox.data-plane-ready
  wait_for 15 "subject b re-registered with DNAT" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.6 '"

  out="$(ip netns exec osb-sandbox-b curl -s -m 5 -H 'Host: alt.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b request did not traverse the shared mitm; got: ${out}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject a request did not traverse the shared mitm; got: ${out}"

  # Per-subject dispatch: b reaching a's host still traverses the shared mitm
  # (its own policy allows the daddr), dispatched by source IP — subject
  # isolation is enforced by the per-subject policy/credential slices.
  out="$(ip netns exec osb-sandbox-b curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b request did not traverse the shared mitm; got: ${out}"
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: alt.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject a request did not traverse the shared mitm; got: ${out}"
  pass "per-subject policy isolation (two sandboxes, distinct rules)"

  # HTTPS data plane: TLS interception + SO_ORIGINAL_DST(443) on the
  # encrypted path. curl -k skips the sandbox-side trust check (the CA
  # delivery into sandboxes is fast-sandbox issue #19); the mitm's own
  # upstream validation is disabled via ssl_insecure (self-signed ext cert).
  # The client=10.99.0.1 oracle proves the mitm proxied it.
  out="$(ip netns exec osb-sandbox-a curl -sk -m 5 -H 'Host: ext.test' https://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "https request did not traverse the shared mitm; got: ${out}"
  out="$(ip netns exec osb-sandbox-b curl -sk -m 5 -H 'Host: alt.test' https://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "subject b https request did not traverse the shared mitm; got: ${out}"
  pass "HTTPS data plane (TLS interception, per-subject dispatch)"

  # Interception-set precision: :8080 is outside {80,443}, so it must go
  # DIRECT (client=10.10.0.5, the sandbox IP).
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2:8080/)"
  echo "${out}" | grep -qi "client=10.10.0.5" || fail ":8080 must NOT be intercepted (direct path); got: ${out}"
  pass "interception set precision (:8080 direct)"

  # Direct proxy-protocol connection to the mitm port: a default-allow
  # sandbox must not bypass the transparent interception by talking to the
  # gateway:18081 itself (the input chain drops non-DNATed traffic to it).
  if ip netns exec osb-sandbox-a curl -s -m 3 -x http://10.10.0.1:18081 -H 'Host: ext.test' http://ext.test/ 2>/dev/null; then
    fail "direct connection to the mitm port must be dropped"
  fi
  pass "direct mitm-port connection rejected"

  # DoH-443 blocking under MITM: the Pod-netns INPUT enforcement chain
  # applies the blocklist to the DNATed 443 (the forward hook only sees the
  # real destination for non-MITM traffic).
  nft list table inet opensandbox-fast-sandbox 2>/dev/null | grep -q 'doh_block_v4 tcp dport 443 drop' \
    || fail "Pod-layer DoH-443 drop missing under MITM"
  if ip netns exec osb-sandbox-a curl -sk -m 2 -o /dev/null https://203.0.113.1/ 2>/dev/null; then
    fail "blocklisted DoH endpoint 203.0.113.1 must be unreachable under MITM"
  fi
  pass "DoH-443 blocking under MITM (blocklist still enforced)"

  # Spoofed-source-IP attack: sandbox b forges sandbox a's IP. The dispatch
  # rules are saddr-keyed (no iifname — on the bridge topology the IP hooks
  # see skb->dev = the bridge, so iifname could never match), so the forged
  # packet follows subject a's policy instead of b's. Spoof protection rests
  # on IPAM (per-sandbox unique IP) and the Pod netns rp_filter strict mode,
  # not on nft. Assert the observable consequence: b's forged-IP traffic is
  # dispatched by source IP to subject a (whose policy allows this target),
  # so it traverses the shared mitm instead of being dropped.
  ip netns exec osb-sandbox-b ip addr add 10.10.0.5/32 dev veth-b 2>/dev/null || true
  out="$(ip netns exec osb-sandbox-b curl -s -m 3 -H 'Host: ext.test' http://10.99.0.2/ 2>/dev/null || true)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "forged-IP traffic must follow subject a's policy (saddr dispatch); got: ${out}"
  pass "forged source IP is dispatched by saddr (subject a's policy, no iifname)"

  # Unload removes the subject's DNAT rule (the rebuild drops it).
  remove_binding b runtime-b att-b
  wait_for 15 "subject b DNAT removed on unload" bash -c "! nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.6'"
  pass "unload removed subject b's DNAT rule"

  # mitmdump death -> healthz 503 -> auto-restart (watchMitmproxy + backoff)
  # -> interception resumes. The 503 window can be sub-second, so poll fast.
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
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "interception must resume after mitmdump restart; got: ${out}"
  pass "mitmdump death recovery (503 -> auto-restart -> interception resumes)"

 ###############################################################################
  info "Test 11c: up -> down transition removes the previous generation's interception table"
  # Subject a's DNAT is live at this point (an up generation). Restarting
  # without the seed must latch the addon down AND remove the interception
  # table — stale rules would keep DNATing 80/443 to a now-unserved mitm
  # port (blackhole).
  if ! nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '; then
    fail "precondition: subject a's interception rule must be installed before the up->down transition"
  fi
  # Kill the up generation FIRST: the interception table intentionally
  # survives the daemon (fail closed), so the down generation's startup
  # cleanup is what must remove it.
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  CA_SEED_DIR="" EXPECT_MITM_DOWN=1 EGRESS_MITM=1 start_egress
  if nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '; then
    fail "up->down transition must remove the interception table (stale DNAT = blackhole)"
  fi
  pass "up->down transition removed the interception table (no blackhole)"

###############################################################################
  info "Test 12: MITM-mode restart recovery (DNAT rebuild + pinned CA stability + action replay)"
  # Test 11c left the addon DOWN (no fingerprint on healthz), so stability is
  # asserted against the pinned seed itself — the previous up generation was
  # proven equal to it in Test 11b, and the restart must land on it again.
  # This restart also exercises the idempotent re-seed over an already-seeded
  # confdir.
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress
  got_fp="$(healthz_fingerprint)"
  [ -n "${got_fp}" ] || fail "pinned CA fingerprint must be present after restart"
  [ "${got_fp}" = "${SEED_FP}" ] || fail "pinned CA fingerprint after restart ${got_fp} must equal the pinned seed ${SEED_FP} (no rotation)"
  pass "pinned CA fingerprint stable across restart (${got_fp:0:12}...)"
  set_binding a 10.10.0.5 9 runtime-a-2 att-a-2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  wait_for 15 "DNAT rebuilt after restart" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '"
  lifecycle_hook a runtime-a-2 att-a-2 sandbox.data-plane-ready
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.1" || fail "interception must work after MITM-mode restart; got: ${out}"
  pass "MITM-mode restart recovery (DNAT + pinned CA + action replay + interception)"

  ###############################################################################
  info "Test 13: chained upstream CONNECT proxy — dual-resolver drop set + profile-wide containment"
  # The chained proxy endpoint (connectproxy relay) lives in the ext netns on
  # :3128: the shared mitmdump dials it as locally generated Pod traffic,
  # while every sandbox path to it must be dropped profile-wide. proxy.test
  # resolves ONLY through the Pod resolver (/etc/hosts) — the dnsproxy's own
  # upstream (127.0.0.1:5300) NXDOMAINs it. That is exactly the
  # split-resolver-authority case from the review: the address mitmdump
  # actually dials is one the dns-proxy authority never returns, and the drop
  # set must still cover it (union of both authorities).
  ip netns exec osb-ext python3 "${SCRIPT_DIR}/fast_sandbox_upstream.py" connectproxy >/dev/null 2>&1 &
  PROXY_PID=$!
  wait_for 5 "connect relay up" bash -c "ip netns exec osb-ext curl -s -m 2 http://127.0.0.1:3128/_count | grep -q 'relayed='"
  relay_count() { ip netns exec osb-ext curl -s -m 2 http://127.0.0.1:3128/_count | sed -n 's/^relayed=//p'; }

  HOSTS_BACKUP="$(mktemp /tmp/fast-sandbox-hosts.XXXXXX)"
  cp /etc/hosts "${HOSTS_BACKUP}"
  # osb-ext shares this hosts file: the relay must resolve absolute-URI hosts.
  echo "10.99.0.2 proxy.test ext.test" >> /etc/hosts

  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 EGRESS_UPSTREAM_PROXY="http://proxy.test:3128" start_egress

  # Containment seeded from the Pod-resolver authority only (the dnsproxy
  # authority NXDOMAINs the name): drop element + forward/input drop rules.
  wait_for 15 "upstream drop set seeded via the Pod resolver" bash -c \
    "nft list set inet opensandbox-fast-sandbox upstream_proxy_v4 2>/dev/null | grep -q '10.99.0.2'"
  nft list table inet opensandbox-fast-sandbox 2>/dev/null | grep -q 'upstream_proxy_v4 tcp dport 3128 drop' \
    || fail "profile-wide forward drop rule missing"
  nft list table inet opensandbox-fast-sandbox 2>/dev/null | grep -q 'ct original ip daddr @upstream_proxy_v4' \
    || fail "input-path containment rule missing (intercepted CONNECTs)"
  pass "drop set seeded from the Pod-resolver authority (split-authority union)"

  # Subject a re-registers with a policy that allows the ext destination and
  # its IP, so any failure to reach the proxy can only be the profile-wide drop.
  bind_and_ready a 10.10.0.5 10 runtime-a-2 att-a-2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'

  # Chained data plane (sandbox -> DNAT -> shared mitm -> CONNECT relay -> ext):
  # the relay counter proves the upstream dial went through the chained proxy
  # (only the mitm chain can reach it — direct sandbox CONNECT is dropped
  # profile-wide), and client=10.99.0.2 is the relay's local source as seen
  # by the ext server, proving the request arrived via the relay.
  before="$(relay_count)"
  out="$(ip netns exec osb-sandbox-a curl -s -m 8 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.2" || fail "chained http did not arrive via the CONNECT relay; got: ${out}"
  after="$(relay_count)"
  [ "${after}" -gt "${before}" ] || fail "chained http must traverse the forward relay (count ${before} -> ${after})"
  out="$(ip netns exec osb-sandbox-a curl -sk -m 8 -H 'Host: ext.test' https://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.2" || fail "chained https did not arrive via the CONNECT relay; got: ${out}"
  [ "$(relay_count)" -gt "${after}" ] || fail "chained https must traverse the CONNECT tunnel"
  pass "chained data plane (sandbox -> mitm -> CONNECT relay -> ext, http + https)"

  # Direct sandbox -> proxy is dropped for a policy-scoped subject.
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null --proxytunnel -x http://10.99.0.2:3128 http://10.99.0.2:8080/ 2>/dev/null; then
    fail "direct sandbox CONNECT to the chained proxy must be dropped"
  fi
  pass "direct proxy CONNECT dropped (policy-scoped subject)"

  # Default-allow subject: still dropped — the containment is profile-wide,
  # above every per-subject rule. Control first: non-proxy traffic flows, so
  # only the proxy endpoint is unreachable. Subject a is still active, so
  # this relay update applies immediately (no data-plane-ready replay —
  # relay writes apply in place on an active subject).
  relay_put_policy a '{"defaultAction":"allow"}'
  out="$(ip netns exec osb-sandbox-a curl -s -m 5 -H 'Host: ext.test' http://10.99.0.2:8080/)"
  echo "${out}" | grep -qi "client=10.10.0.5" || fail "default-allow control request failed (subject inactive?); got: ${out}"
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null --proxytunnel -x http://10.99.0.2:3128 http://10.99.0.2:8080/ 2>/dev/null; then
    fail "default-allow subject must NOT reach the chained proxy (open-relay bypass)"
  fi
  pass "profile-wide containment (default-allow subject still blocked)"

  # Containment survives an egress restart: the drop rule sits ahead of the
  # established accept, the drop elements are PERMANENT (no kernel timeout —
  # they persist in the kernel for the whole daemon downtime, like every
  # other rule in this table), and the chained path works after the replay.
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  nft list set inet opensandbox-fast-sandbox upstream_proxy_v4 2>/dev/null | grep -q '10.99.0.2' \
    || fail "drop element must persist in the kernel while the egress daemon is down"
  EGRESS_MITM=1 EGRESS_UPSTREAM_PROXY="http://proxy.test:3128" start_egress
  wait_for 15 "drop set re-seeded after restart" bash -c \
    "nft list set inet opensandbox-fast-sandbox upstream_proxy_v4 2>/dev/null | grep -q '10.99.0.2'"
  bind_and_ready a 10.10.0.5 12 runtime-a-2 att-a-2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  # The default-allow control write (spec 11) survived this restart — the
  # file supersedes the replayed input — so re-assert the allow-list
  # through the relay before the containment assertions.
  relay_put_policy a '{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  if ip netns exec osb-sandbox-a curl -s -m 3 -o /dev/null --proxytunnel -x http://10.99.0.2:3128 http://10.99.0.2:8080/ 2>/dev/null; then
    fail "containment must survive egress restart"
  fi
  out="$(ip netns exec osb-sandbox-a curl -s -m 8 -H 'Host: ext.test' http://10.99.0.2/)"
  echo "${out}" | grep -qi "client=10.99.0.2" || fail "chained path must work after restart; got: ${out}"
  pass "upstream containment across egress restart (drop re-seeded + chained path intact)"

  ###############################################################################
  info "Test 14: credential vault happy-path matrix — relay CRUD, auth types, substitutions, isolation, durability"
  # OSEP-0022 chain, every hop a real process: relay writes ack only after
  # the vault file commit (persistence is on for the whole run), the addon
  # fetches each subject's active snapshot over the shared unix socket and
  # injects the Go-rendered credentials into matching flows, and an egress
  # restart reloads the vault at the SET_BINDING replay. Covered happy
  # paths: POST create-only, PUT full replace with expectedRevision
  # (rotation), PUT create-when-absent, GET full revision, DELETE, the
  # correct X-Fast-Sandbox-Incarnation precondition, the active ETag/304
  # handshake, every injection auth type (bearer, basic, apiKey,
  # customHeaders) plus query substitutions, host+path binding scope,
  # vault-created-while-denying (pending-policy validation), per-subject
  # isolation, and REMOVE_BINDING file purge across a restart.
  #
  # Binding match rides the Host header (transparent-mode pretty_host
  # prefers it over the original destination IP) and pins scheme http — the
  # default binding scheme is https-only.
  #
  # Decouple from Test 13 first: its chained CONNECT relay would wrap the
  # probe (the ext echo would see the relay's source 10.99.0.2, not the mitm's
  # 10.99.0.1), so drop the helper and restart the egress on the direct mitm
  # path. The restart also unregisters the subjects; replay subject a below.
  VAULT_POLICY_A='{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"},{"action":"allow","target":"api.test"},{"action":"allow","target":"custom.test"},{"action":"allow","target":"basic.test"},{"action":"allow","target":"sub.test"}]}'
  VAULT_POLICY_B='{"defaultAction":"deny","egress":[{"action":"allow","target":"10.99.0.2"},{"action":"allow","target":"ext.test"}]}'
  if [ -n "${PROXY_PID}" ]; then
    kill "${PROXY_PID}" 2>/dev/null || true
    wait "${PROXY_PID}" 2>/dev/null || true
    PROXY_PID=""
  fi
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress
  bind_and_ready a 10.10.0.5 13 runtime-a-2 att-a-2 "${VAULT_POLICY_A}"
  relay_put_policy a "${VAULT_POLICY_A}"
  wait_for 15 "per-subject DNAT (vault preamble)" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '"

  # vault_relay <subject> <method> [body] [incarnation]: one relay op;
  # prints "<code>" and leaves the response body in ${VAULT_BODY}.
  vault_relay() {
    local subj="$1" method="$2" body="${3-}" inc="${4-}" extra=()
    [ -n "${inc}" ] && extra=(-H "X-Fast-Sandbox-Incarnation: ${inc}")
    if [ -n "${body}" ]; then
      extra+=(-d "${body}")
    fi
    curl -s -o "${VAULT_BODY}" -w '%{http_code}' -X"${method}" \
      "http://127.0.0.1:${POLICY_PORT}/vault/${subj}" "${extra[@]}"
  }
  # vault_probe <netns> <host> [uri]: one real request through the DNAT ->
  # shared mitm -> ext echo (the echo prints client= and path= lines plus
  # the request headers it received).
  vault_probe() {
    ip netns exec "osb-sandbox-$1" curl -s -m 5 -H "Host: $2" "http://10.99.0.2${3-/}"
  }
  assert_credential() { # assert_credential <out> <header-line>
    echo "$1" | grep -qi "client=10.99.0.1" || fail "request did not traverse the shared mitm; got: $1"
    echo "$1" | grep -qi "$2" || fail "expected '$2' at the upstream; got: $1"
  }
  assert_no_credential() { # assert_no_credential <out> <secret>
    echo "$1" | grep -qi "client=10.99.0.1" || fail "request did not traverse the shared mitm; got: $1"
    # No credential header may flow at all — a stale-cache injection of a
    # previous revision's secret must fail the probe, not just the named one.
    if echo "$1" | grep -Eqi '^(authorization|x-api-key|x-custom-a|x-custom-b):'; then
      fail "out-of-scope request must carry no credential header; got: $1"
    fi
    if echo "$1" | grep -qi "$2"; then
      fail "credential leaked outside its binding scope; got: $1"
    fi
  }

  V1_BODY='{"credentials":[{"name":"smoke-token","source":{"type":"inline","value":"smoke-vault-secret-1"}}],"bindings":[{"name":"ext-bearer","match":{"hosts":["ext.test"],"schemes":["http"]},"auth":{"type":"bearer","credential":"smoke-token"}}]}'

  # 14a: create-only relay POST — 201, then 409 on the second create; the
  # response is the sanitized State (credential metadata, never values).
  code="$(vault_relay a POST "${V1_BODY}")"
  [ "${code}" = "201" ] || fail "relay POST must create the vault (201), got ${code}: $(cat "${VAULT_BODY}")"
  grep -q '"name":"smoke-token"' "${VAULT_BODY}" || fail "sanitized state must carry credential metadata, got: $(cat "${VAULT_BODY}")"
  if grep -q 'smoke-vault-secret-1' "${VAULT_BODY}"; then
    fail "sanitized state must not carry credential values, got: $(cat "${VAULT_BODY}")"
  fi
  code="$(vault_relay a POST "${V1_BODY}")"
  [ "${code}" = "409" ] || fail "relay POST must be create-only (409 on exists), got ${code}"
  pass "relay POST create-only (201 sanitized, then 409 on exists)"

  # 14b: the real mitm request carries the injected credential; a
  # non-matching host on the same subject stays uncredentialed.
  out="$(vault_probe a ext.test)"
  assert_credential "${out}" 'authorization: bearer smoke-vault-secret-1'
  out="$(vault_probe a alt.test)"
  assert_no_credential "${out}" 'smoke-vault-secret-1'
  pass "real mitm injection (matching host credentialed, alt host uncredentialed)"

  # 14c: PUT full replace (expectedRevision precondition + correct
  # incarnation header — the Fastlet relay's happy shape) rotates the
  # credential, scopes the bearer binding to /private*, and adds every
  # remaining auth type: apiKey, customHeaders, basic, and a passthrough
  # query substitution.
  V2_BODY='{"expectedRevision":1,"credentials":[{"name":"smoke-token","source":{"type":"inline","value":"smoke-vault-secret-2"}},{"name":"api-token","source":{"type":"inline","value":"api-secret-1"}},{"name":"custom-a","source":{"type":"inline","value":"custom-a-secret"}},{"name":"custom-b","source":{"type":"inline","value":"custom-b-secret"}},{"name":"basic-token","source":{"type":"inline","value":"basic-secret"}},{"name":"sub-token","source":{"type":"inline","value":"sub-secret-1"}}],"bindings":[{"name":"ext-bearer","match":{"hosts":["ext.test"],"schemes":["http"],"paths":["/private*"]},"auth":{"type":"bearer","credential":"smoke-token"}},{"name":"api-key","match":{"hosts":["api.test"],"schemes":["http"]},"auth":{"type":"apiKey","name":"X-API-Key","credential":"api-token"}},{"name":"custom","match":{"hosts":["custom.test"],"schemes":["http"]},"auth":{"type":"customHeaders","headers":[{"name":"X-Custom-A","credential":"custom-a"},{"name":"X-Custom-B","credential":"custom-b"}]}},{"name":"basic-auth","match":{"hosts":["basic.test"],"schemes":["http"]},"auth":{"type":"basic","credential":"basic-token"}},{"name":"sub-query","match":{"hosts":["sub.test"],"schemes":["http"]},"auth":{"type":"passthrough","substitutions":[{"credential":"sub-token","placeholder":"$TOKEN","in":["query"]}]}}]}'
  code="$(vault_relay a PUT "${V2_BODY}" runtime-a-2)"
  [ "${code}" = "200" ] || fail "relay PUT replace must be 200, got ${code}: $(cat "${VAULT_BODY}")"
  grep -q '"revision":2' "${VAULT_BODY}" || fail "replaced vault revision must be 2, got: $(cat "${VAULT_BODY}")"
  out="$(vault_probe a ext.test /private)"
  assert_credential "${out}" 'authorization: bearer smoke-vault-secret-2'
  out="$(vault_probe a ext.test)"
  assert_no_credential "${out}" 'smoke-vault-secret-2'
  out="$(vault_probe a api.test)"
  assert_credential "${out}" 'x-api-key: api-secret-1'
  if echo "${out}" | grep -qi 'bearer'; then
    fail "apiKey binding must not leak the bearer credential; got: ${out}"
  fi
  out="$(vault_probe a custom.test)"
  assert_credential "${out}" 'x-custom-a: custom-a-secret'
  assert_credential "${out}" 'x-custom-b: custom-b-secret'
  out="$(vault_probe a basic.test)"
  assert_credential "${out}" 'authorization: basic basic-secret'
  out="$(vault_probe a sub.test '/sub?tok=$TOKEN')"
  assert_credential "${out}" 'tok=sub-secret-1'
  if echo "${out}" | grep -q 'TOKEN'; then
    fail "query substitution must replace the placeholder; got: ${out}"
  fi
  pass "PUT replace (rotation, /private scope, apiKey/customHeaders/basic/query-substitution)"

  # 14d: the addon's active-vault ETag handshake on the shared unix socket
  # (the addon-facing surface) — a conditional re-fetch with the tag just
  # served answers 304.
  ACTIVE_SOCK="/run/opensandbox/credential-proxy/active.sock"
  etag="$(curl -s -D - -o /dev/null --unix-socket "${ACTIVE_SOCK}" "http://localhost/credential-vault/_active?clientIp=10.10.0.5" | tr -d '\r' | sed -n 's/^[Ee][Tt][Aa][Gg]: //p')"
  [ -n "${etag}" ] || fail "active snapshot must serve an ETag"
  code="$(curl -s -o /dev/null -w '%{http_code}' --unix-socket "${ACTIVE_SOCK}" -H "If-None-Match: ${etag}" "http://localhost/credential-vault/_active?clientIp=10.10.0.5")"
  [ "${code}" = "304" ] || fail "conditional active fetch must be 304, got ${code}"
  pass "active-vault ETag/304 handshake"

  # 14e: restart durability — with the daemon down the vault lives only in
  # its file (the relay 404s the unregistered subject), the SET_BINDING
  # replay reloads it from disk, and the whole v2 matrix flows again with
  # no relay write in between.
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress
  code="$(vault_relay a GET)"
  [ "${code}" = "404" ] || fail "unregistered subject must 404 before the replay, got ${code}"
  bind_and_ready a 10.10.0.5 14 runtime-a-2 att-a-2 "${VAULT_POLICY_A}"
  wait_for 15 "DNAT rebuilt after restart" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '"
  code="$(vault_relay a GET)"
  [ "${code}" = "200" ] || fail "vault must survive the restart (reloaded at SET_BINDING), got ${code}"
  grep -q '"revision":2' "${VAULT_BODY}" || fail "reloaded vault revision must be 2, got: $(cat "${VAULT_BODY}")"
  grep -q 'smoke-vault-secret-2' "${VAULT_BODY}" \
    || fail "relay GET must return the full revision including credential values, got: $(cat "${VAULT_BODY}")"
  out="$(vault_probe a ext.test /private)"
  assert_credential "${out}" 'authorization: bearer smoke-vault-secret-2'
  pass "vault survives restart (file reload at SET_BINDING replay, injection resumes)"

  # 14f: per-subject isolation + vault-created-while-denying — subject b is
  # still deny-first when its relay POST lands, so the op validates against
  # the PENDING binding policy; after activation b flows with its own
  # credential while a keeps a separate one.
  VB_BODY='{"credentials":[{"name":"smoke-b-token","source":{"type":"inline","value":"smoke-b-secret"}}],"bindings":[{"name":"ext-bearer","match":{"hosts":["ext.test"],"schemes":["http"],"paths":["/private*"]},"auth":{"type":"bearer","credential":"smoke-b-token"}}]}'
  set_binding b 10.10.0.6 3 runtime-b att-b "${VAULT_POLICY_B}"
  # b's record survived the restarts (MITM section) and supersedes the
  # binding input — the vault validation policy rides the relay.
  relay_put_policy b "${VAULT_POLICY_B}"
  code="$(vault_relay b POST "${VB_BODY}")"
  [ "${code}" = "201" ] || fail "relay POST must accept a denying subject (pending-policy validation), got ${code}: $(cat "${VAULT_BODY}")"
  lifecycle_hook b runtime-b att-b sandbox.runtime-ready
  lifecycle_hook b runtime-b att-b sandbox.data-plane-ready
  wait_for 15 "subject b DNAT" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.6 '"
  out="$(vault_probe b ext.test /private)"
  echo "${out}" | grep -qi 'authorization: bearer smoke-b-secret' \
    || fail "subject b must flow with its own pending-created credential; got: ${out}"
  out="$(vault_probe a ext.test /private)"
  assert_credential "${out}" 'authorization: bearer smoke-vault-secret-2'
  if echo "${out}" | grep -qi 'smoke-b-secret'; then
    fail "subject a must not receive subject b's credential; got: ${out}"
  fi
  pass "per-subject isolation + pending-policy creation (b credentialed while denying)"

  # 14g: teardown purge — REMOVE_BINDING drops the vault file with the
  # subject, so a restart + replay finds nothing to resurrect.
  remove_binding b runtime-b att-b
  wait_for 15 "subject b unloaded" bash -c "! nft list table inet opensandbox-fast-sandbox | grep -q subj_s_b"
  code="$(vault_relay b GET)"
  [ "${code}" = "404" ] || fail "removed subject's vault must 404, got ${code}"
  remove_binding a runtime-a-2 att-a-2
  wait_for 15 "subject a unloaded" bash -c "! nft list table inet opensandbox-fast-sandbox | grep -q subj_s_a"
  code="$(vault_relay a GET)"
  [ "${code}" = "404" ] || fail "removed subject's vault must 404, got ${code}"
  kill "${EGRESS_PID}" 2>/dev/null
  wait "${EGRESS_PID}" 2>/dev/null || true
  EGRESS_PID=""
  EGRESS_MITM=1 start_egress
  bind_and_ready a 10.10.0.5 15 runtime-a-2 att-a-2 "${VAULT_POLICY_A}"
  wait_for 15 "DNAT rebuilt after purge restart" bash -c "nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q 'ip saddr 10.10.0.5 '"
  code="$(vault_relay a GET)"
  [ "${code}" = "404" ] || fail "purged vault must not resurrect across a restart, got ${code}"
  pass "REMOVE_BINDING purges the vault file (nothing resurrects after restart)"

  # 14h: PUT create-when-absent — the purge left no vault, so an
  # expectedRevision-free PUT creates one (201) and flows immediately.
  V3_BODY='{"credentials":[{"name":"smoke-token","source":{"type":"inline","value":"smoke-vault-secret-3"}}],"bindings":[{"name":"ext-bearer","match":{"hosts":["ext.test"],"schemes":["http"],"paths":["/private*"]},"auth":{"type":"bearer","credential":"smoke-token"}}]}'
  code="$(vault_relay a PUT "${V3_BODY}" runtime-a-2)"
  [ "${code}" = "201" ] || fail "relay PUT must create when absent (201), got ${code}: $(cat "${VAULT_BODY}")"
  out="$(vault_probe a ext.test /private)"
  assert_credential "${out}" 'authorization: bearer smoke-vault-secret-3'
  pass "PUT create-when-absent (201, injection flows immediately)"

  # 14i: relay DELETE — 204, the relay GET 404s, and the same matching host
  # drops back to the no-vault plain flow.
  code="$(vault_relay a DELETE "" runtime-a-2)"
  [ "${code}" = "204" ] || fail "relay DELETE must be 204, got ${code}"
  code="$(vault_relay a GET)"
  [ "${code}" = "404" ] || fail "vault must be gone after DELETE, got ${code}"
  out="$(vault_probe a ext.test /private)"
  assert_no_credential "${out}" 'smoke-vault-secret-3'
  pass "relay DELETE stops injection (no-vault plain flow)"
fi

###############################################################################
info "All fast-sandbox smoke tests passed."
