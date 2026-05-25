# Helm Chart Deployment

This document describes how to deploy the OpenSandbox Controller using Helm Chart.

## Prerequisites

- Kubernetes 1.22.4+
- Helm 3.0+
- kubectl configured and able to access the target cluster

## Quick Start

### Option 1: Install from GitHub Release (Recommended)

Download and install the published chart package directly:

```bash
# Install the latest version (0.1.0)
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
  --namespace opensandbox-system \
  --create-namespace
```

To use a custom image:

```bash
helm install opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz \
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
helm install opensandbox-controller ./charts/opensandbox-controller \
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
https://github.com/alibaba/OpenSandbox/releases

Look for tags starting with `helm/opensandbox-controller/`, such as `helm/opensandbox-controller/0.1.0`

### Upgrade to a Specific Version

```bash
# Upgrade directly from GitHub Release
helm upgrade opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.2.0/opensandbox-controller-0.2.0.tgz \
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
    resumePullSecret: registry-pull-secret

imagePullSecrets:
  - name: myregistrykey
```

Install with custom configuration:

```bash
helm install opensandbox-controller ./charts/opensandbox-controller \
  -f custom-values.yaml \
  --namespace opensandbox-system \
  --create-namespace
```

### Common Configuration Examples

#### 1. Adjust Resource Configuration

```bash
helm install opensandbox-controller ./charts/opensandbox-controller \
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
helm install opensandbox-controller ./charts/opensandbox-controller \
  -f affinity-values.yaml \
  --namespace opensandbox-system
```

#### 3. Configure Pause/Resume

```bash
helm install opensandbox-controller ./charts/opensandbox-controller \
  --set controller.snapshot.registry=myregistry.example.com/opensandbox/snapshots \
  --set controller.snapshot.snapshotPushSecret=registry-snapshot-push-secret \
  --set controller.snapshot.resumePullSecret=registry-pull-secret \
  --namespace opensandbox-system
```

## Upgrade

### Upgrade Helm Release

Upgrade from GitHub Release:

```bash
# Upgrade to a specific version
helm upgrade opensandbox-controller \
  https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.2.0/opensandbox-controller-0.2.0.tgz \
  --namespace opensandbox-system
```

Upgrade from local chart:

```bash
helm upgrade opensandbox-controller ./charts/opensandbox-controller \
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
kubectl auth can-i --as=system:serviceaccount:opensandbox-system:opensandbox-opensandbox-controller-controller-manager create pods
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
helm upgrade opensandbox-controller ./charts/opensandbox-controller \
  --set imagePullSecrets[0].name=myregistrykey \
  --namespace opensandbox-system
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
helm install opensandbox-controller ./charts/opensandbox-controller \
  -f values-dev.yaml \
  --namespace opensandbox-dev

# Production environment
helm install opensandbox-controller ./charts/opensandbox-controller \
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
2. Update the version in the corresponding Chart.yaml
3. Package the Helm Chart
4. Create a GitHub Release
5. Publish the .tgz package to the Release

Important versioning note:

- The Helm chart `version` is the chart package version and is released through
  `helm/{component}/{version}` tags.
- The chart `appVersion` is the default image/application version used by that
  chart release.
- The `publish-helm-chart.yml` workflow updates `appVersion` for the published
  release, but intentionally does not auto-bump the chart `version` inside
  `Chart.yaml` on server release branches.
- If you need a specific server image release, set the image tag explicitly
  (for example `--set server.image.tag=v0.1.13`) or publish a new Helm chart
  package version for the chart itself.

#### Option 2: Manual Trigger

1. Visit the GitHub Actions page
2. Select the "Publish Helm Chart" workflow
3. Click "Run workflow"
4. Select the component (e.g. opensandbox-controller)
5. Enter chart_version (e.g. 0.1.0) and app_version (e.g. 0.0.1)
6. Click Run

### Published URL Format

After publishing, users can access the Helm Chart at:

```
https://github.com/alibaba/OpenSandbox/releases/download/helm/{COMPONENT}/{VERSION}/{COMPONENT}-{VERSION}.tgz
```

Example:
```
https://github.com/alibaba/OpenSandbox/releases/download/helm/opensandbox-controller/0.1.0/opensandbox-controller-0.1.0.tgz
```

### Adding a New Helm Chart Component

To add Helm Chart publishing support for a new component:

1. Create a new chart directory under `charts/`
2. Update `.github/workflows/publish-helm-chart.yml`:
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
  CHART_PATH="kubernetes/charts/opensandbox-controller"
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

- [Helm Chart README](charts/opensandbox-controller/README.md) - Full parameter list
- [OpenSandbox Documentation](README.md) - Project documentation
- [Configuration Examples](config/samples/) - Resource configuration examples
