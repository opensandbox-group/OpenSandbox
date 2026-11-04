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

# End-to-end smoke for the mitm CA race hardening and rotation watcher: it
# runs the REAL bootstrap.sh entrypoint against a fake egress that replays a
# real egress restart timeline on the shared volume — a stale export left by
# a previous generation, a purge + fresh export (the issue #1370 race), then
# a second purge + rotated export while the sandbox keeps running (the
# independent-sidecar-restart case). Asserts that bootstrap:
#   1. does NOT trust the stale pre-existing export: it must detect the
#      purge and trust the fresh export within the stabilization window,
#   2. builds the merged bundle from the fresh CA,
#   3. the watcher reinstalls trust for the second rotation in the same
#      sandbox lifetime without a bootstrap re-run.
#
# The host is fully shielded: sudo, java and keytool resolve to logging
# stubs, JAVA_HOME points at a fixture JDK, and a fake certifi module
# sources the merged bundle. Note for CI runners: trust_mitm_ca_jdk also
# scans hardcoded paths like /usr/lib/jvm, where real runner JDKs may pick
# up a junk "opensandbox-mitmproxy" alias — runner VMs are ephemeral, so
# this is accepted here (test_mitm_ca_jdk.py covers JDK logic hermetically).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"
TESTDIR="$(mktemp -d)"
cleanup() {
  # The rotation watcher outlives bootstrap (by design) as an orphaned
  # subshell sharing bootstrap's argv; the stub execd outlives it too.
  pkill -f "$BOOTSTRAP" 2>/dev/null || true
  pkill -f "$TESTDIR/stub-execd" 2>/dev/null || true
  [ -n "${FAKE_EGRESS_PID:-}" ] && kill "$FAKE_EGRESS_PID" 2>/dev/null || true
  rm -rf "$TESTDIR"
}
trap cleanup EXIT

VOL="$TESTDIR/volume" # stands in for the shared /opt/opensandbox volume
HOME_DIR="$TESTDIR/home"
SHIM="$TESTDIR/shim"
LOG="$TESTDIR/bootstrap.log"
mkdir -p "$VOL" "$HOME_DIR" "$SHIM"

MITM_CA="$VOL/mitmproxy-ca-cert.pem"
MERGED="$VOL/merged-ca-certificates.pem"

CA_STALE="$TESTDIR/ca-stale.pem"
CA_FRESH="$TESTDIR/ca-fresh.pem"
CA_ROTATED="$TESTDIR/ca-rotated.pem"
printf '%s\n' 'certificate-generation-stale' > "$CA_STALE"
printf '%s\n' 'certificate-generation-fresh' > "$CA_FRESH"
printf '%s\n' 'certificate-generation-rotated' > "$CA_ROTATED"

# Hermetic merged-bundle source: a fake certifi module so trust_mitm_ca
# never depends on host CA bundles.
FAKE_CERTIFI_PEM="$TESTDIR/certifi-bundle.pem"
printf '%s\n' 'certifi-fake-root-bundle' > "$FAKE_CERTIFI_PEM"
PYMODULE="$TESTDIR/pymod/certifi"
mkdir -p "$PYMODULE"
cat > "$PYMODULE/__init__.py" <<PYEOF
import os


def where():
    return os.environ["FAKE_CERTIFI_PEM"]
PYEOF

# Shield the host: sudo logs and pretends success (so the system-install
# branch is exercised without touching /usr/local/share/ca-certificates),
# java resolves to nothing usable, keytool is a logging stub.
cat > "$SHIM/sudo" <<STUB
#!/bin/sh
echo "sudo \$1" >> "$SHIM/sudo.log"
exit 0
STUB
cat > "$SHIM/java" <<STUB
#!/bin/sh
exit 1
STUB
cat > "$SHIM/keytool" <<STUB
#!/bin/sh
echo "keytool \$*" >> "$SHIM/keytool.log"
exit 0
STUB
chmod +x "$SHIM/sudo" "$SHIM/java" "$SHIM/keytool"

# Fixture JDK so the JDK trust branch runs against a sandboxed store.
FAKE_JDK="$TESTDIR/fake-jdk"
mkdir -p "$FAKE_JDK/bin" "$FAKE_JDK/lib/security"
cp "$SHIM/keytool" "$FAKE_JDK/bin/keytool"
: > "$FAKE_JDK/lib/security/cacerts"

# Stub daemon so bootstrap keeps supervising while the fake egress rotates
# the CA, without needing a real execd.
cat > "$TESTDIR/stub-execd" <<'STUB'
#!/bin/sh
sleep 120
STUB
chmod +x "$TESTDIR/stub-execd"

