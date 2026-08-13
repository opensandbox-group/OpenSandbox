#!/usr/bin/env bash
# 仅用于永久 pytest 用例 test_01g_pvc_unseeded_subpath_initializer 的本地 Kind POC。
# 默认只展示计划；只有显式确认后才会创建唯一的 Kind 集群及其集群内资源。

set -euo pipefail

readonly KIND_CLUSTER='opensandbox-subpath-poc-kind'
readonly KIND_NODE="${KIND_CLUSTER}-control-plane"
readonly LIFECYCLE_LOCAL_PORT='18081'
readonly LOCAL_PROXY_URL='http://127.0.0.1:7897'
readonly POC_RESOURCE_PREFIX='opensandbox-subpath-poc-kind'
readonly TEST_NODE='tests/python/tests/test_sandbox_e2e_sync.py::TestSandboxE2ESync::test_01g_pvc_unseeded_subpath_initializer'
KIND_CLUSTER_CREATION_STARTED=0

SCRIPT_DIRECTORY="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly SCRIPT_DIRECTORY
REPO_ROOT="$(CDPATH= cd -- "${SCRIPT_DIRECTORY}/.." && pwd -P)"
readonly REPO_ROOT
readonly POC_DIRECTORY="${REPO_ROOT}/poc/opensandbox-subpath-poc-f6d69fcf375c7ca1"
readonly STATE_DIRECTORY="${POC_DIRECTORY}/.local-kind-state"
readonly KUBECONFIG_PATH="${STATE_DIRECTORY}/kubeconfig"
readonly SERVER_VALUES_FILE="${STATE_DIRECTORY}/server-values.yaml"
readonly PORT_FORWARD_LOG="${STATE_DIRECTORY}/server-port-forward.log"

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Required command is unavailable: $1"
}

clear_proxy_environment() {
  unset http_proxy https_proxy all_proxy HTTP_PROXY HTTPS_PROXY ALL_PROXY no_proxy NO_PROXY
}

set_kubernetes_no_proxy() {
  # Kind, Kubernetes, port-forward, and pytest must never inherit host proxy routing.
  clear_proxy_environment
  export no_proxy='*'
  export NO_PROXY='*'
}

set_build_tools_proxy() {
  # This short pre-Kind phase may use only the verified local host proxy to
  # cache controller-gen. Docker daemon proxy configuration is not modified.
  clear_proxy_environment
  export http_proxy="$LOCAL_PROXY_URL"
  export https_proxy="$LOCAL_PROXY_URL"
  export HTTP_PROXY="$LOCAL_PROXY_URL"
  export HTTPS_PROXY="$LOCAL_PROXY_URL"
}

remove_private_directory() {
  local directory=$1
  local expected_prefix=$2

  [[ "$directory" == "${expected_prefix}"* && -d "$directory" && ! -L "$directory" ]] || die \
    "Refusing to remove unexpected private directory: ${directory}"
  rm -rf -- "$directory"
}

create_runtime_image_build_docker_wrapper() {
  local wrapper_directory=$1

  cat >"${wrapper_directory}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" != 'build' ]]; then
  exec "$POC_REAL_DOCKER" "$@"
fi
shift

