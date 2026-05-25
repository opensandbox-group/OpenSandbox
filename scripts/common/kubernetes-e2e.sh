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

# Shared helpers for the Kubernetes Python E2E entrypoints:
#   scripts/python-k8s-e2e.sh, scripts/python-k8s-e2e-ingress.sh
# (this file is scripts/common/kubernetes-e2e.sh — library only, not the top-level runner).
# Source after setting REPO_ROOT and the usual E2E_* / image env vars.
#
# Optional:
#   E2E_SERVER_GATEWAY_ENABLED=true — include server.gateway.* in Helm values (ingress-gateway path).
#   E2E_GATEWAY_ROUTE_MODE — when gateway enabled: header | uri (default header). Matches chart server.gateway.gatewayRouteMode.

k8s_e2e_export_kubeconfig() {
  export KUBECONFIG="${KUBECONFIG_PATH}"
  if [ -n "${GITHUB_ENV:-}" ]; then
    echo "KUBECONFIG=${KUBECONFIG_PATH}" >> "${GITHUB_ENV}"
  fi
}

k8s_e2e_setup_kind_and_controller() {
  cd "${REPO_ROOT}/kubernetes"
  make setup-test-e2e KIND_CLUSTER="${KIND_CLUSTER}" KIND_K8S_VERSION="${KIND_K8S_VERSION}"
  kind export kubeconfig --name "${KIND_CLUSTER}" --kubeconfig "${KUBECONFIG_PATH}"

  make docker-build-controller CONTROLLER_IMG="${CONTROLLER_IMG}"
  kind load docker-image --name "${KIND_CLUSTER}" "${CONTROLLER_IMG}"
  make install
  make deploy CONTROLLER_IMG="${CONTROLLER_IMG}"
  kubectl wait --for=condition=available --timeout=180s deployment/opensandbox-controller-manager -n opensandbox-system
  cd "${REPO_ROOT}"
}

k8s_e2e_build_runtime_images() {
  docker build -f server/Dockerfile -t "${SERVER_IMG}" server
  docker build -f components/execd/Dockerfile -t "${EXECD_IMG}" "${REPO_ROOT}"
  docker build -f components/egress/Dockerfile -t "${EGRESS_IMG}" "${REPO_ROOT}"
  if [ "${E2E_SERVER_GATEWAY_ENABLED:-false}" = "true" ]; then
    docker build -f components/ingress/Dockerfile -t "${INGRESS_IMG}" "${REPO_ROOT}"
  fi
  docker pull "${SANDBOX_TEST_IMAGE}"
}

k8s_e2e_kind_load_runtime_images() {
  kind load docker-image --name "${KIND_CLUSTER}" "${SERVER_IMG}"
  kind load docker-image --name "${KIND_CLUSTER}" "${EXECD_IMG}"
  kind load docker-image --name "${KIND_CLUSTER}" "${EGRESS_IMG}"
  if [ "${E2E_SERVER_GATEWAY_ENABLED:-false}" = "true" ]; then
    kind load docker-image --name "${KIND_CLUSTER}" "${INGRESS_IMG}"
  fi
  kind load docker-image --name "${KIND_CLUSTER}" "${SANDBOX_TEST_IMAGE}"
}

k8s_e2e_apply_pvc_and_seed() {
  kubectl get namespace "${E2E_NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${E2E_NAMESPACE}"

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${PV_NAME}
spec:
  capacity:
    storage: 2Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  storageClassName: manual
  hostPath:
    path: /tmp/${PV_NAME}
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
  namespace: ${E2E_NAMESPACE}
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: manual
  resources:
    requests:
      storage: 1Gi
  volumeName: ${PV_NAME}
EOF

  kubectl wait --for=jsonpath='{.status.phase}'=Bound --timeout=120s "pvc/${PVC_NAME}" -n "${E2E_NAMESPACE}"

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: opensandbox-e2e-pvc-seed
  namespace: ${E2E_NAMESPACE}
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: alpine:3.20
      command:
        - /bin/sh
        - -c
        - |
          set -eux
          mkdir -p /data/datasets/train
          echo 'pvc-marker-data' > /data/marker.txt
          echo 'pvc-subpath-marker' > /data/datasets/train/marker.txt
      volumeMounts:
        - name: pvc
          mountPath: /data
  volumes:
    - name: pvc
      persistentVolumeClaim:
        claimName: ${PVC_NAME}
EOF

  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s pod/opensandbox-e2e-pvc-seed -n "${E2E_NAMESPACE}"
  kubectl delete pod/opensandbox-e2e-pvc-seed -n "${E2E_NAMESPACE}" --ignore-not-found=true
}

