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

# Forward mode (OSEP-0030) end-to-end smoke test. Hermetic: no internet.
#
# Wires containers the way the Docker runtime does: the egress sidecar on the
# default bridge with NET_ADMIN, and a "sandbox" container sharing its netns
# without NET_ADMIN / NET_RAW. A local CONNECT gateway and origin record every
# peer, so each check proves both what went through the gateway and that
# nothing reached the origin directly.
#
# Requires Docker. SKIP_BUILD=1 reuses an existing ${IMG}.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

IMG="${IMG:-opensandbox/egress:local}"
PEER_IMG="${PEER_IMG:-python:3.12-alpine}"
FIXTURE="${SCRIPT_DIR}/forward_smoke.py"
PREFIX="egress-smoke-forward"
EGRESS="${PREFIX}-egress"
GATEWAY="${PREFIX}-gateway"
ORIGIN="${PREFIX}-origin"
SANDBOX="${PREFIX}-sandbox"
AUTH="Bearer forward-smoke-token"
SANDBOX_ID="sbx-forward-smoke"

info() { echo "[$(date +%H:%M:%S)] $*"; }
pass() { info "PASS: $*"; }
fail() {
  echo "FAIL: $*" >&2
  for c in "${EGRESS}" "${GATEWAY}" "${ORIGIN}"; do
    echo "----- docker logs ${c} -----" >&2
    docker logs "${c}" >&2 2>&1 || true
  done
  exit 1
}

BAD_EGRESS="${PREFIX}-bad-config"