declare -a build_arguments=()
build_context=''
while (($#)); do
  case "$1" in
    -f|--file|-t|--tag|--build-arg)
      (($# >= 2)) || { printf 'ERROR: docker build wrapper requires a value for %s\n' "$1" >&2; exit 64; }
      build_arguments+=("$1" "$2")
      shift 2
      ;;
    --file=*|--tag=*|--build-arg=*)
      build_arguments+=("$1")
      shift
      ;;
    --)
      shift
      (($# == 1 && -z "$build_context")) || { printf 'ERROR: docker build wrapper accepts exactly one build context\n' >&2; exit 64; }
      build_context="$1"
      shift
      ;;
    -*)
      printf 'ERROR: refusing unsupported docker build flag: %s\n' "$1" >&2
      exit 64
      ;;
    *)
      [[ -z "$build_context" ]] || { printf 'ERROR: docker build wrapper accepts exactly one build context\n' >&2; exit 64; }
      build_context="$1"
      shift
      ;;
  esac
done
[[ -n "$build_context" ]] || { printf 'ERROR: docker build wrapper requires exactly one build context\n' >&2; exit 64; }

exec "$POC_REAL_DOCKER" build --network=host \
  --build-arg "http_proxy=${POC_RUNTIME_BUILD_PROXY_URL}" \
  --build-arg "https_proxy=${POC_RUNTIME_BUILD_PROXY_URL}" \
  --build-arg "HTTP_PROXY=${POC_RUNTIME_BUILD_PROXY_URL}" \
  --build-arg "HTTPS_PROXY=${POC_RUNTIME_BUILD_PROXY_URL}" \
  --build-arg "all_proxy=${POC_RUNTIME_BUILD_PROXY_URL}" \
  --build-arg "ALL_PROXY=${POC_RUNTIME_BUILD_PROXY_URL}" \
  "${build_arguments[@]}" "$build_context"
EOF
  chmod 700 -- "${wrapper_directory}/docker"
}

run_runtime_image_builds_with_local_proxy() {
  local wrapper_directory
  local real_docker
  local original_path=$PATH
  local build_status=0

  real_docker="$(type -P docker)"
  [[ -n "$real_docker" && -x "$real_docker" ]] || die \
    'Unable to resolve the real Docker client for the runtime image build wrapper.'
  wrapper_directory="$(mktemp -d "${STATE_DIRECTORY}/runtime-image-docker-wrapper.XXXXXX")" || die \
    'Unable to create private runtime image build wrapper directory.'
  if ! chmod 700 -- "$wrapper_directory"; then
    remove_private_directory "$wrapper_directory" "${STATE_DIRECTORY}/runtime-image-docker-wrapper."
    die 'Unable to protect private runtime image build wrapper directory.'
  fi
  create_runtime_image_build_docker_wrapper "$wrapper_directory"

  # Only the helper's docker build calls are rewritten. Its docker pull calls
  # remain direct and continue using the Docker daemon's existing proxy.
  export POC_REAL_DOCKER="$real_docker"
  export POC_RUNTIME_BUILD_PROXY_URL="$LOCAL_PROXY_URL"
  export PATH="${wrapper_directory}:${original_path}"
  if ! k8s_e2e_build_runtime_images; then
    build_status=1
  fi
  export PATH="$original_path"
  unset POC_REAL_DOCKER POC_RUNTIME_BUILD_PROXY_URL
  set_kubernetes_no_proxy
  remove_private_directory "$wrapper_directory" "${STATE_DIRECTORY}/runtime-image-docker-wrapper."
  ((build_status == 0)) || die 'Runtime image build/pull helper phase failed.'
}

