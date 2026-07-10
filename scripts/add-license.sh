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

# Add Apache 2.0 license headers to source files that are missing them.
# Usage: run from repo root: ./scripts/add-license.sh

set -euo pipefail

LICENSE_YEAR=$(date +%Y)
LICENSE_OWNER="Alibaba Group Holding Ltd."
LICENSE_MARKER_REGEX="Copyright [0-9]{4} ${LICENSE_OWNER// / }"
LICENSE_TEXT_TEMPLATE=$(
  cat <<'EOF'
Copyright __YEAR__ Alibaba Group Holding Ltd.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
EOF
)

# Basenames to include regardless of extension.
INCLUDE_BASENAMES=("Dockerfile")

# Paths to ignore.
IGNORES=(
  ".git"
  "node_modules"
  ".venv"
  "venv"
  "dist"
  "build"
  "__pycache__"
  "LICENSE"
  "NOTICE"
  "README.md"
  "README_zh.md"
)

has_license() {
  local file="$1"
  head -n 40 "$file" | grep -Eq "$LICENSE_MARKER_REGEX"
}

comment_header() {
  local style="$1"
  local text="$2"
  case "$style" in
    "line:#")
      printf '%s\n' "$text" | sed 's/^/# /'
      ;;
    "line://")
      printf '%s\n' "$text" | sed 's:^:// :'
      ;;
    "block:html")
      printf "<!--\n%s\n-->\n" "$text"
      ;;
    "block:css")
      printf "/*\n%s\n*/\n" "$text"
      ;;
    *)
      return 1
      ;;
  esac
}

should_ignore_path() {
  local file="$1"
  for ignore in "${IGNORES[@]}"; do
    if [[ "$file" == "$ignore" || "$file" == "$ignore"/* ]]; then
      return 0
    fi
  done
  return 1
}

is_k8s_mock_go() {
  local file="${1-}"
  [[ -z "$file" ]] && return 1
  # Skip any Go mocks under kubernetes/internal:
  # - filenames ending with _mock.go
  # - any file under a /mock/ directory
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

is_generated_go() {
  local file="${1-}"
  [[ -z "$file" ]] && return 1
  [[ "${file##*.}" != "go" ]] && return 1

  local base
  base="$(basename "$file")"
  case "$base" in
    zz_generated.*|*.pb.go|*_pb.go|*_gen.go|*_generated.go)
      return 0
      ;;
  esac

  if head -n 5 "$file" | grep -qi "code generated"; then
    return 0
  fi

  return 1
}

style_for_file() {
  local file="${1-}"
  [[ -z "$file" ]] && { echo ""; return; }
  local base ext
  base="$(basename "$file")"
  ext="${file##*.}"

  case "$ext" in
    sh|py|toml|tf|sql) echo "line:#"; return ;;
    go|java|kt|kts|ts|tsx|js|jsx|mjs|cjs|mts|cts) echo "line://"; return ;;
    css) echo "block:css"; return ;;
    html) echo "block:html"; return ;;
  esac

  for b in "${INCLUDE_BASENAMES[@]}"; do
    if [[ "$base" == "$b" ]]; then
      echo "line:#"
      return
    fi
  done

  echo ""
}

process_file() {
  local file="${1-}"
  [[ -z "$file" ]] && return
  local style

  if should_ignore_path "$file"; then
    return
  fi

  if is_k8s_mock_go "$file"; then
    return
  fi

  style="$(style_for_file "$file")"
  [[ -z "$style" ]] && return

  if is_generated_go "$file"; then
    return
  fi

  if has_license "$file"; then
    return
  fi

  local license_text header
  license_text="${LICENSE_TEXT_TEMPLATE/__YEAR__/$LICENSE_YEAR}"
  header="$(comment_header "$style" "$license_text")"

  # Respect shebang: insert after the first line if it starts with #!
  if head -n1 "$file" | grep -q "^#!"; then
    local first rest
    first="$(head -n1 "$file")"
    rest="$(tail -n +2 "$file")"
    printf '%s\n\n%s\n\n%s' "$first" "$header" "$rest" >"$file"
  # Place before DOCTYPE for HTML to avoid breaking rendering.
  elif head -n1 "$file" | grep -qi "^<!doctype"; then
    local body
    body="$(cat "$file")"
    printf '%s\n\n%s' "$header" "$body" >"$file"
  else
    local body
    body="$(cat "$file")"
    printf '%s\n\n%s' "$header" "$body" >"$file"
  fi
  echo "Added license: $file"
}

main() {
  local files
  if [[ "$#" -gt 0 ]]; then
    IFS=$'\n' read -r -d '' -a files < <(git ls-files -- "$@" && printf '\0')
  else
    IFS=$'\n' read -r -d '' -a files < <(git ls-files && printf '\0')
  fi
  for f in "${files[@]}"; do
    process_file "$f"
  done
}

main "$@"
