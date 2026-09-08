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

# High-fidelity fast-sandbox deployment simulation with a REAL Firecracker
# microVM: egress in a container sharing the fastlet container's Pod netns
# (docker bridge), a firecracker VM as the sandbox, real CA delivery into the
# VM rootfs, real system-trust installation, and a real TLS credential-vault
# round trip. Also measures the CA-installation cost (the point of the test).
#
# Topology:
#
#   host (bare metal)
#   └── docker bridge: osb-pod-net            (simulates the Pod's cluster net)
#       └── fastlet container (ubuntu, privileged, /dev/kvm, host netns mounts)
#           │  netns = Pod netns
#           ├── egress container (--network container:fastlet, privileged)
#           │     fleet profile + shared mitmdump; nft/DNAT/loopback services
#           │     all live in the Pod netns
#           └── firecracker microVM (sandbox) in netns osb-fc-vm1
#                 eth0 (tap0) ──br0── veth ──► Pod netns (gateway 10.30.0.1)
#                 rootfs: ubuntu CI image + bootstrap script that installs the
#                 mitm CA into the system trust store, then curls
#                 https://ext.test through the egress MITM (real TLS check).
#
# The CA delivery is real: fastlet loop-mounts the VM rootfs and writes the
# CA exported by egress (the /opt/opensandbox/mitm-ca shared-volume contract,
# fast-sandbox issue #19) before the VM boots.
#
# Requires: root, KVM (/dev/kvm), docker, nft, iproute2, unsquashfs
# (squashfs-tools). Run on a bare-metal Linux host with network access to
# download the firecracker release and the firecracker CI kernel/rootfs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
FC_VERSION="${FC_VERSION:-v1.16.1}"
ASSET_DIR="${FC_ASSET_DIR:-/var/egress/fc-assets}"          # cached downloads
WORK_DIR="${FC_WORK_DIR:-/var/egress/fc-e2e}"               # scratch + mounts
BRIDGE_NET="osb-pod-net"
FASTLET_CT="fastlet-fc-e2e"
EGRESS_CT="egress-fc-e2e"
EGRESS_IMAGE="osb-egress:fc-e2e"

VM_NETNS="osb-fc-vm1"
VM_IP="10.30.0.5"
VM_GW="10.30.0.1"
VM_CIDR="10.30.0.0/24"
EXT_IP="10.99.0.2"
EXT_GW_IP="10.99.0.1"
EXT_CIDR="10.99.0.0/24"
EXT_CT_NETNS="osb-fc-ext"

RULES_DIR="${WORK_DIR}/rules"
CA_ROOT="${WORK_DIR}/opensandbox"       # mounted at /opt/opensandbox
VM_ROOTFS="${WORK_DIR}/vm-rootfs.ext4"  # firecracker rootfs image
VM_ROOTFS_MNT="${WORK_DIR}/vm-rootfs-mnt"
VM_SERIAL="${WORK_DIR}/fc-process.log"
VM_API_SOCK="${WORK_DIR}/fc.sock"
VM_BOOTSTRAP="root/bootstrap.sh"
POLICY_PORT=18080
DNS_UPSTREAM_PORT=5300

DNS_PID=""
EXT_PID=""
FC_PID=""
EGRESS_CT_ID=""
FASTLET_CT_ID=""
START_TS="$(date +%s)"

