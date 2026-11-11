#!/usr/bin/env bash
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

# Run exactly named kernel tests; each enabled test creates its own netns.
set -euo pipefail

if (( $# < 2 )); then
  echo "Usage: $0 <package> <test-name> [<test-name> ...]" >&2
  exit 2
fi
package="$1"
shift
tests=("$@")
for test_name in "${tests[@]}"; do
  if [[ ! "$test_name" =~ ^Test[A-Za-z0-9_]+$ ]]; then
    echo "Invalid top-level Go test name: $test_name" >&2
    exit 2
  fi
done
command -v unshare
command -v nft
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/egress-nft-test.XXXXXX")"
trap 'rm -rf -- "$tmp_dir"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
test_binary="$tmp_dir/policy.test"
log_file="$tmp_dir/output.log"
go test -c -o "$test_binary" "$package"
for test_name in "${tests[@]}"; do
  "$test_binary" -test.list "^${test_name}$" | grep -Fx -- "$test_name"
done
test_pattern="^($(IFS='|'; echo "${tests[*]}"))$"
sudo env OPENSANDBOX_NFT_TEST=1 \
  "$test_binary" -test.run "$test_pattern" -test.v -test.timeout=5m | tee "$log_file"
if grep -q -- '--- SKIP:' "$log_file"; then
  echo "Nft regressions must not skip in privileged CI" >&2
  exit 1
fi
for test_name in "${tests[@]}"; do
  grep -Fx -- "=== RUN   $test_name" "$log_file"
  grep -E "^--- PASS: ${test_name} \\([0-9.]+s\\)$" "$log_file"
done
