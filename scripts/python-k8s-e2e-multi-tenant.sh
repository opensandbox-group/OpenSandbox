#!/bin/bash
# Multi-tenant Kubernetes E2E test
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

set -euxo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common/kubernetes-e2e.sh
source "${SCRIPT_DIR}/common/kubernetes-e2e.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KIND_CLUSTER="${KIND_CLUSTER:-opensandbox-e2e}"
KIND_K8S_VERSION="${KIND_K8S_VERSION:-v1.30.4}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-/tmp/opensandbox-kind-kubeconfig}"
E2E_NAMESPACE="${E2E_NAMESPACE:-opensandbox-e2e}"
SERVER_NAMESPACE="${SERVER_NAMESPACE:-opensandbox-system}"
PVC_NAME="${PVC_NAME:-opensandbox-e2e-pvc-test}"
PV_NAME="${PV_NAME:-opensandbox-e2e-pv-test}"
CONTROLLER_IMG="${CONTROLLER_IMG:-opensandbox/controller:e2e-local}"
SERVER_IMG="${SERVER_IMG:-opensandbox/server:e2e-local}"
EXECD_IMG="${EXECD_IMG:-opensandbox/execd:e2e-local}"
EGRESS_IMG="${EGRESS_IMG:-opensandbox/egress:e2e-local}"
SERVER_RELEASE="${SERVER_RELEASE:-opensandbox-server}"
SERVER_VALUES_FILE="${SERVER_VALUES_FILE:-/tmp/opensandbox-server-values.yaml}"
PORT_FORWARD_LOG="${PORT_FORWARD_LOG:-/tmp/opensandbox-server-port-forward.log}"
SANDBOX_TEST_IMAGE="${SANDBOX_TEST_IMAGE:-ubuntu:latest}"
LIFECYCLE_LOCAL_PORT="${LIFECYCLE_LOCAL_PORT:-8080}"

# Multi-tenant specific
TENANT_PROVIDER="${TENANT_PROVIDER:-file}"  # "file" or "http"
TENANT_API_KEY="mt-e2e-tenant-key"
TENANT_NAMESPACE="${E2E_NAMESPACE}"
HTTP_MOCK_PORT=9999
HTTP_MOCK_PID=""

SERVER_IMG_REPOSITORY="${SERVER_IMG%:*}"
SERVER_IMG_TAG="${SERVER_IMG##*:}"

k8s_e2e_export_kubeconfig
k8s_e2e_setup_kind_and_controller
k8s_e2e_build_runtime_images
k8s_e2e_kind_load_runtime_images
k8s_e2e_apply_pvc_and_seed

# --- Multi-tenant Helm values ---

_write_multi_tenant_file_values() {
  cat > "${SERVER_VALUES_FILE}" <<EOF
server:
  image:
    repository: ${SERVER_IMG_REPOSITORY}
    tag: "${SERVER_IMG_TAG}"
    pullPolicy: IfNotPresent
  replicaCount: 1
  resources:
    limits:
      cpu: "1"
      memory: 2Gi
    requests:
      cpu: "250m"
      memory: 512Mi
configToml: |
  [server]
  host = "0.0.0.0"
  port = 80

  [log]
  level = "INFO"

  [runtime]
  type = "kubernetes"
  execd_image = "${EXECD_IMG}"

  [egress]
  image = "${EGRESS_IMG}"

  [kubernetes]
  namespace = "${E2E_NAMESPACE}"
  workload_provider = "batchsandbox"
  sandbox_create_timeout_seconds = 180
  sandbox_create_poll_interval_seconds = 1.0

  [tenants]
  provider = "file"

  [storage]
  allowed_host_paths = []
tenantsToml: |
  [[tenants]]
  name = "e2e-tenant"
  namespace = "${TENANT_NAMESPACE}"
  api_keys = ["${TENANT_API_KEY}"]
EOF
}