info() { echo "[$(date +%H:%M:%S)] $*"; }
pass() { info "PASS: $*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ts()   { date +%s.%3N; }

# pod_exec runs a command inside the fastlet container (Pod netns).
pod_exec() { docker exec "${FASTLET_CT}" "$@"; }

# pod_ns runs a command in the Pod netns (the fastlet container's netns) from
# the host. netlink creation inside a docker bridge container is brittle on
# some kernel/driver combos ("RTNETLINK answers: Invalid argument"), so veth
# pairs are created on the host and the Pod-side end is moved into the
# container netns, exactly like the fleet smoke test does.
FASTLET_PID=""
pod_ns() { nsenter --net="/proc/${FASTLET_PID}/ns/net" "$@"; }

require() {
  [ "$(id -u)" = "0" ] || fail "requires root (loop mounts, netns, nft)"
  for c in docker nft ip nsenter curl wget python3 mkfs.ext4; do
    command -v "${c}" >/dev/null || fail "missing required command: ${c}"
  done
  [ -r /dev/kvm ] && [ -w /dev/kvm ] || fail "/dev/kvm not accessible (KVM required)"
}

cleanup() {
  set +e
  for pid in "${FC_PID1:-}" "${FC_PID2:-}"; do
    [ -n "${pid}" ] && kill "${pid}" 2>/dev/null
  done
  [ -n "${EGRESS_CT_ID}" ] && docker rm -f "${EGRESS_CT_ID}" >/dev/null 2>&1
  [ -n "${FASTLET_CT_ID}" ] && docker rm -f "${FASTLET_CT_ID}" >/dev/null 2>&1
  docker network rm "${BRIDGE_NET}" >/dev/null 2>&1
  [ -n "${DNS_PID}" ] && kill "${DNS_PID}" 2>/dev/null
  [ -n "${EXT_PID}" ] && kill "${EXT_PID}" 2>/dev/null
  for mnt in "${VM_ROOTFS_MNT1:-}" "${VM_ROOTFS_MNT2:-}"; do
    [ -n "${mnt}" ] && umount "${mnt}" >/dev/null 2>&1
  done
  ip netns del "${VM_NETNS1}" >/dev/null 2>&1
  ip netns del "${VM_NETNS2}" >/dev/null 2>&1
  ip netns del "${EXT_CT_NETNS}" >/dev/null 2>&1
  nft delete table inet opensandbox-fleet 2>/dev/null
  nft delete table inet opensandbox_gateway_dns 2>/dev/null
  nft delete table inet opensandbox_gateway_mitm 2>/dev/null
}
trap cleanup EXIT

# watch_until <timeout_sec> <label> <grep pattern> <file>: polls every 0.2s
# so the recorded observation moment is precise (wait_for's 1s granularity
# would add up to 1s of error to the phase timings).
watch_until() {
  local timeout_sec="$1" label="$2" pattern="$3" file="$4"
  local elapsed=0
  while [ "${elapsed}" -lt "${timeout_sec}" ]; do
    if grep -q "${pattern}" "${file}" 2>/dev/null; then return 0; fi
    sleep 0.2
    elapsed=$((elapsed + 1))
  done
  fail "timed out waiting for: ${label}"
}

# wait_for <timeout_sec> <label> <cmd...>
wait_for() {
  local timeout_sec="$1" label="$2"
  shift 2
  local elapsed=0
  while [ "${elapsed}" -lt "${timeout_sec}" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  fail "timed out waiting for: ${label}"
}

# ---------------------------------------------------------------------------
info "== fast-sandbox + firecracker e2e (real VM, real CA trust) =="
require

ARCH="$(uname -m)"
[ "${ARCH}" = "x86_64" ] || [ "${ARCH}" = "aarch64" ] || fail "unsupported arch: ${ARCH}"
mkdir -p "${ASSET_DIR}" "${WORK_DIR}" "${RULES_DIR}" "${CA_ROOT}/mitm-ca"

# ---------------------------------------------------------------------------
info "Preparing assets (firecracker + kernel + rootfs)"
FC_TGZ="firecracker-${FC_VERSION}-${ARCH}.tgz"
if [ ! -x "${ASSET_DIR}/firecracker" ]; then
  wget -q -O "${ASSET_DIR}/${FC_TGZ}" \
    "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/${FC_TGZ}"
  tar -xzf "${ASSET_DIR}/${FC_TGZ}" -C "${ASSET_DIR}"
  mv -f "${ASSET_DIR}/release-${FC_VERSION}-${ARCH}/firecracker-${FC_VERSION}-${ARCH}" \
    "${ASSET_DIR}/firecracker"
fi

S3="https://s3.amazonaws.com/spec.ccfc.min"
CI_ARTIFACTS_PREFIX="$(curl -fsSL "${S3}?list-type=2&prefix=firecracker-ci/&delimiter=/" \
  | grep -oP '(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)' \
  | sort | tail -1)"
[ -n "${CI_ARTIFACTS_PREFIX}" ] || fail "could not enumerate firecracker CI artifacts"
KERNEL_KEY="$(curl -fsSL "${S3}?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-" \
  | grep -oP "(?<=<Key>)${CI_ARTIFACTS_PREFIX}${ARCH}/vmlinux-[0-9]+\.[0-9]+\.[0-9]{1,3}(?=</Key>)" \
  | sort -V | tail -1)"
[ -n "${KERNEL_KEY}" ] || fail "could not enumerate vmlinux artifacts"
KERNEL_PATH="${ASSET_DIR}/$(basename "${KERNEL_KEY}")"
[ -f "${KERNEL_PATH}" ] || wget -q -O "${KERNEL_PATH}" "${S3}/${KERNEL_KEY}"

ROOTFS_KEY="$(curl -fsSL "${S3}?list-type=2&prefix=${CI_ARTIFACTS_PREFIX}${ARCH}/ubuntu-" \
  | grep -oP "(?<=<Key>)${CI_ARTIFACTS_PREFIX}${ARCH}/ubuntu-[0-9]+\.[0-9]+\.squashfs(?=</Key>)" \
  | sort -V | tail -1)"
[ -n "${ROOTFS_KEY}" ] || fail "could not enumerate ubuntu rootfs artifacts"
SQUASHFS_PATH="${ASSET_DIR}/$(basename "${ROOTFS_KEY}")"
[ -f "${SQUASHFS_PATH}" ] || wget -q -O "${SQUASHFS_PATH}" "${S3}/${ROOTFS_KEY}"

info "assets: firecracker ${FC_VERSION}, kernel $(basename "${KERNEL_PATH}"), rootfs $(basename "${SQUASHFS_PATH}")"

# ---------------------------------------------------------------------------
info "Building the VM rootfs (ubuntu CI image + CA bootstrap)"
# bump ROOTFS_TEMPLATE_VERSION whenever the bootstrap script changes: the
# template is cached on disk and the parameterized bootstrap must reach
# every VM (a stale template silently curls the wrong target).
ROOTFS_TEMPLATE_VERSION=6
if [ ! -f "${VM_ROOTFS}" ] \
   || [ "$(cat "${ASSET_DIR}/rootfs-template.version" 2>/dev/null || true)" != "${ROOTFS_TEMPLATE_VERSION}" ]; then
  UNSQUASH_DIR="${WORK_DIR}/unsquash"
  rm -rf "${UNSQUASH_DIR}"
  mkdir -p "${UNSQUASH_DIR}"
  # The host's unsquashfs may predate zstd (e.g. RHEL ships squashfs-tools
  # 4.3); the ubuntu CI rootfs is zstd-compressed, so unsquash inside an
  # ubuntu container where the toolchain matches the artifact.
  docker run --rm \
    -v "${ASSET_DIR}:/assets:ro" \
    -v "${WORK_DIR}:/work" \
    ubuntu:24.04 bash -c \
      "apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq squashfs-tools >/dev/null 2>&1 && \
       unsquashfs -n -f -d /work/unsquash /assets/$(basename "${SQUASHFS_PATH}")" \
    || fail "unsquashfs failed (in ubuntu container)"

  cat > "${UNSQUASH_DIR}/${VM_BOOTSTRAP}" <<'BOOT'
#!/bin/bash
# fast-sandbox VM bootstrap (firecracker): mount basics, configure the
# network, install the mitm CA into the SYSTEM trust store (the measured
# cost), then run real TLS requests through the egress MITM. The target
# host comes from the kernel cmdline (test_target=), and the OTHER sandbox's
# host is probed too to prove cross-subject isolation.
console() { echo "[$(date -u +%s.%3N)] $*" > /dev/console; }
# /proc must be mounted BEFORE parsing the kernel cmdline (test_target/vm_ip)
mount -t proc proc /proc 2>/dev/null || true
mount -t sysfs sysfs /sys 2>/dev/null || true
TARGET="ext.test"
VM_IP="10.30.0.5"
GW_IP="10.30.0.1"
for arg in $(cat /proc/cmdline); do
  case "${arg}" in
    test_target=*) TARGET="${arg#test_target=}" ;;
    vm_ip=*) VM_IP="${arg#vm_ip=}" ;;
    gw_ip=*) GW_IP="${arg#gw_ip=}" ;;
  esac
