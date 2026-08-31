# QEMU VMState E2E

Run `make test-e2e-qemu` from `kubernetes/` on a Linux amd64 host with `/dev/kvm`. The test allocates a warm QEMU Pod from a `Pool`, pauses it, and restores a standalone Pod from the published rootfs and VMState images. It checks Pool replenishment and detachment, immutable digest references, VMState loader completion, snapshot cleanup, an outer-rootfs marker, a raw Guest disk token, an anonymous mmap token, the Guest boot ID, and a live counter.

See [QEMU VMState Snapshots](../../../docs/kubernetes/qemu-vmstate-snapshots.md) for the workload contract, dependencies, security model, and validation criteria.

Set `KEEP_QEMU_E2E_CLUSTER=true` to retain the dedicated cluster for diagnostics. Set `QEMU_E2E_GOPROXY` when the default Go module proxy is not reachable from Docker builds.

By default the test deploys an unauthenticated `registry:2` Pod in its dedicated Kind cluster. To exercise an authenticated external Registry, pass a repository prefix and a readable Docker `config.json`:

```bash
QEMU_E2E_SNAPSHOT_REGISTRY=registry.example.com/team \
QEMU_E2E_DOCKER_CONFIG=/path/to/config.json \
make test-e2e-qemu
```

The test creates a temporary `kubernetes.io/dockerconfigjson` Secret in its namespace and publishes `<prefix>/qemu-rootfs` plus `<prefix>/qemu-vmstate`. Set `QEMU_E2E_REGISTRY_SECRET` to change the Secret name, or `QEMU_E2E_SNAPSHOT_REGISTRY_INSECURE=true` for an HTTP Registry. Keep credentials outside the repository; the test accepts only the local config path and never prints its contents.

The test uses the standard `Dockerfile` for the controller. A downstream tree with private Go modules can set `QEMU_E2E_CONTROLLER_DOCKERFILE` to its authenticated controller Dockerfile. If that Dockerfile has a heavy runtime stage, set `QEMU_E2E_CONTROLLER_BUILDER_TARGET` (normally `builder`) to copy its compiled binary into the E2E's minimal runtime image.
