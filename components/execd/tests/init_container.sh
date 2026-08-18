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

# Regression test: execd as the sandbox init (OSEP-0018) in a real container.
#
# Builds the execd image and runs it with EXECD_INIT=1 so bootstrap.sh execs
# into execd (PID 1), then verifies the init-mode contract end to end:
#   - execd is PID 1 and the workload is its direct child
#   - orphaned children are reaped (no zombie accumulation under PID 1)
#   - in-namespace `kill -9 1` is inert (kernel signal shield)
#   - the entrypoint exit code is propagated to the container exit code
#   - runtime SIGTERM is forwarded and the workload's status is preserved
#   - with [hardening] enabled, the floor applies (caps/no_new_privs/seccomp/
#     env strip) and the capabilities endpoint reports pid1 + active layers
#   - on a non-PID-1 topology execd degrades to subreaper and says so
#
# Prerequisites: docker (root or in the docker group)
#
# Usage:
#   bash components/execd/tests/init_container.sh
#
# Set EXECD_TEST_IMAGE to an existing image (with /bootstrap.sh, /execd and
# /opt/opensandbox/opensandbox-launcher) to skip the Dockerfile build — handy
# for local iteration; unset builds the full image in CI.
#
# Exit 0 on success, non-zero on failure.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
IMAGE="${EXECD_TEST_IMAGE:-execd-init-container-test:test}"
PREFIX="execd-init-$$"
TESTDIR="$(mktemp -d)"
# mktemp creates mode 700; with hardening the workload has no
# CAP_DAC_OVERRIDE, so the host-side test dir must be fully accessible to
# the container workload (traverse AND write for the hardened.out /
# caps.json the workload produces). Docker Desktop's lax mount permissions
# hide this locally; native Linux mounts do not.
chmod 777 "${TESTDIR}"
RUNNERS=()