done
if [ "${TARGET}" = "ext.test" ]; then CROSS="alt.test"; else CROSS="ext.test"; fi
console "bootstrap start target=${TARGET} ip=${VM_IP}"
ip link set eth0 up
ip addr add "${VM_IP}/24" dev eth0 2>/dev/null || true
ip route add default via "${GW_IP}" 2>/dev/null || true
echo "nameserver 10.30.0.1" > /etc/resolv.conf
console "network up"
console "installing CA into system trust"
mkdir -p /usr/local/share/ca-certificates
cp /opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem \
   /usr/local/share/ca-certificates/opensandbox-mitm.crt
update-ca-certificates > /dev/console 2>&1
console "CA installed"
console "curl https://${TARGET}/"
curl -s -m 15 "https://${TARGET}/" > /opt/result.txt 2>&1
console "curl done, result:"
cat /opt/result.txt > /dev/console
console "cross-probe https://${CROSS}/"
curl -s -m 15 "https://${CROSS}/" > /opt/result-cross.txt 2>&1
console "cross done, result:"
cat /opt/result-cross.txt > /dev/console
console "deny-probe http://10.99.0.9/"
curl -s -m 5 -o /dev/null -w "http_code=%{http_code}\n" http://10.99.0.9/ > /opt/result-deny.txt 2>&1
console "deny done, result:"
cat /opt/result-deny.txt > /dev/console
while true; do sleep 3600; done
BOOT
  chmod +x "${UNSQUASH_DIR}/${VM_BOOTSTRAP}"
  # the CA slot where fastlet delivers the cert (empty until delivery)
  mkdir -p "${UNSQUASH_DIR}/opt/opensandbox/mitm-ca"

  truncate -s 1G "${VM_ROOTFS}"
  mkfs.ext4 -q -F -d "${UNSQUASH_DIR}" "${VM_ROOTFS}"
  rm -rf "${UNSQUASH_DIR}"
  echo "${ROOTFS_TEMPLATE_VERSION}" > "${ASSET_DIR}/rootfs-template.version"
  echo "  rootfs template rebuilt (version ${ROOTFS_TEMPLATE_VERSION})"
else
  echo "  rootfs template cached (version $(cat "${ASSET_DIR}/rootfs-template.version" 2>/dev/null || echo '?'))"
fi

# ---------------------------------------------------------------------------
info "Building the egress image"
# build context is the REPO ROOT (the Dockerfile COPYs components/egress/*
# paths); the Dockerfile itself lives under components/egress.
docker build -q -f "${REPO_ROOT}/components/egress/Dockerfile" -t "${EGRESS_IMAGE}" "${REPO_ROOT}" >/dev/null

# ---------------------------------------------------------------------------
info "Starting the fastlet container (Pod netns on a docker bridge)"
docker network create "${BRIDGE_NET}" >/dev/null 2>&1 || true
FASTLET_CT_ID="$(docker run -d --name "${FASTLET_CT}" \
  --network "${BRIDGE_NET}" --privileged \
  --device /dev/kvm \
  -v /var/run/netns:/var/run/netns:rslave \
  -v "${RULES_DIR}:/var/egress/rules" \
  -v "${CA_ROOT}:/opt/opensandbox" \
  -v "${WORK_DIR}:${WORK_DIR}" \
  -v "${SCRIPT_DIR}:${SCRIPT_DIR}" \
  ubuntu:24.04 sleep infinity)"
docker exec "${FASTLET_CT}" bash -c 'apt-get update -qq && apt-get install -y -qq iproute2 iputils-ping net-tools nftables curl >/dev/null'
FASTLET_PID="$(docker inspect -f '{{.State.Pid}}' "${FASTLET_CT}")"

# ---------------------------------------------------------------------------
info "Simulating fastlet bringing up the external netns + helpers"
ip netns add "${EXT_CT_NETNS}"
ip link add veth-fcext type veth peer name veth-fcext-p
ip link set veth-fcext netns "${EXT_CT_NETNS}"
ip link set veth-fcext-p netns "${FASTLET_PID}"
ip -n "${EXT_CT_NETNS}" link set lo up
ip -n "${EXT_CT_NETNS}" link set veth-fcext up
pod_ns ip link set veth-fcext-p up
ip -n "${EXT_CT_NETNS}" addr add "${EXT_IP}/24" dev veth-fcext
# alias for the always-deny IP (deny.always): the guest deny-probe can then
# distinguish "dropped by the Pod INPUT chain" from "no listener"
ip -n "${EXT_CT_NETNS}" addr add 10.99.0.9/32 dev veth-fcext
pod_ns ip addr add "${EXT_GW_IP}/24" dev veth-fcext-p
ip -n "${EXT_CT_NETNS}" route add default via "${EXT_GW_IP}"

