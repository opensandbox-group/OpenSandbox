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

# Offline verifier: checks the result directly with coreutils, so it needs no
# network access or extra packages. It writes the reward Harbor reads from
# /logs/verifier/reward.txt (1.0 = pass, 0.0 = fail).
set -u

mkdir -p /logs/verifier

expected="Hello from OpenSandbox!"
file="/app/greeting.txt"

if [ -f "$file" ] && [ "$(cat "$file")" = "$expected" ]; then
  echo "PASS: $file has the expected contents"
  echo 1 > /logs/verifier/reward.txt
else
  echo "FAIL: $file is missing or has unexpected contents"
  echo "  expected: $expected"
  echo "  actual:   $(cat "$file" 2>/dev/null || echo '<missing>')"
  echo 0 > /logs/verifier/reward.txt
fi
