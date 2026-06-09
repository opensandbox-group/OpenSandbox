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

set -euo pipefail

TAG=${TAG:-latest}
SERVER_PORT=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_SERVER_PORT:-32889}
LOG_DIR=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LOG_DIR:-/tmp/opensandbox-credential-vault-e2e}
TARGET_CONTAINER=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER:-opensandbox-e2e-credential-vault-target}
TARGET_IMAGE=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IMAGE:-python:3.11-alpine}
SANDBOX_IMAGE=${OPENSANDBOX_SANDBOX_DEFAULT_IMAGE:-opensandbox/code-interpreter:${TAG}}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVER_PID=""

cleanup() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
  fi
  docker rm -f "${TARGET_CONTAINER}" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=opensandbox" | xargs -r docker rm -f || true
  docker run --rm -v /tmp:/host_tmp alpine rm -rf /host_tmp/opensandbox-e2e >/dev/null 2>&1 || true
}
trap cleanup EXIT

wait_http() {
  local url="$1"
  python3 - "$url" <<'PY'
import sys
import time
import urllib.request

url = sys.argv[1]
deadline = time.monotonic() + 60
last_error = None
while time.monotonic() < deadline:
    try:
        with urllib.request.urlopen(url, timeout=1) as response:
            if response.status < 500:
                raise SystemExit(0)
    except Exception as exc:
        last_error = exc
    time.sleep(1)
raise SystemExit(f"Timed out waiting for {url}: {last_error}")
PY
}

mkdir -p "${LOG_DIR}" /tmp/opensandbox-e2e/host-volume-test /tmp/opensandbox-e2e/logs
chmod -R 755 /tmp/opensandbox-e2e

cd "${REPO_ROOT}"

docker build -f components/execd/Dockerfile -t opensandbox/execd:local "${REPO_ROOT}"
docker build -f components/egress/Dockerfile -t opensandbox/egress:local "${REPO_ROOT}"
docker pull "${SANDBOX_IMAGE}"
docker pull "${TARGET_IMAGE}"

docker rm -f "${TARGET_CONTAINER}" >/dev/null 2>&1 || true
docker run -d \
  --name "${TARGET_CONTAINER}" \
  --label opensandbox.e2e=credential-vault \
  -v "${REPO_ROOT}/tests/python/tests/support:/srv:ro" \
  "${TARGET_IMAGE}" \
  python /srv/credential_vault_echo_server.py >/dev/null

TARGET_READY=false
for _ in $(seq 1 30); do
  if docker exec "${TARGET_CONTAINER}" python - <<'PY' >/dev/null 2>&1
import urllib.request
urllib.request.urlopen("http://127.0.0.1/healthz", timeout=1).read()
PY
  then
    TARGET_READY=true
    break
  fi
  sleep 1
done
if [ "${TARGET_READY}" != "true" ]; then
  docker logs "${TARGET_CONTAINER}" || true
  echo "Credential Vault E2E target container did not become ready" >&2
  exit 1
fi

TARGET_IP="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${TARGET_CONTAINER}")"
if [ -z "${TARGET_IP}" ]; then
  docker logs "${TARGET_CONTAINER}" || true
  echo "Failed to determine Credential Vault E2E target container IP" >&2
  exit 1
fi

cat > "${HOME}/.sandbox.toml" <<EOF
[server]
host = "127.0.0.1"
port = ${SERVER_PORT}
api_key = ""
[log]
level = "INFO"
[runtime]
type = "docker"
execd_image = "opensandbox/execd:local"
[egress]
image = "opensandbox/egress:local"
mode = "dns"
[docker]
network_mode = "bridge"
[storage]
allowed_host_paths = ["/tmp/opensandbox-e2e"]
EOF

cd "${REPO_ROOT}/server"
export OPENSANDBOX_INSECURE_SERVER=YES
uv sync
uv run python -m opensandbox_server.main > "${LOG_DIR}/server.log" 2>&1 &
SERVER_PID=$!

wait_http "http://127.0.0.1:${SERVER_PORT}/health"

cd "${REPO_ROOT}/tests/python"
uv sync --all-extras
export OPENSANDBOX_TEST_DOMAIN="localhost:${SERVER_PORT}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY=""
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="${SANDBOX_IMAGE}"
export OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP="${TARGET_IP}"
uv run pytest tests/test_credential_vault_e2e.py -v
