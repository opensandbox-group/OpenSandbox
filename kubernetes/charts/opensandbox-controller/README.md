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

The following table lists the configurable parameters of the chart and their default values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| controller.affinity | object | `{}` | Affinity for controller pod assignment |
| controller.containerSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":false}` | Container security context |
| controller.image | object | `{"pullPolicy":"IfNotPresent","repository":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller","tag":""}` | Controller image configuration |
| controller.image.pullPolicy | string | `"IfNotPresent"` | Image pull policy |
| controller.image.repository | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/controller"` | Controller image repository |
| controller.image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion |
| controller.kubeClient | object | `{"burst":200,"qps":100}` | Kubernetes client rate limiter configuration |
| controller.kubeClient.burst | int | `200` | Burst for Kubernetes client rate limiter. |
| controller.kubeClient.qps | int | `100` | QPS for Kubernetes client rate limiter. |
| controller.leaderElection | object | `{"enabled":true}` | Enable leader election for controller manager |
| controller.livenessProbe | object | `{"enabled":true,"failureThreshold":3,"httpGet":{"path":"/healthz","port":8081},"initialDelaySeconds":15,"periodSeconds":20,"successThreshold":1,"timeoutSeconds":1}` | Liveness probe configuration |
| controller.logLevel | string | `"info"` | Log level for zap logger (debug, info, error) |
| controller.metrics | object | `{"enabled":false,"port":8080,"secure":false}` | controller-runtime metrics endpoint (Prometheus). Disabled by default to preserve the current behavior (the binary defaults to `--metrics-bind-address=0`). |
| controller.metrics.enabled | bool | `false` | Expose the controller-runtime /metrics endpoint (sets `--metrics-bind-address`) |
| controller.metrics.port | int | `8080` | Port for the metrics endpoint |
| controller.metrics.secure | bool | `false` | Serve metrics over HTTPS with authn/authz (`--metrics-secure`). Set to false to serve plain HTTP for scraping without TLS/RBAC (e.g. PodMonitoring). |
| controller.nodeSelector | object | `{}` | Node labels for controller pod assignment |
| controller.podAnnotations | object | `{}` | Additional annotations for controller pods |
| controller.podLabels | object | `{}` | Additional labels for controller pods |
| controller.podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| controller.priorityClassName | string | `""` | Priority class name for controller pods |
| controller.readinessProbe | object | `{"enabled":true,"failureThreshold":3,"httpGet":{"path":"/readyz","port":8081},"initialDelaySeconds":5,"periodSeconds":10,"successThreshold":1,"timeoutSeconds":1}` | Readiness probe configuration |
| controller.replicaCount | int | `1` | Number of controller replicas |
| controller.resources | object | `{"limits":{"cpu":"500m","memory":"128Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}` | Resource requests and limits for the controller |
| controller.snapshot | object | `{"commitJobTimeout":"10m","containerdSocketPath":"","imageCommitterImage":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/image-committer:v0.1.1","imageCommitterPodTemplate":{},"imageCommitterPullSecret":"","registry":"","registryInsecure":false,"resumePullSecret":"","snapshotPushSecret":""}` | Pause/Resume snapshot configuration |
| controller.snapshot.commitJobTimeout | string | `"10m"` | Timeout duration for commit jobs |
| controller.snapshot.containerdSocketPath | string | `""` | Containerd socket path of host. Defaults to empty so the controller uses its built-in default (/var/run/containerd/containerd.sock) without passing the `--containerd-socket-path` flag. |
| controller.snapshot.imageCommitterImage | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/image-committer:v0.1.1"` | Image used for commit operations. DockerHub: opensandbox/image-committer:v0.1.1 |
| controller.snapshot.imageCommitterPodTemplate | object | `{}` | PodTemplateSpec overlay for image-committer commit Job Pods. |
| controller.snapshot.imageCommitterPullSecret | string | `""` | Secret name for pulling the image-committer image in commit Jobs. Required when imageCommitterImage is stored in a private registry. |
| controller.snapshot.registry | string | `""` | OCI registry prefix used for snapshot images. |
| controller.snapshot.registryInsecure | bool | `false` | Use insecure registry mode when pushing snapshot images. |
| controller.snapshot.resumePullSecret | string | `""` | Secret name injected into resumed sandboxes for pulling snapshot images. |
| controller.snapshot.snapshotPushSecret | string | `""` | Secret name used by commit Jobs to push snapshot images. |
| controller.tolerations | list | `[]` | Tolerations for controller pod assignment |
| crds.annotations | object | `{}` | Additional annotations to add to CRDs (will be merged with resource-policy if keep is true) |
| crds.install | bool | `true` | Specifies whether CRDs should be installed |
| crds.keep | bool | `true` | Keep CRDs on chart uninstall (adds helm.sh/resource-policy: keep annotation) |
| extraContainers | list | `[]` | Additional sidecar containers |
| extraEnv | list | `[]` | Additional environment variables for the controller |
| extraInitContainers | list | `[]` | Additional init containers |
| extraVolumeMounts | list | `[]` | Additional volume mounts for the controller |
| extraVolumes | list | `[]` | Additional volumes for the controller |
| fullnameOverride | string | `""` | Override the full name of the chart |
| imagePullSecrets | list | `[]` | Image pull secrets for private registries |
| nameOverride | string | `""` | Override the name of the chart |
| namespaceOverride | string | `""` | Override the namespace where resources will be created If not set, defaults to "opensandbox-system" |
| rbac.create | bool | `true` | Specifies whether RBAC resources should be created |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account |
| serviceAccount.create | bool | `true` | Specifies whether a service account should be created |
| serviceAccount.name | string | `""` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |

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
    imageCommitterImage: my-registry/image-committer:v0.1.1
    imageCommitterPodTemplate:
      metadata:
        labels:
          identity.example/use: "true"
      spec:
        serviceAccountName: snapshot-committer
        containers:
          - name: commit
            resources:
              requests:
                cpu: 100m
                memory: 128Mi
    commitJobTimeout: 15m
    registry: my-registry/snapshots
    registryInsecure: false
    snapshotPushSecret: registry-snapshot-push-secret
    imageCommitterPullSecret: registry-image-committer-pull-secret
    resumePullSecret: registry-pull-secret
```

These values render directly to the controller flags:

- `--image-committer-image`
- `--image-committer-pod-template-file`
- `--commit-job-timeout`
- `--snapshot-registry`
- `--snapshot-registry-insecure`
- `--snapshot-push-secret`
- `--image-committer-pull-secret`
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

- [OpenSandbox GitHub](https://github.com/opensandbox-group/OpenSandbox)
- [Documentation](https://github.com/opensandbox-group/OpenSandbox/blob/main/kubernetes/README.md)
- [Pause and Resume Guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/guides/pause-resume.md)
- [Server Configuration Reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md)
- [Examples](https://github.com/opensandbox-group/OpenSandbox/tree/main/kubernetes/config/samples)

## License

Apache 2.0 License
