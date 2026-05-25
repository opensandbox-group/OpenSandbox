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
  echo "Usage: $0 [egress|execd|ingress|code-interpreter|image-committer] NEW_VERSION" >&2
  echo "       $0 NEW_VERSION   # bumps egress" >&2
  echo "Example: $0 egress v1.0.2" >&2
  echo "Example: $0 execd 1.0.7" >&2
  echo "Example: $0 ingress v1.0.6" >&2
  echo "Example: $0 image-committer v0.1.0" >&2
  exit 1
fi

case "$COMPONENT" in
  egress|execd|ingress|code-interpreter|image-committer) ;;
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
  perl -i -0pe 's{
    (repository:\s+sandbox-registry\.cn-zhangjiakou\.cr\.aliyuncs\.com/opensandbox/ingress\n
     \s+tag:\s+")[^"]+(")
  }{$1'"$NEW_VERSION"'$2}x' "$CHART_VALUES"
  if ! cmp -s "$CHART_VALUES" "$tmpfile"; then
    echo "Updated $CHART_VALUES (server.gateway.image tag for ingress)"
    updated=$((updated + 1))
  fi
  rm -f "$tmpfile"
fi

# Pattern and replacement for this component (e.g. egress:vX.Y.Z -> egress:NEW_VERSION)
PATTERN="${COMPONENT}:v[0-9]+\.[0-9]+\.[0-9]+"
REPLACEMENT="${COMPONENT}:${NEW_VERSION}"

# Do not touch release notes: they document historical image tags and must not be
# rewritten when bumping versions elsewhere.
files=()
while IFS= read -r f; do
  [ -n "$f" ] && files+=("$f")
done < <(grep -rEl \
  --exclude='*RELEASE_NOTES*' \
  --exclude-dir=.git --exclude-dir=__pycache__ --exclude-dir=.venv --exclude-dir=node_modules \
  "$PATTERN" . 2>/dev/null || true)

# Iterate without "${files[@]}" on an empty array (bash 3.x + set -u can error).
for ((i = 0; i < ${#files[@]}; i++)); do
  f="${files[$i]}"
  [ -f "$f" ] || continue
  case "$f" in
    *RELEASE_NOTES*) continue ;;
  esac
  if perl -i -pe "s/$PATTERN/$REPLACEMENT/g" "$f" 2>/dev/null; then
    echo "Updated $f"
    ((updated++)) || true
  fi
done

if [ "$updated" -eq 0 ]; then
  echo "No files were updated (nothing matched for component $COMPONENT)." >&2
  exit 1
fi

echo "Done. Bumped $COMPONENT version to $NEW_VERSION in $updated file(s)."
