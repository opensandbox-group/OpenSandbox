---
title: QEMU VMState Snapshots for QEMU-in-runc Sandboxes
authors:
  - "@fengcone"
creation-date: 2026-08-14
last-updated: 2026-08-19
status: experimental
---

# QEMU VMState Snapshots for QEMU-in-runc Sandboxes

## Summary

Extend the existing rootfs pause/resume implementation with an opt-in QEMU
checkpoint provider. A QEMU snapshot consists of two immutable standard
container images: the existing committed container rootfs and a second image
containing a compressed QEMU migration stream. `SandboxSnapshot.status` binds
both image digests atomically. Resume pulls the VM state image as an init
container and starts QEMU with an incoming migration stream.

The first version uses the configured image registry for both artifacts. It
does not require object storage, a custom OCI artifact media type, `savevm`, or
a Kubernetes RuntimeClass.

This document records the design and its boundaries. Workload authors and
cluster operators should follow the
[QEMU VMState operations guide](../../../docs/kubernetes/qemu-vmstate-snapshots.md)
for image preparation, Helm deployment, Kind validation, and troubleshooting.

## Motivation

Rootfs-only snapshots preserve files but restart all processes. Sandboxes that
run QEMU inside a normal runc container need the QEMU RAM, vCPU, and emulated
device state in addition to their container filesystem. QEMU migration streams
provide that process-level state while keeping the existing rootfs commit path
for the outer container.

### Goals

- Preserve QEMU guest memory, CPU, and migratable device state.
- Preserve existing rootfs-only behavior for workloads that do not opt in.
- Store both artifacts in registries that support normal Docker/OCI images.
- Pull all restore inputs through kubelet with existing image pull secrets.
- Restore by immutable digest and fail before launch when compatibility checks
  do not pass.
- Provide a real KVM/Kind end-to-end test on a Linux host.

### Non-Goals

- Checkpoint arbitrary Linux processes in the outer runc container.
- Automatically detect QEMU by scanning the process table.
- Snapshot writable PVC, hostPath, cloud disk, or emptyDir contents in v1.
- Restore into an arbitrary pre-warmed Pool Pod.
- Guarantee migration across incompatible QEMU versions, machine types, CPU
  models, or device topologies.

## Proposal

### Workload contract

QEMU support is enabled explicitly through Pod template annotations:

```yaml
sandbox.opensandbox.io/checkpoint-provider: qemu
sandbox.opensandbox.io/qemu-container: main
sandbox.opensandbox.io/qemu-qmp-socket: /run/opensandbox/qemu/qmp.sock
sandbox.opensandbox.io/qemu-launch-manifest: /run/opensandbox/qemu/launch.json
sandbox.opensandbox.io/qemu-required-node-class: shenlong-v1 # optional
```

The QEMU image is responsible for:

- exposing a QMP Unix socket at the declared path;
- producing the declared launch manifest;
- using a versioned machine type and repeatable device topology;
- recognizing the injected restore manifest on container startup;
- keeping writable guest disk overlays inside the container rootfs in v1;
- keeping liveness healthy while a checkpoint marker is present.

The launch manifest is an OpenSandbox-defined workload contract, not a QEMU
file and not an executable launch plan. The workload entrypoint produces it
from the same effective values used to construct the QEMU command line. A
static file baked into the image is valid only when every compatibility-
sensitive setting is immutable; runtime generation is preferred when CPU,
memory, disks, or devices are configurable through the Pod template.

QMP and the launch manifest have separate responsibilities:

- QMP controls the live process and exports or resumes the migration stream.
- The launch manifest declares restore compatibility and storage intent that
  QMP cannot reliably reconstruct, including the writable overlay capture
  policy, optional base-image identity, and the workload-defined device
  configuration digest.

The snapshot worker treats `qemuConfigDigest` as an opaque SHA-256 identity
for the compatibility-sensitive QEMU configuration. The workload must derive
it deterministically from a canonical representation of machine, CPU, memory,
disk, network, firmware, and device settings. It must not include transient
values such as PID, socket path, generated MAC address, or timestamps. The v1
worker records this value but does not reconstruct the command line from it.

The snapshot Job is node-trusted infrastructure. QEMU mode uses the host PID
namespace, `SYS_PTRACE`, and a same-path mount of the host containerd runtime
directory so `nerdctl cp` and `nerdctl exec` can reach the target rootfs and
containerd-shim FIFOs. Rootfs-only Jobs retain the narrower socket-only mount.

Existing init containers are retained and run before the OpenSandbox VM state
loader. Their names are not part of the checkpoint contract.

### Artifact model

```text
SandboxSnapshot
  rootfs images[]
    repository + manifest digest
  virtualMachine
    VM state image repository + manifest digest
    compressed payload digest and size
    compatibility summary
```

The VM state image is a normal runnable container image containing:

```text
/usr/local/bin/vmstate-loader
/opensandbox/checkpoint/manifest.json
/opensandbox/checkpoint/vmstate.zst
```

