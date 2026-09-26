# fast-sandbox Helm Chart

A Helm chart for deploying the fast-sandbox Firecracker chain on a Kubernetes cluster: the `sandbox.fast.io` all-in-one control plane (reconcilers + FastPath gRPC) and the node-side firecracker runtime (UDS management API, DART peer discovery, node readiness loop, janitor sidecar).

## Introduction

The chart deploys:

- **fast-sandbox-controller** (Deployment + `fast-sandbox-fastpath` Service): one process running the `sandbox.fast.io` reconcilers and the FastPath gRPC API (development topology, no leader election)
- **firecracker-runtime** (DaemonSet + `dart` headless Service): the node-level firecracker agent (UDS management API used by fastlet Firecracker drivers, node-local DART child for P2P artifact delivery, janitor sidecar sweeping orphaned fastlet resources) running the node readiness loop — host checks, Firecracker asset install, the `sandbox.fast.io/kvm` + `fast-sandbox.io/firecracker-node` scheduling labels and the `FirecrackerReady` condition (the kata-deploy pattern; no manual node labeling)

boxlite, containerd-based runtimes and the upstream central sandbox-proxy are out of scope for this chart (the standalone containerd janitor DaemonSet is therefore not deployed): OpenSandbox deployments reach fastlets through the ingress gateway's direct route resolution (see `manifests/release/build-fast-sandbox.sh`).

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.0+
- The `sandbox.fast.io` CRDs and component RBAC, installed by the [base chart](../base) (`helm install base manifests/charts/base` from the repository root). Keep the namespace values in sync: `fastSandbox.namespaces.*` in base vs `systemNamespace` / `resourceNamespace` here.
- Install this chart **before the lifecycle server** when the server will serve sandboxes through this runtime: the server's `[runtime]`/fsb configuration points at the FastPath gRPC endpoint (`fast-sandbox-fastpath` Service) created here.
- The companion images built from the source pinned in [`manifests/third-party/fast-sandbox.commit`](../third-party/fast-sandbox.commit):

  ```bash
  manifests/release/build-fast-sandbox.sh --load-kind <kind-cluster>
  ```

- Nodes that should serve Firecracker sandboxes need bare-metal KVM (`/dev/kvm`). There is NO manual labeling step: the firecracker-runtime readiness loop verifies each host, installs the Firecracker assets, and applies the `sandbox.fast.io/kvm` + `fast-sandbox.io/firecracker-node` labels plus the `FirecrackerReady` condition itself (rechecking every 5 minutes).

- The agent registry Secret with artifact-store pull credentials (compiled `registry.json`):

  ```bash
  kubectl -n opensandbox-system create secret generic fast-sandbox-agent-registry \
    --from-file=registry.json=<compiled-registry.json>
  ```

## Installing the Chart

```bash
helm install fast-sandbox manifests/charts/fast-sandbox
```

Or as part of the umbrella chart:

```bash
helm install opensandbox manifests/charts/opensandbox \
  --set fast-sandbox.enabled=true
```

## Parameters

