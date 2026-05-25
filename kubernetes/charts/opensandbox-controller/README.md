# OpenSandbox Controller Helm Chart

A Helm chart for deploying the OpenSandbox Kubernetes Controller, which manages sandbox environments with resource pooling, batch delivery, and pause/resume capabilities.

## Introduction

This chart bootstraps an OpenSandbox Controller deployment on a Kubernetes cluster using the Helm package manager. The controller provides:

- **Batch Sandbox Management**: Create and manage multiple identical sandbox environments
- **Resource Pooling**: Maintain pre-warmed resource pools for rapid sandbox provisioning
- **Task Orchestration**: Optional task execution within sandboxes
- **Pause and Resume**: Persist sandbox filesystem state via rootfs snapshot, releasing cluster resources between sessions
- **High Performance**: O(1) time complexity for batch sandbox delivery

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.0+
- Container runtime (Docker, containerd, etc.)

## Installing the Chart

To install the chart with the release name `opensandbox-controller`:

```bash
helm install opensandbox-controller ./opensandbox-controller \
  --set controller.image.repository=<your-registry>/opensandbox-controller \
  --set controller.image.tag=v0.1.0 \
  --namespace opensandbox-system \
  --create-namespace
```

The command deploys OpenSandbox Controller on the Kubernetes cluster with default configuration. The [Parameters](#parameters) section lists the parameters that can be configured during installation.

## Uninstalling the Chart

To uninstall/delete the `opensandbox-controller` deployment:

```bash
helm delete opensandbox-controller -n opensandbox-system
```

The command removes all the Kubernetes components associated with the chart. Note that CRDs are kept by default (can be changed via `crds.keep`).

To also remove the CRDs:

```bash
kubectl delete crd batchsandboxes.sandbox.opensandbox.io
kubectl delete crd pools.sandbox.opensandbox.io
kubectl delete crd sandboxsnapshots.sandbox.opensandbox.io
```

## Parameters

### Global Parameters

| Name | Description | Value |
|------|-------------|-------|
| `nameOverride` | Override the name of the chart | `""` |
| `fullnameOverride` | Override the full name of the chart | `""` |
| `namespaceOverride` | Override the namespace where resources will be created | `""` |

### Controller Parameters

| Name | Description | Value |
|------|-------------|-------|
| `controller.image.repository` | Controller image repository | `opensandbox.io/opensandbox-controller` |
| `controller.image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `controller.image.tag` | Overrides the image tag (default is chart appVersion) | `""` |
| `controller.replicaCount` | Number of controller replicas | `1` |
| `controller.resources.limits.cpu` | CPU resource limits | `500m` |
| `controller.resources.limits.memory` | Memory resource limits | `128Mi` |
| `controller.resources.requests.cpu` | CPU resource requests | `10m` |
| `controller.resources.requests.memory` | Memory resource requests | `64Mi` |
| `controller.logLevel` | Can be one of 'debug', 'info', 'error' | `info` |
| `controller.kubeClient.qps` | QPS for Kubernetes client rate limiter | `100` |
| `controller.kubeClient.burst` | Burst for Kubernetes client rate limiter | `200` |
| `controller.snapshot.imageCommitterImage` | Image used by snapshot commit Jobs | `image-committer:dev` |
| `controller.snapshot.commitJobTimeout` | Timeout duration for snapshot commit Jobs | `10m` |
| `controller.snapshot.registry` | OCI registry prefix used for snapshot images | `""` |
| `controller.snapshot.registryInsecure` | Use insecure registry mode for snapshot pushes | `false` |
| `controller.snapshot.snapshotPushSecret` | Secret name used by commit Jobs to push snapshots | `""` |
| `controller.snapshot.resumePullSecret` | Secret name injected into resumed sandboxes for image pulls | `""` |
| `controller.leaderElection.enabled` | Enable leader election | `true` |
| `controller.nodeSelector` | Node labels for pod assignment | `{}` |
| `controller.tolerations` | Tolerations for pod assignment | `[]` |
| `controller.affinity` | Affinity for pod assignment | `{}` |
| `controller.podLabels` | Additional labels for controller pods | `{}` |
| `controller.podAnnotations` | Additional annotations for controller pods | `{}` |
| `controller.priorityClassName` | Priority class name for controller pods | `""` |

### RBAC Parameters

| Name | Description | Value |
|------|-------------|-------|
| `rbac.create` | Specifies whether RBAC resources should be created | `true` |
| `serviceAccount.create` | Specifies whether a service account should be created | `true` |
| `serviceAccount.annotations` | Annotations to add to the service account | `{}` |
| `serviceAccount.name` | The name of the service account to use | `""` |

### CRD Parameters

| Name | Description | Value |
|------|-------------|-------|
| `crds.install` | Specifies whether CRDs should be installed | `true` |
| `crds.keep` | Keep CRDs on chart uninstall | `true` |
| `crds.annotations` | Annotations to add to CRDs | `{"helm.sh/resource-policy": "keep"}` |

### Additional Parameters

| Name | Description | Value |
|------|-------------|-------|
| `imagePullSecrets` | Image pull secrets for private registries | `[]` |
| `extraEnv` | Additional environment variables | `[]` |
| `extraVolumes` | Additional volumes | `[]` |
| `extraVolumeMounts` | Additional volume mounts | `[]` |
| `extraInitContainers` | Additional init containers | `[]` |
| `extraContainers` | Additional sidecar containers | `[]` |

## Configuration Examples

### Custom Resource Limits

```yaml
controller:
  resources:
    limits:
      cpu: 1000m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Custom Kubernetes Client Rate Limiter

Configure the QPS and Burst for the Kubernetes client to handle high-throughput scenarios:

```yaml
controller:
  kubeClient:
    qps: 100
    burst: 250
```

> Note: Default values are QPS=100, Burst=200.

### Use Private Registry

```yaml
controller:
  image:
    repository: myregistry.example.com/opensandbox-controller
    tag: v0.1.0

imagePullSecrets:
  - name: myregistrykey
```

### Pause/Resume Snapshot Configuration

The chart exposes the snapshot-related settings below:

```yaml
controller:
  snapshot:
    imageCommitterImage: my-registry/image-committer:v0.1.0
    commitJobTimeout: 15m
    registry: my-registry/snapshots
    registryInsecure: false
    snapshotPushSecret: registry-snapshot-push-secret
    resumePullSecret: registry-pull-secret
```

These values render directly to the controller flags:

- `--image-committer-image`
- `--commit-job-timeout`
- `--snapshot-registry`
- `--snapshot-registry-insecure`
- `--snapshot-push-secret`
- `--resume-pull-secret`

### Node Affinity

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

## Usage Examples

After installation, you can create resources:

### Create a Resource Pool

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: example-pool
spec:
  template:
    spec:
      containers:
      - name: sandbox-container
        image: nginx:latest
        ports:
        - containerPort: 80
  capacitySpec:
    bufferMax: 10
    bufferMin: 2
    poolMax: 20
    poolMin: 5
```

### Create a Batch Sandbox

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: example-batch-sandbox
spec:
  replicas: 3
  poolRef: example-pool
```

## Upgrading

To upgrade the chart:

```bash
helm upgrade opensandbox-controller ./opensandbox-controller \
  --namespace opensandbox-system \
  -f custom-values.yaml
```

## Troubleshooting

### Check controller logs

```bash
kubectl logs -n opensandbox-system -l control-plane=controller-manager -f
```

### Check CRD installation

```bash
kubectl get crd | grep opensandbox
```

### Verify RBAC permissions

```bash
kubectl auth can-i --as=system:serviceaccount:opensandbox-system:opensandbox-controller-controller-manager create pods
```

## Additional Resources

- [OpenSandbox GitHub](https://github.com/alibaba/OpenSandbox)
- [Documentation](https://github.com/alibaba/OpenSandbox/blob/main/kubernetes/README.md)
- [Pause and Resume Guide](https://github.com/alibaba/OpenSandbox/blob/main/docs/pause-resume.md)
- [Server Configuration Reference](https://github.com/alibaba/OpenSandbox/blob/main/server/configuration.md)
- [Examples](https://github.com/alibaba/OpenSandbox/tree/main/kubernetes/config/samples)

## License

Apache 2.0 License