cleanup() {
  echo ">> Cleaning up..."
  for c in "${RUNNERS[@]:-}"; do
    docker rm -f "${c}" >/dev/null 2>&1 || true
  done
  rm -rf "${TESTDIR}"
  if [ -z "${EXECD_TEST_IMAGE:-}" ]; then
    docker rmi -f "${IMAGE}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

wait_file() {
  local path="$1"
  local i
  for i in $(seq 1 60); do
    [ -f "$path" ] && return 0
    sleep 0.5
  done
  return 1
}

dump_container_logs() {
  local c="$1"
  echo ">> Container logs for ${c}:"
  docker logs "$c" 2>&1 | grep -E "launcher|landlock|FAIL|init:|hardening|exited" | tail -30 || true
}

echo "========================================="
echo " Init-mode container regression (OSEP-0018)"
echo "========================================="

# -------------------------------------------------------------------
# Build the execd image (unless an existing image was provided).
# -------------------------------------------------------------------
if [ -n "${EXECD_TEST_IMAGE:-}" ]; then
  echo ">> Using prebuilt image ${IMAGE} (EXECD_TEST_IMAGE set)"
else
  echo ">> Building image ${IMAGE}..."
  cd "${REPO_ROOT}"
  docker build \
    -f components/execd/Dockerfile \
    -t "${IMAGE}" \
    --build-arg VERSION=init-container-test \
    . >/dev/null
  echo ">> Image built."
fi

# -------------------------------------------------------------------
# Test 1: execd is PID 1, workload is its child, orphans are reaped,
# and in-namespace kill -9 1 is inert.
# -------------------------------------------------------------------
echo ""
echo ">> Test 1: PID 1 handoff, orphan reaping, signal shield"

cat > "${TESTDIR}/verify_pid1.sh" <<'SCRIPT'
#!/bin/sh
out=/mnt/test/pid1.out
: > "$out"
comm=$(cat /proc/1/comm)
echo "pid1_comm=$comm" >> "$out"
[ "$comm" = "execd" ] || { echo "FAIL: pid1 is $comm" >> "$out"; exit 90; }
ppid=$(awk '{print $4}' /proc/$$/stat)
echo "workload_ppid=$ppid" >> "$out"
[ "$ppid" = "1" ] || { echo "FAIL: workload ppid=$ppid" >> "$out"; exit 91; }

for i in $(seq 1 10); do ( sleep 0.1 ) & done
sleep 3
zombies=0
for p in /proc/[0-9]*; do
  stat=$(cat "$p/stat" 2>/dev/null) || continue
  stat=${stat#*)}          # strip "pid (comm)"
  set -- $stat
  [ "$1" = "Z" ] && [ "$2" = "1" ] && zombies=$((zombies+1))
done
echo "zombies_ppid1=$zombies" >> "$out"
[ "$zombies" = "0" ] || { echo "FAIL: $zombies zombies under pid 1" >> "$out"; exit 92; }

kill -9 1
echo "kill9_1_inert=yes" >> "$out"
exit 0
SCRIPT
chmod +x "${TESTDIR}/verify_pid1.sh"

C1="${PREFIX}-t1"
RUNNERS+=("$C1")
docker run -d --name "$C1" \
  --entrypoint /bootstrap.sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  /mnt/test/verify_pid1.sh >/dev/null
if ! wait_file "${TESTDIR}/pid1.out"; then
  dump_container_logs "$C1"
  fail "test 1: container did not produce pid1.out"
fi
RC=$(docker wait "$C1")
[ "$RC" = "0" ] || fail "test 1: container exited $RC: $(cat "${TESTDIR}/pid1.out")"
grep -q "kill9_1_inert=yes" "${TESTDIR}/pid1.out" || fail "test 1: kill -9 1 was not inert"
grep -q "zombies_ppid1=0" "${TESTDIR}/pid1.out" || fail "test 1: zombies accumulated"
docker rm -f "$C1" >/dev/null
echo "PASS: pid1 handoff, orphan reaping, signal shield"

# -------------------------------------------------------------------
# Test 2: entrypoint exit code propagation.
# -------------------------------------------------------------------
echo ""
echo ">> Test 2: entrypoint exit code propagation"

C2="${PREFIX}-t2"
RUNNERS+=("$C2")
set +e
docker run --rm --name "$C2" \
  --entrypoint /bootstrap.sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  /bin/sh -c 'exit 7'
RC=$?
set -e
[ "$RC" = "7" ] || fail "test 2: container exit code = $RC, want 7"
echo "PASS: entrypoint exit code propagated ($RC)"

# -------------------------------------------------------------------
# Test 3: SIGTERM is forwarded; workload status is preserved.
# -------------------------------------------------------------------
echo ""
echo ">> Test 3: SIGTERM graceful shutdown"

cat > "${TESTDIR}/sigterm.sh" <<'SCRIPT'
#!/bin/sh
trap 'touch /mnt/test/sigterm_received; exit 7' TERM
while :; do sleep 0.5; done
SCRIPT
chmod +x "${TESTDIR}/sigterm.sh"

C3="${PREFIX}-t3"
RUNNERS+=("$C3")
docker run -d --name "$C3" \
  --entrypoint /bootstrap.sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  /mnt/test/sigterm.sh >/dev/null

sleep 3
docker stop -t 10 "$C3" >/dev/null
RC=$(docker wait "$C3")
[ "$RC" = "7" ] || fail "test 3: container exit code after SIGTERM = $RC, want 7"
[ -f "${TESTDIR}/sigterm_received" ] || fail "test 3: workload did not receive SIGTERM"
docker rm -f "$C3" >/dev/null
echo "PASS: SIGTERM forwarded, status preserved ($RC)"

# -------------------------------------------------------------------
# Test 4: hardening floor in a real PID-1 sandbox.
# -------------------------------------------------------------------
echo ""
echo ">> Test 4: hardening floor ([hardening] enabled)"

cat > "${TESTDIR}/isolation.toml" <<'TOML'
[hardening]
enabled = true

[landlock]
enabled = true
TOML

cat > "${TESTDIR}/hardened.sh" <<'SCRIPT'
#!/bin/sh
out=/mnt/test/hardened.out
: > "$out"
# /proc/self is read via the entrypoint process itself (a forked descendant
# resolves its own /proc/<pid> dir, which the Landlock allowlist does not
# cover — the documented /proc/self limitation of OSEP-0018).
nnp=""; sec=""; capeff=""
while IFS= read -r line; do
  case "$line" in
    NoNewPrivs:*) nnp="${line#*:	}" ;;
    Seccomp:*) sec="${line#*:	}" ;;
    CapEff:*) capeff="${line#*:	}" ;;
  esac
done < /proc/self/status
echo "nnp=$nnp sec=$sec capeff=$capeff" >> "$out"

[ "$nnp" = "1" ] || { echo "FAIL: no_new_privs not set ($nnp)" >> "$out"; exit 93; }
[ "$sec" = "2" ] || { echo "FAIL: seccomp not in filter mode ($sec)" >> "$out"; exit 94; }
[ "$capeff" = "0000000000000000" ] || { echo "FAIL: CapEff=$capeff, want zero" >> "$out"; exit 95; }
# The entrypoint keeps bootstrap env (JUPYTER_TOKEN) but never execd's
# control-plane credential (EXECD_ACCESS_TOKEN) — its Jupyter kernels are
# user code and must not see it.
[ -z "${EXECD_ACCESS_TOKEN:-}" ] || { echo "FAIL: execd credential leaked to entrypoint" >> "$out"; exit 96; }
[ "${JUPYTER_TOKEN:-}" = "jt-secret" ] || { echo "FAIL: bootstrap env not preserved" >> "$out"; exit 95; }

# The execd API is guarded by the token we injected via the container env
# (which the floor stripped from this workload's own environment).
for i in $(seq 1 30); do
  wget -qO- --header="X-EXECD-ACCESS-TOKEN: supersecret" \
    http://127.0.0.1:44772/v1/isolated/capabilities > /mnt/test/caps.json 2>/dev/null && break
  sleep 0.5
