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

# Kubernetes E2E (Python) with server ingress.mode=gateway and the chart-deployed
# ingress-gateway (components/ingress). See kubernetes/charts/opensandbox-server/README.md.
#
# Compared to scripts/python-k8s-e2e.sh:
# - Builds/opensandbox/ingress image and sets server.gateway.* so Helm deploys opensandbox-ingress-gateway.
# - Port-forwards both the lifecycle API and the gateway.
# - Sets OPENSANDBOX_TEST_USE_SERVER_PROXY=false so the SDK uses gateway routes + headers from the API.
#
# Route mode (Helm server.gateway.gatewayRouteMode + ingress --mode):
#   Default header. For URI path routing: E2E_GATEWAY_ROUTE_MODE=uri ./scripts/python-k8s-e2e-ingress.sh

set -euxo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common/kubernetes-e2e.sh
source "${SCRIPT_DIR}/common/kubernetes-e2e.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

export E2E_SERVER_GATEWAY_ENABLED=true
export E2E_GATEWAY_ROUTE_MODE="${E2E_GATEWAY_ROUTE_MODE:-header}"

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
INGRESS_IMG="${INGRESS_IMG:-opensandbox/ingress:e2e-local}"
SERVER_RELEASE="${SERVER_RELEASE:-opensandbox-server}"
SERVER_VALUES_FILE="${SERVER_VALUES_FILE:-/tmp/opensandbox-server-values-ingress.yaml}"
PORT_FORWARD_LOG="${PORT_FORWARD_LOG:-/tmp/opensandbox-server-port-forward.log}"
GATEWAY_PORT_FORWARD_LOG="${GATEWAY_PORT_FORWARD_LOG:-/tmp/opensandbox-ingress-gateway-port-forward.log}"
SANDBOX_TEST_IMAGE="${SANDBOX_TEST_IMAGE:-ubuntu:latest}"

GATEWAY_LOCAL_PORT="${GATEWAY_LOCAL_PORT:-8081}"
INGRESS_GATEWAY_ADDRESS="${INGRESS_GATEWAY_ADDRESS:-127.0.0.1:${GATEWAY_LOCAL_PORT}}"
LIFECYCLE_LOCAL_PORT="${LIFECYCLE_LOCAL_PORT:-8080}"

SERVER_IMG_REPOSITORY="${SERVER_IMG%:*}"
SERVER_IMG_TAG="${SERVER_IMG##*:}"
INGRESS_IMG_REPOSITORY="${INGRESS_IMG%:*}"
INGRESS_IMG_TAG="${INGRESS_IMG##*:}"

k8s_e2e_export_kubeconfig
k8s_e2e_setup_kind_and_controller
k8s_e2e_build_runtime_images
k8s_e2e_kind_load_runtime_images
k8s_e2e_apply_pvc_and_seed
k8s_e2e_write_server_helm_values
k8s_e2e_helm_install_server

kubectl port-forward -n "${SERVER_NAMESPACE}" svc/opensandbox-server "${LIFECYCLE_LOCAL_PORT}:80" >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!
kubectl port-forward -n "${SERVER_NAMESPACE}" svc/opensandbox-ingress-gateway "${GATEWAY_LOCAL_PORT}:80" >"${GATEWAY_PORT_FORWARD_LOG}" 2>&1 &
GATEWAY_PORT_FORWARD_PID=$!
cleanup_port_forwards() {
  kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
  kill "${GATEWAY_PORT_FORWARD_PID}" >/dev/null 2>&1 || true
}
trap cleanup_port_forwards EXIT

k8s_e2e_wait_http_ok "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/health"
k8s_e2e_wait_http_ok "http://127.0.0.1:${GATEWAY_LOCAL_PORT}/status.ok"

export OPENSANDBOX_TEST_DOMAIN="localhost:${LIFECYCLE_LOCAL_PORT}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY="kubernetes-e2e"
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="${SANDBOX_TEST_IMAGE}"
export OPENSANDBOX_E2E_RUNTIME="kubernetes"
export OPENSANDBOX_TEST_USE_SERVER_PROXY="false"
export OPENSANDBOX_TEST_SECURE_ACCESS_VERIFIABLE="true"
export OPENSANDBOX_TEST_PVC_NAME="${PVC_NAME}"

k8s_e2e_export_sandbox_resource_env

k8s_e2e_generate_sdk_and_run_kubernetes_mini