The complete compatibility manifest is stored in the image. The CR status
contains a smaller summary for scheduling, validation, and observability.

### Design Q&A

#### Why does a QEMU snapshot contain two images?

A QEMU-in-runc sandbox has two different state domains with different restore
consumers:

1. The **rootfs image** is the committed root filesystem of the outer runc
   container. It contains the QEMU binary and workload files, changes made by
   other processes in the container, and the writable Guest qcow2 overlay when
   that overlay is stored inside the container rootfs. The qcow2 file is the
   Guest disk, not the outer container rootfs itself.
2. The **VMState image** contains the compressed QEMU migration stream. It
   represents the live Guest RAM, vCPU state, and migratable emulated-device
   state at the checkpoint boundary. An injected init container pulls and
   verifies this image before the QEMU container starts with `-incoming`.

The rootfs image is a normal replacement image for the outer container. It is
sufficient to start that container and cold-boot QEMU from the captured qcow2
disk, but it cannot continue the Guest process from the exact paused
instruction and memory state. The VMState image supplies that missing live
state; it is not useful by itself because it must be restored against the
matching Guest disk and QEMU launch topology.

The VMState therefore has a strong compatibility relationship with both the
rootfs snapshot and the launch configuration. The recorded compatibility
summary includes architecture, QEMU version, versioned machine type, CPU
model, vCPU count, RAM size, device-configuration digest, and optional node
class. The Guest OS is not a separate compatibility field: its disk contents
are in qcow2 and its running kernel and process state are already represented
by the disk plus migration stream.

Keeping two images preserves the existing rootfs commit and Kubernetes image
restore path while giving VMState its own streaming, compression, verification,
and loader lifecycle. Combining them would require rebuilding the committed
rootfs image to append a large migration payload and would still require a
pre-start extraction mechanism. `SandboxSnapshot.status` instead publishes the
two immutable manifest digests together, so resume treats them as one atomic
checkpoint even though the Registry stores two images.

#### Why is an OpenSandbox `launch.json` required?

The QEMU migration stream is not a complete, portable recipe for recreating
the source process. Before QEMU can consume `-incoming`, the workload must
start a compatible QEMU process with the same machine type, CPU model, memory,
vCPU count, firmware, disk, network, and device topology.

QMP can control the running process, report selected runtime information, and
export the migration stream, but it cannot reliably recover the complete
workload launch intent. In particular, it does not define which qcow2 file
OpenSandbox must capture, whether that file belongs to the container rootfs,
which immutable base image it depends on, or which normalized device settings
the workload considers restore-compatible.

`launch.json` fills that gap. It is an OpenSandbox-defined declarative manifest
generated by the workload entrypoint from the same effective values used to
construct the QEMU command line. The snapshot worker uses it to:

- validate the QEMU version and required compatibility fields;
- validate writable disk paths and the `capture: rootfs` policy;
- record the disk and compatibility metadata beside the VMState payload; and
- expose a compatibility summary for scheduling, diagnostics, and restore.

The manifest is descriptive metadata, not an executable QEMU configuration.
OpenSandbox does not use it to synthesize the QEMU command line. The workload
entrypoint remains responsible for deterministically recreating that command
and adding `-incoming` during restore.

### Guest disk layout

The v1 provider supports a writable qcow2 overlay in the main container
rootfs. A typical layout is:

```text
/ubuntu-storage/base.qcow2                 # immutable, prepared by an init container
/var/lib/opensandbox/vm/osdisk.delta.qcow2 # writable overlay, captured by rootfs commit
```

The launch manifest records the base digest and overlay path. A writable QEMU
disk under a Kubernetes volume mount is rejected because `nerdctl commit` does
not capture mounted volume contents.

### Pause sequence

1. Stop scheduled OpenSandbox tasks.
2. Resolve the source Pod and validate the explicit QEMU contract.
3. Preflight QMP, compatibility, disk layout, limits, and work space. Volume
   paths come from the source PodSpec instead of nerdctl's Docker-compatible
   inspect view, which is incomplete for CRI-created containers.
4. Export the QEMU migration stream through a file descriptor passed to QEMU
   over QMP and compress it with zstd.
5. Wait for migration completion; the source QEMU enters `postmigrate`.
6. Freeze the outer containers, remove temporary helpers, and commit rootfs
   images.
7. Build and push the standard VM state image and rootfs images.
8. Resolve all registry manifest digests and update the complete
   `SandboxSnapshot.status` in one status write.
9. For an internal pause snapshot, release the source Pod only after the
   snapshot is ready. A standalone public snapshot resumes the source outer
   container and QEMU after artifact publication.

Failures restore both the outer containers and the source guest when possible.
The controller creates a same-node recovery Job if the primary Job cannot
finish its own cleanup.

### Resume sequence