# Fake egress: replays the exact restart timeline from the incident —
#   t=0    stale export still on the volume from the previous generation
#   t=2    egress container starts: main() purges the stale export
#   t=3.5  mitmdump generated the new CA; SyncRootCA exports it
#   t=7.5  egress restarts again (independent of the sandbox): purge
#   t=8.5  rotated CA exported; the watcher must self-heal
cat > "$TESTDIR/fake-egress.sh" <<SCRIPT
#!/bin/sh
cp "$CA_STALE" "$MITM_CA"
sleep 2
rm -f "$MITM_CA"
sleep 1.5
cp "$CA_FRESH" "$MITM_CA"
sleep 4
rm -f "$MITM_CA"
sleep 1
cp "$CA_ROTATED" "$MITM_CA"
sleep 8
SCRIPT
chmod +x "$TESTDIR/fake-egress.sh"
bash "$TESTDIR/fake-egress.sh" &
FAKE_EGRESS_PID=$!

# Real bootstrap entrypoint; test-tuned timings only (no behavior overrides):
# the fake egress purges at t=2, inside the 6s stabilization window.
PATH="$SHIM:$PATH" \
HOME="$HOME_DIR" \
JAVA_HOME="$FAKE_JDK" \
PYTHONPATH="$TESTDIR/pymod" \
FAKE_CERTIFI_PEM="$FAKE_CERTIFI_PEM" \
OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=1 \
OPENSANDBOX_MITM_CA="$MITM_CA" \
OPENSANDBOX_MERGED_CA_PATH="$MERGED" \
MITM_CA_WAIT_TIMEOUT=25 \
MITM_CA_STABILIZE_WINDOW=6 \
MITM_CA_POLL_INTERVAL=0.2 \
MITM_CA_WATCH_INTERVAL=1 \
EXECD="$TESTDIR/stub-execd" \
BOOTSTRAP_CMD="sleep 14" \
bash "$BOOTSTRAP" > "$LOG" 2>&1 &
BOOTSTRAP_PID=$!

fail() {
  echo "FAIL: $1" >&2
  sed -n '1,80p' "$LOG" >&2 || true
  exit 1
}

# 1) Mid-flight: bootstrap must trust the FRESH export after detecting the
#    purge (t~3.5), not the stale one at t=0 and not via window expiry (6s).
ready_seen=""
for _ in $(seq 1 100); do
  if grep -q 'mitm CA ready at' "$LOG" 2>/dev/null; then
    ready_seen=1
    break
  fi
  sleep 0.3
done
[ -n "$ready_seen" ] || fail "bootstrap never reported the mitm CA ready"
ready_line="$(grep -o 'mitm CA ready at .* after [0-9]*s' "$LOG" | tail -n 1)"
ready_seconds="$(printf '%s' "$ready_line" | sed -e 's/.* after //' -e 's/s$//')"
[ "$ready_seconds" -ge 2 ] || fail "bootstrap trusted the CA after ${ready_seconds}s — a stale pre-existing export would be trusted instantly by the old logic"
[ "$ready_seconds" -le 6 ] || fail "bootstrap trusted the CA after ${ready_seconds}s — stale-CA purge was not detected before the stabilization window expired"
# trust_mitm_ca runs just after the ready line and needs a moment (python3
# certifi lookup) before the merged bundle lands on disk.
merged_seen=""
for _ in $(seq 1 20); do
  if [ -f "$MERGED" ] && grep -q 'certificate-generation-fresh' "$MERGED" 2>/dev/null; then
    merged_seen=1
    break
  fi
  sleep 0.3
done
[ -n "$merged_seen" ] || fail "merged bundle does not contain the fresh CA bootstrap just trusted"
grep -q 'certificate-generation-stale' "$MERGED" && fail "merged bundle contains the STALE pre-existing CA" || true

# 2) Join bootstrap (it supervises for ~14s) and assert the watcher healed
#    the second rotation without a bootstrap re-run.
set +e
wait "$BOOTSTRAP_PID"
BOOTSTRAP_STATUS=$?
set -e
[ "$BOOTSTRAP_STATUS" -eq 0 ] || fail "bootstrap exited with status $BOOTSTRAP_STATUS"

grep -q 'watching mitm CA' "$LOG" || fail "rotation watcher did not start"
grep -q 'mitm CA changed' "$LOG" || fail "watcher missed the second rotation"
grep -q 'certificate-generation-rotated' "$MERGED" || fail "watcher did not rebuild the merged bundle with the rotated CA"
grep -q 'certificate-generation-stale' "$MERGED" && fail "merged bundle contains the STALE pre-existing CA" || true

# 3) System-trust install ran once per trust pass when the platform provides
#    update-ca-certificates (Linux CI: 2 = initial + watcher; macOS: the
#    branch is skipped entirely, so 0).
install_passes=0
[ -f "$SHIM/sudo.log" ] && install_passes="$(grep -c '^sudo update-ca-certificates$' "$SHIM/sudo.log" || true)"
case "$install_passes" in
0 | 2) ;;
*) fail "expected 0 or 2 system-trust install passes, got $install_passes" ;;
esac

# 4) No export timeout: the whole point of the race fix.
grep -q 'timed out' "$LOG" && fail "bootstrap timed out waiting for the mitm CA" || true

echo "PASS: bootstrap trusted the fresh CA after purge detection (${ready_seconds}s), watcher healed the second rotation (${install_passes} trust installs)"
