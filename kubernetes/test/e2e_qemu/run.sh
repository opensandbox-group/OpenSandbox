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

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
project_root=$(cd "$script_dir/../.." && pwd)
cluster_name=${QEMU_E2E_KIND_CLUSTER:-opensandbox-qemu-vmstate-e2e}
kind_node_image=${QEMU_E2E_KIND_NODE_IMAGE:-kindest/node:v1.31.0}
context="kind-$cluster_name"
node_name="$cluster_name-control-plane"
namespace=qemu-vmstate-e2e
pool_name=qemu-pool
sandbox_name=qemu
qemu_container=rootfs
restored_pod="$sandbox_name-0"
controller_image=${QEMU_E2E_CONTROLLER_IMAGE:-controller:qemu-vmstate-e2e}
controller_dockerfile=${QEMU_E2E_CONTROLLER_DOCKERFILE:-Dockerfile}
controller_builder_target=${QEMU_E2E_CONTROLLER_BUILDER_TARGET:-}
committer_image=${QEMU_E2E_COMMITTER_IMAGE:-image-committer:qemu-vmstate-e2e}
qemu_image=${QEMU_E2E_QEMU_IMAGE:-opensandbox/qemu-vmstate-e2e:dev}
go_proxy=${QEMU_E2E_GOPROXY:-https://proxy.golang.org,direct}
keep_cluster=${KEEP_QEMU_E2E_CLUSTER:-false}
external_registry=${QEMU_E2E_SNAPSHOT_REGISTRY:-}
registry_docker_config=${QEMU_E2E_DOCKER_CONFIG:-}
registry_secret=${QEMU_E2E_REGISTRY_SECRET:-qemu-vmstate-e2e-registry}
registry_insecure=${QEMU_E2E_SNAPSHOT_REGISTRY_INSECURE:-false}
previous_context=$(kubectl config current-context 2>/dev/null || true)
source_pod=
replacement_pool_pod=

diagnostics() {
  kubectl --context "$context" get pools,pods,jobs,batchsandboxes,sandboxsnapshots -A -o wide >&2 || true
  kubectl --context "$context" -n opensandbox-system logs \
    deployment/opensandbox-controller-manager --tail=200 >&2 || true
  kubectl --context "$context" -n "$namespace" logs \
    -l sandbox.opensandbox.io/sandbox-snapshot-name --all-containers --tail=200 >&2 || true
  if [[ -n "$source_pod" ]]; then
    kubectl --context "$context" -n "$namespace" logs \
      "$source_pod" -c "$qemu_container" --tail=200 >&2 || true
  fi
  kubectl --context "$context" -n "$namespace" logs \
    "$restored_pod" -c "$qemu_container" --tail=200 >&2 || true
}

finish() {
  rc=$?
  if ((rc != 0)); then
    echo "QEMU_VMSTATE_E2E_DIAGNOSTICS begin" >&2
    diagnostics
    echo "QEMU_VMSTATE_E2E_DIAGNOSTICS end" >&2
  fi
  if [[ "$keep_cluster" != "true" ]]; then
    kind delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
  fi
  if [[ -n "$previous_context" ]]; then
    kubectl config use-context "$previous_context" >/dev/null 2>&1 || true
  fi
  exit "$rc"
}
trap finish EXIT

for command in docker kind kubectl sed; do
  command -v "$command" >/dev/null || {
    echo "required command is missing: $command" >&2
    exit 1
  }
done
if [[ ! -c /dev/kvm ]]; then
  echo "/dev/kvm is required for QEMU VMState E2E" >&2
  exit 1
fi
if kind get clusters 2>/dev/null | grep -Fxq "$cluster_name"; then
  echo "Kind cluster already exists: $cluster_name" >&2
  exit 1
fi
if [[ "$registry_insecure" != "true" && "$registry_insecure" != "false" ]]; then
  echo "QEMU_E2E_SNAPSHOT_REGISTRY_INSECURE must be true or false" >&2
  exit 1
fi

registry=
snapshot_push_secret=
resume_pull_secret=
if [[ -n "$external_registry" ]]; then
  registry=${external_registry%/}
  if [[ ! "$registry" =~ ^[a-zA-Z0-9._:-]+(/[a-zA-Z0-9._-]+)*$ ]]; then
    echo "QEMU_E2E_SNAPSHOT_REGISTRY must be a registry/repository prefix without a URL scheme" >&2
    exit 1
  fi
  if [[ -z "$registry_docker_config" || ! -r "$registry_docker_config" ]]; then
    echo "QEMU_E2E_DOCKER_CONFIG must name a readable Docker config.json for an authenticated registry" >&2
    exit 1
  fi
  snapshot_push_secret=$registry_secret
  resume_pull_secret=$registry_secret
fi

cd "$project_root"

if [[ -n "$controller_builder_target" ]]; then
  controller_builder_image="$controller_image-builder"
  docker build \
    --target "$controller_builder_target" \
    --build-arg PACKAGE=./cmd/controller \
    --build-arg GOPROXY="$go_proxy" \
    -t "$controller_builder_image" \
    -f "$controller_dockerfile" .
  docker build \
    --build-arg CONTROLLER_BUILDER_IMAGE="$controller_builder_image" \
    -t "$controller_image" \
    -f test/e2e_qemu/testdata/controller-runtime.Dockerfile .
else
  docker build \
    --build-arg PACKAGE=./cmd/controller \
    --build-arg GOPROXY="$go_proxy" \
    -t "$controller_image" \
    -f "$controller_dockerfile" .
fi
docker build -t "$committer_image" -f Dockerfile.image-committer .
docker build -t "$qemu_image" test/e2e_qemu/testdata

images=("$controller_image" "$committer_image" "$qemu_image")
if [[ -z "$external_registry" ]]; then
  docker pull --platform linux/amd64 registry:2
  images+=(registry:2)
fi

kind create cluster \
  --name "$cluster_name" \
  --image "$kind_node_image" \
  --config test/e2e_qemu/testdata/kind.yaml \
  --wait 120s

kind load docker-image --name "$cluster_name" "${images[@]}"

kubectl --context "$context" apply -f test/e2e_qemu/testdata/namespace.yaml
if [[ -n "$external_registry" ]]; then
  kubectl --context "$context" -n "$namespace" create secret generic "$registry_secret" \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson="$registry_docker_config"
else
  kubectl --context "$context" apply -f test/e2e_qemu/testdata/registry.yaml
  kubectl --context "$context" -n "$namespace" wait \
    --for=condition=Ready pod/registry --timeout=120s

  node_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "kind").IPAddress}}' "$node_name")
  if [[ ! "$node_ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "could not resolve Kind node IP: $node_ip" >&2
    exit 1
  fi
  registry="$node_ip:5000"
  registry_insecure=true

  docker exec "$node_name" sed -i \
    "/^\[proxy_plugins\]/i\\        [plugins.\"io.containerd.grpc.v1.cri\".registry.mirrors.\"$registry\"]\n          endpoint = [\"http://$registry\"]\n" \
    /etc/containerd/config.toml
  docker exec "$node_name" systemctl restart containerd
  kubectl --context "$context" wait --for=condition=Ready node/"$node_name" --timeout=120s
fi

kubectl --context "$context" apply -k config/default
kubectl --context "$context" -n opensandbox-system set image \
  deployment/opensandbox-controller-manager manager="$controller_image"
controller_patch=$(printf '%s' \
  "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"manager\",\"args\":[\"--leader-elect\",\"--health-probe-bind-address=:8081\",\"--snapshot-registry=$registry\",\"--snapshot-registry-insecure=$registry_insecure\",\"--snapshot-push-secret=$snapshot_push_secret\",\"--resume-pull-secret=$resume_pull_secret\",\"--image-committer-image=$committer_image\",\"--commit-job-timeout=15m\",\"--image-committer-pull-secret=\"]}]}}}}")
kubectl --context "$context" -n opensandbox-system patch \
  deployment opensandbox-controller-manager --type=strategic -p="$controller_patch"
kubectl --context "$context" -n opensandbox-system rollout status \
  deployment/opensandbox-controller-manager --timeout=120s

wait_for_phase() {
  local wanted=$1
  local attempts=$2
  local phase
  for _ in $(seq 1 "$attempts"); do
    phase=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$phase" == "$wanted" ]]; then
      return 0
    fi
    sleep 1
  done
  echo "BatchSandbox did not reach phase $wanted; last phase=$phase" >&2
  return 1
}

wait_for_pool_available() {
  local attempts=$1
  local available
  for _ in $(seq 1 "$attempts"); do
    available=$(kubectl --context "$context" -n "$namespace" get pool "$pool_name" \
      -o jsonpath='{.status.available}' 2>/dev/null || true)
    if [[ "${available:-0}" -ge 1 ]]; then
      return 0
    fi
    sleep 1
  done
  echo "Pool did not provide an available Pod; last available=${available:-}" >&2
  return 1
}

wait_for_replacement_pool_pod() {
  local source=$1
  local attempts=$2
  local candidate
  local ready
  for _ in $(seq 1 "$attempts"); do
    while IFS= read -r candidate; do
      if [[ -z "$candidate" || "$candidate" == "$source" ]]; then
        continue
      fi
      ready=$(kubectl --context "$context" -n "$namespace" get pod "$candidate" \
        -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
      if [[ "$ready" == "True" ]]; then
        printf '%s\n' "$candidate"
        return 0
      fi
    done < <(kubectl --context "$context" -n "$namespace" get pod \
      -l sandbox.opensandbox.io/pool-name="$pool_name" \
      -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
    sleep 1
  done
  echo "Pool did not replenish a Ready Pod after allocating $source" >&2
  return 1
}

wait_for_sandbox_ready() {
  local attempts=$1
  local ready
  for _ in $(seq 1 "$attempts"); do
    ready=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
      -o jsonpath='{.status.ready}' 2>/dev/null || true)
    if [[ "${ready:-0}" -ge 1 ]]; then
      return 0
    fi
    sleep 1
  done
  echo "BatchSandbox did not become ready; last ready=${ready:-}" >&2
  return 1
}

kubectl --context "$context" apply -f test/e2e_qemu/testdata/pool.yaml
wait_for_pool_available 180
initial_pool_pod=$(kubectl --context "$context" -n "$namespace" get pod \
  -l sandbox.opensandbox.io/pool-name="$pool_name" \
  -o jsonpath='{.items[0].metadata.name}')
[[ -n "$initial_pool_pod" ]]

kubectl --context "$context" apply -f test/e2e_qemu/testdata/batchsandbox.yaml
wait_for_sandbox_ready 180
allocation=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
  -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/alloc-status}')
source_pod=$(sed -n 's/.*"pods":\["\([^"]*\)".*/\1/p' <<<"$allocation")
if [[ -z "$source_pod" ]]; then
  echo "BatchSandbox allocation does not contain a source Pod: $allocation" >&2
  exit 1
fi
[[ "$source_pod" == "$initial_pool_pod" ]]
kubectl --context "$context" -n "$namespace" wait \
  --for=condition=Ready pod/"$source_pod" --timeout=180s

# Allocation consumes the first warm Pod. The Pool must replenish its buffer
# independently of the Pod that will later be restored as standalone.
replacement_pool_pod=$(wait_for_replacement_pool_pod "$source_pod" 180)

memory_token="QEMU-VMSTATE-MMAP-$(date -u +%Y%m%dT%H%M%SZ)"
disk_token="QEMU-VMSTATE-DISK-$(date -u +%Y%m%dT%H%M%SZ)"
rootfs_token="QEMU-VMSTATE-ROOTFS-$(date -u +%Y%m%dT%H%M%SZ)"
kubectl --context "$context" -n "$namespace" exec "$source_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error --request PUT --data-binary "$memory_token" \
  http://127.0.0.1:18080/value >/dev/null
kubectl --context "$context" -n "$namespace" exec "$source_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error --request PUT --data-binary "$disk_token" \
  http://127.0.0.1:18080/disk >/dev/null
kubectl --context "$context" -n "$namespace" exec "$source_pod" -c "$qemu_container" -- \
  sh -c 'mkdir -p /var/lib/opensandbox && printf "%s" "$1" > /var/lib/opensandbox/rootfs-marker' \
  sh "$rootfs_token"

before=$(kubectl --context "$context" -n "$namespace" exec "$source_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/status)
before_disk=$(kubectl --context "$context" -n "$namespace" exec "$source_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/disk)
before_boot_id=$(sed -n 's/.*"boot_id":"\([^"]*\)".*/\1/p' <<<"$before")
before_counter=$(sed -n 's/.*"counter":\([0-9]*\).*/\1/p' <<<"$before")
source_uid=$(kubectl --context "$context" -n "$namespace" get pod "$source_pod" \
  -o jsonpath='{.metadata.uid}')

kubectl --context "$context" -n "$namespace" patch batchsandbox "$sandbox_name" \
  --type=merge -p='{ "spec": { "pause": true } }'
wait_for_phase Paused 600

snapshot_name="$sandbox_name-pause"
snapshot_format=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.format}')
rootfs_image_uri=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.containers[0].imageUri}')
rootfs_digest=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.containers[0].imageDigest}')
vmstate_image_uri=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.virtualMachine.imageUri}')
vmstate_digest=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.virtualMachine.imageDigest}')
vmstate_size=$(kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" \
  -o jsonpath='{.status.virtualMachine.sizeBytes}')
rootfs_repository=${rootfs_image_uri%:*}
vmstate_repository=${vmstate_image_uri%:*}
paused_pool_ref=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
  -o jsonpath='{.spec.poolRef}')
