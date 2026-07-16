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

set -euo pipefail

echo "Disk usage before Docker cleanup:"
df -h /

containers="$(
  {
    timeout 30s docker ps -aq --filter "label=opensandbox" || true
    timeout 30s docker ps -aq --filter "label=opensandbox.e2e=credential-vault" || true
    timeout 30s docker ps -aq --filter "name=^/opensandbox-e2e-redis$" || true
    timeout 30s docker ps -aq --filter "name=^/opensandbox-e2e-credential-vault-target$" || true
    timeout 30s docker ps -aq --filter "name=^/egress-smoke-" || true
  } | sort -u
)"
if [ -n "${containers}" ]; then
  echo "${containers}" | xargs -r docker rm -f || true
fi

timeout 30s rm -rf "${HOME:-/home/admin}/.docker/buildx/activity"/* || true

echo "Disk usage after Docker cleanup:"
df -h /