# DNS upstream + ext echo both run INSIDE the Pod netns (their loopback and
# routes are Pod-private): the egress container shares them via
# --network container:fastlet.
SSL_DIR="${WORK_DIR}/ssl"
mkdir -p "${SSL_DIR}"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "${SSL_DIR}/key.pem" \
  -out "${SSL_DIR}/cert.pem" -days 1 -subj "/CN=ext.test" >/dev/null 2>&1
# DNS upstream: a plain host process on 0.0.0.0:5300, reached from the Pod
# netns through the docker bridge gateway (the Pod loopback is Pod-private,
# and nsenter-into-container-netns launches proved unreliable here).
BRIDGE_GW="$(docker network inspect "${BRIDGE_NET}" -f '{{(index .IPAM.Config 0).Gateway}}')"
[ -n "${BRIDGE_GW}" ] || fail "could not determine the bridge gateway"
setsid env EXT_ANSWER="ext.test=${EXT_IP},alt.test=${EXT_IP}" \
  python3 "${SCRIPT_DIR}/fleet_upstream.py" dns >"${WORK_DIR}/dns.log" 2>&1 &
DNS_PID=$!
sleep 1
if [ -s "${WORK_DIR}/dns.log" ]; then
  echo "--- dns upstream startup log ---"; cat "${WORK_DIR}/dns.log"
fi
# ext echo lives in the ext netns; launched from the host (setsid so the
# netns-exec parent exiting cannot reap it), with a log for startup errors.
setsid ip netns exec "${EXT_CT_NETNS}" env \
  EXT_SSL_CERT="${SSL_DIR}/cert.pem" EXT_SSL_KEY="${SSL_DIR}/key.pem" \
  python3 "${SCRIPT_DIR}/fleet_upstream.py" ext >"${WORK_DIR}/ext.log" 2>&1 &
EXT_PID=$!
sleep 1
if [ -s "${WORK_DIR}/ext.log" ]; then
  echo "--- ext server startup log ---"; cat "${WORK_DIR}/ext.log"
fi
wait_for 5 "ext http" ip netns exec "${EXT_CT_NETNS}" curl -s -m 2 -o /dev/null "http://127.0.0.1:8080/"
wait_for 5 "ext https" ip netns exec "${EXT_CT_NETNS}" curl -sk -m 2 -o /dev/null "https://127.0.0.1/"
# query from inside the Pod netns through the bridge gateway (curl from the
# fastlet container is used for the Pod loopback services below)
wait_for 10 "dns upstream" pod_ns python3 "${SCRIPT_DIR}/fleet_upstream.py" query "${BRIDGE_GW}" ext.test "${DNS_UPSTREAM_PORT}"

# ---------------------------------------------------------------------------
info "Starting the egress container (shares the fastlet Pod netns)"
T_EGRESS_START="$(ts)"
# mitm home: bind a host dir so the mitmproxy user (uid 10042) always has a
# writable confdir for CA generation, regardless of image-layer permission
# quirks on the host storage driver. Mirror the smoke test's full prep
# (config.yaml included): the fleet smoke CI job generates the CA reliably
# with exactly this confdir content.
MITM_HOME="${WORK_DIR}/mitm-home"
mkdir -p "${MITM_HOME}/.mitmproxy"
cp -f "${SCRIPT_DIR}/../mitmproxy/config.yaml" "${MITM_HOME}/.mitmproxy/config.yaml"
chown -R 10042:10042 "${MITM_HOME}"
docker exec "${FASTLET_CT}" sh -c 'echo 1 > /proc/sys/net/ipv4/ip_forward'
# egress needs nft inside the Pod netns: install it in the fastlet container
pod_exec bash -c 'command -v nft >/dev/null || (apt-get install -y -qq nftables >/dev/null)'
printf '%s\n' '10.99.0.9' > "${RULES_DIR}/deny.always"

EGRESS_CT_ID="$(docker run -d --name "${EGRESS_CT}" \
  --network "container:${FASTLET_CT}" --privileged \
  -v "${RULES_DIR}:/var/egress/rules" \
  -v "${CA_ROOT}:/opt/opensandbox" \
  -v "${MITM_HOME}:/var/lib/mitmproxy" \
  -e OPENSANDBOX_EGRESS_PROFILE=fleet \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM="${BRIDGE_GW}:${DNS_UPSTREAM_PORT}" \
  -e OPENSANDBOX_EGRESS_DNS_UPSTREAM_PROBE=allow.test \
  -e OPENSANDBOX_EGRESS_HTTP_ADDR="127.0.0.1:${POLICY_PORT}" \
  -e OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true \
  -e OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true \
  "${EGRESS_IMAGE}")"

# wait_for_egress: healthz with a container-log dump on failure (the egress
# binary runs under a supervisor, so the reason is in `docker logs`).
wait_for_egress() {
  local elapsed=0
  # the mitmdump addon must be readable by the mitmproxy user (image
  # permission fix landed in the Dockerfile; this covers pre-fix images)
  docker exec "${EGRESS_CT}" chmod a+r /var/egress/mitmscripts/system.py 2>/dev/null || true
  while [ "${elapsed}" -lt 40 ]; do
    if pod_exec curl -sf "http://127.0.0.1:${POLICY_PORT}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "--- egress container logs (tail) ---"
  docker logs "${EGRESS_CT}" 2>&1 | tail -40
  echo "--- mitm confdir + addon permissions ---"
  docker exec "${EGRESS_CT}" ls -la /var/lib/mitmproxy/.mitmproxy/ 2>&1 || true
  docker exec "${EGRESS_CT}" stat -c '%a %U %G %n' /var/egress/mitmscripts/system.py 2>&1 || true
  fail "egress did not become ready (healthz)"
}
wait_for_egress