materialized_containers=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
  -o jsonpath='{.spec.template.spec.containers[*].name}')

[[ "$snapshot_format" == "qemu-v1" ]]
[[ "$rootfs_repository" == "$registry/$sandbox_name-$qemu_container" ]]
[[ "$vmstate_repository" == "$registry/$sandbox_name-vmstate" ]]
[[ "${rootfs_image_uri##*:}" == "${vmstate_image_uri##*:}" ]]
[[ "$rootfs_digest" =~ ^sha256:[0-9a-f]{64}$ ]]
[[ "$vmstate_digest" =~ ^sha256:[0-9a-f]{64}$ ]]
((vmstate_size > 0))
[[ -z "$paused_pool_ref" ]]
[[ " $materialized_containers " == *" $qemu_container "* ]]
kubectl --context "$context" -n "$namespace" wait \
  --for=delete pod/"$source_pod" --timeout=120s

kubectl --context "$context" -n "$namespace" patch batchsandbox "$sandbox_name" \
  --type=merge -p='{ "spec": { "pause": false } }'
wait_for_phase Succeed 600
kubectl --context "$context" -n "$namespace" wait \
  --for=condition=Ready pod/"$restored_pod" --timeout=300s

restored_uid=$(kubectl --context "$context" -n "$namespace" get pod "$restored_pod" \
  -o jsonpath='{.metadata.uid}')
