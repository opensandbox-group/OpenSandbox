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

# Regression tests for the mitm CA rotation watcher: reinstall trust on every
# content change against the installed-generation baseline, stay silent while
# unchanged, adopt a first export when nothing was trusted yet, and retry a
# failed install pass.

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
extract_function "$BOOTSTRAP" watch_mitm_ca > "$TESTDIR/watch.sh"
cat "$TESTDIR/fingerprint.sh" "$TESTDIR/watch.sh" > "$TESTDIR/helpers.sh"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

CA_A="$TESTDIR/ca-a.pem"
CA_B="$TESTDIR/ca-b.pem"
CA_C="$TESTDIR/ca-c.pem"
printf '%s\n' 'certificate-generation-a' > "$CA_A"
printf '%s\n' 'certificate-generation-b' > "$CA_B"
printf '%s\n' 'certificate-generation-c' > "$CA_C"

export MITM_CA_WATCH_INTERVAL=0.3

# Stub trust helpers, defined before sourcing the watcher so the background
# job calls the stubs instead of production code. trust_mitm_ca honors an
# optional fail-once counter to emulate a transient install failure.
TRUST_LOG="$TESTDIR/trust.log"
: > "$TRUST_LOG"
FAIL_ONCE=0
trust_mitm_ca() {
  printf 'system %s\n' "$1" >> "$TRUST_LOG"
  if [ "$FAIL_ONCE" -gt 0 ]; then
    FAIL_ONCE=$((FAIL_ONCE - 1))
    return 1
  fi
  return 0
}
trust_mitm_ca_nss() {
  printf 'nss %s\n' "$1" >> "$TRUST_LOG"
}
trust_mitm_ca_jdk() {
  printf 'jdk %s\n' "$1" >> "$TRUST_LOG"
}

# shellcheck source=/dev/null
. "$TESTDIR/helpers.sh"

fp() {
  _ca_fingerprint "$1"
}

reset_log() {
  : > "$TRUST_LOG"
}

stop_watcher() {
  kill "$1" 2>/dev/null || true
  wait "$1" 2>/dev/null || true
}

count_calls() {
  grep -c "^$1 " "$TRUST_LOG" || true
}

# --- Case A: baseline given — quiet while unchanged, reinstall per rotation.
reset_log
live="$TESTDIR/live-a.pem"
cp "$CA_A" "$live"
watch_mitm_ca "$live" "$(fp "$live")" > "$TESTDIR/watch-a.log" 2>&1 &
watcher=$!
sleep 0.7
cp "$CA_B" "$live"; sleep 0.7          # rotation 1: in-place replacement
rm -f "$live"; sleep 0.3
cp "$CA_C" "$live"; sleep 0.7          # rotation 2: purge, then re-export
stop_watcher "$watcher"
for store in system nss jdk; do
  [ "$(count_calls "$store")" -eq 2 ] || fail "case A: expected 2 $store reinstalls, got $(count_calls "$store")"
done
grep -q 'mitm CA changed' "$TESTDIR/watch-a.log" || fail "case A: rotation not logged"
echo "PASS: case A baseline watcher reinstall per rotation, silent while unchanged"

# --- Case B: empty baseline (nothing trusted yet) — the first export seen
# must install, not become the baseline.
reset_log
live="$TESTDIR/live-b.pem" # absent at arm time
watch_mitm_ca "$live" "" > "$TESTDIR/watch-b.log" 2>&1 &
watcher=$!
sleep 0.5
cp "$CA_A" "$live"; sleep 0.7          # late first export: must install
cp "$CA_B" "$live"; sleep 0.7          # rotation: must install again
stop_watcher "$watcher"
for store in system nss jdk; do
  [ "$(count_calls "$store")" -eq 2 ] || fail "case B: expected 2 $store installs, got $(count_calls "$store")"
done
echo "PASS: case B empty baseline installs the first export it sees"

# --- Case C: stale baseline, file already holds a newer generation — must
# install on the first poll instead of treating it as already done.
reset_log
live="$TESTDIR/live-c.pem"
cp "$CA_B" "$live" # installed generation is A, but the export already holds B
watch_mitm_ca "$live" "$(fp "$CA_A")" > "$TESTDIR/watch-c.log" 2>&1 &
watcher=$!
sleep 1
stop_watcher "$watcher"
[ "$(count_calls system)" -eq 1 ] || fail "case C: expected 1 install for the newer generation, got $(count_calls system)"
echo "PASS: case C stale baseline reinstalls the newer generation on first poll"

# --- Case D: transient install failure must be retried, not dropped.
reset_log
live="$TESTDIR/live-d.pem"
cp "$CA_A" "$live"
FAIL_ONCE=1
watch_mitm_ca "$live" "$(fp "$live")" > "$TESTDIR/watch-d.log" 2>&1 &
watcher=$!
sleep 0.7
cp "$CA_B" "$live" # rotation: first install attempt fails, retry succeeds
sleep 1.2
stop_watcher "$watcher"
[ "$(count_calls system)" -eq 2 ] || fail "case D: expected a failed attempt plus a retry, got $(count_calls system) system calls"
grep -q 'retrying on the next poll' "$TESTDIR/watch-d.log" || fail "case D: retry not logged"
grep -q 'mitm CA changed' "$TESTDIR/watch-d.log" || fail "case D: rotation not logged"
echo "PASS: case D failed install is retried on the next poll"

echo "PASS: watcher behaves correctly in all 4 scenarios"
