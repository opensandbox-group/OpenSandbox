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

# End-to-end smoke: run the REAL bootstrap.sh against a fake egress replaying
# the incident timeline (stale export -> purge -> fresh export, then a second
# purge + rotation while the sandbox keeps running). Asserts that bootstrap
# trusts the fresh export after purge detection (never the stale one), and
# that the watcher heals the second rotation without a bootstrap re-run. The
# host is shielded via stub sudo/java/keytool, a fixture JDK, and a fake
# certifi module; trust_mitm_ca_jdk may still scan /usr/lib/jvm on CI, where
# ephemeral runner JDKs may pick up a junk alias (JDK logic itself is covered
# hermetically by test_mitm_ca_jdk.py).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"
TESTDIR="$(mktemp -d)"
cleanup() {
  # The watcher (a pre-exec subshell sharing bootstrap's argv) and the stub
  # execd outlive bootstrap by design.
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
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'certificate-generation-stale' '-----END CERTIFICATE-----' > "$CA_STALE"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'certificate-generation-fresh' '-----END CERTIFICATE-----' > "$CA_FRESH"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'certificate-generation-rotated' '-----END CERTIFICATE-----' > "$CA_ROTATED"

# Hermetic merged-bundle source: fake certifi module, never host CA bundles.
FAKE_CERTIFI_PEM="$TESTDIR/certifi-bundle.pem"
printf '%s\n' 'certifi-fake-root-bundle' > "$FAKE_CERTIFI_PEM"
PYMODULE="$TESTDIR/pymod/certifi"
mkdir -p "$PYMODULE"
cat > "$PYMODULE/__init__.py" <<PYEOF
import os


def where():
    return os.environ["FAKE_CERTIFI_PEM"]
PYEOF

# Shield the host, for root and non-root runs alike: as root, bootstrap's
# _sudo executes directly and bypasses the sudo stub, so the trust commands
# themselves must be stubbed too (cp and update-ca-* are only used by the
# trust paths inside bootstrap; the fake egress runs outside this PATH).
cat > "$SHIM/sudo" <<STUB
#!/bin/sh
echo "sudo \$1" >> "$SHIM/sudo.log"
if [ "\${FAIL_KEYTOOL_IMPORT:-0}" = "1" ]; then
  for _arg in "\$@"; do
    if [ "\$_arg" = "-importcert" ]; then
      exit 1
    fi
  done
fi
exit 0
STUB
cat > "$SHIM/cp" <<'STUB'
#!/bin/sh
exit 0
STUB
cat > "$SHIM/update-ca-certificates" <<STUB
#!/bin/sh
echo "invoked" >> "$SHIM/update-ca.log"
STUB
cat > "$SHIM/update-ca-trust" <<'STUB'
#!/bin/sh
exit 0
STUB
cat > "$SHIM/java" <<STUB
#!/bin/sh
exit 1
STUB
cat > "$SHIM/keytool" <<STUB
#!/bin/sh
echo "keytool \$*" >> "$SHIM/keytool.log"
if [ "\${FAIL_KEYTOOL_IMPORT:-0}" = "1" ]; then
  for _arg in "\$@"; do
    if [ "\$_arg" = "-importcert" ]; then
      exit 1
    fi
  done
fi
exit 0
STUB
cat > "$SHIM/certutil" <<STUB
#!/bin/sh
mode=""
db=""
nick=""
cert=""
while [ \$# -gt 0 ]; do
  case "\$1" in
    -N|-D|-A|-L) mode="\$1"; shift ;;
    -d) db="\${2#sql:}"; db="\${db#dbm:}"; shift 2 ;;
    -n) nick="\$2"; shift 2 ;;
    -i) cert="\$2"; shift 2 ;;
    -t) shift 2 ;;
    *) shift ;;
  esac
done
case "\$mode" in
  -N) : > "\$db/cert9.db" ;;
  -D) rm -f "\$db/\$nick.pem" ;;
  -A)
    if [ -n "\${FAIL_CERTUTIL_ADD_ONCE:-}" ] && [ -f "\$FAIL_CERTUTIL_ADD_ONCE" ]; then
      rm -f "\$FAIL_CERTUTIL_ADD_ONCE"
      exit 1
    fi
    echo "certutil -A \$nick" >> "$SHIM/certutil.log"; cat "\$cert" > "\$db/\$nick.pem" ;;
  -L) [ -f "\$db/\$nick.pem" ] || exit 1; cat "\$db/\$nick.pem" ;;
