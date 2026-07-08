# opensandbox-controller

![Version: 0.2.0](https://img.shields.io/badge/Version-0.2.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.2.0](https://img.shields.io/badge/AppVersion-0.2.0-informational?style=flat-square)

A Kubernetes operator for managing sandbox environments with resource pooling and batch delivery

**Homepage:** <https://github.com/alibaba/OpenSandbox>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| OpenSandbox Team | <opensandbox@example.com> |  |

## Source Code

* <https://github.com/alibaba/OpenSandbox/tree/main/kubernetes>

## Requirements

Kubernetes: `>=1.21.1-0`

## Values

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
| controller.nodeSelector | object | `{}` | Node labels for controller pod assignment |
| controller.podAnnotations | object | `{}` | Additional annotations for controller pods |
| controller.podLabels | object | `{}` | Additional labels for controller pods |
| controller.podSecurityContext | object | `{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod security context |
| controller.priorityClassName | string | `""` | Priority class name for controller pods |
| controller.readinessProbe | object | `{"enabled":true,"failureThreshold":3,"httpGet":{"path":"/readyz","port":8081},"initialDelaySeconds":5,"periodSeconds":10,"successThreshold":1,"timeoutSeconds":1}` | Readiness probe configuration |
| controller.replicaCount | int | `1` | Number of controller replicas |
| controller.resources | object | `{"limits":{"cpu":"500m","memory":"128Mi"},"requests":{"cpu":"10m","memory":"64Mi"}}` | Resource requests and limits for the controller |
| controller.snapshot | object | `{"commitJobTimeout":"10m","containerdSocketPath":"/var/run/containerd/containerd.sock","imageCommitterImage":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/image-committer:v0.1.0","registry":"","registryInsecure":false,"resumePullSecret":"","snapshotPushSecret":""}` | Pause/Resume snapshot configuration |
| controller.snapshot.commitJobTimeout | string | `"10m"` | Timeout duration for commit jobs |
| controller.snapshot.containerdSocketPath | string | `"/var/run/containerd/containerd.sock"` | Containerd socket path of host |
| controller.snapshot.imageCommitterImage | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/image-committer:v0.1.0"` | Image used for commit operations (must contain nerdctl tool) DockerHub: opensandbox/image-committer:v0.1.0 |
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
| networkPolicy.egress | list | `[]` | Egress rules for network policy |
| networkPolicy.enabled | bool | `false` | Enable network policy |
| networkPolicy.ingress | list | `[]` | Ingress rules for network policy |
| rbac.create | bool | `true` | Specifies whether RBAC resources should be created |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account |
| serviceAccount.create | bool | `true` | Specifies whether a service account should be created |
| serviceAccount.name | string | `""` | The name of the service account to use. If not set and create is true, a name is generated using the fullname template |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
