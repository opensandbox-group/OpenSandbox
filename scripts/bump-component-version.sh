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

# Bump component image versions across the project (image refs like component:vX.Y.Z).
# For ingress, also updates gateway image tag in kubernetes/charts/opensandbox-server/values.yaml.
# Usage: from repo root:
#   ./scripts/bump-component-version.sh egress v1.0.2
#   ./scripts/bump-component-version.sh execd v1.0.7
#   ./scripts/bump-component-version.sh ingress v1.0.6
#   ./scripts/bump-component-version.sh v1.0.2              # same as: egress v1.0.2

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Parse args: COMPONENT NEW_VERSION  or  NEW_VERSION (default component: egress)
COMPONENT=""
NEW_VERSION=""
if [ $# -eq 1 ]; then
  COMPONENT="egress"
  NEW_VERSION="$1"
elif [ $# -eq 2 ]; then
  COMPONENT="$1"
  NEW_VERSION="$2"
else
  echo "Usage: $0 [egress|execd|ingress|code-interpreter|image-committer|nodeagent] NEW_VERSION" >&2
  echo "       $0 NEW_VERSION   # bumps egress" >&2
  echo "Example: $0 egress v1.0.2" >&2
  echo "Example: $0 execd 1.0.7" >&2
  echo "Example: $0 ingress v1.0.6" >&2
  echo "Example: $0 image-committer v0.1.0" >&2
  exit 1
fi

case "$COMPONENT" in
  egress|execd|ingress|code-interpreter|image-committer|nodeagent) ;;
  *)
    echo "Error: unsupported component: $COMPONENT" >&2
    exit 0
    ;;
esac

# Normalize version: ensure 'v' prefix
if [[ ! "$NEW_VERSION" =~ ^v ]]; then
  NEW_VERSION="v${NEW_VERSION}"
fi
updated=0
matched=0
tmpfile=""
active_file=""
scanfile=""

cleanup_tmpfile() {
  if [ -n "$tmpfile" ]; then
    if [ -n "$active_file" ]; then
      cp "$tmpfile" "$active_file" || echo "Error: failed to restore $active_file" >&2
    fi
    rm -f -- "$tmpfile"
  fi
  if [ -n "$scanfile" ]; then
    rm -f -- "$scanfile"
  fi
}
trap cleanup_tmpfile EXIT