esac
exit 0
STUB
chmod +x "$SHIM/sudo" "$SHIM/cp" "$SHIM/update-ca-certificates" "$SHIM/update-ca-trust" "$SHIM/java" "$SHIM/keytool" "$SHIM/certutil"

# Fixture JDK: keeps the JDK trust branch in a sandboxed store.
FAKE_JDK="$TESTDIR/fake-jdk"
mkdir -p "$FAKE_JDK/bin" "$FAKE_JDK/lib/security"
cp "$SHIM/keytool" "$FAKE_JDK/bin/keytool"
: > "$FAKE_JDK/lib/security/cacerts"

# Stub execd: keeps bootstrap supervising without a real daemon.
cat > "$TESTDIR/stub-execd" <<'STUB'
#!/bin/sh
sleep 120
STUB
chmod +x "$TESTDIR/stub-execd"

# Fake egress: replays the incident timeline — stale export at t=0, purge at
# t=2, fresh export at t=3.5, second purge at t=7.5 and rotated export at
# t=8.5 (the watcher must heal the rotation in the same sandbox lifetime).
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
# Pre-seed the stale export synchronously before starting bootstrap so Phase 1
# deterministically exercises the stale-CA-present-at-t=0 timeline.
cp "$CA_STALE" "$MITM_CA"
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
OPENSANDBOX_MITM_CA_STATE_FILE="$TESTDIR/phase1.fp" \
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

# 1) Mid-flight: the fresh export must be trusted via purge detection
#    (not the stale one at t=0, not via window expiry).
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
# trust_mitm_ca lands the merged bundle just after the ready line.
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

# 2) The watcher heals the second rotation without a bootstrap re-run.
set +e
wait "$BOOTSTRAP_PID"
BOOTSTRAP_STATUS=$?
set -e
[ "$BOOTSTRAP_STATUS" -eq 0 ] || fail "bootstrap exited with status $BOOTSTRAP_STATUS"

grep -q 'watching mitm CA' "$LOG" || fail "rotation watcher did not start"
grep -q 'mitm CA changed' "$LOG" || fail "watcher missed the second rotation"
grep -q 'certificate-generation-rotated' "$MERGED" || fail "watcher did not rebuild the merged bundle with the rotated CA"
grep -q 'certificate-generation-stale' "$MERGED" && fail "merged bundle contains the STALE pre-existing CA" || true

# 3) System-trust installs: 2 per trusted generation (initial + watcher),
#    counted via the sudo stub on non-root runs and via the
#    update-ca-certificates stub when root bypasses it; 0 where the
#    system-install branch is skipped entirely (no tooling).
install_passes=0
if [ -f "$SHIM/sudo.log" ]; then
  install_passes="$(grep -c '^sudo update-ca-certificates$' "$SHIM/sudo.log" || true)"
fi
if [ -f "$SHIM/update-ca.log" ]; then
  uc_passes="$(grep -c 'invoked' "$SHIM/update-ca.log" || true)"
  install_passes=$((install_passes + uc_passes))
fi
case "$install_passes" in
0 | 2) ;;
*) fail "expected 0 or 2 system-trust install passes, got $install_passes" ;;
esac

# 4) No export timeout.
grep -q 'timed out' "$LOG" && fail "bootstrap timed out waiting for the mitm CA" || true

# ---- Phase 2: a wait timeout with a pre-existing CA must leave the watcher
# baseline empty. Regression: the wait function's stabilization tracker once
# leaked into the baseline global on the timeout path, seeding the watcher
# with the untrusted CA so nothing was ever installed. Here the timeout fires
# while the CA is present (timeout < stabilize window), and the watcher must
# install that CA immediately (empty baseline) and then the rotation.
VOL2="$TESTDIR/volume2"
MITM_CA2="$VOL2/mitmproxy-ca-cert.pem"
MERGED2="$VOL2/merged-ca-certificates.pem"
LOG2="$TESTDIR/bootstrap-timeout.log"
mkdir -p "$VOL2"
printf '%s\n' 'certificate-timeout-gen1' > "$MITM_CA2" # pre-existing export