restored_rootfs_image=$(kubectl --context "$context" -n "$namespace" get pod "$restored_pod" \
  -o jsonpath='{.spec.containers[?(@.name=="rootfs")].image}')
restored_vmstate_image=$(kubectl --context "$context" -n "$namespace" get pod "$restored_pod" \
  -o jsonpath='{.spec.initContainers[?(@.name=="opensandbox-vmstate-restore")].image}')
init_reason=$(kubectl --context "$context" -n "$namespace" get pod "$restored_pod" \
  -o jsonpath='{.status.initContainerStatuses[?(@.name=="opensandbox-vmstate-restore")].state.terminated.reason}')
restored_rootfs_token=$(kubectl --context "$context" -n "$namespace" exec "$restored_pod" -c "$qemu_container" -- \
  cat /var/lib/opensandbox/rootfs-marker)
after=$(kubectl --context "$context" -n "$namespace" exec "$restored_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/status)
after_disk=$(kubectl --context "$context" -n "$namespace" exec "$restored_pod" -c "$qemu_container" -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/disk)
after_boot_id=$(sed -n 's/.*"boot_id":"\([^"]*\)".*/\1/p' <<<"$after")
after_counter=$(sed -n 's/.*"counter":\([0-9]*\).*/\1/p' <<<"$after")
final_pool_ref=$(kubectl --context "$context" -n "$namespace" get batchsandbox "$sandbox_name" \
  -o jsonpath='{.spec.poolRef}')

[[ "$source_uid" != "$restored_uid" ]]
[[ "$restored_rootfs_image" == "$rootfs_repository@$rootfs_digest" ]]
[[ "$restored_vmstate_image" == "$vmstate_repository@$vmstate_digest" ]]
[[ "$init_reason" == "Completed" ]]
[[ "$restored_rootfs_token" == "$rootfs_token" ]]
[[ "$after" == *"\"value\":\"$memory_token\""* ]]
[[ "$before_disk" == *"\"value\":\"$disk_token\""* ]]
[[ "$after_disk" == *"\"value\":\"$disk_token\""* ]]
[[ -n "$before_boot_id" && "$before_boot_id" == "$after_boot_id" ]]
((after_counter > before_counter))
[[ -z "$final_pool_ref" ]]
wait_for_pool_available 120

if kubectl --context "$context" -n "$namespace" get sandboxsnapshot "$snapshot_name" >/dev/null 2>&1; then
  echo "internal SandboxSnapshot was not deleted after resume" >&2
  exit 1
fi

echo "QEMU_VMSTATE_E2E_RESULT success"
echo "QEMU_VMSTATE_E2E_REGISTRY $registry"
echo "QEMU_VMSTATE_E2E_POOL source=$source_pod replacement=$replacement_pool_pod restored=$restored_pod"
echo "QEMU_VMSTATE_E2E_BEFORE $before"
echo "QEMU_VMSTATE_E2E_AFTER $after"
echo "QEMU_VMSTATE_E2E_DISK $after_disk"
echo "QEMU_VMSTATE_E2E_ROOTFS image=$rootfs_image_uri digest=$rootfs_digest"
echo "QEMU_VMSTATE_E2E_VMSTATE image=$vmstate_image_uri digest=$vmstate_digest size_bytes=$vmstate_size"
