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
# into execd (PID 1), then verifies the smoke-layer init-mode contract:
#   - execd is PID 1 and the workload is its direct child
#   - orphaned children are reaped (no zombie accumulation under PID 1)
#   - in-namespace `kill -9 1` is inert (kernel signal shield)
#   - with [hardening] enabled, the entrypoint keeps bootstrap env
#     (JUPYTER_TOKEN) while execd's credential (EXECD_ACCESS_TOKEN) is
#     stripped by the launcher
#   - packaged executables and scripts have stable 0755 permissions
#   - on a non-PID-1 topology execd degrades to subreaper and says so
#   - while preStart is blocked, /ping stays ready and both the entrypoint
#     and periodic hooks wait; lifecycle transport is stripped from user code
#
# Exit-code propagation, runtime SIGTERM forwarding and the rest of the
# hardening floor are covered by the Python e2e suites
# (tests/python/tests/test_execd_init_e2e.py,
# test_execd_hardening_e2e.py); this script keeps only the cases the e2e
# does not reach.
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
  docker logs "$c" 2>&1 | grep -E "launcher|landlock|FAIL|init:|hardening|exited|lifecycle|error:" | tail -30 || true
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
# Test 0: packaged executables and scripts have stable permissions.
# -------------------------------------------------------------------
echo ""
echo ">> Test 0: packaged executable and script permissions"

if ! docker run --rm \
  --user 65534:65534 \
  --entrypoint /bin/sh \
  "${IMAGE}" \
  -c '
    set -e
    for path in \
      /execd \
      /execd.exe \
      /execd-ebpf \
      /opensandbox-supervisor \
      /bootstrap.sh \
      /install.bat \
      /usr/local/bin/bwrap \
      /usr/local/libexec/opensandbox-session-gate \
      /opt/opensandbox/opensandbox-session-gate \
      /usr/local/libexec/opensandbox-launcher \
      /opt/opensandbox/opensandbox-launcher; do
      mode=$(stat -c %a "$path")
      [ "$mode" = 755 ] || {
        echo "$path mode is $mode, expected 755" >&2
        exit 1
      }
      [ -r "$path" ] && [ -x "$path" ] || {
        echo "$path is not readable and executable by a non-root user" >&2
        exit 1
      }
    done
  '; then
  fail "test 0: packaged executable or script permissions are invalid"
fi
echo "PASS: packaged executables and scripts are mode 0755"

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
# Test 2: entrypoint env inheritance under the hardening floor.
# -------------------------------------------------------------------
echo ""
echo ">> Test 2: entrypoint env inheritance ([hardening] enabled)"

cat > "${TESTDIR}/isolation.toml" <<'TOML'
[hardening]
enabled = true
TOML

cat > "${TESTDIR}/hardened.sh" <<'SCRIPT'
#!/bin/sh
out=/mnt/test/hardened.out
: > "$out"
# The entrypoint keeps bootstrap env (JUPYTER_TOKEN) but never execd's
# control-plane credential (EXECD_ACCESS_TOKEN) — its Jupyter kernels are
# user code and must not see it. (The container env below sets both, so an
# empty EXECD_ACCESS_TOKEN proves the launcher stripped it, not that it was
# absent in the first place.)
[ -z "${EXECD_ACCESS_TOKEN:-}" ] || { echo "FAIL: execd credential leaked to entrypoint" >> "$out"; exit 96; }
[ "${JUPYTER_TOKEN:-}" = "jt-secret" ] || { echo "FAIL: bootstrap env not preserved" >> "$out"; exit 95; }
echo "env_inheritance_ok=yes" >> "$out"
exit 0
SCRIPT
chmod +x "${TESTDIR}/hardened.sh"

C2="${PREFIX}-t2"
RUNNERS+=("$C2")
docker run -d --name "$C2" \
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
  dump_container_logs "$C2"
  fail "test 2: container did not produce hardened.out"
fi
RC=$(docker wait "$C2")
[ "$RC" = "0" ] || fail "test 2: container exited $RC: $(cat "${TESTDIR}/hardened.out")"
grep -q "env_inheritance_ok=yes" "${TESTDIR}/hardened.out" || fail "test 2: env inheritance assertions failed: $(cat "${TESTDIR}/hardened.out")"
docker rm -f "$C2" >/dev/null
echo "PASS: entrypoint env inheritance under the floor"

# -------------------------------------------------------------------
# Test 3: non-PID-1 topology degrades to subreaper.
# -------------------------------------------------------------------
echo ""
echo ">> Test 3: subreaper mode on the Pool-style topology"

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

C3="${PREFIX}-t3"
RUNNERS+=("$C3")
# Pool-style: bootstrap.sh is not the container entrypoint; a shell is PID 1
# and bootstrap (with EXECD_INIT) runs backgrounded, so execd degrades to
# subreaper mode. The background "&" is essential: `sh -c 'cmd'` would exec
# the command and make execd PID 1.
docker run -d --name "$C3" \
  --entrypoint /bin/sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  -c 'EXECD_INIT=1 /bootstrap.sh /mnt/test/subreaper.sh & wait' >/dev/null
