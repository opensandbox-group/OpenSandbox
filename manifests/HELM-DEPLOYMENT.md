# Helm Chart Deployment

This document describes how to deploy the OpenSandbox Controller using Helm Chart.

## Prerequisites

- Kubernetes 1.22.4+
- Helm 3.0+
- kubectl configured and able to access the target cluster

## Quick Start

### Option 1: Install from GitHub Release (Recommended)

Download and install the published chart package directly. The controller
chart no longer bundles the CRDs; install the base chart once per cluster
first:

```bash
# Install the latest version (0.1.0)
# The helm/base release becomes available right after the chart restructure
# merges; until then install from source: helm install base manifests/charts/base
helm install base \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/base/0.1.0/base-0.1.0.tgz

helm install opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

To use a custom image:

```bash
helm install opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --set controller.image.repository=<your-registry>/controller \
  --set controller.image.tag=v0.0.1 \
  --namespace opensandbox-system \
  --create-namespace
```

### Option 2: Install from Local Chart

If building from source, you can use the local chart:

#### 1. Build Images

First build the controller and task-executor images:

```bash
# Build controller image
cd kubernetes
COMPONENT=controller TAG=v0.0.1 ./build.sh

# Build task-executor image
COMPONENT=task-executor TAG=v0.0.1 ./build.sh
```

#### 2. Install the Local Helm Chart

```bash
helm install opensandbox-controller ../manifests/charts/controller \
  --set controller.image.repository=<your-registry>/controller \
  --set controller.image.tag=v0.0.1 \
  --namespace opensandbox-system \
  --create-namespace
```

Or using Makefile:

```bash
make helm-install \
  IMAGE_TAG_BASE=<your-registry>/controller \
  VERSION=v0.0.1
```

### 3. Verify Installation

```bash
# Check Pod status
kubectl get pods -n opensandbox-system

# Check CRDs
kubectl get crd | grep opensandbox

# View installation status
helm status opensandbox-controller -n opensandbox-system

# View installed Chart version
helm list -n opensandbox-system
```

## Version Management

### View Available Versions

Visit GitHub Releases to see all available versions:
https://github.com/opensandbox-group/OpenSandbox/releases

Look for tags starting with `helm/opensandbox-controller/`, such as `helm/opensandbox-controller/0.1.0`

### Upgrade to a Specific Version

```bash
# Upgrade directly from GitHub Release
helm upgrade opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.2.0/opensandbox-controller-0.2.0.tgz \
  --namespace opensandbox-system
```

## Custom Configuration

### Using a Custom Values File

Create a custom values file `custom-values.yaml`:

```yaml
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
helm install opensandbox-controller ../manifests/charts/controller \
  -f custom-values.yaml \
  --namespace opensandbox-system \
  --create-namespace
```

### Common Configuration Examples

#### 1. Adjust Resource Configuration

```bash
helm install opensandbox-controller ../manifests/charts/controller \
  --set controller.resources.limits.cpu=1000m \
  --set controller.resources.limits.memory=512Mi \
  --namespace opensandbox-system
```

#### 2. Configure Node Affinity

Create `affinity-values.yaml`:

```yaml
controller:
  resources:
    limits:
      cpu: 1000m
      memory: 512Mi
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: node-role.kubernetes.io/control-plane
            operator: Exists
```

```bash
helm install opensandbox-controller ../manifests/charts/controller \
  -f affinity-values.yaml \
  --namespace opensandbox-system
```

#### 3. Configure Pause/Resume

```bash
helm install opensandbox-controller ../manifests/charts/controller \
  --set controller.snapshot.registry=myregistry.example.com/opensandbox/snapshots \
  --set controller.snapshot.snapshotPushSecret=registry-snapshot-push-secret \
  --set controller.snapshot.imageCommitterPullSecret=registry-image-committer-pull-secret \
  --set controller.snapshot.resumePullSecret=registry-pull-secret \
  --namespace opensandbox-system
```

## Upgrade

### Upgrade Helm Release

Upgrade from GitHub Release:

```bash
# Upgrade to a specific version
helm upgrade opensandbox-controller \
  https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.2.0/opensandbox-controller-0.2.0.tgz \
  --namespace opensandbox-system
```

Upgrade from local chart:

```bash
helm upgrade opensandbox-controller ../manifests/charts/controller \
  --set controller.image.tag=v0.0.2 \
  --namespace opensandbox-system
```

Or using Makefile:

```bash
make helm-upgrade VERSION=v0.0.2
```

### View Upgrade History

```bash
helm history opensandbox-controller -n opensandbox-system
```

### Rollback

```bash
# Rollback to the previous version
helm rollback opensandbox-controller -n opensandbox-system

# Rollback to a specific revision
helm rollback opensandbox-controller 1 -n opensandbox-system
```

## Uninstall

### Uninstall Helm Release

```bash
helm uninstall opensandbox-controller -n opensandbox-system
```

Or using Makefile:

```bash
make helm-uninstall
```

**Note**: By default, CRDs are retained. To delete CRDs:

```bash
kubectl delete crd batchsandboxes.sandbox.opensandbox.io
kubectl delete crd pools.sandbox.opensandbox.io
kubectl delete crd sandboxsnapshots.sandbox.opensandbox.io
```

### Clean Up Namespace

To completely clean up:

```bash
kubectl delete namespace opensandbox-system
```

## Makefile Commands

The project provides a set of Makefile commands to simplify Helm operations:

```bash
# Lint the Helm Chart
make helm-lint

# Generate Kubernetes manifests (without installing)
make helm-template

# Generate manifests with debug output
make helm-template-debug

# Package the Helm Chart
make helm-package

# Install the Helm Chart
make helm-install