k8s_e2e_write_server_helm_values() {
  local signing_key
  signing_key=$(openssl rand -base64 32 | tr -d '\n')

  {
    cat <<EOF
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
EOF
    if [ "${E2E_SERVER_GATEWAY_ENABLED:-false}" = "true" ]; then
      cat <<EOF
  gateway:
    enabled: true
    host: "${INGRESS_GATEWAY_ADDRESS}"
    gatewayRouteMode: "${E2E_GATEWAY_ROUTE_MODE:-header}"
    dataplaneNamespace: "${E2E_NAMESPACE}"
    replicaCount: 1
    image:
      repository: ${INGRESS_IMG_REPOSITORY}
      tag: "${INGRESS_IMG_TAG}"
    resources:
      limits:
        cpu: "1"
        memory: 1Gi
      requests:
        cpu: "250m"
        memory: 512Mi
    secureAccess:
      activeKey: "a"
      keys:
        - key_id: "a"
          key: "${signing_key}"
EOF
    fi
    cat <<EOF
configToml: |
  [server]
  host = "0.0.0.0"
  port = 80
  api_key = "kubernetes-e2e"

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
  batchsandbox_template_file = "/etc/opensandbox/e2e.batchsandbox-template.yaml"

  [storage]
  allowed_host_paths = []
EOF
  } > "${SERVER_VALUES_FILE}"
}

k8s_e2e_validate_rendered_config_toml() {
  python3 - <<'PY' "${REPO_ROOT}" "${SERVER_VALUES_FILE}"
import subprocess
import sys

try:
    import tomllib
except ModuleNotFoundError:
    import tomli as tomllib

repo_root, values_file = sys.argv[1], sys.argv[2]
chart_path = f"{repo_root}/kubernetes/charts/opensandbox-server"

rendered = subprocess.run(
    ["helm", "template", "opensandbox-server", chart_path, "-f", values_file],
    check=True,
    capture_output=True,
    text=True,
).stdout

config_lines = []
capturing = False
for line in rendered.splitlines():
    if line == "  config.toml: |":
        capturing = True
        continue
    if capturing:
        if line.startswith("---"):
            break
        if line.startswith("    "):
            config_lines.append(line[4:])
            continue
        if line.strip() == "":
            config_lines.append("")
            continue
        break

if not config_lines:
    raise RuntimeError("Failed to extract config.toml from rendered Helm manifest")

tomllib.loads("\n".join(config_lines) + "\n")
PY
}

k8s_e2e_helm_install_server() {
  kubectl get namespace "${SERVER_NAMESPACE}" >/dev/null 2>&1 || kubectl create namespace "${SERVER_NAMESPACE}"
  k8s_e2e_validate_rendered_config_toml

  helm upgrade --install "${SERVER_RELEASE}" "${REPO_ROOT}/kubernetes/charts/opensandbox-server" \
    --namespace "${SERVER_NAMESPACE}" \
    --create-namespace \
    -f "${SERVER_VALUES_FILE}"
  if ! kubectl wait --for=condition=available --timeout=180s deployment/opensandbox-server -n "${SERVER_NAMESPACE}"; then
    kubectl get pods -n "${SERVER_NAMESPACE}" -o wide || true
    kubectl describe deployment/opensandbox-server -n "${SERVER_NAMESPACE}" || true
    kubectl describe pods -n "${SERVER_NAMESPACE}" -l app.kubernetes.io/name=opensandbox-server || true
    kubectl logs -n "${SERVER_NAMESPACE}" deployment/opensandbox-server --all-containers=true || true
    exit 1
  fi
  if [ "${E2E_SERVER_GATEWAY_ENABLED:-false}" = "true" ]; then
    kubectl wait --for=condition=available --timeout=180s deployment/opensandbox-ingress-gateway -n "${SERVER_NAMESPACE}"
  fi
}

k8s_e2e_wait_http_ok() {
  local url="$1"
  local i
  for i in $(seq 1 30); do
    if curl -fsS "${url}" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  curl -fsS "${url}" >/dev/null
}

# Exports for tests/python (see tests/base_e2e_test.get_e2e_sandbox_resource).
k8s_e2e_export_sandbox_resource_env() {
  export OPENSANDBOX_E2E_SANDBOX_CPU="${OPENSANDBOX_E2E_SANDBOX_CPU:-250m}"
  export OPENSANDBOX_E2E_SANDBOX_MEMORY="${OPENSANDBOX_E2E_SANDBOX_MEMORY:-512Mi}"
}

k8s_e2e_generate_sdk_and_run_kubernetes_mini() {
  cd "${REPO_ROOT}/sdks/sandbox/python"
  make generate-api
  cd "${REPO_ROOT}/tests/python"
  uv sync --all-extras --refresh
  make test-kubernetes-mini
}