# ---------------------------------------------------------------------------
# Two sandbox VMs with DIFFERENT policies and credential vaults — the
# deployment's real terminal state. vm1 binds ext.test -> fc-secret,
# vm2 binds alt.test -> fc-secret-2; each VM probes BOTH hosts to prove
# cross-subject isolation on the real data plane.
# ---------------------------------------------------------------------------
VM_NETNS1="osb-fc-vm1"; VM_IP1="10.30.0.5"; VM_TARGET1="ext.test"; VM_SECRET1="fc-secret"
VM_NETNS2="osb-fc-vm2"; VM_IP2="10.30.0.6"; VM_TARGET2="alt.test"; VM_SECRET2="fc-secret-2"
VM_GW="10.30.0.1"
VM_ROOTFS1="${WORK_DIR}/vm1-rootfs.ext4"; VM_ROOTFS_MNT1="${WORK_DIR}/vm1-rootfs-mnt"
VM_ROOTFS2="${WORK_DIR}/vm2-rootfs.ext4"; VM_ROOTFS_MNT2="${WORK_DIR}/vm2-rootfs-mnt"
VM_SERIAL1="${WORK_DIR}/fc1-process.log"; VM_API_SOCK1="${WORK_DIR}/fc1.sock"
VM_SERIAL2="${WORK_DIR}/fc2-process.log"; VM_API_SOCK2="${WORK_DIR}/fc2.sock"

# create_vm_netns <netns> <veth> <tap> <ip> <first>: netns + veth pair + a
# bridge joining the firecracker tap and the veth. ONLY the first VM's pod
# veth carries the shared gateway IP; later VMs get a /32 route back to their
# pod veth instead (two interfaces with the same 10.30.0.1/24 would make the
# Pod netns route return traffic out of the wrong veth — the smoke test's
# multi-sandbox pattern).
create_vm_netns() {
  local netns="$1" veth="$2" tap="$3" ip="$4" first="$5"
  ip netns add "${netns}"
  ip link add "${veth}" type veth peer name "${veth}-p"
  ip link set "${veth}" netns "${netns}"
  ip link set "${veth}-p" netns "${FASTLET_PID}"
  ip -n "${netns}" link set lo up
  ip -n "${netns}" link set "${veth}" up
  pod_ns ip link set "${veth}-p" up
  local gw
  if [ "${first}" = "1" ]; then
    gw="10.30.0.1"
    pod_ns ip addr add "${gw}/24" dev "${veth}-p"
  else
    gw="10.30.0.2"
    # /32 alias: no duplicate subnet route; the VM's own /32 return route
    # keeps return traffic on this veth
    pod_ns ip addr add "${gw}/32" dev "${veth}-p"
    pod_ns ip route add "${ip}/32" dev "${veth}-p"
  fi
  ip -n "${netns}" link add br0 type bridge
  ip -n "${netns}" link set "${veth}" master br0
  ip -n "${netns}" link set br0 up
  ip netns exec "${netns}" ip tuntap add dev "${tap}" mode tap
  ip netns exec "${netns}" ip link set "${tap}" master br0
  ip netns exec "${netns}" ip link set "${tap}" up
}

# bind_subject <uid> <ip> <veth> <gw> <policy>: SET_BINDING with the policy
# (deny-first registration, policy pending until data-plane-ready).
bind_subject() {
  local uid="$1" ip="$2" veth="$3" gw="$4" policy="$5"
  local body encoded
  encoded="$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1]))' "${policy}")"
  body="$(cat <<EOF
{"apiVersion":"sandbox.fast.io/actions/v1","operation":"SET_BINDING","invocationId":"e2e-${uid}-1","sandbox":{"uid":"${uid}"},"revision":{"specGeneration":1,"runtimeInstanceId":"rt-${uid}","attachmentId":"att-${uid}","routeGeneration":1},"attachment":{"network":{"ip":"${ip}","gateway":"${gw}","privateCidr":"10.30.0.0/24","hostVeth":"${veth}-p"}},"binding":{"input":${encoded}}}
EOF
)"
  pod_exec curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/_fastlet/v1/actions" -d "${body}" >/dev/null
}

# activate_subject <uid>: sandbox.data-plane-ready (deny-first -> active).
activate_subject() {
  local uid="$1" body
  body="$(cat <<EOF
{"apiVersion":"sandbox.fast.io/actions/v1","operation":"LIFECYCLE_HOOK","invocationId":"e2e-${uid}-dpr","sandbox":{"uid":"${uid}"},"revision":{"specGeneration":1,"runtimeInstanceId":"rt-${uid}","attachmentId":"att-${uid}","routeGeneration":1},"hook":{"name":"sandbox.data-plane-ready","sequence":1}}
EOF
)"
  pod_exec curl -sSf -XPOST "http://127.0.0.1:${POLICY_PORT}/_fastlet/v1/actions" -d "${body}" >/dev/null
}

# push_vault <uid> <target> <secret>: per-subject credential vault (the
# policy already arrived with SET_BINDING).
push_vault() {
  local uid="$1" target="$2" secret="$3"
  # vm2 must not be able to bind vm1's host (per-subject policy isolation):
  # tested BEFORE the real vault exists, so the 400 comes from the policy
  # validation, not from a duplicate-vault ErrExists.
  if [ "${uid}" = "vm2" ]; then
    local code
    code="$(pod_exec curl -s -o /dev/null -w '%{http_code}' -H "X-Fast-Sandbox-Uid: vm2" -XPOST \
      "http://127.0.0.1:${POLICY_PORT}/credential-vault" \
      -d '{"credentials":[{"name":"x","source":{"type":"inline","value":"leak"}}],"bindings":[{"name":"b","match":{"schemes":["http","https"],"hosts":["ext.test"]},"auth":{"type":"apiKey","name":"X-Api-Key","credential":"x"}}]}')"
    [ "${code}" = "400" ] || fail "vm2 vault must reject a binding outside its policy (ext.test), got ${code}"
  fi
  pod_exec curl -sSf -H "X-Fast-Sandbox-Uid: ${uid}" -XPOST \
    "http://127.0.0.1:${POLICY_PORT}/credential-vault" \
    -d "{\"credentials\":[{\"name\":\"k\",\"source\":{\"type\":\"inline\",\"value\":\"${secret}\"}}],\"bindings\":[{\"name\":\"b\",\"match\":{\"schemes\":[\"http\",\"https\"],\"hosts\":[\"${target}\"]},\"auth\":{\"type\":\"apiKey\",\"name\":\"X-Api-Key\",\"credential\":\"k\"}}]}" >/dev/null
}

