# QEMU-in-runc VMState pause/resume sample

This is an experimental, Alibaba-only sample for running QEMU inside a normal
privileged runc Pod and preserving the Guest process state across
`BatchSandbox` pause/resume. It does not use a Kubernetes `RuntimeClass`.

## State and Registry layout

There are three logical state domains, but only two OCI image references:

1. The outer runc container rootfs is committed as the normal snapshot image.
2. The writable Guest disk `/vm/state.qcow2` is a file inside that rootfs image;
   it is not pushed as a third, independent image.
3. Guest RAM, vCPU, and emulated device state are exported through QMP
   migration into `vmstate.zst` and pushed in a separate VMState image.

While the sandbox is paused, the internal `SandboxSnapshot` records the two
immutable references under `status.containers[]` and `status.virtualMachine`.
On resume, the first reference becomes the QEMU container image. An injected
init container pulls the second reference, verifies `manifest.json` and
`vmstate.zst`, and makes the migration stream available to QEMU `-incoming`.

The writable qcow2 path must be declared with `capture: rootfs` in the QEMU
launch manifest and must not be under a Kubernetes volume mount. PVC,
hostPath, cloud-disk, and external network-peer state are not captured.

## Prerequisites

- Linux amd64 node with `/dev/kvm`.
- Controller configured with `--snapshot-registry`,
  `--snapshot-registry-insecure` when applicable, and the private
  `--image-committer-image` containing QEMU checkpoint support.
- A Registry reachable by the controller Job and node containerd.
- The demo image built from `kubernetes/test/e2e_qemu/testdata` and available
  as `opensandbox/qemu-vmstate-e2e:dev`.
- A compatible QEMU version, machine type, CPU model, vCPU count, memory size,
  and device configuration on restore.

## Validation and workload contract

See
[`docs/kubernetes/qemu-vmstate-snapshots.md`](../../../../../docs/kubernetes/qemu-vmstate-snapshots.md)
for the workload contract, Kind environment setup, command-by-command manual
validation, Registry inspection, and security and compatibility boundaries.
