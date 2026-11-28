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

# Integration smoke test for OSEP-0031 Phase 2 cgroup v2 QoS isolation.
# Builds a Linux execd binary matching the Docker daemon architecture and runs
# it inside a cgroup v2 container (--cgroupns=private --privileged) to verify:
#   1. Auto-mounting cgroup2 at /sys/fs/cgroup when unmounted (Firecracker path)
#   2. Two-leaf hierarchy (opensandbox-system / opensandbox-workload) and adaptive defaults
#   3. GET /v1/isolated/capabilities reporting hardening.cgroup_qos.state=active
#   4. POST /command atomic placement into 0::/opensandbox-workload (CLONE_INTO_CGROUP)
#   5. /ping responsiveness under 8-worker 1-vCPU contention
#   6. Workload-contained OOM kill leaving execd healthy
#
# Prerequisites: docker (with cgroup v2), go
#
# Usage:
#   bash components/execd/tests/cgroup_qos_smoke.sh
#
# Exit 0 on success, non-zero on failure.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
EXECD_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
SMOKE_IMAGE="${SMOKE_IMAGE:-python:3.12-slim}"
TMP_DIR="$(mktemp -d)"

cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

CGROUP_VER="$(docker info --format '{{.CgroupVersion}}')"
if [[ "${CGROUP_VER}" != "2" ]]; then
  echo "SKIP: Docker daemon is not using cgroup v2 (got version=${CGROUP_VER})"
  exit 0
fi

DOCKER_ARCH="$(docker info --format '{{.Architecture}}')"
case "${DOCKER_ARCH}" in
  x86_64|amd64)  TARGET_ARCH="amd64" ;;
  aarch64|arm64) TARGET_ARCH="arm64" ;;
  *)
    echo "Unsupported Docker architecture: ${DOCKER_ARCH}" >&2
    exit 1
    ;;
esac

echo ">> Building linux/${TARGET_ARCH} execd..."
(cd "${EXECD_DIR}" && GOOS=linux GOARCH="${TARGET_ARCH}" CGO_ENABLED=0 go build -o "${TMP_DIR}/execd" .)

cat >"${TMP_DIR}/verify.sh" <<'EOF'
#!/bin/sh
set -eu

cat >/tmp/isolation.toml <<'TOML'
[qos]
entrypoint = "high"

[qos.cgroup]
enabled = true
TOML

echo "=== [1] Simulate unmounted /sys/fs/cgroup (Firecracker cold boot) ==="
umount /sys/fs/cgroup
if mountpoint -q /sys/fs/cgroup 2>/dev/null; then
  echo "FAIL: /sys/fs/cgroup still mounted" >&2
  exit 1
fi

echo "=== [2] Start execd --init with [qos.cgroup] enabled=true ==="
EXECD_INIT=1 EXECD_ISOLATION_CONFIG=/tmp/isolation.toml \
  /usr/local/bin/execd --init --port 44772 -- /bin/sleep 300 >/tmp/execd.log 2>&1 &
EXECD_PID=$!

cleanup_execd() {
  kill "${EXECD_PID}" 2>/dev/null || true
}
trap cleanup_execd EXIT

ready=0
for _ in $(seq 1 50); do
  if python3 -c 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:44772/ping", timeout=1)' 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.1
done
if [ "${ready}" -ne 1 ]; then
  echo "FAIL: execd did not become ready" >&2
  cat /tmp/execd.log >&2 || true
  exit 1
fi

echo "=== [3] Verify auto-mount and cgroup v2 hierarchy ==="
grep cgroup2 /proc/self/mounts
test "$(cat /proc/${EXECD_PID}/cgroup)" = "0::/opensandbox-system"
test "$(cat /sys/fs/cgroup/opensandbox-system/cpu.weight)" = "1000"
test "$(cat /sys/fs/cgroup/opensandbox-workload/cpu.weight)" = "100"
test "$(cat /sys/fs/cgroup/opensandbox-system/memory.low)" = "16777216"
test "$(cat /sys/fs/cgroup/opensandbox-workload/memory.max)" = "234881024"
test "$(cat /sys/fs/cgroup/opensandbox-workload/pids.max)" = "128"