# deliver_ca <rootfs> <mnt> : fastlet loop-mount write of the egress CA
deliver_ca() {
  local rootfs="$1" mnt="$2"
  mkdir -p "${mnt}"
  mount -o loop "${rootfs}" "${mnt}"
  mkdir -p "${mnt}/opt/opensandbox/mitm-ca"
  cp -f "${CA_ROOT}/mitm-ca/mitmproxy-ca-cert.pem" \
    "${mnt}/opt/opensandbox/mitm-ca/mitmproxy-ca-cert.pem"
  umount "${mnt}"
}

# boot_vm <uid> <netns> <rootfs> <tap> <target> <api-sock> <serial> -> sets
# FC_PID, T3_INSTANCE_START
boot_vm() {
  local uid="$1" netns="$2" rootfs="$3" tap="$4" target="$5" api_sock="$6" serial="$7"
  local ip
  local mac
  local gw
  if [ "${uid}" = "vm1" ]; then mac="06:00:AC:10:30:05"; ip="10.30.0.5"; gw="10.30.0.1"; else mac="06:00:AC:10:30:06"; ip="10.30.0.6"; gw="10.30.0.2"; fi
  local args="console=ttyS0 reboot=k panic=1 init=/${VM_BOOTSTRAP} test_target=${target} vm_ip=${ip} gw_ip=${gw}"
  cat > "${WORK_DIR}/${uid}.json" <<EOF
{
  "boot-source": {"kernel_image_path": "${KERNEL_PATH}", "boot_args": "${args}"},
  "drives": [{"drive_id": "rootfs", "path_on_host": "${rootfs}", "is_root_device": true, "is_read_only": false}],
  "machine-config": {"vcpu_count": 1, "mem_size_mib": 256},
  "network-interfaces": [{"iface_id": "net1", "host_dev_name": "${tap}", "guest_mac": "${mac}"}],
  "logger": {"log_path": "${WORK_DIR}/${uid}.log", "level": "Debug", "show_level": true, "show_log_origin": true}
}
EOF
  rm -f "${api_sock}"
  # --config-file starts the microVM automatically; firecracker runs in the
  # VM netns so its tap attachment matches the bridge prepared above. The
  # guest console (ttyS0) is forwarded to firecracker's stdout (serial).
  nsenter --net="/var/run/netns/${netns}" "${FC_BIN}" \
    --api-sock "${api_sock}" \
    --config-file "${WORK_DIR}/${uid}.json" >"${serial}" 2>&1 &
  FC_PID=$!
  if [ "${uid}" = "vm1" ]; then FC_PID1=$!; else FC_PID2=$!; fi
  T3_INSTANCE_START="$(ts)"
}

# wait_fc_socket <api_sock> <serial> <label>
wait_fc_socket() {
  local api_sock="$1" serial="$2" label="$3"
  local elapsed=0
  while [ "${elapsed}" -lt 20 ]; do
    if test -S "${api_sock}"; then return 0; fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  echo "--- ${label} firecracker process log ---"
  cat "${serial}" 2>&1 | head -30
  fail "${label} firecracker did not open its API socket"
}

# ---------------------------------------------------------------------------
info "Simulating the VM netns pairs (firecracker taps bridged into the Pod netns)"
create_vm_netns "${VM_NETNS1}" veth-fc1 tap-fc1 "${VM_IP1}" 1
create_vm_netns "${VM_NETNS2}" veth-fc2 tap-fc2 "${VM_IP2}" 0

info "Binding the sandbox subjects (SET_BINDING -> deny-first)"
T_SLOT_WRITE="$(ts)"
bind_subject vm1 "${VM_IP1}" veth-fc1 10.30.0.1 '{"defaultAction":"deny","egress":[{"action":"allow","target":"'"${VM_TARGET1}"'"},{"action":"allow","target":"10.99.0.2"}]}'
bind_subject vm2 "${VM_IP2}" veth-fc2 10.30.0.2 '{"defaultAction":"deny","egress":[{"action":"allow","target":"'"${VM_TARGET2}"'"},{"action":"allow","target":"10.99.0.2"}]}'
wait_for 30 "CA exported by egress" test -s "${CA_ROOT}/mitm-ca/mitmproxy-ca-cert.pem"
T0_CA_EXPORT="$(ts)"
for ip in "${VM_IP1}" "${VM_IP2}"; do
  local_elapsed=0
  while [ "${local_elapsed}" -lt 30 ]; do
    if nsenter --net="/proc/${FASTLET_PID}/ns/net" nft list table inet opensandbox_gateway_mitm 2>/dev/null | grep -q "ip saddr ${ip} "; then
      break
    fi
    sleep 1
    local_elapsed=$((local_elapsed + 1))
  done
  [ "${local_elapsed}" -lt 30 ] || {
    echo "--- egress container logs (tail) ---"
    docker logs "${EGRESS_CT}" 2>&1 | tail -30
    fail "subject ${ip} did not register (DNAT missing)"
  }
