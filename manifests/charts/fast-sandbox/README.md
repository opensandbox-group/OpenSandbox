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
- OverlayBD OCI on-demand loading (`runtime.oci.enabled=true`) additionally needs kernel ublk support on every runtime node (`CONFIG_BLK_DEV_UBLK`, `/dev/ublkc*`; `modprobe ublk_drv` when built as a module) and an OCI registry reachable from the cluster: SandboxTemplates publish `oci://` artifacts there and nodes read blocks on demand. The SandboxPool's fastlet containers must set `FAST_SANDBOX_OCI_ENABLED=true` separately — pools are not managed by this chart. See the fast-sandbox [OverlayBD OCI guide](https://github.com/opensandbox-group/fast-sandbox/blob/main/docs/guides/overlaybd-oci-sandboxes.md) for the credential (registry Secret + `fast-sandbox-registry` ConfigMap) and StateRoot-sharing requirements.
- With `runtime.oci.enabled=true`, this chart enables the ublk warm pool at low/high watermarks of 4/8 with 2 GiB devices, matching the OpenSandbox template API's default disk capacity. Configure `runtime.oci.poolSizeGB` for templates built at another capacity; see the matching requirements below. Standalone fast-sandbox keeps pooling disabled by default.

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

## OCI warm pool and template capacity

The warm pool holds idle writable ublk devices on each node. `poolLow=4` is the target idle reserve; `poolHigh=8` is the maximum retained on release, not a limit on running Sandboxes. Setting `runtime.oci.poolLow=0` disables pooling. The pool is separate from SandboxPool fastlet capacity, template prewarming, and the OverlayBD block cache.

**`runtime.oci.poolSizeGB` must exactly match the built template's rootfs virtual capacity.** Despite the parameter name, the unit is GiB (1 GiB = 2^30 bytes). It is not the compressed image size, used disk space, or `runtime.oci.cacheSizeGB`. A larger pool device cannot serve a smaller template; mismatches fall back to cold device creation. Writable mode and block size must also match.

For OpenSandbox template builds, `resourceLimits.disk` is projected to `SandboxTemplate.spec.output.rootfsSize` and passed to `fsb-vm build --disk-size-gb`. When disk is omitted, the OpenSandbox API explicitly chooses `2Gi`, so this chart defaults to `poolSizeGB: 2`. This differs from the standalone fast-sandbox CRD's `30Gi` default and the standalone `fsb-vm build` default of 20 GiB. The integration CI explicitly builds `disk=2Gi` templates and sets the pool to 2 GiB as well.

Check `machine.rootfsSizeMiB` in the built snapfiles artifact's `manifest.json`, since the pool matches the actual built rootfs. For whole-GiB capacities, `poolSizeGB = rootfsSizeMiB / 1024`; for example, 61440 MiB requires `60` and 204800 MiB requires `200`. Update the pool configuration whenever template capacity changes. Helm does not discover templates or derive this value automatically.

For a template built with `resourceLimits.disk: 60Gi`, use:

```yaml
runtime:
  oci:
    enabled: true
    poolLow: 4
    poolHigh: 8
    poolSizeGB: 60
```

For a `200Gi` template, set `poolSizeGB: 200`. These are values for the standalone fast-sandbox chart; under the OpenSandbox umbrella chart, nest them beneath `fast-sandbox` (for example, `fast-sandbox.runtime.oci.poolSizeGB`).

Each upstream daemon supports only one pool capacity. A single daemon cannot accelerate both 60 GiB and 200 GiB templates with one setting. To accelerate both, use separate node groups with matching daemon configurations, or implement separate daemons and capacity-based routing for the same node; the current chart/runtime does not provide that routing. After deployment, inspect `/v1/pool` through the node daemon's Unix socket and check `enabled`, `size_gb`, `idle`, and the changes in `hits`/`misses` during creates.

## Parameters

