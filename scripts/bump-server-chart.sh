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

# Also keep Chart.yaml appVersion in sync with the server release, so installs
# from the repo report the right version (the APP VERSION column in `helm list`).
# Until now appVersion was only patched at chart-package time by
# publish-helm-chart.yml, leaving the committed Chart.yaml stale at 0.1.0.
# chart `version` is intentionally left to the helm release process.
CHART_FILE="kubernetes/charts/opensandbox-server/Chart.yaml"
if [ ! -f "$CHART_FILE" ]; then
  echo "Error: missing $CHART_FILE" >&2
  exit 1
fi
if ! grep -qE '^appVersion:' "$CHART_FILE"; then
  echo "Error: appVersion line not found in $CHART_FILE" >&2
  exit 1
fi
APP_VERSION="${NEW_VERSION#v}"   # appVersion has no leading "v"
perl -i -pe 's/^appVersion:.*/appVersion: "'"$APP_VERSION"'"/' "$CHART_FILE"
echo "Updated $CHART_FILE: appVersion -> $APP_VERSION"