cleanup() {
  docker rm -f "${SANDBOX}" "${EGRESS}" "${GATEWAY}" "${ORIGIN}" "${BAD_EGRESS}" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

if [ "${SKIP_BUILD:-0}" != "1" ]; then
  info "Building image ${IMG}"
  docker build -t "${IMG}" -f "${REPO_ROOT}/components/egress/Dockerfile" "${REPO_ROOT}"
fi
docker pull -q "${PEER_IMG}" >/dev/null

ip_of() { docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"; }
client() { docker exec "${SANDBOX}" python /forward_smoke.py client "$@"; }
gateway_log() { docker logs "${GATEWAY}" 2>&1; }
origin_log() { docker logs "${ORIGIN}" 2>&1; }
healthz_code() { curl -s -o /dev/null -w '%{http_code}' "http://${EGRESS_API}/healthz" || true; }

wait_healthz() {
  local want="$1" timeout="$2" code=""
  for _ in $(seq 1 $((timeout * 2))); do
    code="$(healthz_code)"
    [ "${code}" = "${want}" ] && return 0
    sleep 0.5
  done
  fail "egress /healthz: want ${want}, last ${code}"
}

info "Test: forward mode without gateway authorization fails startup"
# The image supervisor restarts a crashed egress, so the container keeps
# running; what matters is that egress exits before installing anything and
# never reports ready (the server's readiness wait then fails the create).
docker run -d --name "${BAD_EGRESS}" --cap-add=NET_ADMIN \
  -e OPENSANDBOX_EGRESS_MODE=forward \
  -e OPENSANDBOX_EGRESS_UPSTREAM_PROXY=http://192.0.2.1:3128 \
  -e OPENSANDBOX_EGRESS_SANDBOX_ID="${SANDBOX_ID}" \
  "${IMG}" >/dev/null
for _ in $(seq 1 30); do
  docker logs "${BAD_EGRESS}" 2>&1 | grep -q "invalid forward mode configuration" && break
  sleep 0.5
done
docker logs "${BAD_EGRESS}" 2>&1 | grep -q "invalid forward mode configuration: forwarder: gateway authorization is required" \
  || fail "egress did not reject forward mode without OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH"
if docker logs "${BAD_EGRESS}" 2>&1 | grep -q "forward mode ready"; then
  fail "egress became ready without gateway authorization"
fi
docker rm -f "${BAD_EGRESS}" >/dev/null
pass "missing authorization is fatal"

info "Starting origin and gateway"
docker run -d --name "${ORIGIN}" -v "${FIXTURE}:/forward_smoke.py:ro" \
  "${PEER_IMG}" python -u /forward_smoke.py origin >/dev/null
docker run -d --name "${GATEWAY}" -v "${FIXTURE}:/forward_smoke.py:ro" \
  -e AUTH="${AUTH}" -e DENY_PORTS=81 \
  "${PEER_IMG}" python -u /forward_smoke.py gateway 3128 >/dev/null
for _ in $(seq 1 20); do
  if origin_log | grep -q '"event": "listening"' && gateway_log | grep -q '"event": "listening"'; then
    break
  fi
  sleep 0.5
done
origin_log | grep -q '"event": "listening"' || fail "origin did not start listening"
gateway_log | grep -q '"event": "listening"' || fail "gateway did not start listening"
ORIGIN_IP="$(ip_of "${ORIGIN}")"
GATEWAY_IP="$(ip_of "${GATEWAY}")"
info "origin=${ORIGIN_IP} gateway=${GATEWAY_IP}"

info "Starting egress (forward) and sandbox"
docker run -d --name "${EGRESS}" \
  --cap-add=NET_ADMIN \
  --sysctl net.ipv6.conf.all.disable_ipv6=1 \
  --sysctl net.ipv6.conf.default.disable_ipv6=1 \
  -e OPENSANDBOX_EGRESS_MODE=forward \
  -e OPENSANDBOX_EGRESS_UPSTREAM_PROXY="http://${GATEWAY_IP}:3128" \
  -e OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH="${AUTH}" \
  -e OPENSANDBOX_EGRESS_SANDBOX_ID="${SANDBOX_ID}" \
  -p 127.0.0.1::18080 \
  "${IMG}" >/dev/null
EGRESS_API="$(docker port "${EGRESS}" 18080/tcp | head -n1)"
wait_healthz 200 30
pass "/healthz ready with reachable gateway"

docker run -d --name "${SANDBOX}" \
  --network "container:${EGRESS}" \
  --cap-drop NET_ADMIN --cap-drop NET_RAW \
  -v "${FIXTURE}:/forward_smoke.py:ro" \
  "${PEER_IMG}" sleep infinity >/dev/null

expect_relayed() {
  local port="$1" body
  body="$(client tcp "${ORIGIN_IP}" "${port}")" || fail "TCP ${port} via gateway should succeed"
  [ "${body}" = "origin-${port}" ] || fail "unexpected body on ${port}: ${body}"
  gateway_log | grep -F "\"target\": \"${ORIGIN_IP}:${port}\"" | grep -F '"method": "CONNECT"' \
    | grep -F '"auth": true' | grep -F "\"sandbox_id\": \"${SANDBOX_ID}\"" | grep -qF '"status": 200' \
    || fail "gateway did not log an authenticated CONNECT ${ORIGIN_IP}:${port} with sandbox id"
  origin_log | grep -F "\"port\": ${port}" | grep -qF "\"peer\": \"${GATEWAY_IP}\"" \
    || fail "origin did not see ${port} from the gateway"
  if origin_log | grep -F "\"port\": ${port}" | grep -vqF "\"peer\": \"${GATEWAY_IP}\""; then
    fail "origin saw ${port} from a peer other than the gateway"
  fi
  pass "TCP ${port} relayed via CONNECT ${ORIGIN_IP}:${port} with identity"
}

info "Test: TCP is relayed byte-for-byte through the gateway"
expect_relayed 80
expect_relayed 8443

info "Test: gateway denial resets the connection with no direct fallback"
if client tcp "${ORIGIN_IP}" 81 >/dev/null 2>&1; then
  fail "TCP 81 should fail when the gateway denies it"
fi
gateway_log | grep -F "\"target\": \"${ORIGIN_IP}:81\"" | grep -qF '"status": 403' \
  || fail "gateway did not see the denied CONNECT"
if origin_log | grep -qF '"port": 81'; then
  fail "origin received TCP 81 directly"
fi
pass "denied CONNECT is not retried directly"

info "Test: UDP is dropped"
if client udp "${ORIGIN_IP}" 9999 >/dev/null 2>&1; then
  fail "UDP should be dropped"
fi
if origin_log | grep -qF '"event": "udp"'; then
  fail "origin received UDP"
fi
pass "UDP dropped"

info "Test: link-local metadata address is dropped before the gateway"
if client tcp 169.254.169.254 80 >/dev/null 2>&1; then
  fail "169.254.169.254 should be unreachable"
fi
if gateway_log | grep -qF '169.254.'; then
  fail "link-local destination reached the gateway"
fi
pass "link-local dropped"

info "Test: the sandbox cannot talk to the gateway directly"
client tcp "${GATEWAY_IP}" 3128 >/dev/null 2>&1 || true
gateway_log | grep -F "\"target\": \"${GATEWAY_IP}:3128\"" | grep -qF '"method": "CONNECT"' \
  || fail "direct dial to the gateway was not captured into a CONNECT"
pass "direct gateway dial is captured and relayed, never raw"

info "Test: policy API is disabled"
code="$(curl -s -o /dev/null -w '%{http_code}' -XPOST "http://${EGRESS_API}/policy" -d '{"defaultAction":"allow"}')"
[ "${code}" = "409" ] || fail "POST /policy: want 409, got ${code}"
pass "policy API returns 409"

info "Test: gateway outage turns /healthz unhealthy and never falls back"
docker stop -t 1 "${GATEWAY}" >/dev/null
wait_healthz 503 20
before="$(origin_log | grep -c '"event": "tcp"' || true)"
if client tcp "${ORIGIN_IP}" 80 >/dev/null 2>&1; then
  fail "TCP should fail while the gateway is down"
fi
after="$(origin_log | grep -c '"event": "tcp"' || true)"
[ "${before}" = "${after}" ] || fail "origin was reached directly while the gateway was down"
pass "gateway down: /healthz 503, no direct egress"

info "Test: killed egress leaves the sandbox network closed"
before_kill="$(origin_log | grep -c -E '"event": "(tcp|udp)"' || true)"
docker kill "${EGRESS}" >/dev/null
if client tcp "${ORIGIN_IP}" 80 >/dev/null 2>&1; then
  fail "TCP should fail after egress is killed"
fi
if client udp "${ORIGIN_IP}" 9999 >/dev/null 2>&1; then
  fail "UDP should fail after egress is killed"
fi
after_kill="$(origin_log | grep -c -E '"event": "(tcp|udp)"' || true)"
[ "${before_kill}" = "${after_kill}" ] || fail "origin was reached after egress was killed"
pass "egress killed: enforcement retained (fail closed)"

info "All forward mode smoke tests passed."
