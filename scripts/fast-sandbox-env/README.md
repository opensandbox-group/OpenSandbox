# fast-sandbox integration environment

One-command fast-sandbox integration environment driven from the
OpenSandbox repository: a two-node kind cluster (KVM passthrough) running
the fast-sandbox Firecracker chain from a `master` checkout, with the
OpenSandbox egress sidecar attached to the default pool. Initializes the
environment and the `firecracker-egress-pool` SandboxPool (egress +
P2P defaults) — **no sandboxes are created**.

Usage (Linux KVM host; details, prerequisites, and env knobs in the
[guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/guides/fast-sandbox-integration-env.md)):

```bash
./scripts/fast-sandbox-env/fast-sandbox-env.sh up        # full environment + pool
./scripts/fast-sandbox-env/fast-sandbox-env.sh status    # component / pool / DART health
./scripts/fast-sandbox-env/fast-sandbox-env.sh pool      # re-apply the pool only
./scripts/fast-sandbox-env/fast-sandbox-env.sh down      # teardown, host left clean
```

Layout:

```
fast-sandbox-env.sh          entrypoint: up / down / status / pool
manifests/
  cluster/kind-cluster.yaml    two-node kind cluster, KVM/tun/shm mounts,
                               per-node state-root subdirectories
  node/firecracker-installer.yaml  firecracker + jailer + arch-aware kernel
  node/dart-service.yaml       headless Service backing DART peer discovery
  node/runtime-agent.yaml      per-node agent + DART child (P2P data plane)
  pool/firecracker-egress-pool.yaml  SandboxPool: egress attached + P2P spread
```

Canonical fast-sandbox manifests (CRDs, RBAC, control plane) are applied
from the fast-sandbox checkout (`FSB_DIR`, default sibling
`../fast-sandbox`, tracked to `master`) and are not duplicated here.