PATH="$SHIM:$PATH" \
HOME="$HOME_DIR" \
JAVA_HOME="$FAKE_JDK" \
PYTHONPATH="$TESTDIR/pymod" \
FAKE_CERTIFI_PEM="$FAKE_CERTIFI_PEM" \
OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=1 \
OPENSANDBOX_MITM_CA="$MITM_CA2" \
OPENSANDBOX_MERGED_CA_PATH="$MERGED2" \
OPENSANDBOX_MITM_CA_STATE_FILE="$TESTDIR/phase2.fp" \
MITM_CA_WAIT_TIMEOUT=2 \
MITM_CA_STABILIZE_WINDOW=20 \
MITM_CA_POLL_INTERVAL=0.2 \
MITM_CA_WATCH_INTERVAL=0.5 \
EXECD="$TESTDIR/stub-execd" \
BOOTSTRAP_CMD="sleep 10" \
bash "$BOOTSTRAP" > "$LOG2" 2>&1 &
BOOTSTRAP2_PID=$!

sleep 3 # the timeout (t=2) has fired and the watcher is armed
# Empty baseline must install the pre-existing CA on the first poll — under
# the baseline leak this poll sees cur == prev and never installs.
gen1_seen=""
for _ in $(seq 1 15); do
  if [ -f "$MERGED2" ] && grep -q 'certificate-timeout-gen1' "$MERGED2" 2>/dev/null; then
    gen1_seen=1
    break
  fi
  sleep 0.3
done
[ -n "$gen1_seen" ] || fail "phase 2: watcher never installed the pre-existing CA (baseline leak on the timeout path)"

printf '%s\n' 'certificate-timeout-gen2' > "$MITM_CA2"

set +e
wait "$BOOTSTRAP2_PID"
BOOTSTRAP2_STATUS=$?
set -e
[ "$BOOTSTRAP2_STATUS" -eq 0 ] || fail "phase 2: bootstrap exited with status $BOOTSTRAP2_STATUS"

grep -q 'timed out after 2s' "$LOG2" || fail "phase 2: the wait did not time out as intended"
grep -q 'mitm CA changed' "$LOG2" || fail "phase 2: watcher did not install after the post-timeout change"
merged2_seen=""
for _ in $(seq 1 20); do
  if [ -f "$MERGED2" ] && grep -q 'certificate-timeout-gen2' "$MERGED2" 2>/dev/null; then
    merged2_seen=1
    break
  fi
  sleep 0.3
done
[ -n "$merged2_seen" ] || fail "phase 2: watcher did not rebuild the merged bundle with the post-timeout CA"

# ---- Phase 3: statically baked CA fast path (Firecracker RootfsOnly cold boot).
# With MITM_CA_STABILIZE_WINDOW=0 and MITM_CA_WATCH_INTERVAL=0:
#   - Boot 1 installs once, records the rootfs fingerprint marker, and does not
#     start the background watcher.
#   - Boot 2 with the same CA + persisted marker skips redundant system/JDK
#     reinstalls while still exporting NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE,
#     and SSL_CERT_FILE.
#   - Boot 3 with a rotated CA detects the fingerprint drift and reinstalls.
VOL3="$TESTDIR/volume3"
MITM_CA3="$VOL3/mitmproxy-ca-cert.pem"
MERGED3="$VOL3/merged-ca-certificates.pem"
STATE3="$TESTDIR/phase3.fp"
LOG3_1="$TESTDIR/bootstrap-p3-1.log"
LOG3_2="$TESTDIR/bootstrap-p3-2.log"
LOG3_3="$TESTDIR/bootstrap-p3-3.log"
ENV3_2="$TESTDIR/p3-2.env"
mkdir -p "$VOL3"
cp "$CA_FRESH" "$MITM_CA3"

count_trust_runs() {
  _c=0
  for _f in "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"; do
    if [ -f "$_f" ]; then
      _c=$((_c + $(wc -l < "$_f" | tr -d ' ')))
    fi
  done
  echo "$_c"
}

run_p3_boot() {
  _out_log="$1"
  _cmd="$2"
  PATH="$SHIM:$PATH" \
  HOME="$HOME_DIR" \
  JAVA_HOME="${P3_JAVA_HOME:-$FAKE_JDK}" \
  PYTHONPATH="$TESTDIR/pymod" \
  FAKE_CERTIFI_PEM="$FAKE_CERTIFI_PEM" \
  OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=1 \
  OPENSANDBOX_MITM_CA="$MITM_CA3" \
  OPENSANDBOX_MERGED_CA_PATH="$MERGED3" \
  OPENSANDBOX_MITM_CA_STATE_FILE="$STATE3" \
  MITM_CA_WAIT_TIMEOUT=10 \
  MITM_CA_STABILIZE_WINDOW=0 \
  MITM_CA_POLL_INTERVAL=5 \
  MITM_CA_WATCH_INTERVAL=0 \
  EXECD="$TESTDIR/stub-execd" \
  BOOTSTRAP_CMD="$_cmd" \
  bash "$BOOTSTRAP" > "$_out_log" 2>&1
}

rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
run_p3_boot "$LOG3_1" "true" || fail "phase 3 boot 1 failed"
[ -s "$STATE3" ] || fail "phase 3 boot 1: installed CA marker not written"
[ -s "$MERGED3" ] || fail "phase 3 boot 1: merged bundle missing"
grep -q 'mitm CA ready at .* after 0s' "$LOG3_1" || fail "phase 3 boot 1: did not trust immediately"
grep -q 'watcher disabled' "$LOG3_1" || fail "phase 3 boot 1: watcher was not disabled"
boot1_trust="$(count_trust_runs)"
[ "$boot1_trust" -ge 1 ] || fail "phase 3 boot 1: expected system trust install, got $boot1_trust"

rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
run_p3_boot "$LOG3_2" "printf 'NODE=%s\nREQ=%s\nSSL=%s\n' \"\$NODE_EXTRA_CA_CERTS\" \"\$REQUESTS_CA_BUNDLE\" \"\$SSL_CERT_FILE\" > \"$ENV3_2\"" \
  || fail "phase 3 boot 2 failed"
grep -q 'skipping reinstall' "$LOG3_2" || fail "phase 3 boot 2: did not take idempotent skip path"
boot2_trust="$(count_trust_runs)"
[ "$boot2_trust" -eq 0 ] || fail "phase 3 boot 2: expected 0 system trust installs on cold-boot skip, got $boot2_trust"
[ ! -s "$SHIM/keytool.log" ] || fail "phase 3 boot 2: keytool should not run on cold-boot skip"
[ ! -s "$SHIM/certutil.log" ] || fail "phase 3 boot 2: certutil -A should not run on cold-boot skip"
grep -q "^NODE=$MITM_CA3$" "$ENV3_2" || fail "phase 3 boot 2: NODE_EXTRA_CA_CERTS not exported"
grep -q "^REQ=$MERGED3$" "$ENV3_2" || fail "phase 3 boot 2: REQUESTS_CA_BUNDLE not exported"
grep -q "^SSL=$MERGED3$" "$ENV3_2" || fail "phase 3 boot 2: SSL_CERT_FILE not exported"

# Boot 2b: a new JDK installed after the initial bootstrap (e.g., before a
# RootfsOnly snapshot) must invalidate the skip path so its cacerts receives
# the CA on restore.
FAKE_JDK2="$TESTDIR/fake-jdk-added"
mkdir -p "$FAKE_JDK2/bin" "$FAKE_JDK2/lib/security"
cp "$SHIM/keytool" "$FAKE_JDK2/bin/keytool"
printf 'new-jdk-cacerts\n' > "$FAKE_JDK2/lib/security/cacerts"
LOG3_2B="$TESTDIR/bootstrap-p3-2b.log"
rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
P3_JAVA_HOME="$FAKE_JDK2" run_p3_boot "$LOG3_2B" "true" || fail "phase 3 boot 2b failed"
grep -q 'skipping reinstall' "$LOG3_2B" && fail "phase 3 boot 2b: must reinstall when a new JDK appears" || true
[ -s "$SHIM/keytool.log" ] || fail "phase 3 boot 2b: keytool must run for the newly installed JDK"

# Boot 2c: overwriting the NSS alias with a different certificate must
# invalidate the skip path even though the alias still exists.
LOG3_2C="$TESTDIR/bootstrap-p3-2c.log"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'certificate-other-nss' '-----END CERTIFICATE-----' > "$HOME_DIR/.pki/nssdb/opensandbox-mitmproxy.pem"
rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
P3_JAVA_HOME="$FAKE_JDK2" run_p3_boot "$LOG3_2C" "true" || fail "phase 3 boot 2c failed"
grep -q 'skipping reinstall' "$LOG3_2C" && fail "phase 3 boot 2c: must reinstall when NSS alias cert drifts" || true
[ -s "$SHIM/certutil.log" ] || fail "phase 3 boot 2c: certutil -A must run when NSS alias cert drifts"

