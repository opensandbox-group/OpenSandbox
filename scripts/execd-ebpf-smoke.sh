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

# execd-ebpf bare-container smoke test.
#
# Runs the execd-ebpf observation variant in a bare container on THIS
# machine's kernel — no server, no SDK — generates exec / connect /
# privilege events via `docker exec` (same cgroup, so the sandbox-cgroup
# filter passes), and asserts they land in the rotating JSONL audit file.
#
# This is the empirical validation the OSEP calls out as missing:
#   - the BPF hooks (sched_process_exec, inet_sock_set_state,
#     commit_creds) attach on the actual host kernel, incl. the 5.10-5.15
#     inline-filename fallback in audit.bpf.c
#   - the cgroup filter scopes events to the sandbox cgroup
#   - ringbuf decode -> JSONL envelope (ts/event/sandbox_id/pid/comm)
#
# Required host preconditions (checked below):
#   - kernel >= 5.8 with BTF:            /sys/kernel/btf/vmlinux
#   - cgroup v2:                         0:: in /proc/self/cgroup
#   - tracefs:                           /sys/kernel/tracing (or debugfs)
#   - Docker available on the host
#
# Container privileges (the minimal set; --privileged also works):
#   --cap-add=CAP_BPF --cap-add=CAP_PERFMON --cap-add=CAP_SYSLOG
#   -v /sys/kernel/tracing:/sys/kernel/tracing:ro
#
# Usage: bash scripts/execd-ebpf-smoke.sh [--image opensandbox/execd:local] [--build]

set -euo pipefail

IMAGE="opensandbox/execd:local"
BUILD=0
while [ $# -gt 0 ]; do
  case "$1" in
    --image) IMAGE="$2"; shift 2 ;;
    --build) BUILD=1; shift ;;
    *) echo "usage: $0 [--image IMG] [--build]" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR="$(mktemp -d /tmp/execd-ebpf-smoke.XXXXXX)"
CTR_NAME="execd-ebpf-smoke-$RANDOM"
AUDIT_FILE="/var/log/opensandbox/ebpf-audit.jsonl"
cleanup() {
  docker rm -f "${CTR_NAME}" >/dev/null 2>&1 || true
  rm -rf "${WORKDIR}"
}
trap cleanup EXIT

echo "== host preconditions =="
[ -e /sys/kernel/btf/vmlinux ] || {
  echo "FAIL: no /sys/kernel/btf/vmlinux — kernel has no BTF; eBPF CO-RE cannot load" >&2
  exit 1
}
grep -q '^0::' /proc/self/cgroup || {
  echo "FAIL: no cgroup v2 hierarchy in /proc/self/cgroup" >&2
  exit 1
}
TRACEFS=""
for p in /sys/kernel/tracing /sys/kernel/debug/tracing; do
  if [ -e "$p/events" ]; then TRACEFS="$p"; break; fi
done
[ -n "$TRACEFS" ] || {
  echo "FAIL: no tracefs events dir (/sys/kernel/tracing or debugfs)" >&2
  exit 1
}
echo "OK: BTF present, cgroup v2, tracefs at $TRACEFS"

if [ "${BUILD}" = "1" ]; then
  docker build -t "${IMAGE}" -f components/execd/Dockerfile "${REPO_ROOT}"
fi

# Minimal isolation TOML: only the eBPF section, audit file in a host dir.
mkdir -p "${WORKDIR}/audit"
# Pre-create the audit file writable by the container (root) and readable by
# the host runner (non-root): lumberjack appends in place, so the file stays
# world-readable and the runner can assert on it after the container stops.
: > "${WORKDIR}/audit/ebpf-audit.jsonl"
chmod 0666 "${WORKDIR}/audit/ebpf-audit.jsonl"
cat > "${WORKDIR}/ebpf.toml" <<EOF
[ebpf]
enabled = true
observe = ["exec", "connect", "privilege"]
audit_file = "${AUDIT_FILE}"
EOF

echo "== starting execd-ebpf container ($CTR_NAME) =="
docker run -d --rm --name "${CTR_NAME}" \
  --cap-add=CAP_BPF \
  --cap-add=CAP_PERFMON \
  --cap-add=CAP_SYSLOG \
  -v "${TRACEFS}:${TRACEFS}:ro" \
  -v "${WORKDIR}/ebpf.toml:/etc/opensandbox/ebpf-smoke.toml:ro" \
  -v "${WORKDIR}/audit:/var/log/opensandbox" \
  -e OPENSANDBOX_ID="smoke-sbx-0001" \
  -e EXECD_ISOLATION_CONFIG="/etc/opensandbox/ebpf-smoke.toml" \
  --entrypoint /execd-ebpf \
  "${IMAGE}" \
  --isolation-config /etc/opensandbox/ebpf-smoke.toml >/dev/null

