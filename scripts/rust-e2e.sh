#!/bin/bash
# Copyright 2026 The OpenSandbox Authors
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

# Builds the Rust SDK and runs its live-server E2E suite against a local
# lifecycle server + execd. Mirrors scripts/go-e2e.sh and scripts/java-e2e.sh.

set -euxo pipefail

TAG=${TAG:-latest}
RUN_CODE_INTERPRETER_E2E=${RUN_CODE_INTERPRETER_E2E:-false}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVER_PID=""

source "${REPO_ROOT}/scripts/credential-vault-e2e-target.sh"

cleanup_server() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}

cleanup() {
  cleanup_server
  cleanup_credential_vault_e2e_target
}
trap cleanup EXIT

# build execd image locally (context must include internal/)
docker build -f components/execd/Dockerfile -t opensandbox/execd:local "${REPO_ROOT}"

# prepare required images from registry
docker pull opensandbox/code-interpreter:${TAG}
echo "-------- Eval test images --------"
docker images

# prepare hostpath volume for e2e test
mkdir -p /tmp/opensandbox-e2e/host-volume-test
mkdir -p /tmp/opensandbox-e2e/logs
echo "opensandbox-e2e-marker" > /tmp/opensandbox-e2e/host-volume-test/marker.txt
chmod -R 755 /tmp/opensandbox-e2e

# prepare Docker named volume for pvc e2e test
docker volume rm opensandbox-e2e-pvc-test 2>/dev/null || true
docker volume create opensandbox-e2e-pvc-test
# seed the named volume with a marker file and subpath test data via a temporary container
docker run --rm -v opensandbox-e2e-pvc-test:/data alpine sh -c "\
  echo 'pvc-marker-data' > /data/marker.txt && \
  mkdir -p /data/datasets/train && \
  echo 'pvc-subpath-marker' > /data/datasets/train/marker.txt"
echo "-------- RUST E2E test logs for execd --------" > /tmp/opensandbox-e2e/logs/execd.log

export OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE="${OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE:-opensandbox/code-interpreter:${TAG}}"
setup_credential_vault_e2e_target

# setup server
cd server
export OPENSANDBOX_INSECURE_SERVER=YES
uv sync
# The server address comes from the config file (~/.sandbox.toml unless
# SANDBOX_CONFIG_PATH points elsewhere), so resolve it instead of assuming the
# default localhost:8080.
SERVER_ADDRESS="$(uv run python -c '
from opensandbox_server.config import load_config

server = load_config().server
host = "127.0.0.1" if server.host in ("", "0.0.0.0", "::") else server.host
print(f"{host}:{server.port}")
')"
# `opensandbox_server.main` runs uvicorn with reload=True; the reloader watches
# the server directory and stops mid-suite when server.log is rewritten, which
# kills the E2E run. Start a plain (non-reloading) server instead.
uv run python -c "
import uvicorn
from opensandbox_server.config import load_config
from opensandbox_server.main import app
config = load_config()
uvicorn.run(app, host=config.server.host, port=config.server.port, reload=False)
" > server.log 2>&1 &
SERVER_PID=$!
cd ..

# point the SDK e2e suites at the address the server actually bound
export OPENSANDBOX_TEST_DOMAIN="${OPENSANDBOX_TEST_DOMAIN:-${SERVER_ADDRESS}}"
echo "lifecycle server: ${OPENSANDBOX_TEST_DOMAIN}"

# wait for server
server_ready=false
for _ in $(seq 1 60); do
  if curl -fsS "http://${OPENSANDBOX_TEST_DOMAIN}/health" > /dev/null 2>&1; then
    server_ready=true
    break
  fi
  if ! kill -0 "${SERVER_PID}" 2>/dev/null; then
    break
  fi
  sleep 1
done
if [ "${server_ready}" != true ]; then
  echo "lifecycle server did not become ready at ${OPENSANDBOX_TEST_DOMAIN}" >&2
  cat server/server.log >&2
  exit 1
fi

# run Rust e2e tests
cd sdks/sandbox/rust
mkdir -p reports
OPENSANDBOX_E2E=1 \
  RUN_CODE_INTERPRETER_E2E="${RUN_CODE_INTERPRETER_E2E}" \
  cargo test --test e2e_lifecycle --test e2e_execd --test e2e_vault -- --test-threads=1 2>&1 | tee reports/test-output.txt