done
T_DNAT_READY="$(ts)"
pass "egress up; CA exported at ${T0_CA_EXPORT}; both subjects registered deny-first (DNAT ready at ${T_DNAT_READY})"

# ---------------------------------------------------------------------------
info "Activating policies (data-plane-ready) + pushing credential vaults"
activate_subject vm1
activate_subject vm2
push_vault vm1 "${VM_TARGET1}" "${VM_SECRET1}"
push_vault vm2 "${VM_TARGET2}" "${VM_SECRET2}"
pass "policy active + vault pushed (vm1=${VM_TARGET1}/${VM_SECRET1}, vm2=${VM_TARGET2}/${VM_SECRET2})"

# ---------------------------------------------------------------------------
info "Preparing the VM rootfs images (template copy + CA delivery per VM)"
cp -f "${VM_ROOTFS}" "${VM_ROOTFS1}"
cp -f "${VM_ROOTFS}" "${VM_ROOTFS2}"
T1_DELIVERY_START="$(ts)"
deliver_ca "${VM_ROOTFS1}" "${VM_ROOTFS_MNT1}"
deliver_ca "${VM_ROOTFS2}" "${VM_ROOTFS_MNT2}"
T2_DELIVERY_DONE="$(ts)"
pass "CA delivered into both VM rootfs images (+$(awk "BEGIN{printf \"%.1f\", ${T2_DELIVERY_DONE}-${T1_DELIVERY_START}}")s)"

# ---------------------------------------------------------------------------
info "Booting both firecracker microVMs (in their netns)"
FC_BIN="${ASSET_DIR}/firecracker"
boot_vm vm1 "${VM_NETNS1}" "${VM_ROOTFS1}" tap-fc1 "${VM_TARGET1}" "${VM_API_SOCK1}" "${VM_SERIAL1}"
T3_VM1="$(ts)"
boot_vm vm2 "${VM_NETNS2}" "${VM_ROOTFS2}" tap-fc2 "${VM_TARGET2}" "${VM_API_SOCK2}" "${VM_SERIAL2}"
T3_VM2="$(ts)"
wait_fc_socket "${VM_API_SOCK1}" "${VM_SERIAL1}" "vm1"
wait_fc_socket "${VM_API_SOCK2}" "${VM_SERIAL2}" "vm2"

# ---------------------------------------------------------------------------
info "Waiting for both VM bootstraps (system trust CA install + real TLS curls)"
watch_until 90 "vm1 bootstrap" "bootstrap start" "${VM_SERIAL1}"
T4_BOOTSTRAP="$(ts)"
watch_until 90 "vm1 CA installed" "CA installed" "${VM_SERIAL1}"
T5_CA_HOST="$(ts)"
watch_until 90 "vm1 curl result" "X-Api-Key" "${VM_SERIAL1}"
T6_CURL_DONE="$(ts)"
vm2_elapsed=0
while [ "${vm2_elapsed}" -lt 90 ]; do
  if grep -q "X-Api-Key" "${VM_SERIAL2}" 2>/dev/null; then break; fi
  sleep 0.2
  vm2_elapsed=$((vm2_elapsed + 1))
done
if [ "${vm2_elapsed}" -ge 90 ]; then
  echo "--- vm2 console (tail) ---"
  tail -40 "${VM_SERIAL2}" 2>&1
  echo "--- vm2 firecracker process state ---"
  ps -p "${FC_PID2:-}" -o pid,stat,etime,cmd 2>&1 || true
  echo "--- vm2 guest result files (loop-mount read) ---"
  mkdir -p "${VM_ROOTFS_MNT2}"
  if mount -o loop "${VM_ROOTFS2}" "${VM_ROOTFS_MNT2}" 2>/dev/null; then
    echo "--- /opt/result.txt ---"; cat "${VM_ROOTFS_MNT2}/opt/result.txt" 2>&1 || true
    echo "--- /opt/result-cross.txt ---"; cat "${VM_ROOTFS_MNT2}/opt/result-cross.txt" 2>&1 || true
    umount "${VM_ROOTFS_MNT2}" 2>/dev/null || true
  else
    echo "loop mount failed (rootfs busy or still in use)"
  fi
  echo "--- Pod netns conntrack (vm2 sources) ---"
  nsenter --net="/proc/${FASTLET_PID}/ns/net" conntrack -L 2>/dev/null | grep "10.30.0.6" | head -10 || true
  echo "--- vm2 netns bridge state ---"
  ip netns exec "${VM_NETNS2}" bridge link 2>&1 | head -8 || true
  echo "--- vm2 netns L3 path test (bridge IP + full TLS curl through the Pod netns) ---"
  ip netns exec "${VM_NETNS2}" ip addr add 10.30.0.100/24 dev br0 2>/dev/null || true
  ip netns exec "${VM_NETNS2}" ip link set br0 up
  ip netns exec "${VM_NETNS2}" ping -c 1 -W 2 10.99.0.2 2>&1 || true
  ip netns exec "${VM_NETNS2}" curl -sk -m 5 --resolve ext.test:443:10.99.0.2 https://ext.test/ 2>&1 | head -8 || true
  echo "--- fc2 firecracker log (network errors) ---"
  grep -iE "tap|net|error|fail" "${WORK_DIR}/vm2.log" 2>&1 | tail -15 || true
  echo "--- conntrack tool available? ---"
  command -v conntrack || echo "conntrack-tools NOT installed (conntrack dump above is unreliable)"
  echo "--- veth-fc2-p state in Pod netns ---"
  pod_ns ip -br link show veth-fc2-p 2>&1 || true
  echo "--- Pod netns routes ---"
  pod_ns ip route 2>&1 | head -8 || true
  fail "vm2 curl result timed out"
