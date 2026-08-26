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

# Runs the execd-as-init E2E (OSEP-0018) against a local Docker-bridge
# server. The server config (~/.sandbox.toml) is written by the workflow
# with runtime.execd_run_as_init = true.

set -euxo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVER_PID=""

cleanup() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

docker build -f components/execd/Dockerfile -t opensandbox/execd:local "${REPO_ROOT}"
docker pull opensandbox/code-interpreter:${TAG:-latest}

mkdir -p /tmp/opensandbox-e2e/logs
echo "-------- EXECD INIT E2E test logs for execd --------" > /tmp/opensandbox-e2e/logs/execd.log

cd server
export OPENSANDBOX_INSECURE_SERVER=YES
uv sync
uv run python -m opensandbox_server.main > server.log 2>&1 &
SERVER_PID=$!
cd ..

sleep 10

cd sdks/sandbox/python && make generate-api
cd ../../..

cd tests/python
uv sync --all-extras --refresh
uv run pytest tests/test_execd_init_e2e.py -v
