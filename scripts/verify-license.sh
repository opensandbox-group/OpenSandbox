#!/bin/bash
# Copyright 2025 Alibaba Group Holding Ltd.
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

# This script verifies that required files contain the Apache 2.0 license header.
# It scans tracked source files and fails with a list of violations if any header
# is missing.

set -euo pipefail

# Print CI diagnostics
echo "License verification started at: $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo "Runner: $(hostname) ($(uname -srm))"
echo "User: $(whoami)"
echo "Working directory: $(pwd)"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CURRENT_YEAR="$(date +%Y)"
MIN_YEAR="2025"
LICENSE_OWNER="Alibaba Group Holding Ltd."
LICENSE_REGEX="Copyright [0-9]{4} ${LICENSE_OWNER// / }"

# File extensions that are expected to carry a license header.
LICENSE_EXTS=(
  go py sh kt kts java ts tsx js jsx toml html css sql tf
)

# Explicit file basenames that should also be checked (e.g., Dockerfile)
LICENSE_BASENAMES=(
  Dockerfile
)

# Paths to ignore entirely.
IGNORED_PATHS=(
  "LICENSE"
  "NOTICE"
  "docs/"
  "scripts/spec-doc/index.html" # Generated doc
)

is_k8s_mock_go() {
  local file="${1-}"
  [[ -z "$file" ]] && return 1
  if [[ "$file" != kubernetes/internal/* ]]; then
    return 1
  fi
  if [[ "$file" == *"_mock.go" ]]; then
    return 0
  fi
  if [[ "$file" == */mock/*.go ]]; then
    return 0
  fi
  return 1
}

is_generated_to_skip() {
  local file="$1"
  if [[ "$file" == *"deepcopy.go" ]]; then
    return 0
  fi
  return 1
}

cd "$REPO_ROOT"

is_ignored() {
  local file="$1"
  for ignore in "${IGNORED_PATHS[@]}"; do
    if [[ "$ignore" == */ ]]; then
      if [[ "$file" == "$ignore"* ]]; then
        return 0
      fi
    elif [[ "$file" == "$ignore" ]]; then
      return 0
    fi
  done
  return 1
}

has_expected_extension() {
  local file="$1"
  local ext="${file##*.}"
  for candidate in "${LICENSE_EXTS[@]}"; do
    if [[ "$ext" == "$candidate" ]]; then
      return 0
    fi
  done
  return 1
}

has_expected_basename() {
  local file="$1"
  local base
  base="$(basename "$file")"
  for candidate in "${LICENSE_BASENAMES[@]}"; do
    if [[ "$base" == "$candidate" ]]; then
      return 0
    fi
  done
  return 1
}

missing=()

while IFS= read -r file; do
  if is_ignored "$file"; then
    continue
  fi
  if is_k8s_mock_go "$file"; then
    continue
  fi
  if is_generated_to_skip "$file"; then
    continue
  fi

  if ! has_expected_extension "$file" && ! has_expected_basename "$file"; then
    continue
  fi

  header="$(head -n 25 "$file")"
  if ! echo "$header" | grep -Eq "$LICENSE_REGEX"; then
    missing+=("$file")
    continue
  fi
  found_year="$(echo "$header" | grep -Eo "$LICENSE_REGEX" | head -n1 | grep -Eo '[0-9]{4}')"
  if [[ -z "$found_year" || "$found_year" -gt "$CURRENT_YEAR" || "$found_year" -lt "$MIN_YEAR" ]]; then
    missing+=("$file")
  fi
done < <(git -C "$REPO_ROOT" ls-files)

if ((${#missing[@]} > 0)); then
  echo "Missing license header in the following files:"
  printf ' - %s\n' "${missing[@]}"
  exit 1
fi

echo "License headers verified."