fi
T6_VM2_DONE="$(ts)"
# guest-clock timestamps (aligned with the host clock at boot via kvm-clock)
T4_KERNEL_TS="$(grep -oP '^\[\K[0-9.]+(?=\] bootstrap start)' "${VM_SERIAL1}" | head -1 || true)"
T5_CA_TS="$(grep -oP '^\[\K[0-9.]+(?=\] CA installed)' "${VM_SERIAL1}" | head -1 || true)"
T6_CA_CURL_TS="$(grep -oP '^\[\K[0-9.]+(?=\] curl done)' "${VM_SERIAL1}" | head -1 || true)"

echo "=== vm1 console (tail) ==="
grep -E "bootstrap|curl done|cross done" "${VM_SERIAL1}" | tail -8
echo "--- vm1 result ---"
grep -A5 "curl done" "${VM_SERIAL1}" | grep -E "X-Api-Key|client=" | head -4
echo "=== vm2 console (tail) ==="
grep -E "bootstrap|curl done|cross done" "${VM_SERIAL2}" | tail -8
echo "--- vm2 result ---"
grep -A5 "curl done" "${VM_SERIAL2}" | grep -E "X-Api-Key|client=" | head -4

echo "=== credential checks ==="
# own-host injection
grep -A5 "curl done" "${VM_SERIAL1}" | grep -qi "X-Api-Key: fc-secret" \
  || fail "vm1 credential injection missing"
grep -A5 "curl done" "${VM_SERIAL1}" | grep -q "client=10.99.0.1" \
  || fail "vm1 request did not traverse the shared mitm"
grep -A5 "curl done" "${VM_SERIAL2}" | grep -qi "X-Api-Key: fc-secret-2" \
  || fail "vm2 credential injection missing"
grep -A5 "curl done" "${VM_SERIAL2}" | grep -q "client=10.99.0.1" \
  || fail "vm2 request did not traverse the shared mitm"
# cross-subject isolation: neither VM may receive the OTHER's credential.
# The cross host is outside the VM's own policy, so the request is typically
# denied at DNS resolution already (the strongest isolation — the flow never
# leaves the sandbox); it may also be proxied without a matching binding.
# Either way: NO X-Api-Key may appear.
grep -A5 "cross done" "${VM_SERIAL1}" | grep -qi "X-Api-Key" \
  && fail "vm1 must NOT receive a credential for alt.test (vm2's host)"
grep -A5 "cross done" "${VM_SERIAL2}" | grep -qi "X-Api-Key" \
  && fail "vm2 must NOT receive a credential for ext.test (vm1's host)"
# deny-path on the real data plane: the always-deny target (10.99.0.9, which
# the ext netns DOES serve) must be dropped by the Pod INPUT chain — the
# authoritative layer for MITM traffic in the firecracker form (A2 cannot
# see guest traffic at all).
grep -A3 "deny done" "${VM_SERIAL1}" | grep -q "http_code=200" \
  && fail "vm1 deny-probe (10.99.0.9) must be dropped by the Pod INPUT chain"
grep -A3 "deny done" "${VM_SERIAL2}" | grep -q "http_code=200" \
  && fail "vm2 deny-probe (10.99.0.9) must be dropped by the Pod INPUT chain"
pass "deny-path enforced at the Pod INPUT chain in both real VMs"

echo
echo "=== phase timing (vm1; all wall-clock on the host unless noted) ==="
echo "  egress start -> CA export:       $(awk "BEGIN{printf \"%.3f\", ${T0_CA_EXPORT}-${T_EGRESS_START}}")s"
echo "  SET_BINDING -> subjects DNAT:   $(awk "BEGIN{printf \"%.3f\", ${T_DNAT_READY}-${T_SLOT_WRITE}}")s"
echo "  CA export -> delivery done:      $(awk "BEGIN{printf \"%.3f\", ${T2_DELIVERY_DONE}-${T0_CA_EXPORT}}")s"
echo "  vm1 InstanceStart -> bootstrap:  $(awk "BEGIN{printf \"%.3f\", ${T4_BOOTSTRAP}-${T3_VM1}}")s   (kernel boot)"
echo "  vm1 bootstrap -> CA installed:   $(awk "BEGIN{printf \"%.3f\", ${T5_CA_HOST}-${T4_BOOTSTRAP}}")s   (net cfg + CA install)"
echo "  vm1 CA installed -> curl result: $(awk "BEGIN{printf \"%.3f\", ${T6_CURL_DONE}-${T5_CA_HOST}}")s   (TLS round trip)"
echo "  vm1 InstanceStart -> curl:       $(awk "BEGIN{printf \"%.3f\", ${T6_CURL_DONE}-${T3_VM1}}")s   (VM total)"
echo "  vm2 InstanceStart -> curl:       $(awk "BEGIN{printf \"%.3f\", ${T6_VM2_DONE}-${T3_VM2}}")s   (VM total, parallel)"
echo "  end-to-end (CA export -> vm1 curl): $(awk "BEGIN{printf \"%.3f\", ${T6_CURL_DONE}-${T0_CA_EXPORT}}")s"
if [ -n "${T4_KERNEL_TS}" ] && [ -n "${T5_CA_TS}" ] && [ -n "${T6_CA_CURL_TS}" ]; then
  echo "--- guest-clock deltas (CA cost inside the VM) ---"
  echo "  kernel start -> CA install:     $(awk "BEGIN{printf \"%.3f\", ${T5_CA_TS}-${T4_KERNEL_TS}}")s (guest clock)"
  echo "  CA install -> curl done:        $(awk "BEGIN{printf \"%.3f\", ${T6_CA_CURL_TS}-${T5_CA_TS}}")s (guest clock)"
fi
pass "real firecracker TLS + per-VM credential-vault e2e succeeded (two VMs, isolated policies)"
