# OpenSandbox Helm Deployment

This document describes how to deploy OpenSandbox with Helm: the cluster
foundation (CRDs), the controller, the lifecycle server, and the optional
ingress gateway, node agent, and fast-sandbox runtime.

> **Where charts come from.** Charts are versioned sources in this repository.
> They are not published as standalone `.tgz` packages — the release flow is
> umbrella-based (see [Release Process](#release-process-maintainers)). Install
> from a checkout of the version you want. The legacy per-component
> `helm/{component}/{version}` chart releases are frozen at `0.2.x` and no
> longer produced.

## Prerequisites

- Kubernetes 1.21.1+ (matching the charts' `kubeVersion`)
- Helm 3.0+
- kubectl configured and able to access the target cluster

## Quick Start

Check out the version you want to deploy (use the latest `release-X.Y.Z` tag,
or `main` for development), then install.

### Option 1: Umbrella Chart (Recommended)

Everything in one release — CRDs, controller, server, and optional components:

```bash
cd manifests/charts

# Package the sub-charts (charts/ is git-ignored, rebuilt every time)
helm dependency build opensandbox

# Install all components
helm install opensandbox opensandbox \
  --namespace opensandbox-system \
  --create-namespace
```

Optional components default to off; enable what you need:

```bash
helm install opensandbox opensandbox \
  --namespace opensandbox-system \
  --create-namespace \
  --set ingress-gateway.enabled=true \
  --set fast-sandbox.enabled=true
```

To announce the gateway from the server, also set `opensandbox-server.server.gateway.*`
(see [Ingress Gateway](#ingress-gateway) below).

### Option 2: Per-Component Releases

Install the foundation first, then the components you need:

```bash
# 1. Cluster-scoped resources: OpenSandbox CRDs, fast-sandbox CRDs, RBAC
helm install base manifests/charts/base

# 2. Controller (control plane)
helm install opensandbox-controller manifests/charts/controller \
  --namespace opensandbox-system \
  --create-namespace

# 3. Optional: fast-sandbox runtime — install BEFORE the server when the
#    server will serve sandboxes through the fsb runtime (its [runtime]
#    config points at the FastPath endpoint this chart creates)
helm install fast-sandbox manifests/charts/fast-sandbox

# 4. Lifecycle API server (optional but typical)
helm install opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --create-namespace

# 5. Optional: ingress gateway and node agent
helm install ingress-gateway manifests/charts/ingress-gateway \
  --namespace opensandbox-system
helm install opensandbox-node-agent manifests/charts/node-agent \
  --namespace opensandbox-system
```

### Using Custom Images

Every chart defaults to the images published for its `appVersion`
(`release-<appVersion>` tags). To use your own registry:

```bash
helm install opensandbox-controller manifests/charts/controller \
  --set controller.image.repository=<your-registry>/controller \
  --set controller.image.tag=<your-tag> \
  --namespace opensandbox-system \
  --create-namespace
```

If you build images from source:

```bash
cd kubernetes
COMPONENT=controller TAG=<your-tag> ./build.sh
COMPONENT=task-executor TAG=<your-tag> ./build.sh
```

Or via the Makefile (VERSION must be plain semver; the chart adds the `v`
prefix for image tags automatically):

```bash
cd manifests
make helm-install IMAGE_TAG_BASE=<your-registry>/controller VERSION=0.0.1
```

### Verify Installation

```bash
# Check Pod status
kubectl get pods -n opensandbox-system

# Check CRDs (three from the base chart)
kubectl get crd | grep opensandbox

# View installation status and chart version
helm status opensandbox -n opensandbox-system
helm list -n opensandbox-system
```

## Version Management

- Umbrella releases are tagged `release-X.Y.Z` in this repository (plus a Go
  SDK companion tag `sdks/sandbox/go/vX.Y.Z` on the same commit). Check out the
  tag to deploy that exact version.
- Chart `version` and image `appVersion` move together per umbrella release;
  individual charts are not released separately.
- To see available versions: `git tag -l 'release-*'` or the
  [releases page](https://github.com/opensandbox-group/OpenSandbox/releases).

### Upgrade to a Specific Version

```bash
git fetch --tags
git checkout release-1.2.0

cd manifests/charts
helm dependency build opensandbox
helm upgrade opensandbox opensandbox --namespace opensandbox-system
```

## Custom Configuration

### Using a Custom Values File

Create a custom values file `custom-values.yaml`:

```yaml
opensandbox-controller:
  controller:
    image:
      repository: myregistry.example.com/opensandbox-controller
      tag: v0.1.0

    resources:
      limits:
        cpu: 1000m
        memory: 512Mi
      requests:
        cpu: 100m
        memory: 128Mi

    logLevel: debug

    snapshot:
      registry: myregistry.example.com/opensandbox/snapshots
      snapshotPushSecret: registry-snapshot-push-secret
      imageCommitterPullSecret: registry-image-committer-pull-secret
      resumePullSecret: registry-pull-secret

imagePullSecrets:
  - name: myregistrykey
```

Install with custom configuration:

```bash
helm install opensandbox opensandbox -f custom-values.yaml \
  --namespace opensandbox-system \
  --create-namespace
```

Per-chart equivalents use the unprefixed keys (e.g. `controller.image.*` with
`manifests/charts/controller`).

### Common Configuration Examples

#### 1. Adjust Resource Configuration

```bash
helm install opensandbox-controller manifests/charts/controller \
  --set controller.resources.limits.cpu=1000m \
  --set controller.resources.limits.memory=512Mi \
  --namespace opensandbox-system
```

#### 2. Configure Node Affinity

Create `affinity-values.yaml`:

```yaml
controller:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/control-plane
            operator: Exists
```

```bash
helm install opensandbox-controller manifests/charts/controller \
  -f affinity-values.yaml \
  --namespace opensandbox-system
```

#### 3. Configure Pause/Resume

```bash
helm install opensandbox-controller manifests/charts/controller \
  --set controller.snapshot.registry=myregistry.example.com/opensandbox/snapshots \
  --set controller.snapshot.snapshotPushSecret=registry-snapshot-push-secret \
  --set controller.snapshot.imageCommitterPullSecret=registry-image-committer-pull-secret \
  --set controller.snapshot.resumePullSecret=registry-pull-secret \
  --namespace opensandbox-system
```

#### 4. Configure the Ingress Gateway

```bash
helm install ingress-gateway manifests/charts/ingress-gateway \
  --namespace opensandbox-system \
  --set gateway.service.type=LoadBalancer
```

## Upgrade

Upgrade from a checked-out version:

```bash
git checkout release-1.2.0
cd manifests/charts && helm dependency build opensandbox
helm upgrade opensandbox opensandbox -n opensandbox-system
```

Upgrade from a local chart with a new image:

```bash
helm upgrade opensandbox-controller manifests/charts/controller \
  --set controller.image.tag=v0.0.2 \
  --namespace opensandbox-system
```

Or using Makefile:

```bash
make helm-upgrade VERSION=0.0.2
```

### View Upgrade History

```bash
helm history opensandbox -n opensandbox-system
```

### Rollback

```bash
# Rollback to the previous revision
helm rollback opensandbox -n opensandbox-system

# Rollback to a specific revision
helm rollback opensandbox 1 -n opensandbox-system
```

## Uninstall

```bash
helm uninstall opensandbox -n opensandbox-system
```

Or using Makefile:

```bash
make helm-uninstall
```

**Note**: By default, CRDs are retained (`helm.sh/resource-policy: keep`). To
delete them:

```bash
kubectl delete crd batchsandboxes.sandbox.opensandbox.io
kubectl delete crd pools.sandbox.opensandbox.io
kubectl delete crd sandboxsnapshots.sandbox.opensandbox.io
```

The `opensandbox-dataplane` namespace created by the base chart is also kept on
uninstall; delete it manually once its data is no longer needed.

### Clean Up Namespace

To completely clean up:

```bash
kubectl delete namespace opensandbox-system
kubectl delete namespace opensandbox-dataplane   # fast-sandbox dataplane, if used
```

## Makefile Commands

The `manifests/Makefile` provides targets to simplify Helm operations:

```bash
# Lint all Helm charts and verify umbrella dependencies
make helm-lint

# Generate Kubernetes manifests (controller chart) without installing
make helm-template

# Generate manifests with debug output
make helm-template-debug

# Package a chart (chart version comes from its Chart.yaml)
make helm-package

# Install base + controller
make helm-install

# Upgrade the controller release
make helm-upgrade

# Uninstall the controller release
make helm-uninstall

# Perform a dry-run install
make helm-dry-run

# Regenerate chart READMEs (requires helm-docs)
make helm-docs

# Run lint + package
make helm-all
```

## Verify Deployment

### 1. Check Controller Status

```bash
kubectl get deployment -n opensandbox-system
kubectl get pods -n opensandbox-system
kubectl logs -n opensandbox-system -l control-plane=controller-manager -f
```

### 2. Verify CRDs

```bash
kubectl get crd batchsandboxes.sandbox.opensandbox.io -o yaml
kubectl get crd pools.sandbox.opensandbox.io -o yaml
kubectl get crd sandboxsnapshots.sandbox.opensandbox.io -o yaml
```

### 3. Create Test Resources

From the repository root:

```bash
# Create a Pool
kubectl apply -f kubernetes/config/samples/sandbox_v1alpha1_pool.yaml

# Create a BatchSandbox
kubectl apply -f kubernetes/config/samples/sandbox_v1alpha1_batchsandbox.yaml

# View status
kubectl get pools -n opensandbox-system
kubectl get batchsandboxes -n opensandbox-system
```

## Troubleshooting

### Chart Validation Failure

```bash
# Lint the Chart
make helm-lint

# View detailed template output
make helm-template-debug
```

### Controller Fails to Start

```bash
# View Pod status
kubectl describe pod -n opensandbox-system -l control-plane=controller-manager

# View logs
kubectl logs -n opensandbox-system -l control-plane=controller-manager

# Check RBAC permissions
kubectl auth can-i --as=system:serviceaccount:opensandbox-system:opensandbox-controller-manager create pods
```

### Image Pull Failure

```bash
# Check image configuration
helm get values opensandbox-controller -n opensandbox-system

# Add an image pull secret
kubectl create secret docker-registry myregistrykey \
  --docker-server=<your-registry> \
  --docker-username=<username> \
  --docker-password=<password> \
  -n opensandbox-system

# Reinstall with the secret
helm upgrade opensandbox-controller manifests/charts/controller \
  --set imagePullSecrets[0].name=myregistrykey \
  --namespace opensandbox-system
```

## Ingress Gateway

The ingress gateway (`components/ingress`) proxies sandbox traffic and is
deployed by its own chart. The lifecycle server only *announces* it: set
`server.gateway.enabled=true` (chart `server`) so the server returns the
gateway address to clients.

```bash
helm install ingress-gateway manifests/charts/ingress-gateway \
  --namespace opensandbox-system

helm install opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --set server.gateway.enabled=true \
  --set server.gateway.host=gateway.example.com
```

Keep `server.gateway.gatewayRouteMode` in sync with `gateway.gatewayRouteMode`
of the ingress-gateway chart.

### Secure-Access Keys (OSEP-0011)

For signed, expiring sandbox routes, the server signs route tokens and the
gateway verifies them with the **same symmetric key ring**. Provide the ring to
both charts, either inline (plaintext in values — dev only):

```bash
--set server.gateway.secureAccess.activeKey=a \
--set 'server.gateway.secureAccess.keys[0].key_id=a' \
--set 'server.gateway.secureAccess.keys[0].key=<base64-secret>'
```

or from an existing Secret (`secureAccess.existingSecret`) with two entries:
`keys` (`a=<base64-secret>[,b=...]`) and `active-key` (`a`). The charts wire it
in as environment variables so key material never appears in values, the
server ConfigMap, or pod args. The two forms are mutually exclusive; after
rotating the Secret, `kubectl rollout restart` the server Deployment.

## fast-sandbox Runtime (Firecracker)

The optional `fast-sandbox` chart deploys the fast-sandbox Firecracker chain
(`sandbox.fast.io`): the all-in-one control plane (reconcilers + FastPath
gRPC), the janitor, and the node-side firecracker runtime (UDS management
API, DART P2P delivery, janitor sidecar, and the node readiness loop that
installs the Firecracker assets and self-labels the nodes). Only
Firecracker is covered; boxlite and other non-Firecracker runtimes are out
of scope, and the upstream central sandbox-proxy is not deployed
(OpenSandbox reaches fastlets through the ingress gateway's direct route
resolution).

The CRDs (`sandbox.fast.io`) and the component RBAC ship in the `base` chart
(gated by `fastSandbox.*` values), so install `base` first.

### 1. Build the companion images from the pinned source

The upstream source is pinned Git-LFS-pointer style in
[`manifests/third-party/fast-sandbox.commit`](third-party/fast-sandbox.commit)
(repo + commit). The build script materializes a checkout of exactly that
commit and builds the six Firecracker-scope images (controller, fastlet,
fastlet-proxy, janitor, firecracker-runtime,
sandboxtemplate-builder):

```bash
# Build all images; --load-kind also pushes them into a kind cluster
manifests/release/build-fast-sandbox.sh --load-kind <kind-cluster>

# List the image refs that would be built
manifests/release/build-fast-sandbox.sh --list-images
```

`REGISTRY` / `TAG` environment variables override the default
`fast-sandbox/<component>:dev` refs — keep the chart `image.*` values in
sync when you override them. To publish, `--push` follows the
`components/*/build.sh` convention: bare `--push` retags and pushes to
`docker.io/opensandbox` and
`sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox` (plus
`$GHCR_REPO/<component>` when `GHCR_REPO` is set), `--push r1[,r2...]`
pushes to exactly the listed registries, and a `v*` `TAG` additionally
pushes `:latest`. Images are linux/amd64 only.

### 2. Prepare the cluster

Prepare each node's `runtime.stateRoot` (`fast-sandbox.runtime.stateRoot` in the
umbrella chart) on a filesystem with reflink
support before deploying the runtime. See the
[state-disk operations guide](../docs/architecture/fast-sandbox/storage.md#prepare-the-node-state-disk)
for dedicated-disk setup, the loop-backed XFS helper, sizing, and readiness checks.

```bash
# Nodes need bare-metal KVM (/dev/kvm); no manual labeling — the
# firecracker-runtime readiness loop verifies each host, installs the
# Firecracker assets, and applies sandbox.fast.io/kvm +
# fast-sandbox.io/firecracker-node itself.

# Provision the agent registry Secret (artifact-store pull credentials,
# compiled registry.json)
kubectl -n opensandbox-system create secret generic fast-sandbox-agent-registry \
  --from-file=registry.json=<compiled-registry.json>
```

### 3. Install

Install `base` first, then this chart — and both **before the lifecycle
server** when the server will serve sandboxes through this runtime (its
`[runtime]`/fsb configuration points at the FastPath endpoint created here):

```bash
helm install base manifests/charts/base          # sandbox.fast.io CRDs + RBAC
helm install fast-sandbox manifests/charts/fast-sandbox
```

Or through the umbrella chart:

```bash
helm install opensandbox manifests/charts/opensandbox \
  --set fast-sandbox.enabled=true
```

Two key systems apply (do not conflate them):

- The fast-sandbox controller's Ed25519 route keys default to the published
  development-only test keys (`fast-sandbox.io/development-only: "true"`
  label). For production, set `routeKeys.existingSecret` or
  `routeKeys.privateKey` / `routeKeys.publicKey` with
  `routeKeys.developmentOnly=false`.
- The OpenSandbox f1.* route-scope ring is HMAC-SHA256 and independent: the
  server signs with `[ingress.secure_access]` (`server.gateway.secureAccess`
  in charts/server) and the ingress gateway verifies with the same symmetric
  ring (`gateway.secureAccess` in charts/ingress-gateway). Configure both
  with matching key material for production.

Point the OpenSandbox server's
`[runtime]`/fsb configuration and the ingress gateway's
`--provider-type=fast-sandbox` at the deployed FastPath endpoint to
serve sandboxes through this runtime (see
`scripts/fast-sandbox-env` for a working reference).

### Bumping the pinned fast-sandbox commit

```bash
# 1. Update the commit line in manifests/third-party/fast-sandbox.commit
# 2. Re-sync the vendored CRDs (byte-identical to the pinned checkout)
manifests/release/build-fast-sandbox.sh --no-build --sync-crds   # or: make -C manifests helm-gen-fast-sandbox-crds
# 3. Rebuild the images and redeploy
manifests/release/build-fast-sandbox.sh --load-kind <kind-cluster>
helm upgrade fast-sandbox manifests/charts/fast-sandbox
```

## Advanced Configuration

### Multi-Environment Deployment

Create dedicated values files for different environments:

#### values-dev.yaml
```yaml
opensandbox-controller:
  controller:
    logLevel: debug
    resources:
      limits:
        cpu: 200m
        memory: 128Mi
```

#### values-prod.yaml
```yaml
opensandbox-controller:
  controller:
    logLevel: warn
    replicaCount: 3
    resources:
      limits:
        cpu: 1000m
        memory: 512Mi
    affinity:
      podAntiAffinity:
        requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchExpressions:
            - key: control-plane
              operator: In
              values:
              - controller-manager
          topologyKey: kubernetes.io/hostname
```

Deploy to different environments:

```bash
# Development environment
helm install opensandbox-dev manifests/charts/opensandbox \
  -f values-dev.yaml \
  --namespace opensandbox-dev \
  --create-namespace

# Production environment
helm install opensandbox-prod manifests/charts/opensandbox \
  -f values-prod.yaml \
  --namespace opensandbox-prod \
  --create-namespace
```

> Note: the controller chart currently uses fixed resource names (see the
> `controller` chart README). Running multiple controller releases in one
> cluster is not supported; separate **clusters** per environment.

## Release Process (Maintainers)

OpenSandbox releases are umbrella releases: one version, one tag family for
the whole platform (see `docs/community/release-automation.md`).

1. Prepare the release branch:

   ```bash
   # One-shot version bump: chart versions/appVersions, image tags, SDK
   # versions, Chart.lock, chart READMEs — then commits the result
   manifests/release/create-umbrella-release.sh --version X.Y.Z --bump-only
   ```

2. Write the hand-authored release notes at `docs/releases/X.Y.Z.md` and
   commit them on the release branch.
3. Cut the release:

   ```bash
   manifests/release/create-umbrella-release.sh --version X.Y.Z --push --release
   ```

   This verifies version consistency, renders the BOM
   (`docs/releases/X.Y.Z.yaml`), and mints the `release-X.Y.Z` tag plus the Go
   companion tag `sdks/sandbox/go/vX.Y.Z` on the BOM commit.
4. CI (`.github/workflows/release-umbrella.yml`) builds and pushes
   `opensandbox/<component>:release-X.Y.Z` images, pins their digests into the
   BOM, and publishes the GitHub Release. `rc` versions (`X.Y.Z-rc.N`) follow
   the same flow with the `-rc` template; SDK artifacts only move at stable
   releases.

Pull requests that touch the umbrella chart, release workflows, release-smoke
scripts, or Python lifecycle clients are gated by the **Helm Release Smoke**
workflow (`.github/workflows/helm-release-test.yml`), which installs the exact
umbrella package in Kind and verifies the core controller, server,
authentication, and BatchSandbox lifecycle.

### Adding a New Chart

1. Create the chart under `manifests/charts/<name>/` with a `README.md`
   (add a `README.md.gotmpl` so `helm-docs` manages it).
2. Add it as a dependency of the umbrella chart (`manifests/charts/opensandbox/Chart.yaml`)
   with an `enabled`-style condition if it is optional.
3. Add the chart name to the chart lists in
   `manifests/release/bump-versions.sh` and
   `manifests/release/create-umbrella-release.sh` so version bumps cover it.

### Local Testing

```bash
cd manifests
make helm-lint        # lint every chart + umbrella dependency check
make helm-template    # render the controller chart
make helm-docs        # regenerate chart READMEs (CI fails on drift)
```

## References

- [Charts README](README.md) — chart layout and source-of-truth rules
- [Controller chart](charts/controller/README.md) — full parameter list
- [Server chart](charts/server/README.md)
- [Kubernetes deployment guide](../docs/deployment/index.md)
- [Release automation](../docs/community/release-automation.md)