echo "== waiting for the capabilities endpoint =="
for _ in $(seq 1 30); do
  if docker exec "${CTR_NAME}" wget -qO- http://127.0.0.1:44772/v1/isolated/capabilities > "${WORKDIR}/caps.json" 2>/dev/null; then
    break
  fi
  sleep 1
done
[ -s "${WORKDIR}/caps.json" ] || {
  echo "FAIL: capabilities endpoint never came up" >&2
  docker logs "${CTR_NAME}" 2>&1 | tail -30 >&2 || true
  exit 1
}

echo "== hardening/ebpf report =="
set +e
python3 - "${WORKDIR}/caps.json" <<'PY'
import json, sys
caps = json.load(open(sys.argv[1]))
ebpf = (caps.get("hardening") or {}).get("ebpf") or {}
state = ebpf.get("state", "missing")
message = ebpf.get("message", "")
print(f"ebpf state: {state}")
if message:
    print(f"ebpf message: {message}")
# "unsupported" (no BTF/caps) still boots execd, but no hooks attach —
# nothing will be written. "degraded" means some hooks are down but the
# rest keep auditing; "active" means full exec/connect/privilege coverage.
if state == "unsupported":
    print("FAIL: ebpf state = unsupported (hooks did not attach)", file=sys.stderr)
    sys.exit(1)
if state == "degraded" and "hooks not active" in message:
    # Partial hook degrade (e.g. commit_creds kprobe failing on a given
    # kernel): keep the smoke running — exec+connect must still flow and
    # be asserted below — but flag the container-log dump.
    sys.exit(2)
if state != "active":
    print(f"FAIL: unexpected ebpf state = {state}", file=sys.stderr)
    sys.exit(1)
PY
report_rc=$?
set -e
if [ "${report_rc}" -eq 1 ]; then
  exit 1
fi
if [ "${report_rc}" -eq 2 ]; then
  echo "NOTE: hooks degraded — dumping container logs for the load/attach error"
  docker logs "${CTR_NAME}" 2>&1 | tail -60 || true
fi

echo "== generating events inside the sandbox cgroup =="
docker exec "${CTR_NAME}" sh -c '/bin/sleep 0.05; echo exec-event-ok >/dev/null' >/dev/null
docker exec "${CTR_NAME}" wget -qO- http://example.com/ >/dev/null 2>&1 || true
# Privilege event: busybox su setuids -> commit_creds fires.
docker exec "${CTR_NAME}" su nobody -s /bin/sh -c 'true' >/dev/null 2>&1 || true

# Give the ringbuf consumer a moment to drain.
sleep 2

echo "== audit file =="
[ -s "${WORKDIR}/audit/ebpf-audit.jsonl" ] || {
  echo "FAIL: audit file empty/missing — hooks attached but no events decoded" >&2
  docker logs "${CTR_NAME}" 2>&1 | tail -30 >&2 || true
  exit 1
}
wc -l "${WORKDIR}/audit/ebpf-audit.jsonl"
python3 - "${WORKDIR}/audit/ebpf-audit.jsonl" <<'PY' || exit 1
import json, sys
kinds, ok = {"exec": 0, "connect": 0, "privilege": 0}, True
for line in open(sys.argv[1]):
    ev = json.loads(line)
    kinds[ev.get("event", "?")] = kinds.get(ev.get("event", "?"), 0) + 1
    assert ev.get("sandbox_id") == "smoke-sbx-0001", ev
    assert "pid" in ev and "comm" in ev and "ts" in ev, ev
print("event counts:", kinds)
for want in ("exec", "connect"):
    if kinds[want] == 0:
        print(f"FAIL: no {want} events", file=sys.stderr)
        ok = False
if kinds["privilege"] == 0:
    # `su` (or any setuid transition) reliably fires commit_creds; a zero
    # count means the privilege hook attached but events are broken, which
    # the smoke exists to catch — fail, not warn.
    print("FAIL: no privilege events (commit_creds hook not producing)", file=sys.stderr)
    ok = False
sys.exit(0 if ok else 1)
PY

echo "PASS: execd-ebpf smoke (state=active, exec+connect+privilege events decoded)"
