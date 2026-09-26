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

# Smoke test: multiple overlay mounts per session (issue #1228).
#
# Runs real bubblewrap namespaces with the exact argv format the execd
# isolation layer generates for multiple overlays, and asserts the live
# mount semantics:
#   1. single overlay (persist)          — legacy path still works
#   2. root overlay + workspace overlay  — separate uppers, nested shadowing
#   3. ephemeral + rw + ro in one ns     — tmpfs upper, ro write denial
#
# The argv mirrors buildArgvWithLifecycle: namespace flags, ro-bind root,
# /tmp + /run + /dev + /proc, overlay segments ordered shallow→deep, upper
# root hidden via tmpfs, --die-with-parent.
#
# Prerequisites: bwrap v0.11+ (supports --tmp-overlay) on PATH or $BWRAP_BIN;
# root privileges (passwordless sudo) for namespace creation.
#
# Usage:
#   bash components/execd/tests/smoke_overlays.sh
#   BWRAP_BIN=/path/to/bwrap bash components/execd/tests/smoke_overlays.sh
#
# Exit 0 on success, non-zero on failure.
set -euo pipefail

# Work happens in a container-local directory: overlayfs upper/work dirs
# need a real local filesystem, not a bind mount or network share.
SMOKE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/smoke-overlays.XXXXXX")"

# Namespace creation needs root (or accept a degraded unprivileged path).
SUDO=""
if [ "$(id -u)" != "0" ]; then
  if sudo -n true 2>/dev/null; then
    SUDO="sudo -n"
  else
    echo ">> SKIPPED: overlay smoke needs root and passwordless sudo is unavailable."
    exit 0
  fi
fi

cleanup() {
  echo ">> Cleaning up..."
  # In-namespace writes run via $SUDO, so upper files may be root-owned.
  # Never let cleanup failure mask the test result.
  $SUDO rm -rf "${SMOKE_DIR}" || true
}
trap cleanup EXIT

BWRAP="${BWRAP_BIN:-$(command -v bwrap 2>/dev/null || true)}"
if [ -z "${BWRAP}" ]; then
  echo ">> bwrap not found (set BWRAP_BIN or install bwrap v0.11+)."
  exit 1
fi

echo ">> bwrap: $("${BWRAP}" --version 2>&1)"

fail() {
  echo ">> FAIL: $*"
  exit 1
}

assert_content() { # <path> <expected>
  local path="$1" expected="$2"
  [ -f "${path}" ] || fail "expected file missing: ${path}"
  local got
  got="$(cat "${path}")"
  [ "${got}" = "${expected}" ] || fail "content mismatch in ${path}: got '${got}', want '${expected}'"
}

assert_absent() {
  [ ! -e "$1" ] || fail "unexpected file exists: $1"
}

# Namespace prefix shared by every scenario (ShareNet=true: no --unshare-net).
ns_flags=(
  --unshare-pid
  --unshare-uts --hostname sandbox
  --unshare-ipc --unshare-cgroup
  --ro-bind / /
  --tmpfs /tmp
  --tmpfs /run --dev /dev --proc /proc
)

# run_ns <name> <script> <argv...> — runs bwrap, fails on non-zero exit.
run_ns() {
  local name="$1" script="$2"
  shift 2
  local out rc
  set +e
  out="$($SUDO "${BWRAP}" "$@" --die-with-parent -- /bin/sh -c "${script}" 2>&1)"
  rc=$?
  set -e
  echo "${out}" | sed 's/^/     | /'
  [ "${rc}" -eq 0 ] || fail "${name}: bwrap exited ${rc}"
}

echo ""
echo "========================================="
echo " Smoke Test: multi-overlay mounts per session"
echo "========================================="

# -------------------------------------------------------------------
# Scenario 1: single overlay with persisted upper (legacy path).
# -------------------------------------------------------------------
echo ""
echo ">> Scenario 1: single overlay, persisted upper..."

ISO1="${SMOKE_DIR}/upper-root"
WS1="${SMOKE_DIR}/ws1"
ID1="s1"
UP1="${ISO1}/${ID1}/upper"
WK1="${ISO1}/${ID1}/work"
mkdir -p "${UP1}" "${WK1}" "${WS1}"
echo "L" > "${WS1}/lower.txt"

