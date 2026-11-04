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

# Regression tests for the mitm CA rotation watcher: egress can restart
# independently of the sandbox container (kubelet under restartPolicy Always)
# and rotates the exported CA on every restart while bootstrap never re-runs.
# The watcher must reinstall trust for every content change and must NOT
# reinstall while the CA is unchanged.

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

# Stub the production trust helpers: the watcher must call each of them on
# every rotation. Defined before sourcing the extracted watcher so the
# background job inherits the stubs instead of production code.
TRUST_LOG="$TESTDIR/trust.log"
: > "$TRUST_LOG"
trust_mitm_ca() {
  printf 'system %s\n' "$1" >> "$TRUST_LOG"
}
trust_mitm_ca_nss() {
  printf 'nss %s\n' "$1" >> "$TRUST_LOG"
}
trust_mitm_ca_jdk() {
  printf 'jdk %s\n' "$1" >> "$TRUST_LOG"
}

# shellcheck source=/dev/null
. "$TESTDIR/helpers.sh"

CA_A="$TESTDIR/ca-a.pem"
CA_B="$TESTDIR/ca-b.pem"
printf '%s\n' 'certificate-generation-a' > "$CA_A"
printf '%s\n' 'certificate-generation-b' > "$CA_B"

live="$TESTDIR/live-ca.pem"
cp "$CA_A" "$live"

export MITM_CA_WATCH_INTERVAL=0.3
watch_mitm_ca "$live" > "$TESTDIR/watch.log" 2>&1 &
watcher=$!

sleep 1
# Steady state: unchanged CA must not trigger a reinstall.
grep -q 'certificate-generation-a' "$live" || fail "fixture broken"

# Rotation 1: egress exports a new CA in place.
cp "$CA_B" "$live"
sleep 1

# Rotation 2: full egress restart cycle — purge, then re-export another CA.
rm -f "$live"
sleep 0.5
printf '%s\n' 'certificate-generation-c' > "$live"
sleep 1

kill "$watcher" 2>/dev/null || true
wait "$watcher" 2>/dev/null || true

system_calls="$(grep -c '^system ' "$TRUST_LOG" || true)"
nss_calls="$(grep -c '^nss ' "$TRUST_LOG" || true)"
jdk_calls="$(grep -c '^jdk ' "$TRUST_LOG" || true)"

[ "$system_calls" -eq 2 ] || fail "expected exactly 2 system-store reinstalls (one per rotation; steady state must stay silent), got $system_calls"
[ "$nss_calls" -eq 2 ] || fail "expected exactly 2 NSS reinstalls, got $nss_calls"
[ "$jdk_calls" -eq 2 ] || fail "expected exactly 2 JDK reinstalls, got $jdk_calls"

grep -q 'mitm CA changed' "$TESTDIR/watch.log" || fail "watcher did not log the rotation"
grep -q 'restart long-running processes' "$TESTDIR/watch.log" || fail "watcher did not tell users to restart processes with cached trust"
grep -q 'watching mitm CA' "$TESTDIR/watch.log" || fail "watcher did not announce itself"

# The reinstall path must also refresh the merged bundle via trust_mitm_ca:
# verify the stub saw the live CA path on the LAST reinstall (post-rotation 2).
last_system="$(grep '^system ' "$TRUST_LOG" | tail -n 1)"
[ "$last_system" = "system $live" ] || fail "unexpected reinstall target: $last_system"

echo "PASS: watcher reinstalls trust on every rotation and stays quiet otherwise"
