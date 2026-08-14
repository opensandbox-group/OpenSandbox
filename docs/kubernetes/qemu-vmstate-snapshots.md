---
title: QEMU VMState Snapshots
description: Configure and operate process-level pause and resume for QEMU running inside a runc sandbox.
---

# QEMU VMState Snapshots

OpenSandbox can opt a QEMU-in-runc workload into process-level pause and resume. The normal container snapshot still preserves the outer root filesystem and the writable guest disk overlay. A second, ordinary container image stores the compressed QEMU migration stream and its compatibility manifest.

This mode is experimental. It restores the QEMU process and Guest memory; other processes in the outer runc container restart from the `BatchSandbox` Pod template.

The initial `qemu-v1` implementation supports same-`BatchSandbox` pause/resume. The public snapshot clone API does not yet restore QEMU VMState because its snapshot record does not persist the complete Pod template and QEMU launch plan. A standalone public snapshot operation resumes its source VM after publishing the artifacts.

## Snapshot contents

| State | Storage |
|---|---|
| Outer container rootfs | Existing rootfs snapshot image |
| Writable guest qcow2 overlay | Rootfs snapshot image; the overlay must not be under a Kubernetes volume mount |
| Guest RAM, vCPU, and emulated device state | VMState image containing a zstd-compressed QEMU migration stream |
| Immutable guest base image | Original image layer or an independently available read-only volume |
| PVC, hostPath, cloud disk, network peer state | Not copied by OpenSandbox |

Both images are pushed to the configured image Registry and recorded by manifest digest in one `SandboxSnapshot.status`. Resume never relies on mutable tags.

## Workload contract

Set these annotations on `BatchSandbox.spec.template.metadata`:

```yaml
annotations:
  sandbox.opensandbox.io/checkpoint-provider: qemu
  sandbox.opensandbox.io/qemu-container: qemu
  sandbox.opensandbox.io/qemu-qmp-socket: /run/qemu/qmp.sock
  sandbox.opensandbox.io/qemu-launch-manifest: /run/qemu/launch.json
  # Optional: constrain restore to compatible nodes.
  sandbox.opensandbox.io/qemu-required-node-class: shenlong-v1
```

The QEMU container must create the QMP Unix socket and launch manifest before it becomes Ready. OpenSandbox selects this mode only from the annotation; it does not scan process names.

Example launch manifest:

```json
{
  "formatVersion": "qemu-v1",
  "architecture": "amd64",
  "qemuVersion": "6.2.0",
  "machineType": "pc-q35-6.2",
  "cpuModel": "host",
  "vcpus": 2,
  "memoryBytes": 536870912,
  "qemuConfigDigest": "sha256:...",
  "disks": [
    {
      "id": "osdisk",
      "overlayPath": "/vm/state.qcow2",
      "capture": "rootfs"
    }
  ]
}
```

Use an explicitly versioned QEMU machine type. The workload entrypoint must detect `OPENSANDBOX_RESTORE_MODE=qemu-v1` and start QEMU with the loader stream supplied in `OPENSANDBOX_VMSTATE_DIR`:

```bash
qemu_args+=(
  -incoming
  "exec:$OPENSANDBOX_VMSTATE_DIR/vmstate-loader stream --dir $OPENSANDBOX_VMSTATE_DIR"
)
```

## Pause and resume sequence

1. The controller resolves the annotated QEMU container and schedules the snapshot Job on the same node.
2. The worker validates that every writable overlay declared as `capture: rootfs` is outside the container's Kubernetes volume mount paths.
3. The worker uses QMP migration to export VM state, compresses it with zstd, and puts it into a standard container image.
4. After QEMU reaches post-migration state, the worker freezes the outer containers, commits their root filesystems, and pushes both image types.
5. The controller publishes rootfs and VMState manifest digests atomically, then removes the source Pod.
6. On resume, a loader init container restores and verifies the VMState files in an `emptyDir`. It is appended after all user init containers.
7. The recreated QEMU container consumes the migration stream. Other outer-container processes start normally from the Pod template.

`savevm` is not used. It creates an internal snapshot coupled to qcow2 storage and is not the transport for live Guest RAM used here.

## Cluster dependencies and security