run_ns "scenario 1" \
  "echo smoke > ${WS1}/s.txt; cat ${WS1}/lower.txt" \
  "${ns_flags[@]}" \
  --overlay-src "${WS1}" --overlay "${UP1}" "${WK1}" "${WS1}" \
  --tmpfs "${ISO1}"

assert_content "${UP1}/s.txt" "smoke"
assert_content "${WS1}/lower.txt" "L"
assert_absent "${WS1}/s.txt"
echo ">> Scenario 1 PASSED. ✓"

# -------------------------------------------------------------------
# Scenario 2: root overlay + workspace overlay (issue #1228 headline).
# System writes (/usr) and workspace writes must be tracked by separate
# uppers; the nested workspace overlay must shadow the root overlay.
# -------------------------------------------------------------------
echo ""
echo ">> Scenario 2: root overlay + workspace overlay..."

ISO2="${SMOKE_DIR}/upper-root2"
WS2="${SMOKE_DIR}/ws2"
ID2="s2"
ROOT_UP="${ISO2}/${ID2}/root-upper"
ROOT_WK="${ISO2}/${ID2}/root-work"
WS_UP="${ISO2}/${ID2}/ws-upper"
WS_WK="${ISO2}/${ID2}/ws-work"
mkdir -p "${ROOT_UP}" "${ROOT_WK}" "${WS_UP}" "${WS_WK}" "${WS2}"
echo "L" > "${WS2}/lower.txt"

run_ns "scenario 2" \
  "echo R > /usr/smoke-root.txt; echo W > ${WS2}/smoke-ws.txt; cat ${WS2}/lower.txt" \
  "${ns_flags[@]}" \
  --overlay-src / --overlay "${ROOT_UP}" "${ROOT_WK}" / \
  --overlay-src "${WS2}" --overlay "${WS_UP}" "${WS_WK}" "${WS2}" \
  --tmpfs "${ISO2}"

assert_content "${ROOT_UP}/usr/smoke-root.txt" "R"
assert_content "${WS_UP}/smoke-ws.txt" "W"
assert_content "${WS2}/lower.txt" "L"
# Nesting: /workspace writes must not bypass into the root upper.
assert_absent "${ROOT_UP}/workspace"
echo ">> Scenario 2 PASSED. ✓"

# -------------------------------------------------------------------
# Scenario 3: mixed modes in one namespace — ephemeral overlay (tmpfs
# upper), plain rw mount, and a read-only mount with write denial.
# -------------------------------------------------------------------
echo ""
echo ">> Scenario 3: ephemeral overlay + rw + ro..."

EPH="${SMOKE_DIR}/eph"
RW="${SMOKE_DIR}/data"
RO="${SMOKE_DIR}/ro"
mkdir -p "${EPH}" "${RW}" "${RO}"
echo "RO" > "${RO}/locked.txt"

run_ns "scenario 3" \
  "echo e > ${EPH}/e.txt; echo d > ${RW}/d.txt; cat ${RO}/locked.txt; \
if echo bad > ${RO}/denied.txt 2>/dev/null; then echo RO-WRITE-OK; exit 42; fi; \
test -f ${EPH}/e.txt && echo EPHEMERAL-OK" \
  "${ns_flags[@]}" \
  --overlay-src "${EPH}" --tmp-overlay "${EPH}" \
  --bind "${RW}" "${RW}" \
  --ro-bind "${RO}" "${RO}"

assert_content "${RW}/d.txt" "d"
assert_content "${RO}/locked.txt" "RO"
assert_absent "${RO}/denied.txt"
# Ephemeral upper lives on tmpfs inside the namespace: nothing on the host.
assert_absent "${EPH}/e.txt"
echo ">> Scenario 3 PASSED. ✓"

# -------------------------------------------------------------------
# Summary
# -------------------------------------------------------------------
echo ""
echo "========================================="
echo " Smoke Test PASSED"
echo "========================================="
echo "  single overlay persist: in-ns write → host upper, lower untouched"
echo "  root + workspace overlays: separate uppers, nested shadowing works"
echo "  ephemeral + rw + ro: tmpfs upper ephemeral, ro write denied"
echo "  bwrap: ${BWRAP}"
echo ""