echo "=== [4] Verify GET /v1/isolated/capabilities ==="
python3 - <<'PY'
import json, urllib.request
data = json.loads(urllib.request.urlopen("http://127.0.0.1:44772/v1/isolated/capabilities", timeout=2).read())
st = data["hardening"]["cgroup_qos"]
print("cgroup_qos:", json.dumps(st))
assert st["state"] == "active", st
assert "auto_mounted=true" in st["message"], st
PY

echo "=== [5] Verify POST /command atomic placement in /opensandbox-workload ==="
python3 - <<'PY'
import json, urllib.request
req = urllib.request.Request(
    "http://127.0.0.1:44772/command",
    data=json.dumps({"command": "cat /proc/self/cgroup"}).encode(),
    headers={"Content-Type": "application/json"},
)
body = urllib.request.urlopen(req, timeout=5).read().decode()
assert "0::/opensandbox-workload" in body, body
print("OK: command placed in 0::/opensandbox-workload")
PY

echo "=== [6] CPU contention check (8 busy workers on 1 vCPU) ==="
python3 - <<'PY'
import json, time, urllib.request
burn_cmd = "for i in 1 2 3 4 5 6 7 8; do python3 -c 'x=0\nwhile True: x+=1' & done; wait"
req = urllib.request.Request(
    "http://127.0.0.1:44772/command",
    data=json.dumps({"command": burn_cmd, "background": True}).encode(),
    headers={"Content-Type": "application/json"},
)
urllib.request.urlopen(req, timeout=5).read()
time.sleep(0.5)
latencies = []
for _ in range(20):
    t0 = time.monotonic()
    urllib.request.urlopen("http://127.0.0.1:44772/ping", timeout=2).read()
    latencies.append((time.monotonic() - t0) * 1000.0)
latencies.sort()
print(f"20x /ping under 8-worker 1-vCPU burn: min={latencies[0]:.2f}ms p50={latencies[10]:.2f}ms p95={latencies[18]:.2f}ms max={latencies[-1]:.2f}ms")
PY
for p in $(cat /sys/fs/cgroup/opensandbox-workload/cgroup.procs); do
  kill -9 "${p}" 2>/dev/null || true
done
sleep 0.2

echo "=== [7] Workload OOM isolation check ==="
python3 - <<'PY'
import json, urllib.request
oom_cmd = "python3 -c 'a = bytearray(240 * 1024 * 1024); a[0::4096] = b\"x\" * (len(a)//4096)'"
req = urllib.request.Request(
    "http://127.0.0.1:44772/command",
    data=json.dumps({"command": oom_cmd}).encode(),
    headers={"Content-Type": "application/json"},
)
body = urllib.request.urlopen(req, timeout=15).read().decode()
assert "signal: killed" in body, body
assert urllib.request.urlopen("http://127.0.0.1:44772/ping", timeout=2).status == 200
PY

grep -q '^oom_kill [1-9]' /sys/fs/cgroup/opensandbox-workload/memory.events
grep -q '^oom_kill 0$' /sys/fs/cgroup/opensandbox-system/memory.events
echo "=== ALL CGROUP V2 QOS SMOKE CHECKS PASSED ==="
EOF
chmod +x "${TMP_DIR}/verify.sh"

echo ">> Running cgroup v2 smoke container (${SMOKE_IMAGE})..."
docker run --rm \
  --cgroupns=private \
  --privileged \
  --cpus=1 \
  --memory=256m \
  --memory-swap=256m \
  --pids-limit=256 \
  -v "${TMP_DIR}/execd:/usr/local/bin/execd:ro" \
  -v "${TMP_DIR}/verify.sh:/verify.sh:ro" \
  "${SMOKE_IMAGE}" /verify.sh