if [ "$COMPONENT" = "nodeagent" ]; then
  if [[ ! "$NEW_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
    echo "Error: invalid nodeagent version: $NEW_VERSION" >&2
    exit 1
  fi
  CHART_VALUES="kubernetes/charts/opensandbox-node-agent/values.yaml"
  if [ ! -f "$CHART_VALUES" ]; then
    echo "Error: missing $CHART_VALUES" >&2
    exit 1
  fi
  NODEAGENT_REPO='repository: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/nodeagent'
  if ! grep -qF "$NODEAGENT_REPO" "$CHART_VALUES"; then
    echo "Error: expected nodeagent repository line not found in $CHART_VALUES" >&2
    exit 1
  fi
  tmpfile="$(mktemp)"
  cp "$CHART_VALUES" "$tmpfile"
  active_file="$CHART_VALUES"
  if ! NEW_VERSION="$NEW_VERSION" perl -i -0pe '
    $matched = s{(^([ \t]+)repository:[ \t]+sandbox-registry\.cn-zhangjiakou\.cr\.aliyuncs\.com/opensandbox/nodeagent[ \t]*\n(?:(?:\2[^\n]*|[ \t]*(?:#[^\n]*)?)\n)*?\2tag:[ \t]+")[^"\n]*(")}{$1 . $ENV{NEW_VERSION} . $3}em;
    END { exit 1 unless $matched }
  ' "$CHART_VALUES"; then
    echo "Error: failed to update nodeagent image tag in $CHART_VALUES" >&2
    cp "$tmpfile" "$CHART_VALUES"
    exit 1
  fi
  if ! cmp -s "$CHART_VALUES" "$tmpfile"; then
    echo "Updated $CHART_VALUES (nodeagent image tag)"
    updated=$((updated + 1))
  else
    echo "$CHART_VALUES already uses $NEW_VERSION"
  fi
  matched=1
  active_file=""
  rm -f "$tmpfile"
  tmpfile=""
fi

# Helm values: gateway ingress image uses repository + tag (not ingress:vX in one string).
CHART_VALUES="kubernetes/charts/opensandbox-server/values.yaml"
if [ "$COMPONENT" = "ingress" ]; then
  INGRESS_REPO='repository: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress'
  if [ ! -f "$CHART_VALUES" ]; then
    echo "Error: missing $CHART_VALUES" >&2
    exit 1
  fi
  if ! grep -qF "$INGRESS_REPO" "$CHART_VALUES"; then
    echo "Error: expected ingress gateway repository line not found in $CHART_VALUES" >&2
    exit 1
  fi
  tmpfile="$(mktemp)"
  cp "$CHART_VALUES" "$tmpfile"
  active_file="$CHART_VALUES"
  if ! NEW_VERSION="$NEW_VERSION" perl -i -0pe '
  $matched = s{(^([ \t]+)repository:[ \t]+sandbox-registry\.cn-zhangjiakou\.cr\.aliyuncs\.com/opensandbox/ingress[ \t]*\n(?:(?:\2[^\n]*|[ \t]*(?:#[^\n]*)?)\n)*?\2tag:[ \t]+")[^"\n]*(")}{$1 . $ENV{NEW_VERSION} . $3}em;
  END { exit 1 unless $matched }
  ' "$CHART_VALUES"; then
    echo "Error: failed to update ingress image tag in $CHART_VALUES" >&2
    cp "$tmpfile" "$CHART_VALUES"
    exit 1
  fi
  if ! cmp -s "$CHART_VALUES" "$tmpfile"; then
    echo "Updated $CHART_VALUES (server.gateway.image tag for ingress)"
    updated=$((updated + 1))
  else
    echo "$CHART_VALUES already uses $NEW_VERSION"
  fi
  matched=1
  active_file=""
  rm -f "$tmpfile"
  tmpfile=""
fi

# Match the complete version tag, including prerelease-style suffixes, so a
# bump never leaves an old suffix attached to the new version.
PATTERN="${COMPONENT}:v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z]+)*([^0-9A-Za-z_-]|$)"

# Do not touch release notes: they document historical image tags and must not be
# rewritten when bumping versions elsewhere.
files=()
scanfile="$(mktemp)"
grep_status=0
grep -rEIl \
  --exclude='*RELEASE_NOTES*' \
  --exclude-dir=.git --exclude-dir=__pycache__ --exclude-dir=.venv --exclude-dir=node_modules \
  "$PATTERN" . >"$scanfile" || grep_status=$?
if [ "$grep_status" -gt 1 ]; then
  echo "Error: failed to scan files for component $COMPONENT" >&2
  exit 1
fi
while IFS= read -r f; do
  [ -n "$f" ] && files+=("$f")
done <"$scanfile"
rm -f "$scanfile"
scanfile=""

# Iterate without "${files[@]}" on an empty array (bash 3.x + set -u can error).
for ((i = 0; i < ${#files[@]}; i++)); do
  f="${files[$i]}"
  [ -f "$f" ] || continue
  case "$f" in
    *RELEASE_NOTES*) continue ;;
  esac
  tmpfile="$(mktemp)"
  cp "$f" "$tmpfile"
  active_file="$f"
  if ! COMPONENT="$COMPONENT" NEW_VERSION="$NEW_VERSION" perl -i -pe '
    our $matched;
    $matched += s{\Q$ENV{COMPONENT}\E:v[0-9]+\.[0-9]+\.[0-9]+(?:[.-][0-9A-Za-z]+)*(?=[^0-9A-Za-z_-]|\z)}{$ENV{COMPONENT} . ":" . $ENV{NEW_VERSION}}ge;
    END { exit 1 unless $matched }
  ' "$f"; then
    cp "$tmpfile" "$f"
    echo "Error: failed to update $f" >&2
    exit 1
  fi
  matched=1
  if ! cmp -s "$f" "$tmpfile"; then
    echo "Updated $f"
    updated=$((updated + 1))
  fi
  active_file=""
  rm -f "$tmpfile"
  tmpfile=""
done

if [ "$updated" -eq 0 ]; then
  if [ "$matched" -ne 0 ]; then
    echo "No files needed updating; $COMPONENT already uses $NEW_VERSION."
    exit 0
  fi
  echo "No files were updated (nothing matched for component $COMPONENT)." >&2
  exit 1
fi

echo "Done. Bumped $COMPONENT version to $NEW_VERSION in $updated file(s)."
