#!/bin/bash
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
