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

# Regression test: a workload image without certutil must receive an
# actionable Chromium/Chrome NSS warning without making bootstrap fail.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"
TESTDIR="$(mktemp -d)"
trap 'rm -rf "$TESTDIR"' EXIT

mkdir -p "$TESTDIR/home" "$TESTDIR/empty-bin"
printf '%s\n' 'test certificate' > "$TESTDIR/mitm-ca.pem"

# Load the production helper without executing the rest of bootstrap.sh.
awk '
  /^trust_mitm_ca_nss\(\) \{/ { capture = 1 }
  capture { print }
  capture && /^}/ { exit }
' "$BOOTSTRAP" > "$TESTDIR/trust_mitm_ca_nss.sh"
# shellcheck source=/dev/null
. "$TESTDIR/trust_mitm_ca_nss.sh"

assert_missing_certutil_warns() {
  label="$1"
  home_mode="$2"
  stderr="$TESTDIR/stderr-$label"

  set +e
  case "$home_mode" in
    valid)
      HOME="$TESTDIR/home" PATH="$TESTDIR/empty-bin" \
        trust_mitm_ca_nss "$TESTDIR/mitm-ca.pem" 2> "$stderr"
      ;;
    unset)
      (unset HOME; PATH="$TESTDIR/empty-bin" \
        trust_mitm_ca_nss "$TESTDIR/mitm-ca.pem") 2> "$stderr"
      ;;
    nonexistent)
      HOME="$TESTDIR/missing-home" PATH="$TESTDIR/empty-bin" \
        trust_mitm_ca_nss "$TESTDIR/mitm-ca.pem" 2> "$stderr"
      ;;
    *)
      echo "FAIL: unknown HOME mode $home_mode" >&2
      exit 1
      ;;
  esac
  status=$?
  set -e

  if [ "$status" -ne 0 ]; then
    echo "FAIL: $label made NSS trust setup fail with status $status" >&2
    exit 1
  fi
  if ! grep -q 'certutil not found' "$stderr"; then
    echo "FAIL: $label did not emit an actionable warning" >&2
    exit 1
  fi
  if ! grep -q 'nss-tools' "$stderr"; then
    echo "FAIL: $label did not name the Alpine nss-tools package" >&2
    exit 1
  fi
  if ! grep -q 'libnss3-tools' "$stderr"; then
    echo "FAIL: $label did not name the Debian/Ubuntu libnss3-tools package" >&2
    exit 1
  fi
}

assert_missing_certutil_warns 'valid HOME' valid
assert_missing_certutil_warns 'unset HOME' unset
assert_missing_certutil_warns 'nonexistent HOME' nonexistent

echo "PASS: missing certutil warns with package guidance and remains best-effort"