done
grep -q '"init_mode":"pid1"' /mnt/test/caps.json || { echo "FAIL: init_mode != pid1" >> "$out"; exit 97; }
grep -q '"cap_drop":{"state":"active"' /mnt/test/caps.json || { echo "FAIL: cap_drop not active" >> "$out"; exit 98; }
grep -q '"seccomp":{"state":"active"' /mnt/test/caps.json || { echo "FAIL: seccomp not active" >> "$out"; exit 99; }
if grep -q '"landlock":{"state":"active"' /mnt/test/caps.json; then
  echo "landlock=active" >> "$out"
  if cat /proc/1/environ >/dev/null 2>&1; then
    echo "FAIL: /proc/1/environ readable despite landlock" >> "$out"; exit 90
  fi
else
  # Kernel without Landlock (e.g. some CI VM kernels): the launcher fails
  # open and the layer reports unsupported.
  grep -q '"landlock":{"state":"unsupported"' /mnt/test/caps.json || { echo "FAIL: landlock neither active nor unsupported" >> "$out"; exit 91; }
  echo "landlock=unsupported (skipped)" >> "$out"
fi
# The default image has no eBPF code; the layer must report disabled.
grep -q '"ebpf":{"state":"disabled"' /mnt/test/caps.json || { echo "FAIL: ebpf not disabled in the default image" >> "$out"; exit 92; }
echo "hardened_ok=yes" >> "$out"
exit 0
SCRIPT
chmod +x "${TESTDIR}/hardened.sh"

C4="${PREFIX}-t4"
RUNNERS+=("$C4")
docker run -d --name "$C4" \
  --entrypoint /bootstrap.sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -e EXECD_ISOLATION_CONFIG=/mnt/test/isolation.toml \
  -e EXECD_ACCESS_TOKEN=supersecret \
  -e JUPYTER_TOKEN=jt-secret \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  /mnt/test/hardened.sh >/dev/null
if ! wait_file "${TESTDIR}/hardened.out"; then
  dump_container_logs "$C4"
  fail "test 4: container did not produce hardened.out"
fi
RC=$(docker wait "$C4")
[ "$RC" = "0" ] || fail "test 4: container exited $RC: $(cat "${TESTDIR}/hardened.out")"
grep -q "hardened_ok=yes" "${TESTDIR}/hardened.out" || fail "test 4: floor assertions failed: $(cat "${TESTDIR}/hardened.out")"
docker rm -f "$C4" >/dev/null
echo "PASS: hardening floor active in a PID-1 sandbox"

# -------------------------------------------------------------------
# Test 5: non-PID-1 topology degrades to subreaper.
# -------------------------------------------------------------------
echo ""
echo ">> Test 5: subreaper mode on the Pool-style topology"

cat > "${TESTDIR}/subreaper.sh" <<'SCRIPT'
#!/bin/sh
out=/mnt/test/subreaper.out
: > "$out"
for i in $(seq 1 30); do
  wget -qO- http://127.0.0.1:44772/v1/isolated/capabilities > /mnt/test/subreaper.json 2>/dev/null && break
  sleep 0.5
done
grep -q '"init_mode":"subreaper"' /mnt/test/subreaper.json || { echo "FAIL: init_mode != subreaper" >> "$out"; exit 97; }
echo "subreaper_ok=yes" >> "$out"
exit 0
SCRIPT
chmod +x "${TESTDIR}/subreaper.sh"

C5="${PREFIX}-t5"
RUNNERS+=("$C5")
# Pool-style: bootstrap.sh is not the container entrypoint; a shell is PID 1
# and bootstrap (with EXECD_INIT) runs backgrounded, so execd degrades to
# subreaper mode. The background "&" is essential: `sh -c 'cmd'` would exec
# the command and make execd PID 1.
docker run -d --name "$C5" \
  --entrypoint /bin/sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  -c 'EXECD_INIT=1 /bootstrap.sh /mnt/test/subreaper.sh & wait' >/dev/null
if ! wait_file "${TESTDIR}/subreaper.out"; then
  dump_container_logs "$C5"
  fail "test 5: container did not produce subreaper.out"
fi
RC=$(docker wait "$C5")
[ "$RC" = "0" ] || fail "test 5: container exited $RC: $(cat "${TESTDIR}/subreaper.out")"
grep -q "subreaper_ok=yes" "${TESTDIR}/subreaper.out" || fail "test 5: subreaper assertions failed"
docker rm -f "$C5" >/dev/null
echo "PASS: subreaper degradation reported"

# -------------------------------------------------------------------
echo ""
echo "========================================="
echo " Init-mode container regression PASSED"
echo "========================================="
echo "  image: ${IMAGE}"
echo "  cases: pid1 handoff / reaping / signal shield /"
echo "         exit propagation / SIGTERM / hardening floor / subreaper"
