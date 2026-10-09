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

set -euxo pipefail

# Nightly full-stack node-agent log collection E2E.
#
# Deploys the minimal full OpenSandbox stack on Kind (controller via Kustomize,
# server via Helm with the kubernetes/batchsandbox runtime) plus the node-agent
# chart with its file sink. A focused Python E2E test then creates a real
# sandbox through the server, and the test verifies that the node-agent
# collects the sandbox container stdout into the node-local file-sink
# directory and writes the finalization marker after the sandbox is deleted.
#
# Reuses the shared helpers from scripts/common/kubernetes-e2e.sh.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=common/kubernetes-e2e.sh
source "${SCRIPT_DIR}/common/kubernetes-e2e.sh"

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

KIND_CLUSTER="${KIND_CLUSTER:-opensandbox-e2e}"
KIND_K8S_VERSION="${KIND_K8S_VERSION:-v1.30.4}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-/tmp/opensandbox-kind-kubeconfig}"
E2E_NAMESPACE="${E2E_NAMESPACE:-opensandbox-e2e}"
SERVER_NAMESPACE="${SERVER_NAMESPACE:-opensandbox-system}"
CONTROLLER_IMG="${CONTROLLER_IMG:-opensandbox/controller:e2e-local}"
SERVER_IMG="${SERVER_IMG:-opensandbox/server:e2e-local}"
EXECD_IMG="${EXECD_IMG:-opensandbox/execd:e2e-local}"
EGRESS_IMG="${EGRESS_IMG:-opensandbox/egress:e2e-local}"
SERVER_RELEASE="${SERVER_RELEASE:-opensandbox-server}"
SERVER_VALUES_FILE="${SERVER_VALUES_FILE:-/tmp/opensandbox-server-values.yaml}"
PORT_FORWARD_LOG="${PORT_FORWARD_LOG:-/tmp/opensandbox-server-port-forward.log}"
SANDBOX_TEST_IMAGE="${SANDBOX_TEST_IMAGE:-ubuntu:latest}"
LIFECYCLE_LOCAL_PORT="${LIFECYCLE_LOCAL_PORT:-8080}"

NODEAGENT_IMG="${NODEAGENT_IMG:-opensandbox/nodeagent:e2e-local}"
NODEAGENT_RELEASE="${NODEAGENT_RELEASE:-nodeagent}"
NODEAGENT_CLUSTER_ID="${NODEAGENT_CLUSTER_ID:-nightly-smoke}"
NODEAGENT_DATA_ROOT="${NODEAGENT_DATA_ROOT:-/var/lib/opensandbox/nodeagent-data}"
NODEAGENT_TESTS="${NODEAGENT_TESTS:-tests/test_nodeagent_k8s_log_collection_e2e.py}"

SERVER_IMG_REPOSITORY="${SERVER_IMG%:*}"
SERVER_IMG_TAG="${SERVER_IMG##*:}"
NODEAGENT_IMG_REPOSITORY="${NODEAGENT_IMG%:*}"
NODEAGENT_IMG_TAG="${NODEAGENT_IMG##*:}"
KIND_NODE="${KIND_CLUSTER}-control-plane"

k8s_e2e_export_kubeconfig
k8s_e2e_setup_kind_and_controller
k8s_e2e_build_runtime_images
k8s_e2e_kind_load_runtime_images
k8s_e2e_write_server_helm_values
k8s_e2e_helm_install_server

# The server does not create its sandbox namespace: the mini-e2e flow creates
# it as a side effect of the PVC seeding step, which this minimal flow skips.
kubectl get namespace "${E2E_NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${E2E_NAMESPACE}"

docker build -f components/nodeagent/Dockerfile -t "${NODEAGENT_IMG}" "${REPO_ROOT}"
kind load docker-image --name "${KIND_CLUSTER}" "${NODEAGENT_IMG}"

helm upgrade --install "${NODEAGENT_RELEASE}" "${REPO_ROOT}/manifests/charts/node-agent" \
  --namespace "${SERVER_NAMESPACE}" \
  --set-string image.repository="${NODEAGENT_IMG_REPOSITORY}" \
  --set-string image.tag="${NODEAGENT_IMG_TAG}" \
  --set image.pullPolicy=Never \
  --set config.clusterID="${NODEAGENT_CLUSTER_ID}" \
  --set config.endedStateRetention=1m \
  --wait --timeout 180s

nodeagent_daemonset="$(kubectl get daemonset -n "${SERVER_NAMESPACE}" \
  -l app.kubernetes.io/component=node-agent \
  -o jsonpath='{.items[0].metadata.name}')"
kubectl rollout status "daemonset/${nodeagent_daemonset}" -n "${SERVER_NAMESPACE}" --timeout=180s
kubectl wait --for=condition=Ready pod -n "${SERVER_NAMESPACE}" \
  -l app.kubernetes.io/component=node-agent --timeout=120s

kubectl port-forward -n "${SERVER_NAMESPACE}" svc/opensandbox-server \
  "${LIFECYCLE_LOCAL_PORT}:80" >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!
trap 'kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true' EXIT

k8s_e2e_wait_http_ok "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/health"

export OPENSANDBOX_TEST_DOMAIN="localhost:${LIFECYCLE_LOCAL_PORT}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY="kubernetes-e2e"
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="${SANDBOX_TEST_IMAGE}"
export OPENSANDBOX_E2E_RUNTIME="kubernetes"
export OPENSANDBOX_TEST_USE_SERVER_PROXY="true"
export OPENSANDBOX_E2E_NAMESPACE="${E2E_NAMESPACE}"

k8s_e2e_export_sandbox_resource_env

export NODEAGENT_SMOKE_CLUSTER_ID="${NODEAGENT_CLUSTER_ID}"
export NODEAGENT_SMOKE_DATA_ROOT="${NODEAGENT_DATA_ROOT}"
export NODEAGENT_SMOKE_KIND_NODE="${KIND_NODE}"

cd "${REPO_ROOT}/sdks/sandbox/python"
make generate-api
cd "${REPO_ROOT}/tests/python"
uv sync --all-extras --refresh
uv run pytest "${NODEAGENT_TESTS}"
