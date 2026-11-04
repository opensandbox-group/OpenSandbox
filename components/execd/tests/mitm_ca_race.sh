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

# Regression tests for the mitm CA wait/race hardening: a CA file that exists
# when bootstrap starts polling may be stale from a previous egress generation
# (upstream issue #1370: a stale export lets bootstrap pass its readiness check
# and install a CA mitmproxy no longer signs with). The wait must only trust
# an export it saw appear, or one that survived a full stabilization window.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"
TESTDIR="$(mktemp -d)"
trap 'rm -rf "$TESTDIR"' EXIT

mkdir -p "$TESTDIR"

extract_function() {
  file="$1"
  name="$2"
  awk '
    /^'"$name"'\(\) \{/ { capture = 1 }
    capture { print }
    capture && /^}/ { exit }
  ' "$file"
}

extract_function "$BOOTSTRAP" _ca_fingerprint > "$TESTDIR/fingerprint.sh"
extract_function "$BOOTSTRAP" wait_for_trustworthy_mitm_ca > "$TESTDIR/wait.sh"
cat "$TESTDIR/fingerprint.sh" "$TESTDIR/wait.sh" > "$TESTDIR/helpers.sh"
# shellcheck source=/dev/null
. "$TESTDIR/helpers.sh"

CA_OLD="$TESTDIR/ca-stale.pem"
CA_NEW="$TESTDIR/ca-fresh.pem"
printf '%s\n' 'certificate-generation-old' > "$CA_OLD"
printf '%s\n' 'certificate-generation-new' > "$CA_NEW"

export MITM_CA_POLL_INTERVAL=0.1

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

now_s() {
  date +%s
}

# run_wait <target> <stabilize> <timeout>: sets WAIT_STATUS and WAIT_ELAPSED.
# Must be called in the current shell (not a command substitution) so the
# globals propagate.
run_wait() {
  target="$1"
  stabilize="$2"
  timeout="$3"
  start="$(now_s)"
  set +e
  MITM_CA_STABILIZE_WINDOW="$stabilize" MITM_CA_WAIT_TIMEOUT="$timeout" \
    wait_for_trustworthy_mitm_ca "$target"
  WAIT_STATUS=$?
  set -e
  WAIT_ELAPSED=$(( $(now_s) - start ))
}

# 1) Absent at start, appears mid-poll: trusted immediately, no stabilize wait.
target="$TESTDIR/case1.pem"
{ sleep 1; cp "$CA_NEW" "$target"; } &
writer=$!
run_wait "$target" 30 15
elapsed="$WAIT_ELAPSED"
wait "$writer" || true
[ "$WAIT_STATUS" -eq 0 ] || fail "case 1: wait returned $WAIT_STATUS"
grep -q 'certificate-generation-new' "$target" || fail "case 1: wrong CA content"
[ "$elapsed" -le 5 ] || fail "case 1: took ${elapsed}s, mid-poll appearance should be trusted immediately"
echo "PASS: case 1 mid-poll appearance trusted immediately (${elapsed}s)"

# 2) Stale file present, egress purges then re-exports: must detect the purge
#    and trust the fresh export well before the stabilization window fires.
target="$TESTDIR/case2.pem"
cp "$CA_OLD" "$target"
{ sleep 1; rm -f "$target"; sleep 1; cp "$CA_NEW" "$target"; } &
writer=$!
run_wait "$target" 30 20
elapsed="$WAIT_ELAPSED"
wait "$writer" || true
[ "$WAIT_STATUS" -eq 0 ] || fail "case 2: wait returned $WAIT_STATUS"
grep -q 'certificate-generation-new' "$target" || fail "case 2: wrong CA content"
[ "$elapsed" -lt 30 ] || fail "case 2: took ${elapsed}s, purge detection failed (window expired)"
[ "$elapsed" -le 6 ] || fail "case 2: took ${elapsed}s, purge detected too late"
echo "PASS: case 2 purge-then-export detected, fresh CA trusted (${elapsed}s)"

# 3) Stale file present, never replaced: trusted only after the full window.
target="$TESTDIR/case3.pem"
cp "$CA_OLD" "$target"
run_wait "$target" 2 15
elapsed="$WAIT_ELAPSED"
[ "$WAIT_STATUS" -eq 0 ] || fail "case 3: wait returned $WAIT_STATUS"
grep -q 'certificate-generation-old' "$target" || fail "case 3: file must stay untouched"
[ "$elapsed" -ge 2 ] || fail "case 3: trusted after ${elapsed}s, stabilization window not honored"
echo "PASS: case 3 unchanged pre-existing CA trusted after stabilization window (${elapsed}s)"

# 4) Stale file replaced in place (no purge): replacement trusted immediately.
target="$TESTDIR/case4.pem"
cp "$CA_OLD" "$target"
{ sleep 1; cp "$CA_NEW" "$target"; } &
writer=$!
run_wait "$target" 30 15
elapsed="$WAIT_ELAPSED"
wait "$writer" || true
[ "$WAIT_STATUS" -eq 0 ] || fail "case 4: wait returned $WAIT_STATUS"
grep -q 'certificate-generation-new' "$target" || fail "case 4: wrong CA content"
[ "$elapsed" -lt 30 ] || fail "case 4: took ${elapsed}s, in-place replacement not detected"
echo "PASS: case 4 in-place replacement detected (${elapsed}s)"

# 5) File never appears: timeout must be honored and reported as failure.
target="$TESTDIR/case5.pem"
run_wait "$target" 1 1
elapsed="$WAIT_ELAPSED"
[ "$WAIT_STATUS" -eq 1 ] || fail "case 5: timeout must return 1, got $WAIT_STATUS"
[ ! -e "$target" ] || fail "case 5: target must stay absent"
echo "PASS: case 5 timeout honored (${elapsed}s)"

# 6) Mitm CA wait must set MITM_CA_WAIT_SECONDS for the readiness log line.
target="$TESTDIR/case6.pem"
cp "$CA_NEW" "$target"
MITM_CA_STABILIZE_WINDOW=1 MITM_CA_WAIT_TIMEOUT=10 \
  wait_for_trustworthy_mitm_ca "$target" || fail "case 6: wait failed"
[ "${MITM_CA_WAIT_SECONDS:-}" -ge 0 ] || fail "case 6: MITM_CA_WAIT_SECONDS not set"
echo "PASS: case 6 MITM_CA_WAIT_SECONDS=${MITM_CA_WAIT_SECONDS}s exported"

echo "PASS: mitm CA race hardening behaves correctly in all 6 scenarios"