- Linux nodes with `/dev/kvm` exposed to the QEMU container.
- The image Registry must accept Docker schema 2 or OCI image manifests and be reachable from both snapshot Jobs and kubelet/containerd.
- The QEMU version, versioned machine type, CPU model, vCPU count, RAM size, device configuration digest, and optional node class must match on restore.
- The snapshot Job needs the host containerd runtime directory, host PID namespace, and `SYS_PTRACE`. It already operates inside the node-level trust boundary because it controls the containerd socket. Pod Security and admission policies must explicitly allow this Job.
- VMState can contain credentials and user data from Guest RAM. Apply the same access control, encryption, retention, and deletion policy as sensitive persistent storage.
- Plan Registry capacity from measured compressed payload size. Compression depends on Guest memory contents; it is not guaranteed to be small.

## Validation

The dedicated E2E allocates a warm QEMU-in-runc Pod from a `Pool`, writes independent values into an anonymous mmap, the raw Guest disk, and the outer rootfs, snapshots both images, and restores a standalone Pod. It verifies Pool replenishment and detachment, immutable rootfs and VMState image digests, loader completion, snapshot cleanup, all three values, the Guest boot ID, and the live counter:

```bash
cd kubernetes
make test-e2e-qemu
```

The test requires Linux amd64, Docker, Kind, kubectl, and `/dev/kvm`. By default it deploys an isolated `registry:2` Pod in the Kind cluster. Set `QEMU_E2E_SNAPSHOT_REGISTRY` and `QEMU_E2E_DOCKER_CONFIG` to verify the same flow against an authenticated external Registry; the two repositories are `<prefix>/qemu-rootfs` and `<prefix>/qemu-vmstate`. Set `KEEP_QEMU_E2E_CLUSTER=true` to retain the dedicated Kind cluster for diagnostics. See the [E2E README](https://github.com/alibaba/OpenSandbox/tree/main/kubernetes/test/e2e_qemu) for all overrides.

### Reusable Kind validation environment

The E2E harness can build the images, create a dedicated KVM-enabled Kind
cluster, deploy the controller and Registry, run the reference Pool flow, and
leave the environment available for manual checks:

```bash
cd kubernetes
KEEP_QEMU_E2E_CLUSTER=true make test-e2e-qemu
```

The resulting context is `kind-opensandbox-qemu-vmstate-e2e`. The controller
runs in `opensandbox-system`, while the in-cluster `registry:2` Pod and the
automated test objects run in `qemu-vmstate-e2e`:

```bash
kubectl --context kind-opensandbox-qemu-vmstate-e2e \
  -n opensandbox-system get deploy,pod
kubectl --context kind-opensandbox-qemu-vmstate-e2e \
  -n qemu-vmstate-e2e get pod,service,pool,batchsandbox
```

To exercise an authenticated external Registry instead, provide its repository
prefix and a local Docker `config.json`; the harness creates the temporary
Kubernetes pull/push Secret without printing its contents:

```bash
QEMU_E2E_SNAPSHOT_REGISTRY=registry.example.com/team \
QEMU_E2E_DOCKER_CONFIG=/path/to/config.json \
KEEP_QEMU_E2E_CLUSTER=true \
make test-e2e-qemu
```

The commands below use a separate `qemu-manual` namespace, so the retained E2E
objects do not need to be deleted first.

### Manual validation in a prepared Kind cluster

This flow deliberately leaves lifecycle actions to the operator. It assumes
the controller, snapshot Registry, image-committer image, and demo QEMU image
are already installed in a Kind cluster with `/dev/kvm`. From the repository
root, select that cluster and create only the standalone `BatchSandbox`:

```bash
export OSB_QEMU_CONTEXT=kind-opensandbox-qemu-vmstate-e2e
export OSB_QEMU_NAMESPACE=qemu-manual
export OSB_QEMU_SANDBOX=qemu-standalone
export OSB_QEMU_POD=qemu-standalone-0

test -c /dev/kvm
kubectl --context "$OSB_QEMU_CONTEXT" get nodes
kubectl --context "$OSB_QEMU_CONTEXT" -n opensandbox-system \
  rollout status deployment/opensandbox-controller-manager
kubectl --context "$OSB_QEMU_CONTEXT" apply \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/standalone.yaml
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  wait --for=condition=Ready pod/"$OSB_QEMU_POD" --timeout=180s
```

The demo Guest runs the HTTP server and the mutable memory map in the same PID
1 process. QEMU user networking forwards the outer container's loopback port
`18080` to Guest port `8080`; the service is not exposed on the Pod IP.

| Endpoint | Purpose |
| --- | --- |
| `GET /healthz` | Verify that the resumed Guest process is serving requests |
| `PUT /value` | Store a value of up to 512 bytes in the process memory map |
| `GET /value` | Read the current memory-map value |
| `GET /status` | Read PID, Guest boot ID, mmap-backed live counter, and value |
| `PUT /disk` | Store a marker in the raw writable Guest disk |
| `GET /disk` | Read the raw Guest disk marker |

Define a phase waiter and put three independent markers in Guest memory, the
Guest disk, and the outer container rootfs:

```bash
wait_for_sandbox_phase() {
  local wanted=$1
  local phase=
  for _ in $(seq 1 180); do
    phase=$(kubectl --context "$OSB_QEMU_CONTEXT" \
      -n "$OSB_QEMU_NAMESPACE" get batchsandbox "$OSB_QEMU_SANDBOX" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [[ "$phase" == "$wanted" ]]; then
      return 0
    fi
    sleep 2
  done
  echo "timed out waiting for phase=$wanted; last phase=$phase" >&2
  return 1
}

export OSB_MEMORY_TOKEN="MANUAL-MMAP-$(date -u +%Y%m%dT%H%M%SZ)"
export OSB_DISK_TOKEN="MANUAL-DISK-$(date -u +%Y%m%dT%H%M%SZ)"
export OSB_ROOTFS_TOKEN="MANUAL-ROOTFS-$(date -u +%Y%m%dT%H%M%SZ)"

kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- \
  curl --fail --silent --show-error --request PUT \
  --data-binary "$OSB_MEMORY_TOKEN" http://127.0.0.1:18080/value
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- \
  curl --fail --silent --show-error --request PUT \
  --data-binary "$OSB_DISK_TOKEN" http://127.0.0.1:18080/disk
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- sh -c \
  'mkdir -p /var/lib/opensandbox && printf "%s" "$1" > /var/lib/opensandbox/rootfs-marker' \
  sh "$OSB_ROOTFS_TOKEN"

OSB_BEFORE=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/status)
OSB_SOURCE_UID=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get pod "$OSB_QEMU_POD" -o jsonpath='{.metadata.uid}')
printf 'before: %s\nsource uid: %s\n' "$OSB_BEFORE" "$OSB_SOURCE_UID"
```

In another terminal, watch the resources involved in pause and resume:

```bash
watch -n 1 kubectl --context "$OSB_QEMU_CONTEXT" \
  -n "$OSB_QEMU_NAMESPACE" get batchsandbox,sandboxsnapshot,pod,job
```

The initial manifest intentionally omits `spec.pause`: setting it to `false`
on a brand-new object is interpreted as an explicit resume request. Pause the
sandbox by adding `spec.pause: true`, then inspect the two immutable image
artifacts:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  edit batchsandbox "$OSB_QEMU_SANDBOX"
wait_for_sandbox_phase Paused

export OSB_SNAPSHOT_NAME="$OSB_QEMU_SANDBOX-pause"
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get sandboxsnapshot "$OSB_SNAPSHOT_NAME" -o yaml
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  wait --for=delete pod/"$OSB_QEMU_POD" --timeout=120s
```

The snapshot must report `status.format: qemu-v1`, a rootfs image digest under
`status.containers`, and a different VMState image digest plus a non-zero
compressed size under `status.virtualMachine`.

Resume by editing `spec.pause` to `false`, then verify process-level
continuity and both filesystem layers:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  edit batchsandbox "$OSB_QEMU_SANDBOX"
wait_for_sandbox_phase Succeed
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  wait --for=condition=Ready pod/"$OSB_QEMU_POD" --timeout=180s

OSB_AFTER=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/status)
OSB_AFTER_DISK=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- \
  curl --fail --silent --show-error http://127.0.0.1:18080/disk)
OSB_AFTER_ROOTFS=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_QEMU_POD" -c qemu -- cat /var/lib/opensandbox/rootfs-marker)
OSB_RESTORED_UID=$(kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get pod "$OSB_QEMU_POD" -o jsonpath='{.metadata.uid}')

OSB_BEFORE_BOOT_ID=$(printf '%s\n' "$OSB_BEFORE" | sed -n 's/.*"boot_id":"\([^"]*\)".*/\1/p')
OSB_AFTER_BOOT_ID=$(printf '%s\n' "$OSB_AFTER" | sed -n 's/.*"boot_id":"\([^"]*\)".*/\1/p')
OSB_BEFORE_COUNTER=$(printf '%s\n' "$OSB_BEFORE" | sed -n 's/.*"counter":\([0-9]*\).*/\1/p')
OSB_AFTER_COUNTER=$(printf '%s\n' "$OSB_AFTER" | sed -n 's/.*"counter":\([0-9]*\).*/\1/p')

[[ "$OSB_SOURCE_UID" != "$OSB_RESTORED_UID" ]]
[[ "$OSB_BEFORE_BOOT_ID" == "$OSB_AFTER_BOOT_ID" ]]
((OSB_AFTER_COUNTER > OSB_BEFORE_COUNTER))
[[ "$OSB_AFTER" == *"\"value\":\"$OSB_MEMORY_TOKEN\""* ]]
[[ "$OSB_AFTER_DISK" == *"\"value\":\"$OSB_DISK_TOKEN\""* ]]
[[ "$OSB_AFTER_ROOTFS" == "$OSB_ROOTFS_TOKEN" ]]

printf 'after: %s\ndisk: %s\nrootfs: %s\nrestored uid: %s\n' \
  "$OSB_AFTER" "$OSB_AFTER_DISK" "$OSB_AFTER_ROOTFS" "$OSB_RESTORED_UID"
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get pod "$OSB_QEMU_POD" -o jsonpath='{range .status.initContainerStatuses[*]}{.name}{"="}{.state.terminated.reason}{"\n"}{end}'
```

All assertions must return zero. The Pod UID must change, the Guest boot ID
must remain the same, the live counter must increase, all three tokens must
match, and the VMState loader init container must report `Completed`. The
internal `SandboxSnapshot` is deleted after a successful resume. Remove only
the test object when finished; the Kind infrastructure remains available:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" delete \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/standalone.yaml
```

### Manual validation with a Pool

Create the Pool first and wait for its warm QEMU Pod. Then create a
`BatchSandbox` that allocates that Pod:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" apply \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/pool.yaml
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get pool qemu-pool -w
```

After the Pool reports ready capacity, stop the watch and run:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" apply \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/pooled-sandbox.yaml
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get batchsandbox qemu-pooled -w
```

Read the allocated Pod name from the allocation annotation and write the same
three markers used by the standalone case:

```bash
export OSB_POOLED_POD=$(kubectl --context "$OSB_QEMU_CONTEXT" \
  -n "$OSB_QEMU_NAMESPACE" get batchsandbox qemu-pooled \
  -o jsonpath='{.metadata.annotations.sandbox\.opensandbox\.io/alloc-status}' \
  | jq -r '.pods[0]')

export OSB_POOL_MEMORY_TOKEN="POOL-MMAP-$(date -u +%Y%m%dT%H%M%SZ)"
export OSB_POOL_DISK_TOKEN="POOL-DISK-$(date -u +%Y%m%dT%H%M%SZ)"
export OSB_POOL_ROOTFS_TOKEN="POOL-ROOTFS-$(date -u +%Y%m%dT%H%M%SZ)"

kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_POOLED_POD" -c qemu -- \
  curl --fail --silent --show-error --request PUT \
  --data-binary "$OSB_POOL_MEMORY_TOKEN" http://127.0.0.1:18080/value
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_POOLED_POD" -c qemu -- \
  curl --fail --silent --show-error --request PUT \
  --data-binary "$OSB_POOL_DISK_TOKEN" http://127.0.0.1:18080/disk
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  exec "$OSB_POOLED_POD" -c qemu -- sh -c \
  'mkdir -p /var/lib/opensandbox && printf "%s" "$1" > /var/lib/opensandbox/rootfs-marker' \
  sh "$OSB_POOL_ROOTFS_TOKEN"
```

Run `kubectl edit batchsandbox qemu-pooled -n qemu-manual` and add
`spec.pause: true`. On successful pause, verify the Pool-specific handoff:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get batchsandbox qemu-pooled \
  -o jsonpath='phase={.status.phase}{" poolRef="}{.spec.poolRef}{" templateContainers="}{.spec.template.spec.containers[*].name}{"\n"}'
kubectl --context "$OSB_QEMU_CONTEXT" -n "$OSB_QEMU_NAMESPACE" \
  get sandboxsnapshot qemu-pooled-pause -o yaml
```

The phase must be `Paused`. The controller materializes the allocated Pod's
template into the `BatchSandbox` and clears `spec.poolRef`; this detach is
intentional, so resume creates an independent Pod instead of returning to the
Pool. Edit `spec.pause` back to `false`, wait for `qemu-pooled-0` to become
Ready, then call `GET /status`, `GET /disk`, and the rootfs marker check against
it.

Clean up the Pool example without deleting the Kind cluster:

```bash
kubectl --context "$OSB_QEMU_CONTEXT" delete \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/pooled-sandbox.yaml
kubectl --context "$OSB_QEMU_CONTEXT" delete \
  -f kubernetes/config/samples/alibaba/qemu-vmstate/pool.yaml
```