# Upgrade the Helm Chart
make helm-upgrade

# Uninstall the Helm Chart
make helm-uninstall

# Test the installed Chart
make helm-test

# Perform a dry-run install
make helm-dry-run

# Run all Helm-related tasks
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
```

### 3. Create Test Resources

```bash
# Create a Pool
kubectl apply -f config/samples/sandbox_v1alpha1_pool.yaml

# Create a BatchSandbox
kubectl apply -f config/samples/sandbox_v1alpha1_batchsandbox.yaml

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
helm upgrade opensandbox-controller ../manifests/charts/controller \
  --set imagePullSecrets[0].name=myregistrykey \
  --namespace opensandbox-system
```

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
controller:
  logLevel: debug
  resources:
    limits:
      cpu: 200m
      memory: 128Mi
```

#### values-prod.yaml
```yaml
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
helm install opensandbox-controller ../manifests/charts/controller \
  -f values-dev.yaml \
  --namespace opensandbox-dev

# Production environment
helm install opensandbox-controller ../manifests/charts/controller \
  -f values-prod.yaml \
  --namespace opensandbox-prod
```

## Publishing Helm Charts (Maintainers)

### Automated Publishing

Publish Helm Charts automatically via GitHub Actions:

#### Option 1: Trigger via Git Tag

```bash
# Publish opensandbox-controller chart version 0.1.0
git tag helm/opensandbox-controller/0.1.0
git push origin helm/opensandbox-controller/0.1.0
```

Tag naming convention: `helm/{component}/{version}`
- `helm`: Prefix indicating this is a Helm Chart release
- `{component}`: Component name, e.g. `opensandbox-controller`
- `{version}`: Version number, e.g. `0.1.0`

This automatically triggers the workflow to:
1. Parse the tag to extract component and version
2. Verify the tag version matches the chart `version`
3. Preserve the committed chart `appVersion`
4. Package the Helm chart once and hold that exact `.tgz` with its SHA-256
5. Re-download and statically verify the held package
6. For the `opensandbox` umbrella chart, install the same `.tgz` in Kind and
   verify the core controller, server, authentication, and BatchSandbox
   lifecycle
7. Request approval through the `release` environment
8. Attest the tested package and checksum, upload them to a draft GitHub
   Release, verify the uploaded bytes, and publish the stable Release

Important versioning note:

- The Helm chart `version` is the chart package version and is released through
  `helm/{component}/{version}` tags.
- Stable publication accepts `X.Y.Z` chart and app versions. Pre-release
  versions require an explicit pre-release publication flow and are not marked
  `production-ready` by this workflow.
- The chart `appVersion` is the default image/application version used by that
  chart release.
- Tag-triggered publishing preserves the committed chart `appVersion` and
  verifies that the tag matches the committed chart `version`. Manual runs
  confirm the committed `appVersion` instead of rewriting release source, and
  must run from the exact existing Helm release tag.
- If you need a specific server image release, set the image tag explicitly
  (for example `--set server.image.tag=v0.1.13`) or publish a new Helm chart
  package version for the chart itself.

#### Option 2: Manual Trigger

Create and push the protected Helm tag first, then dispatch the workflow from
that exact tag ref. For example:

```bash
# Helm charts are no longer published; deploy from the repo at a
# release tag:
# gh workflow run release-umbrella.yml \
  --repo opensandbox-group/OpenSandbox \
  --ref helm/opensandbox-controller/0.1.0 \
  -f component=opensandbox-controller \
  -f chart_version=0.1.0 \
  -f app_version=0.0.1
```

The workflow rejects a manual run whose selected `--ref` is not exactly
`helm/{component}/{chart_version}`, or whose `app_version` does not match the
committed chart metadata. This keeps the environment deployment, attestation
source ref, packaged bytes, and GitHub Release tied to the same protected tag.

Only the stable umbrella `opensandbox` Release is marked `production-ready`,
and only after its exact package passes the Kind core-lifecycle gate.
Standalone chart Releases are marked `package-verified`.

Pull requests that change the release workflows, release-smoke scripts,
umbrella chart, or Python lifecycle clients run the same exact-package Kind
smoke through the `Helm Release Smoke` workflow before merge.

### Published URL Format

After publishing, users can access the Helm Chart at:

```
https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/{COMPONENT}/{VERSION}/{COMPONENT}-{VERSION}.tgz
```

Example:
```
https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz
```

### Adding a New Helm Chart Component

To add Helm Chart publishing support for a new component:

1. Create a new chart directory under `charts/`
2. Charts are not published; version changes land via the release prep commit (`create-umbrella-release.sh --bump-only`):
   - Add the new component to `workflow_dispatch.inputs.component.options`
   - Add the component path mapping in the "Set chart path" step

Example:
```yaml
# Add to workflow_dispatch inputs
options:
  - opensandbox-controller
  - new-component  # new entry

# Add to Set chart path step
if [ "$COMPONENT" == "opensandbox-controller" ]; then
  CHART_PATH="manifests/charts/controller"
elif [ "$COMPONENT" == "new-component" ]; then
  CHART_PATH="path/to/new-component/chart"
fi
```

### Local Test of the Publishing Process

Before publishing, test locally:

```bash
# Package the Chart
make helm-package

# Validate the packaged Chart
helm lint opensandbox-controller-*.tgz

# Test installation
helm install test-release opensandbox-controller-*.tgz \
  --namespace test \
  --create-namespace \
  --dry-run
```

## References

- [Helm Chart README](../../manifests/charts/controller/README.md) - Full parameter list
- [OpenSandbox Documentation](../README.md) - Project documentation
- [Configuration Examples](../config/samples/) - Resource configuration examples
