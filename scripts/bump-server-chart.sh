#!/usr/bin/env bash
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

# Bump server.image.tag in opensandbox-server Helm values (only that field).
# Usage from repo root:
#   ./scripts/bump-server-chart.sh v0.1.9
#   ./scripts/bump-server-chart.sh 0.1.9

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

NEW_VERSION="${1:-}"
if [ -z "$NEW_VERSION" ]; then
  echo "Usage: $0 VERSION" >&2
  exit 1
fi

if [[ ! "$NEW_VERSION" =~ ^v ]]; then
  NEW_VERSION="v${NEW_VERSION}"
fi

FILE="kubernetes/charts/opensandbox-server/values.yaml"
if [ ! -f "$FILE" ]; then
  echo "Error: missing $FILE" >&2
  exit 1
fi

# Match tag line immediately after opensandbox/server repository (not gateway/ingress).
SERVER_REPO='repository: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server'
if ! grep -qF "$SERVER_REPO" "$FILE"; then
  echo "Error: expected server repository line not found in $FILE" >&2
  exit 1
fi

perl -i -0pe 's{
  (repository:\s+sandbox-registry\.cn-zhangjiakou\.cr\.aliyuncs\.com/opensandbox/server\n
   \s+tag:\s+")[^"]+(")
}{$1'"$NEW_VERSION"'$2}x' "$FILE"

echo "Updated $FILE: server.image.tag -> $NEW_VERSION"
