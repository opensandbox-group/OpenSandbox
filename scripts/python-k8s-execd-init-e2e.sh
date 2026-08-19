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

# Runs the execd-as-init E2E (OSEP-0018) on a Kind cluster with the
# Kubernetes runtime and runtime.execd_run_as_init = true.

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
SANDBOX_TEST_IMAGE="${SANDBOX_TEST_IMAGE:-opensandbox/code-interpreter:latest}"
LIFECYCLE_LOCAL_PORT="${LIFECYCLE_LOCAL_PORT:-8080}"

SERVER_IMG_REPOSITORY="${SERVER_IMG%:*}"
SERVER_IMG_TAG="${SERVER_IMG##*:}"

export E2E_EXECD_RUN_AS_INIT=true

k8s_e2e_export_kubeconfig
k8s_e2e_setup_kind_and_controller
k8s_e2e_build_runtime_images
k8s_e2e_kind_load_runtime_images
k8s_e2e_apply_pvc_and_seed
# The hardened isolation TOML travels to sandboxes via a ConfigMap mounted by
# the e2e batchsandbox template (optional: true), and the hardening e2e points
# EXECD_ISOLATION_CONFIG at it per request. No server config needed.
kubectl create configmap opensandbox-e2e-execd-isolation \
  --namespace "${E2E_NAMESPACE}" \
  --from-file=isolation.hardened.toml="${REPO_ROOT}/components/execd/configs/isolation.hardened.toml" \
  --dry-run=client -o yaml | kubectl apply -f -
k8s_e2e_write_server_helm_values
k8s_e2e_helm_install_server

kubectl port-forward -n "${SERVER_NAMESPACE}" svc/opensandbox-server "${LIFECYCLE_LOCAL_PORT}:80" >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!
trap 'kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true' EXIT

# Capture the sandbox pod specs and BatchSandbox CRs while the tests run: the
# hardening leg has twice observed PVC mount paths showing up as noexec tmpfs
# inside the container, so the pod-spec dump is needed to see what the
# controller actually created.
(
  for _ in $(seq 1 600); do
    kubectl get pods -n "${E2E_NAMESPACE}" -o yaml > /tmp/opensandbox-e2e-pods.yaml 2>/dev/null || true
    kubectl get batchsandboxes -n "${E2E_NAMESPACE}" -o yaml > /tmp/opensandbox-e2e-batchsandboxes.yaml 2>/dev/null || true
    sleep 2
  done
) &
SPEC_WATCHER_PID=$!
trap 'kill "${SPEC_WATCHER_PID}" >/dev/null 2>&1 || true; kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true' EXIT

k8s_e2e_wait_http_ok "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/health"

export OPENSANDBOX_TEST_DOMAIN="localhost:${LIFECYCLE_LOCAL_PORT}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY="kubernetes-e2e"
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="${SANDBOX_TEST_IMAGE}"
export OPENSANDBOX_E2E_RUNTIME="kubernetes"
export OPENSANDBOX_TEST_USE_SERVER_PROXY="true"
export OPENSANDBOX_TEST_PVC_NAME="${PVC_NAME}"

k8s_e2e_export_sandbox_resource_env

cd "${REPO_ROOT}/sdks/sandbox/python"
make generate-api
cd "${REPO_ROOT}/tests/python"
uv sync --all-extras --refresh
uv run pytest tests/test_execd_init_e2e.py -v
uv run pytest tests/test_execd_hardening_e2e.py -v -k "TestHardeningE2E"