if ! wait_file "${TESTDIR}/subreaper.out"; then
  dump_container_logs "$C3"
  fail "test 3: container did not produce subreaper.out"
fi
RC=$(docker wait "$C3")
[ "$RC" = "0" ] || fail "test 3: container exited $RC: $(cat "${TESTDIR}/subreaper.out")"
grep -q "subreaper_ok=yes" "${TESTDIR}/subreaper.out" || fail "test 3: subreaper assertions failed"
docker rm -f "$C3" >/dev/null
echo "PASS: subreaper degradation reported"

# -------------------------------------------------------------------
# Test 4: init mode runs lifecycle hooks before and alongside workload.
# -------------------------------------------------------------------
echo ""
echo ">> Test 4: init-mode lifecycle hooks"

cat > "${TESTDIR}/lifecycle.sh" <<'SCRIPT'
#!/bin/sh
set -eu
out=/mnt/test/lifecycle.out
touch /mnt/test/entrypoint.started
[ "$(cat /proc/1/comm)" = "execd" ] || { echo "FAIL: execd is not PID 1" > "$out"; exit 98; }
[ -f /mnt/test/prestart.done ] || { echo "FAIL: preStart did not finish before entrypoint" > "$out"; exit 99; }
[ -z "${OPENSANDBOX_LIFECYCLE:-}" ] || { echo "FAIL: lifecycle transport leaked" > "$out"; exit 100; }

i=0
while [ ! -f /mnt/test/periodic.twice ] && [ "$i" -lt 100 ]; do
  sleep 0.2
  i=$((i+1))
done
[ -f /mnt/test/periodic.twice ] \
  || { echo "FAIL: periodic hook did not run twice" > "$out"; exit 101; }
printf 'lifecycle_hooks_ok=yes\n' > "$out"
exit 0
SCRIPT
chmod +x "${TESTDIR}/lifecycle.sh"

C4="${PREFIX}-t4"
RUNNERS+=("$C4")
docker run -d --name "$C4" \
  --entrypoint /bootstrap.sh \
  -e EXECD=/execd \
  -e EXECD_INIT=1 \
  -e 'OPENSANDBOX_LIFECYCLE={"preStart":{"command":["/bin/sh","-c","touch /mnt/test/prestart.started; while [ ! -f /mnt/test/prestart.release ]; do sleep 0.1; done; touch /mnt/test/prestart.done"],"timeoutSeconds":300},"periodic":[{"name":"checkpoint","schedule":"@every 1s","command":["/bin/sh","-c","touch /mnt/test/periodic.started; echo periodic >> /mnt/test/periodic.sequence; test $(grep -c periodic /mnt/test/periodic.sequence) -ge 2 && touch /mnt/test/periodic.twice || true"]}]}' \
  -v "${TESTDIR}:/mnt/test" \
  "${IMAGE}" \
  /mnt/test/lifecycle.sh >/dev/null
if ! wait_file "${TESTDIR}/prestart.started"; then
  dump_container_logs "$C4"
  fail "test 4: preStart did not reach the startup barrier"
fi
if ! docker exec "$C4" /bin/sh -c \
  'i=0; while [ "$i" -lt 30 ]; do wget -qO- http://127.0.0.1:44772/ping >/dev/null && exit 0; sleep 0.5; i=$((i+1)); done; exit 1'; then
  dump_container_logs "$C4"
  fail "test 4: execd /ping was unavailable while preStart was blocked"
fi
# Keep the barrier closed long enough to observe an incorrectly started entrypoint or periodic hook.
sleep 2
[ ! -f "${TESTDIR}/entrypoint.started" ] \
  || fail "test 4: entrypoint started before preStart was released"
[ ! -f "${TESTDIR}/periodic.started" ] \
  || fail "test 4: periodic hook started before preStart was released"
touch "${TESTDIR}/prestart.release"
if ! wait_file "${TESTDIR}/lifecycle.out"; then
  dump_container_logs "$C4"
  fail "test 4: container did not produce lifecycle.out"
fi
RC=$(docker wait "$C4")
[ "$RC" = "0" ] || fail "test 4: container exited $RC: $(cat "${TESTDIR}/lifecycle.out")"
grep -q "lifecycle_hooks_ok=yes" "${TESTDIR}/lifecycle.out" \
  || fail "test 4: lifecycle assertions failed: $(cat "${TESTDIR}/lifecycle.out")"
docker rm -f "$C4" >/dev/null
echo "PASS: init-mode health availability, preStart ordering, periodic execution, and environment isolation"

# -------------------------------------------------------------------
echo ""
echo "========================================="
echo " Init-mode container regression PASSED"
echo "========================================="
echo "  image: ${IMAGE}"
echo "  cases: packaged permissions / pid1 handoff / reaping /"
echo "         signal shield / env inheritance / subreaper / lifecycle hooks"