# Boot 2d: a failed JDK import must NOT persist the marker, so the next boot
# retries instead of skipping broken Java trust.
LOG3_2D_1="$TESTDIR/bootstrap-p3-2d-1.log"
LOG3_2D_2="$TESTDIR/bootstrap-p3-2d-2.log"
rm -f "$STATE3" "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
FAIL_KEYTOOL_IMPORT=1 P3_JAVA_HOME="$FAKE_JDK2" run_p3_boot "$LOG3_2D_1" "true" || fail "phase 3 boot 2d-1 failed"
[ ! -e "$STATE3" ] || fail "phase 3 boot 2d-1: failed JDK import must not persist installed marker"
rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
P3_JAVA_HOME="$FAKE_JDK2" run_p3_boot "$LOG3_2D_2" "true" || fail "phase 3 boot 2d-2 failed"
grep -q 'skipping reinstall' "$LOG3_2D_2" && fail "phase 3 boot 2d-2: must retry after prior failed JDK import" || true
[ -s "$SHIM/keytool.log" ] || fail "phase 3 boot 2d-2: keytool must run on retry"
[ -s "$STATE3" ] || fail "phase 3 boot 2d-2: marker should be written once all stores succeed"

rm -f "$SHIM/sudo.log" "$SHIM/update-ca.log" "$SHIM/keytool.log" "$SHIM/certutil.log"
cp "$CA_ROTATED" "$MITM_CA3"
run_p3_boot "$LOG3_3" "true" || fail "phase 3 boot 3 failed"
grep -q 'skipping reinstall' "$LOG3_3" && fail "phase 3 boot 3: must reinstall when CA changes" || true
boot3_trust="$(count_trust_runs)"
[ "$boot3_trust" -ge 1 ] || fail "phase 3 boot 3: expected reinstall on rotated CA, got $boot3_trust"
grep -q 'certificate-generation-rotated' "$MERGED3" || fail "phase 3 boot 3: merged bundle not updated"

# ---- Phase 4: a failed NSS import at bootstrap must be retried by the
# watcher. certutil -D has already dropped the previous alias, so without a
# retry the NSS store trusts no MITM CA for the rest of the sandbox lifetime.
VOL4="$TESTDIR/volume4"
MITM_CA4="$VOL4/mitmproxy-ca-cert.pem"
LOG4="$TESTDIR/bootstrap-nss-retry.log"
NSS_CERT="$HOME_DIR/.pki/nssdb/opensandbox-mitmproxy.pem"
mkdir -p "$VOL4"
printf '%s\n' '-----BEGIN CERTIFICATE-----' 'certificate-nss-retry' '-----END CERTIFICATE-----' > "$MITM_CA4"
: > "$TESTDIR/certutil-fail-once"

FAIL_CERTUTIL_ADD_ONCE="$TESTDIR/certutil-fail-once" \
PATH="$SHIM:$PATH" \
HOME="$HOME_DIR" \
JAVA_HOME="$FAKE_JDK" \
PYTHONPATH="$TESTDIR/pymod" \
FAKE_CERTIFI_PEM="$FAKE_CERTIFI_PEM" \
OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=1 \
OPENSANDBOX_MITM_CA="$MITM_CA4" \
OPENSANDBOX_MERGED_CA_PATH="$VOL4/merged-ca-certificates.pem" \
OPENSANDBOX_MITM_CA_STATE_FILE="$TESTDIR/phase4.fp" \
MITM_CA_WAIT_TIMEOUT=10 \
MITM_CA_STABILIZE_WINDOW=0 \
MITM_CA_POLL_INTERVAL=0.2 \
MITM_CA_WATCH_INTERVAL=0.5 \
EXECD="$TESTDIR/stub-execd" \
BOOTSTRAP_CMD="true" \
bash "$BOOTSTRAP" > "$LOG4" 2>&1 || fail "phase 4: bootstrap failed"

grep -q 'failed to import mitm CA into NSS' "$LOG4" || fail "phase 4: the bootstrap NSS import did not fail as intended"
nss_retried=""
for _ in $(seq 1 20); do
  if grep -q 'certificate-nss-retry' "$NSS_CERT" 2>/dev/null; then
    nss_retried=1
    break
  fi
  sleep 0.3
done
[ -n "$nss_retried" ] || fail "phase 4: watcher did not retry the NSS import that failed at bootstrap"

echo "PASS: bootstrap trusted the fresh CA after purge detection (${ready_seconds}s), watcher healed the second rotation (${install_passes} trust installs), timeout path leaves an empty baseline, baked-CA fast path skips redundant reinstalls, and a failed NSS import is retried"
