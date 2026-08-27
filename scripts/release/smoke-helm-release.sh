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

readonly DEFAULT_KIND_NODE_IMAGE="kindest/node:v1.30.13@sha256:8673291894dc400e0fb4f57243f5fdc6e355ceaa765505e0e73941aa1b6e0b80"
readonly DEFAULT_SANDBOX_IMAGE="ubuntu:24.04@sha256:4fbb8e6a8395de5a7550b33509421a2bafbc0aab6c06ba2cef9ebffbc7092d90"
readonly RELEASE_NAME="opensandbox-release-smoke"
readonly CONTROL_NAMESPACE="opensandbox-system"
readonly SANDBOX_NAMESPACE="opensandbox"
readonly API_KEY_SECRET="opensandbox-api-key"

usage() {
  cat <<'EOF'
Usage:
  scripts/release/smoke-helm-release.sh --package <opensandbox-X.Y.Z.tgz> [options] -- <command> [args...]

Required:
  --package <path>          Exact, already packaged all-in-one opensandbox chart.
  -- <command> [args...]    Child E2E command to run after the release is healthy.

Options:
  --artifacts-dir <path>    Directory for render output, observations, and diagnostics.
  --kind-node-image <ref>   Kind node image (default: pinned Kubernetes v1.30.13).
  --sandbox-image <ref>     Sandbox image passed to the child command (default: pinned Ubuntu 24.04).
  --help                    Show this help.

The child command inherits KUBECONFIG and these E2E variables:
  OPENSANDBOX_TEST_DOMAIN, OPENSANDBOX_TEST_PROTOCOL,
  OPENSANDBOX_TEST_API_KEY, OPENSANDBOX_SANDBOX_DEFAULT_IMAGE,
  OPENSANDBOX_E2E_RUNTIME, OPENSANDBOX_TEST_USE_SERVER_PROXY,
  OPENSANDBOX_E2E_NAMESPACE, OPENSANDBOX_E2E_SANDBOX_CPU, and
  OPENSANDBOX_E2E_SANDBOX_MEMORY.
EOF
}

log() {
  echo "[helm-smoke] $*"
}

warn() {
  echo "[helm-smoke][warn] $*" >&2
}

die() {
  echo "[helm-smoke][error] $*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Missing required command: $1"
}

metadata_value() {
  local key="$1"
  local metadata="$2"
  awk -F': *' -v key="$key" '$1 == key {gsub(/^"|"$/, "", $2); print $2; exit}' <<<"$metadata"
}

package_path=""
artifacts_dir="${HELM_SMOKE_ARTIFACTS_DIR:-}"
kind_node_image="${KIND_NODE_IMAGE:-$DEFAULT_KIND_NODE_IMAGE}"
sandbox_image="${SANDBOX_TEST_IMAGE:-$DEFAULT_SANDBOX_IMAGE}"
child_command=()