The following table lists the configurable parameters of the chart and their default values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| artifactStore.endpoint | string | `""` | S3-compatible endpoint (empty = AWS default) |
| artifactStore.store | string | `"s3://sandbox-images/publish"` | Store URI root for published artifacts (golden images, snapshots) |
| controller.enabled | bool | `true` | Whether the control plane Deployment + FastPath Service are installed |
| controller.fastletProxyImage | string | `"opensandbox/fsb-fastlet-proxy:release-1.2.1-rc.1"` | Image injected as the platform-owned Fastlet Proxy sidecar into fastlet Pods. Defaults to the released companion image published by the umbrella release (build-fast-sandbox.sh); override for clusters that cannot pull from docker.io. |
| controller.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| controller.image.repository | string | `"opensandbox/fsb-controller"` | Controller image repository (built by manifests/release/build-fast-sandbox.sh) |
| controller.image.tag | string | `""` | Image tag. Empty uses the release-<appVersion> companion image tag. |
| controller.kubeClientBurst | int | `0` | API-server client burst budget for the rate limiter (upstream "--kube-client-burst" flag). Must be set together with kubeClientQPS; 0 keeps the client-go default. |
| controller.kubeClientQPS | int | `0` | API-server client rate limit in queries per second for the shared controller client (upstream "--kube-client-qps" flag; speeds up the initial cache-sync LISTs on large clusters; watches are exempt). Must be set together with kubeClientBurst; 0 keeps the client-go default. |
| controller.namespaces | list | `[]` | Namespaces the controller informer cache watches; empty watches the whole cluster (upstream "--namespaces" flag, introduced at the f44561a pin). When set, the list must cover every namespace holding SandboxPool/Sandbox/SandboxTemplate objects plus systemNamespace (the runtime environment ConfigMap); the controller refuses to start otherwise. Keep empty for multi-tenant deployments where sandboxes span many namespaces (the OpenSandbox default); scope it only when this control plane shares a large cluster with unrelated workloads and the cluster-wide initial cache sync is too heavy. |
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
| runtime.config | string | `""` | Agent config (agent.yaml). Empty = the chart default (upstream config/dev/agent-config.yaml with the dart discover URL pointing at this chart's namespace). socket/stateRoot/registryConfig/p2p are startup-only; the nodeReadiness section (fcVersion, interval, minFree, minMemory) hot-reloads on every readiness pass. |
| runtime.dartPeerPort | int | `9000` | DART P2P peer listen port (also the headless dart Service port) |
| runtime.enabled | bool | `true` | Whether the firecracker-runtime DaemonSet, its RBAC, the agent config ConfigMap and the dart headless Service are installed |
| runtime.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| runtime.image.repository | string | `"opensandbox/fsb-firecracker-runtime"` | Runtime image repository (built by manifests/release/build-fast-sandbox.sh) |
| runtime.image.tag | string | `""` | Image tag. Empty uses the release-<appVersion> companion image tag. |
| runtime.nodeSelector | object | `{}` | Node selector. Empty by default: the runtime applies the firecracker scheduling labels itself, so it must run on every candidate node. Pin it with your own coarse selector only if the cluster hosts unrelated node pools. |
| runtime.oci | object | `{"cacheSizeGB":16,"enabled":false,"poolHigh":8,"poolLow":4,"poolSizeGB":2}` | OverlayBD OCI on-demand loading: installs the fsb-vm + OverlayBD tools bundle (init container), sets FAST_SANDBOX_OCI_ENABLED on the agent, and runs one overlaybd-ublkd sidecar per node. Nodes need KVM plus kernel ublk support. Pool fastlets set the flag separately (pools are not managed by this chart). |
| runtime.oci.cacheSizeGB | int | `16` | Shared overlaybd block cache capacity (OVERLAYBD_CACHE_SIZE_GB). |
| runtime.oci.enabled | bool | `false` | Whether the node OCI stack (tools init + overlaybd-ublkd sidecar + agent flag + janitor oci-state-root) is installed |
| runtime.oci.poolHigh | int | `8` | Maximum idle ublk devices retained on release; not a Sandbox count limit. Must be >= poolLow. |
| runtime.oci.poolLow | int | `4` | Target idle ublk device count when OCI is enabled. Set to 0 to disable pooling. |
| runtime.oci.poolSizeGB | int | `2` | Pool device capacity in GiB (OVERLAYBD_POOL_SIZE_GB). Default 2 matches the OpenSandbox template API's default resourceLimits.disk=2Gi. Must exactly match the built template rootfs capacity; a mismatch falls back to cold device creation. For 60Gi/200Gi templates set 60/200, respectively; a 200Gi pool cannot serve a 60Gi template. One daemon supports one capacity. The standalone fast-sandbox CRD default (30Gi) and fsb-vm build default (20Gi) differ. Check machine.rootfsSizeMiB in the snapfiles artifact's manifest.json: for whole-GiB templates, poolSizeGB = rootfsSizeMiB / 1024. This is unrelated to image layer size, disk usage or runtime.oci.cacheSizeGB, and is not inferred by Helm. |
| runtime.registrySecret | string | `"fast-sandbox-agent-registry"` | Secret carrying the compiled agent registry configuration (registry.json key with artifact-store pull credentials); must be provisioned by the operator. |
| runtime.resources | object | `{"agent":{},"janitor":{},"ublkd":{}}` | Container resources for the DaemonSet. Setting requests matters more than limits here: without them the pod is BestEffort and is evicted first under node pressure, taking down the management API for every fastlet on the node. Limits stay off by default so artifact pulls and the readiness loop are not throttled or OOM-killed at their spikes. |
| runtime.resources.agent | object | `{}` | Resources for the firecracker-runtime agent container. |
| runtime.resources.janitor | object | `{}` | Resources for the janitor sidecar container. |
| runtime.resources.ublkd | object | `{}` | Resources for the overlaybd-ublkd sidecar container (runtime.oci.enabled only). |
| runtime.socketDir | string | `"/run/fast-sandbox/firecracker"` | Node hostPath sharing the agent UDS socket with fastlet Pods |
| runtime.stateRoot | string | `"/var/lib/fast-sandbox/firecracker"` | Node hostPath holding per-node Firecracker state (rootfs, snapshots). Each node needs its own directory; do not share across nodes. |
| runtime.tolerations | list | `[]` | Additional tolerations for the firecracker-runtime DaemonSet pod, appended to the built-in control-plane toleration. Needed when target nodes carry infra taints (e.g. sigma.ali resource-pool taints). |
| runtimeEnvironments | string | `"version: v1alpha2\nenvironments:\n  default:\n    containerd:\n      socket: /run/containerd/containerd.sock\n      namespace: k8s.io\n      defaultSnapshotter: overlayfs\n      root: /var/lib/containerd\n    kubelet:\n      root: /var/lib/kubelet\n    runtimes:\n      container: {}\n      gvisor: {}\n      kata-qemu: {}\n      kata-clh: {}\n      kata-fc:\n        snapshotter: blockfile\n        configPath: /opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml\n      kata-dragonball:\n        configPath: /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml\n      boxlite: {}\n      firecracker:\n        firecracker:\n          binaryPath: /opt/fast-sandbox/firecracker/firecracker\n          jailerPath: /opt/fast-sandbox/firecracker/jailer\n          # No kernelPath: restore never boots a kernel (the bundled\n          # vmlinux.bin was dropped upstream); direct-boot operators\n          # pin one here explicitly.\n          rootfsPath: /var/lib/fast-sandbox/firecracker/rootfs\n          stateRoot: /var/lib/fast-sandbox/firecracker"` |  |
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