1. Validate both immutable image references and the compatibility summary.
2. Rewrite container images to `repository@sha256:digest`.
3. Append a VM state loader init container after the existing init containers.
4. Mount a shared restore `emptyDir` into the loader and QEMU container.
5. Start the original container command with an injected restore manifest.
6. The QEMU entrypoint starts the same machine with `-incoming` and waits for
   QMP to report `running` before readiness succeeds.
7. Outer helper processes start normally and reconnect to the restored guest.

For pooled sandboxes, pause continues to solidify the allocated Pod template
and detach the `BatchSandbox` from the Pool. Resume creates a standalone Pod
from that exact template.

### API changes

`SandboxSnapshot.status` gains additive fields:

- `format`: `rootfs-v1` or `qemu-v1`;
- `virtualMachine`: image reference, payload metadata, manifest digest, and
  compatibility summary.

Existing objects without `status.format` are interpreted as `rootfs-v1`.

### Annotation contract changes

| Key | Writer | Reader |
|---|---|---|
| `sandbox.opensandbox.io/checkpoint-provider` | Pool/BatchSandbox template owner | snapshot controller |
| `sandbox.opensandbox.io/qemu-container` | Pool/BatchSandbox template owner | snapshot worker |
| `sandbox.opensandbox.io/qemu-qmp-socket` | QEMU workload owner | snapshot worker |
| `sandbox.opensandbox.io/qemu-launch-manifest` | QEMU workload owner | snapshot worker |
| `sandbox.opensandbox.io/qemu-required-node-class` | platform operator | resume controller |

## Implementation details

- Add a provider abstraction under `internal/snapshot` and keep the current
  rootfs provider as the default.
- Refactor `cmd/image-committer` into orchestration, rootfs, QEMU, and registry
  packages instead of adding QEMU logic to the existing monolithic command.
- Add small static `qemu-checkpoint-helper` and `vmstate-loader` binaries.
- Construct a normal container image for VM state and push it with the existing
  registry credentials.
- Extend `BatchSandboxReconciler.continueResume` to inject the loader and use
  immutable rootfs references.
- Keep v1 restore scoped to same-`BatchSandbox` pause/resume. The server rejects
  public QEMU snapshot clone as unsupported until its snapshot record persists
  the complete Pod template and QEMU launch plan.

## Risks and mitigations

- **Secret material in guest RAM:** use a dedicated private snapshot repository,
  retention controls, audit, and encryption at rest.
- **Large VM state:** enforce configurable byte/time limits and explicit worker
  ephemeral-storage requests. Compression effectiveness is workload-dependent.
- **Compatibility failure:** record and validate versioned machine type, QEMU
  version, CPU model, memory, vCPU count, config digest, and node class.
- **Tag mutation:** tags are publication handles only; restore always uses the
  manifest digest.
- **Probe restart during checkpoint:** the QEMU workload contract requires a
  checkpoint-aware supervisor/liveness path.
- **Mounted writable disks:** reject them in v1 instead of producing an
  incomplete snapshot.
- **Node-level worker privileges:** isolate snapshot-capable nodes and use
  admission policy to allow host PID, `SYS_PTRACE`, and the containerd runtime
  directory only for the image-committer Job identity.

## Upgrade strategy

The provider defaults to rootfs when annotations are absent. All CRD fields are
additive, so existing `BatchSandbox`, `Pool`, and `SandboxSnapshot` resources
continue to use the current behavior without conversion.

## Test plan

- Unit-test annotation parsing, immutable references, compatibility validation,
  QMP state transitions, manifest verification, and Pod injection.
- Controller tests cover rootfs backward compatibility and QEMU success/failure
  reconciliation.
- Registry tests push, independently pull, and verify a standard VM state image.
- A Linux KVM/Kind E2E writes Guest mmap, raw Guest disk, and outer rootfs
  tokens, pauses, restores a new Pod/QEMU process, and verifies all tokens plus
  the Guest boot ID and live counter.
- Failure cases cover invalid QMP paths, missing KVM, mounted writable disks,
  corrupted payloads, registry authorization, upload interruption, and
  incompatible CPU/machine configuration.

## Implementation history

- [x] 2026-08-14: QEMU-in-runc and registry compatibility PoC completed.
- [x] 2026-08-14: Proposal drafted and implementation branch created.
- [x] 2026-08-19: QEMU snapshot implementation prepared for review.
- [x] 2026-08-14: Dedicated Linux KVM/Kind E2E passed with a new Pod, stable
  Guest boot ID, continuous mmap counter, and preserved mmap, raw Guest disk,
  and outer-rootfs values; the compressed 512 MiB Guest VMState payload was
  59,477,362 bytes.
- [x] 2026-08-17: Pool allocation and restore passed against an authenticated
  external Registry. Pause materialized and detached the allocated Pod,
  the Pool replenished its warm capacity, and resume created a standalone Pod
  from immutable rootfs and VMState digests. The compressed 512 MiB VMState
  payload was 63,320,397 bytes.