while (($# > 0)); do
  case "$1" in
    --package)
      (($# >= 2)) || die "--package requires a path"
      package_path="$2"
      shift 2
      ;;
    --artifacts-dir)
      (($# >= 2)) || die "--artifacts-dir requires a path"
      artifacts_dir="$2"
      shift 2
      ;;
    --kind-node-image)
      (($# >= 2)) || die "--kind-node-image requires an image reference"
      kind_node_image="$2"
      shift 2
      ;;
    --sandbox-image)
      (($# >= 2)) || die "--sandbox-image requires an image reference"
      sandbox_image="$2"
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    --)
      shift
      child_command=("$@")
      break
      ;;
    *)
      die "Unknown argument: $1"
      ;;
  esac
done

[[ -n "$package_path" ]] || die "--package is required"
[[ ${#child_command[@]} -gt 0 ]] || die "A child command is required after --"
[[ -f "$package_path" ]] || die "Helm package does not exist: $package_path"
[[ "$package_path" == *.tgz ]] || die "Helm package must be a .tgz file: $package_path"

for cmd in awk comm cp curl docker grep head helm jq kind kubectl mktemp mv openssl rm sed seq sha256sum sort tar tee timeout tr; do
  require_cmd "$cmd"
done

package_dir="$(cd "$(dirname "$package_path")" && pwd -P)"
package_path="${package_dir}/$(basename "$package_path")"

run_token="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$-${RANDOM}"
run_token="$(printf '%s' "$run_token" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
cluster_name="opensandbox-helm-smoke-${run_token}"
cluster_name="${cluster_name:0:63}"

if kind get clusters 2>/dev/null | grep -Fxq "$cluster_name"; then
  die "Generated Kind cluster name already exists: $cluster_name"
fi

runtime_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
if [[ -z "$artifacts_dir" ]]; then
  artifacts_dir="${runtime_root%/}/${cluster_name}-artifacts"
fi
mkdir -p "$artifacts_dir"
artifacts_dir="$(cd "$artifacts_dir" && pwd -P)"

work_dir="$(mktemp -d "${runtime_root%/}/opensandbox-helm-smoke-work.XXXXXX")"
kubeconfig_path="${work_dir}/kubeconfig"
values_file="${artifacts_dir}/smoke-values.yaml"
rendered_file="${artifacts_dir}/rendered.yaml"
render_warnings_file="${artifacts_dir}/render-warnings.txt"
port_forward_log="${artifacts_dir}/server-port-forward.log"
observer_dir="${artifacts_dir}/runtime-observer"
mkdir -p "$observer_dir"

export KUBECONFIG="$kubeconfig_path"

cluster_owned=0
port_forward_pid=""
runtime_observer_pid=""
diagnostics_collected=0
run_outcome="failed"

stop_process() {
  local pid="$1"
  local killer_pid
  [[ -n "$pid" ]] || return 0
  if kill -0 "$pid" >/dev/null 2>&1; then
    kill -TERM "$pid" >/dev/null 2>&1 || true
    (
      sleep 10
      if kill -0 "$pid" >/dev/null 2>&1; then
        warn "Process ${pid} did not stop after SIGTERM; sending SIGKILL"
        kill -KILL "$pid" >/dev/null 2>&1 || true
      fi
    ) &
    killer_pid=$!
    wait "$pid" >/dev/null 2>&1 || true
    kill -TERM "$killer_pid" >/dev/null 2>&1 || true
    wait "$killer_pid" >/dev/null 2>&1 || true
  else
    wait "$pid" >/dev/null 2>&1 || true
  fi
}

diagnostic_kubectl() {
  timeout --kill-after=5s 30s kubectl --request-timeout=10s "$@"
}

diagnostic_helm() {
  timeout --kill-after=5s 30s helm "$@"
}

collect_pod_logs() {
  local namespace pod container safe_name
  diagnostic_kubectl get pods --all-namespaces -o json 2>/dev/null |
    jq -r '.items[] | [.metadata.namespace, .metadata.name] | @tsv' |
    while IFS=$'\t' read -r namespace pod; do
      [[ -n "$namespace" && -n "$pod" ]] || continue
      safe_name="${namespace}__${pod}"
      diagnostic_kubectl logs --namespace "$namespace" "pod/${pod}" \
        --all-containers=true --prefix=true \
        >"${artifacts_dir}/pod-logs-${safe_name}.txt" 2>&1 || true
    done

  diagnostic_kubectl get pods --all-namespaces -o json 2>/dev/null |
    jq -r '
      .items[] as $pod
      | (
          ($pod.status.initContainerStatuses // [])
          + ($pod.status.containerStatuses // [])
          + ($pod.status.ephemeralContainerStatuses // [])
        )[]
      | select((.restartCount // 0) > 0)
      | [$pod.metadata.namespace, $pod.metadata.name, .name]
      | @tsv
    ' |
    while IFS=$'\t' read -r namespace pod container; do
      [[ -n "$namespace" && -n "$pod" && -n "$container" ]] || continue
      safe_name="${namespace}__${pod}__${container}"
      diagnostic_kubectl logs --namespace "$namespace" "pod/${pod}" \
        --container "$container" --previous --prefix=true \
        >"${artifacts_dir}/pod-logs-${safe_name}-previous.txt" 2>&1 || true
    done
}

collect_diagnostics() {
  ((diagnostics_collected == 0)) || return 0
  diagnostics_collected=1

  log "Collecting ${run_outcome} diagnostics in ${artifacts_dir}"
  {
    echo "outcome=${run_outcome}"
    echo "cluster=${cluster_name}"
    echo "package=${package_path}"
    echo "package_sha256=$(sha256sum "$package_path" | awk '{print $1}')"
    echo "kind_node_image=${kind_node_image}"
    echo "sandbox_image=${sandbox_image}"
  } >"${artifacts_dir}/result.txt"

  timeout --kill-after=5s 10s kind get clusters >"${artifacts_dir}/kind-clusters.txt" 2>&1 || true
  if ! grep -Fxq "$cluster_name" "${artifacts_dir}/kind-clusters.txt"; then
    return 0
  fi

  diagnostic_kubectl cluster-info >"${artifacts_dir}/cluster-info.txt" 2>&1 || true
  diagnostic_kubectl get nodes -o wide >"${artifacts_dir}/nodes-wide.txt" 2>&1 || true
  diagnostic_kubectl get nodes -o yaml >"${artifacts_dir}/nodes.yaml" 2>&1 || true
  diagnostic_kubectl get crds -o yaml >"${artifacts_dir}/crds.yaml" 2>&1 || true
  diagnostic_kubectl get deployments --all-namespaces -o yaml >"${artifacts_dir}/deployments.yaml" 2>&1 || true
  diagnostic_kubectl get pods --all-namespaces -o wide >"${artifacts_dir}/pods-wide.txt" 2>&1 || true
  diagnostic_kubectl get pods --all-namespaces -o yaml >"${artifacts_dir}/pods.yaml" 2>&1 || true
  diagnostic_kubectl get services --all-namespaces -o yaml >"${artifacts_dir}/services.yaml" 2>&1 || true
  diagnostic_kubectl get endpointslices --all-namespaces -o yaml >"${artifacts_dir}/endpointslices.yaml" 2>&1 || true
  diagnostic_kubectl get batchsandboxes --all-namespaces -o yaml >"${artifacts_dir}/batchsandboxes.yaml" 2>&1 || true
  diagnostic_kubectl get events --all-namespaces --sort-by=.lastTimestamp >"${artifacts_dir}/events.txt" 2>&1 || true
  diagnostic_kubectl describe pods --all-namespaces >"${artifacts_dir}/pods-describe.txt" 2>&1 || true
  diagnostic_kubectl describe deployments --all-namespaces >"${artifacts_dir}/deployments-describe.txt" 2>&1 || true
  diagnostic_helm status "$RELEASE_NAME" --namespace "$CONTROL_NAMESPACE" --output yaml \
    >"${artifacts_dir}/helm-status.yaml" 2>&1 || true
  diagnostic_helm get values "$RELEASE_NAME" --namespace "$CONTROL_NAMESPACE" --all \
    >"${artifacts_dir}/helm-values.yaml" 2>&1 || true
  diagnostic_helm get manifest "$RELEASE_NAME" --namespace "$CONTROL_NAMESPACE" \
    >"${artifacts_dir}/helm-manifest.yaml" 2>&1 || true
  collect_pod_logs
  timeout --kill-after=5s 60s kind export logs "${artifacts_dir}/kind-logs" --name "$cluster_name" \
    >"${artifacts_dir}/kind-export-logs.txt" 2>&1 || true
}

delete_owned_cluster() {
  ((cluster_owned == 1)) || return 0
  if [[ ! "$cluster_name" =~ ^opensandbox-helm-smoke-[a-z0-9-]+$ ]]; then
    warn "Refusing to delete unexpected cluster name: $cluster_name"
    return 1
  fi
  log "Deleting owned Kind cluster ${cluster_name}"
  timeout --kill-after=5s 120s kind delete cluster --name "$cluster_name" || return 1
  timeout --kill-after=5s 10s kind get clusters >"${work_dir}/kind-clusters-after-delete.txt" || return 1
  if grep -Fxq "$cluster_name" "${work_dir}/kind-clusters-after-delete.txt"; then
    warn "Owned Kind cluster still exists after delete: ${cluster_name}"
    return 1
  fi
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e

  stop_process "$runtime_observer_pid"
  stop_process "$port_forward_pid"
  collect_diagnostics
  if ! delete_owned_cluster; then
    warn "Failed to delete owned Kind cluster ${cluster_name}"
    echo "cluster_cleanup=failed" >>"${artifacts_dir}/result.txt"
    ((status == 0)) && status=1
  else
    echo "cluster_cleanup=passed" >>"${artifacts_dir}/result.txt"
  fi

  if [[ -n "$work_dir" && -d "$work_dir" && "$(basename "$work_dir")" == opensandbox-helm-smoke-work.* ]]; then
    rm -rf -- "$work_dir"
  fi

  log "Artifacts: ${artifacts_dir}"
  exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

chart_metadata="$(helm show chart "$package_path")"
chart_name="$(metadata_value name "$chart_metadata")"
chart_version="$(metadata_value version "$chart_metadata")"
chart_app_version="$(metadata_value appVersion "$chart_metadata")"

[[ "$chart_name" == "opensandbox" ]] || die "Expected all-in-one chart 'opensandbox', got '${chart_name}'"
[[ -n "$chart_version" ]] || die "Packaged chart has no version"
[[ -n "$chart_app_version" ]] || die "Packaged chart has no appVersion"

tar -tzf "$package_path" >"${work_dir}/archive-files.txt"
if grep -Eq '(^/|(^|/)\.\.(/|$))' "${work_dir}/archive-files.txt"; then
  die "Helm package contains an unsafe archive path"
fi
package_extract_dir="${work_dir}/package"
mkdir -p "$package_extract_dir"
tar -xzf "$package_path" -C "$package_extract_dir"
embedded_server_chart="${package_extract_dir}/opensandbox/charts/opensandbox-server"
for embedded_chart_name in opensandbox-controller opensandbox-server opensandbox-node-agent; do
  embedded_chart_path="${package_extract_dir}/opensandbox/charts/${embedded_chart_name}"
  [[ -f "${embedded_chart_path}/Chart.yaml" ]] || \
    die "Exact package is missing expanded embedded chart opensandbox/charts/${embedded_chart_name}/Chart.yaml"
  helm show chart "$embedded_chart_path" \
    >"${artifacts_dir}/${embedded_chart_name}-chart-metadata.yaml"
done
server_metadata="$(helm show chart "$embedded_server_chart")"
server_app_version="$(metadata_value appVersion "$server_metadata")"
[[ -n "$server_app_version" ]] || die "Embedded server chart has no appVersion"

sha256sum "$package_path" >"${artifacts_dir}/package.sha256"
printf '%s\n' "$chart_metadata" >"${artifacts_dir}/chart-metadata.yaml"
printf '%s\n' "$server_metadata" >"${artifacts_dir}/server-chart-metadata.yaml"
helm lint "$package_path" >"${artifacts_dir}/helm-lint.txt" 2>&1

cat >"$values_file" <<EOF
opensandbox-server:
  server:
    replicaCount: 1
    resources:
      limits:
        cpu: "1"
        memory: 2Gi
      requests:
        cpu: "250m"
        memory: 512Mi
    env:
      - name: OPENSANDBOX_SERVER_API_KEY
        valueFrom:
          secretKeyRef:
            name: ${API_KEY_SECRET}
            key: api-key
EOF

helm template "$RELEASE_NAME" "$package_path" \
  --namespace "$CONTROL_NAMESPACE" \
  --values "$values_file" \
  >"$rendered_file" 2>"$render_warnings_file"

grep -Fq 'name: opensandbox-controller-manager' "$rendered_file" || \
  die "Exact package did not render the controller Deployment"
grep -Fq 'name: opensandbox-server' "$rendered_file" || \
  die "Exact package did not render the server resources"
if grep -Fq -- '--containerd-socket-path' "$rendered_file"; then
  die "Rendered controller unexpectedly contains --containerd-socket-path"
fi

mapfile -t rendered_images < <(
  awk '$1 == "image:" {gsub(/^"|"$/, "", $2); print $2}' "$rendered_file" | sort -u
)
execd_image="$(
  sed -n 's/^[[:space:]]*execd_image = "\([^"]*\)"[[:space:]]*$/\1/p' "$rendered_file" |
    head -n 1
)"
[[ ${#rendered_images[@]} -ge 2 ]] || die "Could not discover controller and server images from exact package render"
[[ -n "$execd_image" ]] || die "Could not discover execd image from exact package render"
printf '%s\n' "${rendered_images[@]}" "$execd_image" "$sandbox_image" | sort -u \
  >"${artifacts_dir}/images.txt"

log "Creating unique Kind cluster ${cluster_name} with ${kind_node_image}"
cluster_owned=1
kind create cluster \
  --name "$cluster_name" \
  --image "$kind_node_image" \
  --kubeconfig "$kubeconfig_path" \
  --wait 180s

kubectl wait --for=condition=Ready nodes --all --timeout=120s
kubectl get nodes -o json | jq -e '
  .items | length > 0 and all(.[]; .status.nodeInfo.architecture == "amd64")
' >/dev/null || die "Smoke requires linux/amd64 Kind nodes"

pull_and_maybe_load_image() {
  local image="$1"
  local attempt
  for attempt in 1 2 3; do
    if docker pull --platform linux/amd64 "$image"; then
      break
    fi
    if ((attempt == 3)); then
      die "Failed to pull image after ${attempt} attempts: $image"
    fi
    warn "Image pull attempt ${attempt} failed for ${image}; retrying"
    sleep $((attempt * 5))
  done

  docker image inspect --format '{{json .}}' "$image" |
    jq -c --arg requested_reference "$image" \
      '. + {requestedReference: $requested_reference}' \
      >>"${artifacts_dir}/docker-image-inspect.jsonl"

  # kind load imports digest-only images under a synthetic import-* reference.
  # On containerd 2.x this can leave kubelet resolving the image ID through a
  # reference that no longer exists, so let the node pull immutable digests.
  if [[ "$image" == *@sha256:* ]]; then
    log "Using direct cluster pull for digest-qualified image ${image}"
    return
  fi

  kind load docker-image --name "$cluster_name" "$image"
}

: >"${artifacts_dir}/docker-image-inspect.jsonl"
while IFS= read -r image; do
  [[ -n "$image" ]] || continue
  log "Preparing release smoke image ${image}"
  pull_and_maybe_load_image "$image"
done <"${artifacts_dir}/images.txt"

jq -s -e '
  length > 0 and all(.[];
    ((.requestedReference // "") | length) > 0 and
    ((.RepoDigests // []) | length) > 0 and
    ((.Id // "") | startswith("sha256:"))
  )
' "${artifacts_dir}/docker-image-inspect.jsonl" >/dev/null || \
  die "A pulled release image is missing a requested reference, RepoDigest, or image ID"
jq -r '
  [
    .requestedReference,
    ((.RepoDigests // []) | sort | join(",")),
    .Id
  ] | @tsv
' "${artifacts_dir}/docker-image-inspect.jsonl" |
  sort >"${artifacts_dir}/tested-images.tsv"
[[ "$(wc -l <"${artifacts_dir}/tested-images.tsv" | tr -d '[:space:]')" == \
  "$(wc -l <"${artifacts_dir}/images.txt" | tr -d '[:space:]')" ]] || \
  die "Did not record one tested image identity for every release image"

kubectl create namespace "$CONTROL_NAMESPACE"
kubectl create namespace "$SANDBOX_NAMESPACE"

api_key="$(openssl rand -hex 32)"
if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  echo "::add-mask::${api_key}"
fi
kubectl create secret generic "$API_KEY_SECRET" \
  --namespace "$CONTROL_NAMESPACE" \
  --from-literal="api-key=${api_key}"

log "Installing exact package $(basename "$package_path")"
helm install "$RELEASE_NAME" "$package_path" \
  --namespace "$CONTROL_NAMESPACE" \
  --values "$values_file" \
  --wait \
  --wait-for-jobs \
  --timeout 5m

for crd in \
  batchsandboxes.sandbox.opensandbox.io \
  pools.sandbox.opensandbox.io \
  sandboxsnapshots.sandbox.opensandbox.io; do
  kubectl wait --for=condition=Established "crd/${crd}" --timeout=60s
done

kubectl rollout status \
  --namespace "$CONTROL_NAMESPACE" \
  deployment/opensandbox-controller-manager \
  --timeout=180s
kubectl rollout status \
  --namespace "$CONTROL_NAMESPACE" \
  deployment/opensandbox-server \
  --timeout=180s

assert_deployment_pods() {
  local name="$1"
  local selector="$2"
  local output_file="$3"

  kubectl get pods --namespace "$CONTROL_NAMESPACE" --selector "$selector" -o json >"$output_file"
  jq -e '
    (.items | length) == 1 and
    all(.items[];
      ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) == 1 and
      (.status.containerStatuses | length) > 0 and
      all(.status.containerStatuses[];
        .ready == true and
        .restartCount == 0 and
        ((.imageID // "") | length) > 0
      )
    )
  ' "$output_file" >/dev/null || \
    die "${name} Pod is not singular, Ready, restart-free, and backed by a resolved imageID"
}

assert_control_plane_logs_clean() {
  local name="$1"
  local selector="$2"
  local output_file="$3"

  kubectl logs \
    --namespace "$CONTROL_NAMESPACE" \
    --selector "$selector" \
    --all-containers=true \
    --prefix=true \
    --tail=-1 \
    >"$output_file" 2>&1
  if grep -Eq 'flag provided but not defined|Startup blocked|panic:' "$output_file"; then
    die "${name} logs contain a fatal startup signature; see ${output_file}"
  fi
}

controller_selector="control-plane=controller-manager,app.kubernetes.io/instance=${RELEASE_NAME}"
server_selector="app.kubernetes.io/name=opensandbox-server,app.kubernetes.io/instance=${RELEASE_NAME}"
assert_deployment_pods controller "$controller_selector" "${artifacts_dir}/controller-pods.json"
assert_deployment_pods server "$server_selector" "${artifacts_dir}/server-pods.json"

kubectl get deployment opensandbox-controller-manager \
  --namespace "$CONTROL_NAMESPACE" -o json \
  >"${artifacts_dir}/controller-deployment.json"
kubectl get deployment opensandbox-server \
  --namespace "$CONTROL_NAMESPACE" -o json \
  >"${artifacts_dir}/server-deployment.json"
jq -e '
  .spec.replicas == 1 and
  any(.spec.template.spec.containers[];
    .name == "main" and
    .resources.requests.cpu == "250m" and
    .resources.requests.memory == "512Mi" and
    .resources.limits.cpu == "1" and
    .resources.limits.memory == "2Gi"
  )
' "${artifacts_dir}/server-deployment.json" >/dev/null || \
  die "Live server Deployment does not contain only the intended scheduling overrides"
jq -e '
  [
    .spec.template.spec.containers[]
    | select(.name == "manager")
    | .args[]?
    | select(startswith("--containerd-socket-path"))
  ] | length == 0
' "${artifacts_dir}/controller-deployment.json" >/dev/null || \
  die "Live controller Deployment contains --containerd-socket-path"
jq -e '
  all(.items[];
    [
      .spec.containers[]
      | select(.name == "manager")
      | .args[]?
      | select(startswith("--containerd-socket-path"))
    ] | length == 0
  )
' "${artifacts_dir}/controller-pods.json" >/dev/null || \
  die "Live controller Pod contains --containerd-socket-path"

kubectl get endpointslices \
  --namespace "$CONTROL_NAMESPACE" \
  --selector kubernetes.io/service-name=opensandbox-server \
  -o json >"${artifacts_dir}/server-endpointslices.json"
jq -e '
  [.items[].endpoints[]? | select(.conditions.ready == true)] | length > 0
' "${artifacts_dir}/server-endpointslices.json" >/dev/null || \
  die "opensandbox-server has no ready EndpointSlice endpoint"

# Catch immediate crash loops or late readiness regressions after rollout success.
sleep 15
assert_deployment_pods controller "$controller_selector" "${artifacts_dir}/controller-pods-stable.json"
assert_deployment_pods server "$server_selector" "${artifacts_dir}/server-pods-stable.json"

: >"$port_forward_log"
kubectl port-forward \
  --namespace "$CONTROL_NAMESPACE" \
  --address 127.0.0.1 \
  service/opensandbox-server \
  :80 >"$port_forward_log" 2>&1 &
port_forward_pid=$!

local_port=""
for _ in $(seq 1 60); do
  if ! kill -0 "$port_forward_pid" >/dev/null 2>&1; then
    die "Server port-forward exited before becoming ready; see ${port_forward_log}"
  fi
  local_port="$(
    sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) -> 80$/\1/p' "$port_forward_log" |
      head -n 1
  )"
  [[ -z "$local_port" ]] || break
  sleep 1
done
[[ -n "$local_port" ]] || die "Timed out discovering server port-forward port"
server_base_url="http://127.0.0.1:${local_port}"

for _ in $(seq 1 60); do
  if curl --fail --silent --show-error --max-time 5 \
    "${server_base_url}/health" >"${artifacts_dir}/health.json"; then
    break
  fi
  sleep 1
done
jq -e '.status == "healthy"' "${artifacts_dir}/health.json" >/dev/null || \
  die "Server /health did not return the expected healthy response"

version_status="$(
  curl --silent --show-error --max-time 10 \
    --header "OPEN-SANDBOX-API-KEY: ${api_key}" \
    --output "${artifacts_dir}/version.json" \
    --write-out '%{http_code}' \
    "${server_base_url}/version"
)"
case "$version_status" in
  200)
    jq -e --arg expected "$server_app_version" '.version == $expected' \
      "${artifacts_dir}/version.json" >/dev/null || \
      die "Server /version does not match embedded server appVersion ${server_app_version}"
    ;;
  404)
    warn "Server image ${server_app_version} does not expose the optional /version endpoint"
    ;;
  *)
    die "Server /version returned HTTP ${version_status}; expected 200 or compatibility 404"
    ;;
esac

no_key_status="$(
  curl --silent --show-error --max-time 10 \
    --output "${artifacts_dir}/list-without-key.json" \
    --write-out '%{http_code}' \
    "${server_base_url}/v1/sandboxes?pageSize=1"
)"
[[ "$no_key_status" == "401" ]] || \
  die "Protected sandbox list returned ${no_key_status} without an API key; expected 401"
jq -e '.code == "MISSING_API_KEY"' "${artifacts_dir}/list-without-key.json" >/dev/null || \
  die "Missing-key response did not carry MISSING_API_KEY"

invalid_key_status="$(
  curl --silent --show-error --max-time 10 \
    --header "OPEN-SANDBOX-API-KEY: invalid-${run_token}" \
    --output "${artifacts_dir}/list-with-invalid-key.json" \
    --write-out '%{http_code}' \
    "${server_base_url}/v1/sandboxes?pageSize=1"
)"
[[ "$invalid_key_status" == "401" ]] || \
  die "Protected sandbox list returned ${invalid_key_status} with an invalid API key; expected 401"
jq -e '.code == "INVALID_API_KEY"' "${artifacts_dir}/list-with-invalid-key.json" >/dev/null || \
  die "Invalid-key response did not carry INVALID_API_KEY"

with_key_status=""
for _ in $(seq 1 30); do
  if with_key_status="$(
    curl --silent --show-error --max-time 10 \
      --header "OPEN-SANDBOX-API-KEY: ${api_key}" \
      --output "${artifacts_dir}/list-with-key.json" \
      --write-out '%{http_code}' \
      "${server_base_url}/v1/sandboxes?pageSize=1"
  )"; then
    [[ "$with_key_status" != "200" ]] || break
  fi
  sleep 1
done
[[ "$with_key_status" == "200" ]] || \
  die "Protected sandbox list returned ${with_key_status} with the configured API key; expected 200"
jq -e '(.items | type) == "array" and (.pagination | type) == "object"' \
  "${artifacts_dir}/list-with-key.json" >/dev/null || \
  die "Authenticated sandbox list response has an unexpected shape"

initial_batchsandbox_count="$(
  kubectl get batchsandboxes --namespace "$SANDBOX_NAMESPACE" -o json | jq '.items | length'
)"
initial_runtime_pod_count="$(
  kubectl get pods --namespace "$SANDBOX_NAMESPACE" --selector opensandbox.io/id -o json | jq '.items | length'
)"
[[ "$initial_batchsandbox_count" == "0" && "$initial_runtime_pod_count" == "0" ]] || \
  die "Sandbox namespace is not clean before child E2E command"

observe_runtime() {
  local batchsandbox_tmp="${observer_dir}/batchsandboxes.json.tmp"
  local pods_tmp="${observer_dir}/pods.json.tmp"
  local verified_pods_tmp="${observer_dir}/verified-pods.json.tmp"

  while true; do
    if kubectl --request-timeout=10s get batchsandboxes --namespace "$SANDBOX_NAMESPACE" -o json \
      >"$batchsandbox_tmp" 2>/dev/null; then
      mv "$batchsandbox_tmp" "${observer_dir}/batchsandboxes-latest.json"
      if jq -e '.items | length > 0' "${observer_dir}/batchsandboxes-latest.json" >/dev/null; then
        touch "${observer_dir}/saw-batchsandbox"
        cp "${observer_dir}/batchsandboxes-latest.json" "${observer_dir}/batchsandboxes-observed.json"
      fi
      jq -r '
        .items[]
        | select(
            ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) > 0
          )
        | .metadata.name
      ' "${observer_dir}/batchsandboxes-latest.json" \
        >>"${observer_dir}/batchsandbox-ready-names.txt"
    fi

    if kubectl --request-timeout=10s get pods --namespace "$SANDBOX_NAMESPACE" --selector opensandbox.io/id -o json \
      >"$pods_tmp" 2>/dev/null; then
      mv "$pods_tmp" "${observer_dir}/pods-latest.json"
      if jq -e '.items | length > 0' "${observer_dir}/pods-latest.json" >/dev/null; then
        touch "${observer_dir}/saw-runtime-pod"
        cp "${observer_dir}/pods-latest.json" "${observer_dir}/pods-observed.json"
      fi

      jq '
        .items
        | map(select(
            ([.status.conditions[]? | select(.type == "Ready" and .status == "True")] | length) > 0 and
            ([.status.initContainerStatuses[]?
              | select(
                  .name == "execd-installer" and
                  .state.terminated.exitCode == 0 and
                  .restartCount == 0 and
                  ((.imageID // "") | length) > 0
                )
             ] | length) > 0 and
            ([.status.containerStatuses[]?
              | select(
                  .name == "sandbox" and
                  .ready == true and
                  .restartCount == 0 and
                  ((.imageID // "") | length) > 0
                )
             ] | length) > 0
          ))
      ' "${observer_dir}/pods-latest.json" >"$verified_pods_tmp"

      if jq -e 'length > 0' "$verified_pods_tmp" >/dev/null; then
        mv "$verified_pods_tmp" "${observer_dir}/verified-runtime-pods.json"
        jq -r '.[].metadata.labels["opensandbox.io/id"]' \
          "${observer_dir}/verified-runtime-pods.json" \
          >>"${observer_dir}/verified-runtime-pod-ids.txt"
      else
        rm -f "$verified_pods_tmp"
      fi
    fi
    sleep 1
  done
}

: >"${observer_dir}/batchsandbox-ready-names.txt"
: >"${observer_dir}/verified-runtime-pod-ids.txt"
observe_runtime &
runtime_observer_pid=$!

export OPENSANDBOX_TEST_DOMAIN="127.0.0.1:${local_port}"
export OPENSANDBOX_TEST_PROTOCOL="http"
export OPENSANDBOX_TEST_API_KEY="$api_key"
export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="$sandbox_image"
export OPENSANDBOX_E2E_RUNTIME="kubernetes"
export OPENSANDBOX_TEST_USE_SERVER_PROXY="true"
export OPENSANDBOX_E2E_NAMESPACE="$SANDBOX_NAMESPACE"
export OPENSANDBOX_E2E_SANDBOX_CPU="250m"
export OPENSANDBOX_E2E_SANDBOX_MEMORY="512Mi"
export OPENSANDBOX_EXECD_IMAGE="$execd_image"
export HELM_SMOKE_ARTIFACTS_DIR="$artifacts_dir"
export HELM_SMOKE_RELEASE_NAME="$RELEASE_NAME"

log "Running child E2E command"
printf '[helm-smoke] Command:'
printf ' %q' "${child_command[@]}"
printf '\n'
set +e
"${child_command[@]}" 2>&1 | tee "${artifacts_dir}/child-command.log"
pipeline_status=("${PIPESTATUS[@]}")
set -e
child_status="${pipeline_status[0]}"
tee_status="${pipeline_status[1]}"

if ((tee_status != 0)); then
  die "Failed to persist child E2E command output (tee status ${tee_status})"
fi

if ((child_status != 0)); then
  warn "Child E2E command failed with status ${child_status}"
  exit "$child_status"
fi

wait_for_runtime_cleanup() {
  local deadline=$((SECONDS + 120))
  local batchsandbox_count runtime_pod_count
  local query_failure_reported=0
  while ((SECONDS < deadline)); do
    if batchsandbox_count="$(
      kubectl --request-timeout=10s get batchsandboxes --namespace "$SANDBOX_NAMESPACE" -o json |
        jq '.items | length'
    )" && runtime_pod_count="$(
      kubectl --request-timeout=10s get pods --namespace "$SANDBOX_NAMESPACE" --selector opensandbox.io/id -o json |
        jq '.items | length'
    )"; then
      if [[ "$batchsandbox_count" == "0" && "$runtime_pod_count" == "0" ]]; then
        return 0
      fi
    elif ((query_failure_reported == 0)); then
      warn "Transient Kubernetes API failure while waiting for runtime cleanup; retrying"
      query_failure_reported=1
    fi
    sleep 2
  done
  return 1
}

wait_for_runtime_cleanup || die "BatchSandbox or runtime Pod remained after successful child cleanup"
sleep 2
stop_process "$runtime_observer_pid"
runtime_observer_pid=""

[[ -f "${observer_dir}/saw-batchsandbox" ]] || \
  die "Child command succeeded but no BatchSandbox was observed"
[[ -f "${observer_dir}/saw-runtime-pod" ]] || \
  die "Child command succeeded but no labeled runtime Pod was observed"
[[ -s "${observer_dir}/batchsandbox-ready-names.txt" ]] || \
  die "No BatchSandbox with Ready=True was observed"
[[ -s "${observer_dir}/verified-runtime-pod-ids.txt" ]] || \
  die "No Ready runtime Pod with successful execd-installer and resolved imageIDs was observed"

comm -12 \
  <(sort -u "${observer_dir}/batchsandbox-ready-names.txt") \
  <(sort -u "${observer_dir}/verified-runtime-pod-ids.txt") \
  >"${observer_dir}/verified-sandbox-ids.txt"
[[ -s "${observer_dir}/verified-sandbox-ids.txt" ]] || \
  die "Ready BatchSandbox and fully verified runtime Pod did not share a sandbox ID"

kubectl get batchsandboxes --namespace "$SANDBOX_NAMESPACE" -o json \
  >"${observer_dir}/batchsandboxes-after-cleanup.json"
kubectl get pods --namespace "$SANDBOX_NAMESPACE" --selector opensandbox.io/id -o json \
  >"${observer_dir}/pods-after-cleanup.json"

kubectl rollout status \
  --namespace "$CONTROL_NAMESPACE" \
  deployment/opensandbox-controller-manager \
  --timeout=60s
kubectl rollout status \
  --namespace "$CONTROL_NAMESPACE" \
  deployment/opensandbox-server \
  --timeout=60s
assert_deployment_pods controller "$controller_selector" "${artifacts_dir}/controller-pods-after-child.json"
assert_deployment_pods server "$server_selector" "${artifacts_dir}/server-pods-after-child.json"
assert_control_plane_logs_clean \
  controller "$controller_selector" "${artifacts_dir}/controller-logs-after-child.txt"
assert_control_plane_logs_clean \
  server "$server_selector" "${artifacts_dir}/server-logs-after-child.txt"

run_outcome="passed"
log "Exact Helm package smoke passed: $(basename "$package_path")"
log "Chart ${chart_version}, app ${chart_app_version}, server ${server_app_version}"