_write_multi_tenant_http_values() {
  # Get the host IP reachable from Kind containers.
  # On Linux CI, the Kind Docker bridge gateway is the host.
  # On Mac, host.docker.internal works but not on Linux without extra setup.
  local host_ip
  host_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' "${KIND_CLUSTER}-control-plane" 2>/dev/null | tr -d '[:space:]')
  if [ -z "${host_ip}" ]; then
    # Fallback: inspect the "kind" network
    host_ip=$(docker network inspect kind -f '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null | tr -d '[:space:]')
  fi
  if [ -z "${host_ip}" ]; then
    host_ip="172.18.0.1"
  fi
  echo "HTTP tenant mock endpoint: http://${host_ip}:${HTTP_MOCK_PORT}/verify"
  # Verify the mock is reachable from host before deploying
  curl -fsS "http://${host_ip}:${HTTP_MOCK_PORT}/verify" -H "OPEN-SANDBOX-API-KEY: ${TENANT_API_KEY}" || {
    echo "WARN: mock not reachable at ${host_ip}:${HTTP_MOCK_PORT}, trying 127.0.0.1"
    # On CI, host-to-host should work via 0.0.0.0 binding
    curl -fsS "http://127.0.0.1:${HTTP_MOCK_PORT}/verify" -H "OPEN-SANDBOX-API-KEY: ${TENANT_API_KEY}"
  }

  cat > "${SERVER_VALUES_FILE}" <<EOF
server:
  image:
    repository: ${SERVER_IMG_REPOSITORY}
    tag: "${SERVER_IMG_TAG}"
    pullPolicy: IfNotPresent
  replicaCount: 1
  resources:
    limits:
      cpu: "1"
      memory: 2Gi
    requests:
      cpu: "250m"
      memory: 512Mi
configToml: |
  [server]
  host = "0.0.0.0"
  port = 80

  [log]
  level = "INFO"

  [runtime]
  type = "kubernetes"
  execd_image = "${EXECD_IMG}"

  [egress]
  image = "${EGRESS_IMG}"

  [kubernetes]
  namespace = "${E2E_NAMESPACE}"
  workload_provider = "batchsandbox"
  sandbox_create_timeout_seconds = 180
  sandbox_create_poll_interval_seconds = 1.0

  [tenants]
  provider = "http"
  endpoint = "http://${host_ip}:${HTTP_MOCK_PORT}/verify"
  timeout_seconds = 5
  max_stale_seconds = 60

  [storage]
  allowed_host_paths = []
EOF
}

# --- HTTP mock tenant provider ---

_start_http_mock_provider() {
  python3 - "${HTTP_MOCK_PORT}" "${TENANT_API_KEY}" "${TENANT_NAMESPACE}" <<'PYEOF' &
import json
import sys
from http.server import HTTPServer, BaseHTTPRequestHandler

port = int(sys.argv[1])
valid_key = sys.argv[2]
namespace = sys.argv[3]

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        api_key = self.headers.get("OPEN-SANDBOX-API-KEY", "")
        if api_key == valid_key:
            resp = {"namespace": namespace, "ttl": 60}
            self.send_response(200)
        else:
            resp = {"code": "UNAUTHORIZED", "message": "Invalid API key"}
            self.send_response(401)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(resp).encode())

    def log_message(self, format, *args):
        pass

print(f"Mock HTTP tenant provider on :{port}", flush=True)
HTTPServer(("0.0.0.0", port), Handler).serve_forever()
PYEOF
  HTTP_MOCK_PID=$!
  echo "HTTP mock provider started (PID=${HTTP_MOCK_PID})"
  sleep 1
  # Verify it's running
  curl -fsS "http://127.0.0.1:${HTTP_MOCK_PORT}/verify" \
    -H "OPEN-SANDBOX-API-KEY: ${TENANT_API_KEY}" | jq .
}

_stop_http_mock_provider() {
  if [ -n "${HTTP_MOCK_PID}" ]; then
    kill "${HTTP_MOCK_PID}" >/dev/null 2>&1 || true
  fi
}

# --- Main ---

if [ "${TENANT_PROVIDER}" = "http" ]; then
  _start_http_mock_provider
  trap '_stop_http_mock_provider; kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true' EXIT
  _write_multi_tenant_http_values
else
  trap 'kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true' EXIT
  _write_multi_tenant_file_values
fi

k8s_e2e_helm_install_server

kubectl port-forward -n "${SERVER_NAMESPACE}" svc/opensandbox-server "${LIFECYCLE_LOCAL_PORT}:80" >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!

k8s_e2e_wait_http_ok "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/health"

# --- Verify multi-tenant auth ---

echo "=== Verifying multi-tenant auth ==="

# Valid tenant key should work
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/v1/sandboxes" \
  -H "OPEN-SANDBOX-API-KEY: ${TENANT_API_KEY}")
if [ "${HTTP_CODE}" != "200" ]; then
  echo "FAIL: valid tenant key got HTTP ${HTTP_CODE}, expected 200"
  exit 1
fi
echo "PASS: valid tenant key → 200"

# Invalid key should get 401
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/v1/sandboxes" \
  -H "OPEN-SANDBOX-API-KEY: invalid-key")
if [ "${HTTP_CODE}" != "401" ]; then
  echo "FAIL: invalid key got HTTP ${HTTP_CODE}, expected 401"
  exit 1
fi
echo "PASS: invalid key → 401"

# No key should get 401
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/v1/sandboxes")
if [ "${HTTP_CODE}" != "401" ]; then
  echo "FAIL: no key got HTTP ${HTTP_CODE}, expected 401"
  exit 1
fi
echo "PASS: no key → 401"

echo "=== Multi-tenant auth verification passed ==="

# --- Run SDK mini E2E with tenant key ---

export OPENSANDBOX_TEST_DOMAIN="localhost:${LIFECYCLE_LOCAL_PORT}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY="${TENANT_API_KEY}"
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="${SANDBOX_TEST_IMAGE}"
export OPENSANDBOX_E2E_RUNTIME="kubernetes"
export OPENSANDBOX_TEST_USE_SERVER_PROXY="true"
export OPENSANDBOX_TEST_PVC_NAME="${PVC_NAME}"

k8s_e2e_export_sandbox_resource_env

k8s_e2e_generate_sdk_and_run_kubernetes_mini