The following table lists the configurable parameters of the chart and their default values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| artifactStore.endpoint | string | `""` | S3-compatible endpoint (empty = AWS default) |
| artifactStore.store | string | `"s3://sandbox-images/publish"` | Store URI root for published artifacts (golden images, snapshots) |
| controller.enabled | bool | `true` | Whether the control plane Deployment + FastPath Service are installed |
| controller.fastletProxyImage | string | `"fast-sandbox/fastlet-proxy:dev"` | Image injected as the platform-owned Fastlet Proxy sidecar into fastlet Pods. Override for clusters that cannot pull from docker.io. |
| controller.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| controller.image.repository | string | `"opensandbox/fsb-controller"` | Controller image repository (built by manifests/release/build-fast-sandbox.sh) |
| controller.image.tag | string | `""` | Image tag. Empty uses the release-<appVersion> companion image tag. |
| controller.replicaCount | int | `1` | Number of controller replicas (no leader election; keep 1) |
| controller.resources | object | `{"limits":{"cpu":"1","memory":"512Mi"},"requests":{"cpu":"100m","memory":"128Mi"}}` | Resource requests and limits for the controller |
| controller.sandboxtemplateBuilderImage | string | `""` | Image that executes SandboxTemplate golden-image builds (builder Pods are created by the controller; build it with manifests/release/build-fast-sandbox.sh). Empty uses the release-<appVersion> companion image tag. |
| controller.sandboxtemplateBuilderPodSpec | string | `""` | Raw PodSpec fragment (YAML) merged into every SandboxTemplate build Pod by the controller: whitelisted scheduling fields only (tolerations appended; affinity and topologySpreadConstraints replaced). Rendered as the fast-sandbox-builder-pod-template ConfigMap; editing the live ConfigMap applies to the next build without a rollout. |
| controller.tolerations | list | `[]` | Tolerations for the controller pod |
| fullnameOverride | string | `""` | Override the full name of the chart |
| imagePullSecrets | list | `[]` | Image pull secrets for every workload in this chart |
| janitor.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| janitor.image.repository | string | `"opensandbox/fsb-janitor"` | Janitor image repository (built by manifests/release/build-fast-sandbox.sh) |
| janitor.image.tag | string | `""` | Image tag. Empty uses the release-<appVersion> companion image tag. |
| janitor.orphanTimeout | string | `"30s"` | Orphan timeout before cleanup |
| janitor.scanInterval | string | `"2m"` | Orphan scan interval |
| nameOverride | string | `""` | Override the name of the chart |
| resourceNamespace | string | `"opensandbox-dataplane"` | Namespace for fast-sandbox resource objects (SandboxPools, Templates, Sandboxes and the fastlet/builder Pods they spawn). Must match base.fastSandbox.namespaces.resources. |
| routeKeys.create | bool | `true` | Specifies whether the fast-sandbox-route-keys Secret is created here |
| routeKeys.developmentOnly | bool | `true` | Mark the Secret with fast-sandbox.io/development-only (set false when provisioning real keys via privateKey/publicKey) |
| routeKeys.existingSecret | string | `""` | Use an existing Secret instead of creating one (its keys must be private-key / public-key) |
| routeKeys.privateKey | string | `"nWGxne/9WmC6hEr0kuwsxERJxWl7MmkZcDusAxyuf2A="` | Ed25519 private key (base64) used by the controller's route signer |
| routeKeys.publicKey | string | `"11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo="` | Ed25519 public key (base64) used by the controller's route verifier |
| runtime.config | string | `""` | Agent config (agent.yaml). Empty = the chart default (upstream config/dev/agent-config.yaml with the dart discover URL pointing at this chart's namespace). socket/stateRoot/registryConfig/p2p are startup-only; the nodeReadiness section (fcVersion, kernelURL, interval, minFree, minMemory) hot-reloads on every readiness pass. |
| runtime.dartPeerPort | int | `9000` | DART P2P peer listen port (also the headless dart Service port) |
| runtime.enabled | bool | `true` | Whether the firecracker-runtime DaemonSet, its RBAC, the agent config ConfigMap and the dart headless Service are installed |
| runtime.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| runtime.image.repository | string | `"opensandbox/fsb-firecracker-runtime"` | Runtime image repository (built by manifests/release/build-fast-sandbox.sh) |
| runtime.image.tag | string | `""` | Image tag. Empty uses the release-<appVersion> companion image tag. |
| runtime.nodeSelector | object | `{}` | Node selector. Empty by default: the runtime applies the firecracker scheduling labels itself, so it must run on every candidate node. Pin it with your own coarse selector only if the cluster hosts unrelated node pools. |
| runtime.registrySecret | string | `"fast-sandbox-agent-registry"` | Secret carrying the compiled agent registry configuration (registry.json key with artifact-store pull credentials); must be provisioned by the operator. |
| runtime.resources | object | `{"agent":{},"janitor":{}}` | Container resources for the DaemonSet. Setting requests matters more than limits here: without them the pod is BestEffort and is evicted first under node pressure, taking down the management API for every fastlet on the node. Limits stay off by default so artifact pulls and the readiness loop are not throttled or OOM-killed at their spikes. |
| runtime.resources.agent | object | `{}` | Resources for the firecracker-runtime agent container. |
| runtime.resources.janitor | object | `{}` | Resources for the janitor sidecar container. |
| runtime.socketDir | string | `"/run/fast-sandbox/firecracker"` | Node hostPath sharing the agent UDS socket with fastlet Pods |
| runtime.stateRoot | string | `"/var/lib/fast-sandbox/firecracker"` | Node hostPath holding per-node Firecracker state (rootfs, snapshots). Each node needs its own directory; do not share across nodes. |
| runtime.tolerations | list | `[]` | Additional tolerations for the firecracker-runtime DaemonSet pod, appended to the built-in control-plane toleration. Needed when target nodes carry infra taints (e.g. sigma.ali resource-pool taints). |
| runtimeEnvironments | string | `"version: v1alpha2\nenvironments:\n  default:\n    containerd:\n      socket: /run/containerd/containerd.sock\n      namespace: k8s.io\n      defaultSnapshotter: overlayfs\n      root: /var/lib/containerd\n    kubelet:\n      root: /var/lib/kubelet\n    runtimes:\n      container: {}\n      gvisor: {}\n      kata-qemu: {}\n      kata-clh: {}\n      kata-fc:\n        snapshotter: blockfile\n        configPath: /opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml\n      kata-dragonball:\n        configPath: /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml\n      boxlite: {}\n      firecracker:\n        firecracker:\n          binaryPath: /opt/fast-sandbox/firecracker/firecracker\n          jailerPath: /opt/fast-sandbox/firecracker/jailer\n          kernelPath: /opt/fast-sandbox/firecracker/vmlinux.bin\n          rootfsPath: /var/lib/fast-sandbox/firecracker/rootfs\n          stateRoot: /var/lib/fast-sandbox/firecracker"` |  |
| systemNamespace | string | `"opensandbox-system"` | Namespace for the fast-sandbox control plane workloads (the shared OpenSandbox system namespace). Must match base.fastSandbox.namespaces.system (where the ServiceAccounts live). |

## Signing keys: two independent systems

There are two distinct key systems in a fast-sandbox-on-OpenSandbox deployment; do not mix them:

1. **fast-sandbox controller route keys (Ed25519)** — the `fast-sandbox-route-keys` Secret this chart creates (consumed by the controller as `FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY` / `FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY`). By default it holds the published development-only test keys (labeled `fast-sandbox.io/development-only: "true"`). For production, either provision the Secret yourself and set `routeKeys.existingSecret`, or set `routeKeys.privateKey` / `routeKeys.publicKey` with `routeKeys.developmentOnly=false`.
2. **OpenSandbox f1.* route-scope key ring (HMAC-SHA256)** — the server signs sandbox endpoint scopes with its `[ingress.secure_access]` key (`server.gateway.secureAccess` in charts/server) and the ingress gateway verifies them with the same symmetric ring (`--secure-access-keys`, `gateway.secureAccess` in charts/ingress-gateway). An operator must configure this ring on both charts with matching key material; the controller's Ed25519 public key must NOT be used here.

## Uninstalling the Chart

```bash
helm delete fast-sandbox
```

The CRDs and component RBAC are owned by the base release and are not affected (CRDs carry the `helm.sh/resource-policy: keep` annotation, so they must be deleted manually afterwards).

## License

Apache 2.0 License