load_runtime_images_into_poc_kind() {
  local nodes
  local image
  local expected_reference
  local first_component
  local references
  local reference
  local reference_found=0
  local -a runtime_images=("$SERVER_IMG" "$EXECD_IMG" "$EGRESS_IMG" "$SANDBOX_TEST_IMAGE")

  # Kind's docker-image loader currently imports with ctr --all-platforms,
  # which can fail with Docker 29 OCI manifest indexes (Kind #4224/#4066).
  # This POC owns exactly one verified node, so import each exact local image
  # directly without --all-platforms; never target another Kind cluster/node.
  nodes="$(kind get nodes --name "$KIND_CLUSTER")" || die \
    "Unable to resolve nodes for owned POC Kind cluster: ${KIND_CLUSTER}"
  [[ "$nodes" == "$KIND_NODE" ]] || die \
    "Expected exactly one owned POC Kind node ${KIND_NODE}; received: ${nodes}"

  for image in "${runtime_images[@]}"; do
    docker image inspect "$image" >/dev/null 2>&1 || die \
      "Required local runtime image is unavailable for POC Kind import: ${image}"
    if ! docker save "$image" | docker exec --privileged -i "$KIND_NODE" \
      ctr --namespace=k8s.io images import --digests --snapshotter=overlayfs -; then
      die "Unable to import exact runtime image into owned POC Kind node: ${image}"
    fi
    # ctr interprets a reference argument containing slashes as a filter. List
    # only this exact POC node's references without a filter and compare in the
    # runner. Docker Hub imports gain docker.io/ (and bare names gain library/).
    if [[ "$image" != */* ]]; then
      expected_reference="docker.io/library/${image}"
    else
      first_component="${image%%/*}"
      if [[ "$first_component" == *.* || "$first_component" == *:* || "$first_component" == 'localhost' ]]; then
        expected_reference="$image"
      else
        expected_reference="docker.io/${image}"
      fi
    fi
    references="$(docker exec "$KIND_NODE" ctr --namespace=k8s.io images ls --quiet)" || die \
      "Unable to verify imported runtime image on owned POC Kind node: ${image}"
    reference_found=0
    while IFS= read -r reference; do
      [[ "$reference" == "$expected_reference" ]] && reference_found=1
    done <<< "$references"
    ((reference_found)) || die \
      "Owned POC Kind node lacks expected imported runtime image reference: ${expected_reference}"
  done
}

apply_poc_pvc_and_seed() {
  local seed_pod_name="${POC_RESOURCE_PREFIX}-pvc-seed"

  # The shared helper hard-codes alpine:3.20, whose Docker 29 OCI archive
  # cannot be imported into this Kind POC. This runner-owned fixture keeps the
  # same PV/PVC and seed data semantics, but uses the already loaded Ubuntu
  # SANDBOX_TEST_IMAGE. It does not create any business sandbox workload.
  kubectl get namespace "$E2E_NAMESPACE" >/dev/null 2>&1 || \
    kubectl create namespace "$E2E_NAMESPACE"

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

  kubectl wait --for=jsonpath='{.status.phase}'=Bound --timeout=120s \
    "pvc/${PVC_NAME}" -n "$E2E_NAMESPACE"

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${seed_pod_name}
  namespace: ${E2E_NAMESPACE}
spec:
  restartPolicy: Never
  containers:
    - name: seed
      image: ${SANDBOX_TEST_IMAGE}
      imagePullPolicy: IfNotPresent
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

  kubectl wait --for=jsonpath='{.status.phase}'=Succeeded --timeout=120s \
    "pod/${seed_pod_name}" -n "$E2E_NAMESPACE"
  kubectl delete pod "$seed_pod_name" -n "$E2E_NAMESPACE" --ignore-not-found=true
}

ensure_make_tool_cache() {
  local tool_path=$1
  local tool_version=$2
  local tool_directory=$3
  local tool_module=$4
  local marker_path="${tool_path}-${tool_version}"
  local temporary_gobin
  local temporary_tool_path
  local tool_name
  local version_ready=0
  local invalid_poc_marker=0
  local resolved_tool_path

  # kubernetes/Makefile's go-install-tool expects an executable versioned file
  # and then runs ln -sf $(target)-$(version) $(target). Keep this exact layout
  # project-local and never follow symlinks for the versioned executable.
  [[ "$marker_path" == "${tool_directory}/"* ]] || die \
    "Refusing Make tool version path outside the Kubernetes tool directory: ${marker_path}"
  if [[ -e "$marker_path" || -L "$marker_path" ]]; then
    [[ ! -L "$marker_path" ]] || die \
      "Kubernetes Make tool version path must not be a symlink: ${marker_path}"
    if [[ -f "$marker_path" && -x "$marker_path" ]]; then
      version_ready=1
    elif [[ -f "$marker_path" && ! -s "$marker_path" ]]; then
      # Only the prior POC implementation created empty exact version paths.
      invalid_poc_marker=1
    else
      die "Kubernetes Make tool version path is not an executable regular file: ${marker_path}"
    fi
  fi

  if ((version_ready)); then
    [[ -L "$tool_path" ]] || die \
      "Kubernetes Make active tool must be a symlink to its versioned executable: ${tool_path}"
    resolved_tool_path="$(readlink -f -- "$tool_path")" || die \
      "Unable to resolve Kubernetes Make active tool symlink: ${tool_path}"
    [[ "$resolved_tool_path" == "$marker_path" ]] || die \
      "Kubernetes Make active tool resolves to an unexpected path: ${tool_path}"
    return
  fi

  if ((invalid_poc_marker)); then
    # Repair only the exact empty marker created by this POC. Its active path
    # must either point to it (controller-gen) or be an executable regular file
    # beside it (kustomize); any other layout is refused as potentially foreign.
    if [[ -L "$tool_path" ]]; then
      resolved_tool_path="$(readlink -f -- "$tool_path")" || die \
        "Unable to resolve invalid Kubernetes Make active tool symlink: ${tool_path}"
      [[ "$resolved_tool_path" == "$marker_path" ]] || die \
        "Refusing to replace active tool with an unexpected symlink target: ${tool_path}"
    else
      [[ -f "$tool_path" && ! -L "$tool_path" && -x "$tool_path" ]] || die \
        "Refusing to replace unexpected invalid Kubernetes Make active tool: ${tool_path}"
    fi
    rm -f -- "$tool_path" "$marker_path"
  elif [[ -e "$tool_path" || -L "$tool_path" ]]; then
    die "Kubernetes Make active tool exists without a valid versioned executable: ${tool_path}"
  fi

  tool_name="${tool_path##*/}"
  temporary_gobin="$(mktemp -d "${STATE_DIRECTORY}/go-tool-cache.XXXXXX")" || die \
    "Unable to create private Go tool cache directory for ${tool_name}."
  if ! chmod 700 -- "$temporary_gobin"; then
    remove_private_directory "$temporary_gobin" "${STATE_DIRECTORY}/go-tool-cache."
    die "Unable to protect private Go tool cache directory for ${tool_name}."
  fi
  set_build_tools_proxy
  if ! GOBIN="$temporary_gobin" go install "${tool_module}@${tool_version}"; then
    set_kubernetes_no_proxy
    remove_private_directory "$temporary_gobin" "${STATE_DIRECTORY}/go-tool-cache."
    die "Unable to cache required ${tool_name} ${tool_version} before Kind creation."
  fi
  set_kubernetes_no_proxy
  temporary_tool_path="${temporary_gobin}/${tool_name}"
  [[ -f "$temporary_tool_path" && ! -L "$temporary_tool_path" && -x "$temporary_tool_path" ]] || {
    remove_private_directory "$temporary_gobin" "${STATE_DIRECTORY}/go-tool-cache."
    die "Downloaded ${tool_name} is not an executable regular file."
  }
  # A hard link publishes without replacement atomically. Both paths are under
  # this worktree, so the private GOBIN and target remain on the same filesystem.
  if ! ln -- "$temporary_tool_path" "$marker_path"; then
    remove_private_directory "$temporary_gobin" "${STATE_DIRECTORY}/go-tool-cache."
    die "Refusing to overwrite Kubernetes Make tool version path: ${marker_path}"
  fi
  rm -f -- "$temporary_tool_path"
  remove_private_directory "$temporary_gobin" "${STATE_DIRECTORY}/go-tool-cache."
  [[ -f "$marker_path" && ! -L "$marker_path" && -x "$marker_path" ]] || die \
    "Kubernetes Make tool version executable was not safely published: ${marker_path}"

  # ln creates the active symlink without replacing a concurrently created or
  # pre-existing tool path.
  ln -s -- "$marker_path" "$tool_path" || die \
    "Refusing to overwrite Kubernetes Make active tool: ${tool_path}"
  [[ -L "$tool_path" && "$(readlink -f -- "$tool_path")" == "$marker_path" ]] || die \
    "Kubernetes Make active tool link was not safely published: ${tool_path}"
}

preflight_docker_resolver() {
  local preflight_directory
  local preflight_id
  local preflight_image
  local build_status=0

  # Exercise the Docker daemon's normal BuildKit resolver path before Kind is
  # created. Its globally configured proxy is external to this runner.
  preflight_directory="$(mktemp -d "${STATE_DIRECTORY}/buildx-proxy-preflight.XXXXXX")" || die \
    'Unable to create private Docker resolver preflight directory.'
  if ! chmod 700 -- "$preflight_directory"; then
    remove_private_directory "$preflight_directory" "${STATE_DIRECTORY}/buildx-proxy-preflight."
    die 'Unable to protect private Docker resolver preflight directory.'
  fi
  preflight_id="$(openssl rand -hex 16)" || {
    remove_private_directory "$preflight_directory" "${STATE_DIRECTORY}/buildx-proxy-preflight."
    die 'Unable to generate a unique Docker resolver preflight image tag.'
  }
  preflight_image="opensandbox/poc-docker-resolver-preflight:${preflight_id}"
  if docker image inspect "$preflight_image" >/dev/null 2>&1; then
    remove_private_directory "$preflight_directory" "${STATE_DIRECTORY}/buildx-proxy-preflight."
    die 'Generated Docker resolver preflight image tag already exists; refusing to replace it.'
  fi
  if ! printf '%s\n' 'FROM golang:1.25.12-alpine' >"${preflight_directory}/Dockerfile"; then
    remove_private_directory "$preflight_directory" "${STATE_DIRECTORY}/buildx-proxy-preflight."
    die 'Unable to write private Docker resolver preflight Dockerfile.'
  fi

  if ! docker build --pull=false --tag "$preflight_image" "$preflight_directory"; then
    build_status=1
  fi
  if docker image inspect "$preflight_image" >/dev/null 2>&1; then
    docker image rm --force "$preflight_image" >/dev/null || die \
      "Unable to remove only the generated Docker resolver preflight image: ${preflight_image}"
  fi
  remove_private_directory "$preflight_directory" "${STATE_DIRECTORY}/buildx-proxy-preflight."
  ((build_status == 0)) || die \
    'Docker resolver preflight failed for golang:1.25.12-alpine; refusing to create a Kind cluster.'
}

cache_controller_gen() {
  local kubernetes_directory="${REPO_ROOT}/kubernetes"
  local kubernetes_bin_directory="${kubernetes_directory}/bin"
  local controller_gen_path="${kubernetes_bin_directory}/controller-gen"
  local make_metadata
  local make_controller_gen_path
  local controller_tools_version

  # Ask the Kubernetes Makefile for its expanded target and version rather
  # than duplicating either value in this runner.
  make_metadata="$(make -s --no-print-directory -C "$kubernetes_directory" \
    --eval 'print-poc-controller-gen: ; @printf "%s|%s\\n" "$(CONTROLLER_GEN)" "$(CONTROLLER_TOOLS_VERSION)"' \
    print-poc-controller-gen)" || die \
    'Unable to read controller-gen target and version from kubernetes/Makefile.'
  IFS='|' read -r make_controller_gen_path controller_tools_version <<< "$make_metadata"
  [[ "$make_controller_gen_path" == "$controller_gen_path" && "$controller_tools_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die \
    'Kubernetes Makefile controller-gen target or version is not safe for this POC cache.'
  [[ -d "$kubernetes_directory" && ! -L "$kubernetes_directory" ]] || die \
    "Kubernetes directory must be a non-symlink directory: ${kubernetes_directory}"
  if [[ -e "$kubernetes_bin_directory" || -L "$kubernetes_bin_directory" ]]; then
    [[ -d "$kubernetes_bin_directory" && ! -L "$kubernetes_bin_directory" ]] || die \
      "Kubernetes tool directory must be a non-symlink directory: ${kubernetes_bin_directory}"
  else
    mkdir -p -- "$kubernetes_bin_directory"
    chmod 700 -- "$kubernetes_bin_directory"
  fi
  ensure_make_tool_cache "$controller_gen_path" "$controller_tools_version" \
    "$kubernetes_bin_directory" 'sigs.k8s.io/controller-tools/cmd/controller-gen'
}

cache_kustomize() {
  local kubernetes_directory="${REPO_ROOT}/kubernetes"
  local kubernetes_bin_directory="${kubernetes_directory}/bin"
  local kustomize_path="${kubernetes_bin_directory}/kustomize"
  local make_metadata
  local make_kustomize_path
  local kustomize_version

  # Ask the Kubernetes Makefile for its expanded target and version rather
  # than duplicating either value in this runner.
  make_metadata="$(make -s --no-print-directory -C "$kubernetes_directory" \
    --eval 'print-poc-kustomize: ; @printf "%s|%s\\n" "$(KUSTOMIZE)" "$(KUSTOMIZE_VERSION)"' \
    print-poc-kustomize)" || die \
    'Unable to read kustomize target and version from kubernetes/Makefile.'
  IFS='|' read -r make_kustomize_path kustomize_version <<< "$make_metadata"
  [[ "$make_kustomize_path" == "$kustomize_path" && "$kustomize_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die \
    'Kubernetes Makefile kustomize target or version is not safe for this POC cache.'
  [[ -d "$kubernetes_directory" && ! -L "$kubernetes_directory" ]] || die \
    "Kubernetes directory must be a non-symlink directory: ${kubernetes_directory}"
  if [[ -e "$kubernetes_bin_directory" || -L "$kubernetes_bin_directory" ]]; then
    [[ -d "$kubernetes_bin_directory" && ! -L "$kubernetes_bin_directory" ]] || die \
      "Kubernetes tool directory must be a non-symlink directory: ${kubernetes_bin_directory}"
  else
    mkdir -p -- "$kubernetes_bin_directory"
    chmod 700 -- "$kubernetes_bin_directory"
  fi
  ensure_make_tool_cache "$kustomize_path" "$kustomize_version" \
    "$kubernetes_bin_directory" 'sigs.k8s.io/kustomize/kustomize/v5'
}

cache_kubernetes_go_modules() {
  local kubernetes_directory="${REPO_ROOT}/kubernetes"
  local go_mod_path="${kubernetes_directory}/go.mod"
  local go_sum_path="${kubernetes_directory}/go.sum"

  # controller-gen can load packages whose module dependencies are not needed
  # by its own installation. Cache only the existing module graph; do not run
  # Make targets or generate source before the isolated Kind phase.
  for path in "$go_mod_path" "$go_sum_path"; do
    [[ -f "$path" && ! -L "$path" && -r "$path" ]] || die \
      "Kubernetes Go module file must be a readable non-symlink regular file: ${path}"
  done
  set_build_tools_proxy
  if ! (cd "$kubernetes_directory" && go mod download); then
    set_kubernetes_no_proxy
    die 'Unable to cache Kubernetes Go module dependencies before Kind creation.'
  fi
  set_kubernetes_no_proxy
}

validate_fixed_paths() {
  [[ -d "$POC_DIRECTORY" && ! -L "$POC_DIRECTORY" ]] || die \
    "POC directory must be a non-symlink directory: ${POC_DIRECTORY}"
  if [[ -e "$STATE_DIRECTORY" || -L "$STATE_DIRECTORY" ]]; then
    [[ -d "$STATE_DIRECTORY" && ! -L "$STATE_DIRECTORY" ]] || die \
      "POC state directory must be a non-symlink directory: ${STATE_DIRECTORY}"
  fi
  for path in "$KUBECONFIG_PATH" "$SERVER_VALUES_FILE" "$PORT_FORWARD_LOG"; do
    [[ ! -L "$path" ]] || die "Refusing symlinked POC state file: ${path}"
  done
}

print_plan() {
  note 'OpenSandbox 本地 Kind ensureSubPathDirectory POC（默认 dry-run）'
  note "唯一受管 Kind 集群: ${KIND_CLUSTER}"
  note "固定本地生命周期端口: 127.0.0.1:${LIFECYCLE_LOCAL_PORT}（不使用 18080/18090）"
  note "POC 状态目录: ${STATE_DIRECTORY}"
  note "固定 pytest 节点: ${TEST_NODE}"
  note '确认执行后将按以下既有公共 helper 流程运行：'
  note '  1. scripts/common/kubernetes-e2e.sh:k8s_e2e_setup_kind_and_controller'
  note '  2. scripts/common/kubernetes-e2e.sh:k8s_e2e_build_runtime_images'
  note '  3. scripts/common/kubernetes-e2e.sh:k8s_e2e_kind_load_runtime_images'
  note '  4. scripts/common/kubernetes-e2e.sh:k8s_e2e_apply_pvc_and_seed'
  note '  5. scripts/common/kubernetes-e2e.sh:k8s_e2e_write_server_helm_values / k8s_e2e_helm_install_server'
  note "  6. uv run pytest -q ${TEST_NODE}"
  note '创建 Kind 前会先通过本机 127.0.0.1:7897 代理缓存 Makefile 指定的 controller-gen/kustomize，并写入可执行版本文件及其兼容的活动符号链接，再清除进程代理并设置 no_proxy=*。'
  note '同一预备阶段会执行 kubernetes/go.mod 的 go mod download，仅填充 Go module cache，不执行生成或 Make 目标。'
  note '随后通过 Docker daemon 的默认 BuildKit 以临时 golang:1.25.12-alpine Dockerfile 执行 docker build --pull=false 验证镜像解析；失败则不创建集群。'
  note '仅运行时镜像 helper 期间，临时包装 docker build 为保持 Docker 本地镜像格式的 docker build --network=host，并传入本机 127.0.0.1:7897 代理 build args；docker pull 保持直连 Docker daemon。'
  note '运行时镜像（包括 Ubuntu seed fixture 镜像）只会导入唯一受管节点 opensandbox-subpath-poc-kind-control-plane；为规避 Docker 29 OCI manifest index 的 Kind --all-platforms 导入兼容性问题，使用 docker save | ctr images import（不带 --all-platforms）。'
  note 'PVC fixture 使用 runner 专属的 Ubuntu seed Pod 写入既有 datasets/train 与 marker 数据，不调用硬编码 alpine:3.20 的公共 helper。'
  note 'Kind、Kubernetes、port-forward 和 pytest 阶段均设置 no_proxy=*；不会修改 Docker daemon 的外部代理配置。'
  note '测试通过 OpenSandbox 生命周期 API 创建 sandbox；本脚本不直接创建 BatchSandbox 或业务 Pod。'
  note '公共 PVC helper 会写入其既有基线标记；目标用例使用随机、此前未种子化的 subPath。'
  note '退出时仅删除本次创建的固定 Kind 集群。'
}

validate_dependencies() {
  local command
  for command in kind docker kubectl helm make go uv curl openssl python3 ss; do
    require_command "$command"
  done
}

assert_cluster_absent() {
  local cluster
  while IFS= read -r cluster; do
    [[ "$cluster" != "$KIND_CLUSTER" ]] || die \
      "Refusing to use existing Kind cluster ${KIND_CLUSTER}; it may not be owned by this POC run."
  done < <(kind get clusters)
}

assert_lifecycle_port_free() {
  local listeners
  listeners="$(ss -H -ltn "sport = :${LIFECYCLE_LOCAL_PORT}")" || die \
    "Unable to inspect fixed lifecycle port ${LIFECYCLE_LOCAL_PORT}."
  [[ -z "$listeners" ]] || die \
    "Fixed lifecycle port 127.0.0.1:${LIFECYCLE_LOCAL_PORT} is already in use; refusing to touch its listener."
}

prepare_state_directory() {
  validate_fixed_paths
  mkdir -p -- "$STATE_DIRECTORY"
  chmod 700 -- "$STATE_DIRECTORY"
  validate_fixed_paths
}

cleanup_poc_resources() {
  local status=$?
  local cluster

  trap - EXIT
  if ((KIND_CLUSTER_CREATION_STARTED)); then
    while IFS= read -r cluster; do
      if [[ "$cluster" == "$KIND_CLUSTER" ]]; then
        # The name is constant and was absent before this run began.
        kind delete cluster --name "$KIND_CLUSTER" || \
          printf 'WARN: unable to delete only owned Kind cluster %s\n' "$KIND_CLUSTER" >&2
        break
      fi
    done < <(kind get clusters 2>/dev/null || true)
  fi
  exit "$status"
}

run_confirmed_poc() {
  validate_dependencies
  assert_cluster_absent
  assert_lifecycle_port_free
  prepare_state_directory

  # 禁止代理影响 Kind、Kubernetes API、kubectl port-forward 或本地生命周期 API。
  set_kubernetes_no_proxy
  export DOCKER_BUILDKIT=1

  export KIND_CLUSTER
  export KIND_K8S_VERSION="${KIND_K8S_VERSION:-v1.30.4}"
  export KUBECONFIG_PATH
  export E2E_NAMESPACE="${POC_RESOURCE_PREFIX}-e2e"
  export SERVER_NAMESPACE="${POC_RESOURCE_PREFIX}-system"
  export PV_NAME="${POC_RESOURCE_PREFIX}-pv"
  export PVC_NAME="${POC_RESOURCE_PREFIX}-pvc"
  export CONTROLLER_IMG="opensandbox/controller:subpath-poc-kind"
  export SERVER_IMG="opensandbox/server:subpath-poc-kind"
  export EXECD_IMG="opensandbox/execd:subpath-poc-kind"
  export EGRESS_IMG="opensandbox/egress:subpath-poc-kind"
  export SERVER_RELEASE="${POC_RESOURCE_PREFIX}-server"
  export SERVER_VALUES_FILE
  export PORT_FORWARD_LOG
  export SANDBOX_TEST_IMAGE="${SANDBOX_TEST_IMAGE:-ubuntu:latest}"
  export LIFECYCLE_LOCAL_PORT
  export SERVER_IMG_REPOSITORY="${SERVER_IMG%:*}"
  export SERVER_IMG_TAG="${SERVER_IMG##*:}"

  # shellcheck source=common/kubernetes-e2e.sh
  source "${SCRIPT_DIRECTORY}/common/kubernetes-e2e.sh"

  trap cleanup_poc_resources EXIT
  # Fetch the Makefile-required local tool in a tightly scoped pre-Kind proxy
  # phase. It restores no_proxy=* before any Kind or Kubernetes command.
  cache_controller_gen
  cache_kustomize
  cache_kubernetes_go_modules
  # The normal Docker BuildKit resolver must succeed before Kind can create
  # anything. The daemon proxy is external and is not changed by this runner.
  preflight_docker_resolver

  k8s_e2e_export_kubeconfig
  KIND_CLUSTER_CREATION_STARTED=1
  k8s_e2e_setup_kind_and_controller
  run_runtime_image_builds_with_local_proxy
  load_runtime_images_into_poc_kind
  apply_poc_pvc_and_seed
  k8s_e2e_write_server_helm_values
  # The shared helper does not set the chart namespace override; append this
  # runner-owned value so all server chart resources stay in the isolated POC namespace.
  printf '\nnamespaceOverride: "%s"\n' "$SERVER_NAMESPACE" >> "$SERVER_VALUES_FILE"
  k8s_e2e_helm_install_server

  kubectl port-forward -n "$SERVER_NAMESPACE" svc/opensandbox-server \
    "${LIFECYCLE_LOCAL_PORT}:80" >"$PORT_FORWARD_LOG" 2>&1 &
  k8s_e2e_wait_http_ok "http://127.0.0.1:${LIFECYCLE_LOCAL_PORT}/health"

  export OPENSANDBOX_TEST_DOMAIN="localhost:${LIFECYCLE_LOCAL_PORT}"
  export OPENSANDBOX_TEST_PROTOCOL='http'
  export OPENSANDBOX_TEST_API_KEY='kubernetes-e2e'
  export OPENSANDBOX_SANDBOX_DEFAULT_IMAGE="$SANDBOX_TEST_IMAGE"
  export OPENSANDBOX_E2E_RUNTIME='kubernetes'
  export OPENSANDBOX_TEST_USE_SERVER_PROXY='true'
  export OPENSANDBOX_TEST_PVC_NAME="$PVC_NAME"
  k8s_e2e_export_sandbox_resource_env

  cd "${REPO_ROOT}/tests/python"
  uv sync --all-extras --refresh
  uv run pytest -q \
    'tests/test_sandbox_e2e_sync.py::TestSandboxE2ESync::test_01g_pvc_unseeded_subpath_initializer'
}

[[ "$#" -eq 0 ]] || die 'This POC runner accepts no arguments; cluster, paths, port, and pytest node are fixed.'
validate_fixed_paths
print_plan

if [[ "${LOCAL_KIND_POC_CONFIRM:-}" != '1' ]]; then
  note 'Dry run only: no docker, kind, kubectl, Helm, build, port-forward, or pytest command was executed.'
  note 'To run only this isolated POC, set LOCAL_KIND_POC_CONFIRM=1.'
  exit 0
fi

run_confirmed_poc
