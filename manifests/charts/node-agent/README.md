# OpenSandbox Node Agent Helm Chart

This chart deploys one Node Agent per Linux node. It runs one or more Sources
compiled into the image and sends sandbox records to one configured file or
Alibaba Cloud OSS sink. The published image includes the `container-logs`
Source for CRI stdout/stderr and the opt-in `syscalls` Source for cgroup-scoped
Linux system-call records from non-pooled OpenSandbox Pods.

The `syscalls` Source requires Linux kernel 5.11 or newer, cgroup v2, and a
tracefs mount. It tracks the runtime's container cgroup; delegated descendant
cgroups are not covered by this first implementation.

The chart is disabled by default when used through the umbrella OpenSandbox
chart. For OSS, create a Secret containing `access-key-id`,
`access-key-secret`, and optionally `session-token`, then set
`sink.type=oss` and `sink.oss.existingSecret`.

Node-local checkpoint state and source-data retention jointly define the
delivery guarantee. Do not remove the state host path, rotate durable-file
outputs externally, or change a sink target without draining the Agent.

## Configuration

The following table lists the configurable parameters of the chart and their default values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity for node agent pod assignment. |
| config | object | `{"clusterID":"dev-cluster","dropPolicy":"block","endedStateRetention":"24h","maxLineBytes":1048576,"memoryBudgetBytes":268435456,"partialTimeout":"5s","perSandboxQueueBytes":16777216,"perSandboxRateLimit":0,"pprofAddr":"","retryMaxInterval":"30s","serverAddr":":8080","sinkTimeout":"30s","sources":["container-logs"],"stateDir":"/var/lib/opensandbox/nodeagent","stateMaxBytes":1073741824}` | Runtime configuration for the node agent. |
| config.clusterID | string | `"dev-cluster"` | Cluster identifier reported in collected records. |
| config.dropPolicy | string | `"block"` | Delivery policy when a sink is unavailable (block or drop). |
| config.endedStateRetention | string | `"24h"` | Ended-sandbox retention for the container-logs Source. |
| config.maxLineBytes | int | `1048576` | Maximum log-line length for the container-logs Source. |
| config.memoryBudgetBytes | int | `268435456` | Memory budget for buffering sandbox records on the node. |
| config.partialTimeout | string | `"5s"` | Partial-line timeout for the container-logs Source. |
| config.perSandboxQueueBytes | int | `16777216` | Maximum queued bytes per sandbox before backpressure applies. |
| config.perSandboxRateLimit | int | `0` | Per-sandbox rate limit (bytes/s); 0 disables the limit. |
| config.pprofAddr | string | `""` | Optional pprof listen address; empty disables pprof. |
| config.retryMaxInterval | string | `"30s"` | Maximum interval between sink retries. |
| config.serverAddr | string | `":8080"` | Address the node agent health/pprof HTTP server binds to. |
| config.sinkTimeout | string | `"30s"` | Timeout for a single sink write attempt. |
| config.sources | list | `["container-logs"]` | Sources to enable. The image must contain every named Source. |
| config.stateDir | string | `"/var/lib/opensandbox/nodeagent"` | Host path where node-local checkpoint state is persisted. |
| config.stateMaxBytes | int | `1073741824` | Maximum bytes of checkpoint state to keep on the node. |
| containerSecurityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsGroup":0,"runAsUser":0}` | Container-level security context for the node agent container. |
| enabled | bool | `true` | Whether the node-agent is enabled (used by the umbrella opensandbox chart). |
| extraEnv | list | `[]` | Additional environment variables for the node agent container. |
| fullnameOverride | string | `""` | Override the full name of the chart. |
| hostPaths | object | `{"cgroup":"/sys/fs/cgroup","fileData":"/var/lib/opensandbox/nodeagent-data","logs":"/var/log/pods","state":"/var/lib/opensandbox/nodeagent","tracing":"/sys/kernel/tracing"}` | Host paths available to the node agent; enabled Sources and Sinks select mounts. |
| hostPaths.cgroup | string | `"/sys/fs/cgroup"` | Host cgroup v2 hierarchy used only by the syscalls Source. |
| hostPaths.fileData | string | `"/var/lib/opensandbox/nodeagent-data"` | Host path for file sink data (must match sink.file.path). |
| hostPaths.logs | string | `"/var/log/pods"` | Pod-log host path used only by the container-logs Source. |
| hostPaths.state | string | `"/var/lib/opensandbox/nodeagent"` | Host path for checkpoint state (must match config.stateDir). |
| hostPaths.tracing | string | `"/sys/kernel/tracing"` | Host tracefs used only by the syscalls Source. |
| image | object | `{"pullPolicy":"IfNotPresent","repository":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/nodeagent","tag":""}` | Node agent image configuration. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/nodeagent"` | Node agent image repository. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for the node agent DaemonSet. Each entry: {name: <secret-name>}. |
| nameOverride | string | `""` | Override the name of the chart. |
| namespaceOverride | string | `""` | Override the namespace where resources will be created (default: opensandbox-system). |
| nodeSelector | object | `{}` | Node labels for node agent pod assignment. |
| podAnnotations | object | `{}` | Additional annotations for node agent pods. |
| podLabels | object | `{}` | Additional labels for node agent pods. |
| podSecurityContext | object | `{"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context for node agent pods. |
| priorityClassName | string | `""` | Priority class name for node agent pods. |
| rbac | object | `{"create":true}` | RBAC configuration. |
| rbac.create | bool | `true` | Whether RBAC resources should be created. |
| resources | object | `{"limits":{"cpu":1,"memory":"512Mi"},"requests":{"cpu":"50m","memory":"128Mi"}}` | Resource requests and limits for the node agent DaemonSet. |
| serviceAccount | object | `{"annotations":{},"create":true,"name":""}` | Service account configuration. |
| serviceAccount.annotations | object | `{}` | Annotations to add to the service account. |
| serviceAccount.create | bool | `true` | Whether a service account should be created. |
| serviceAccount.name | string | `""` | The name of the service account to use. If empty and create is true, a name is generated. |
| sink | object | `{"file":{"maxBytes":1073741824,"maxFiles":16,"maxTotalBytes":10737418240,"path":"/var/lib/opensandbox/nodeagent-data","retention":"24h"},"oss":{"accessKeyIDKey":"access-key-id","accessKeySecretKey":"access-key-secret","bucket":"","endpoint":"","existingSecret":"","keyPrefix":"logs","sessionTokenKey":"session-token"},"type":"file"}` | Sink configuration for collected records. |
| sink.file | object | `{"maxBytes":1073741824,"maxFiles":16,"maxTotalBytes":10737418240,"path":"/var/lib/opensandbox/nodeagent-data","retention":"24h"}` | File sink configuration (used when sink.type is file). |
| sink.file.maxBytes | int | `1073741824` | Maximum size of a single output file before rotation. |
| sink.file.maxFiles | int | `16` | Maximum number of rotated output files to keep. |
| sink.file.maxTotalBytes | int | `10737418240` | Maximum total bytes of rotated output files to keep. |
| sink.file.path | string | `"/var/lib/opensandbox/nodeagent-data"` | Directory where collected records are written. |
| sink.file.retention | string | `"24h"` | Retention window for rotated output files. |
| sink.oss | object | `{"accessKeyIDKey":"access-key-id","accessKeySecretKey":"access-key-secret","bucket":"","endpoint":"","existingSecret":"","keyPrefix":"logs","sessionTokenKey":"session-token"}` | Alibaba Cloud OSS sink configuration (used when sink.type is oss). |
| sink.oss.accessKeyIDKey | string | `"access-key-id"` | Key in the Secret holding the access key id. |
| sink.oss.accessKeySecretKey | string | `"access-key-secret"` | Key in the Secret holding the access key secret. |
| sink.oss.bucket | string | `""` | OSS bucket name. |
| sink.oss.endpoint | string | `""` | OSS endpoint. |
| sink.oss.existingSecret | string | `""` | Name of a Secret holding OSS credentials. |
| sink.oss.keyPrefix | string | `"logs"` | Key prefix for objects written to OSS. |
| sink.oss.sessionTokenKey | string | `"session-token"` | Key in the Secret holding the optional session token. |
| sink.type | string | `"file"` | Sink type: file or oss. |
| terminationGracePeriodSeconds | int | `60` | Grace period (seconds) before the node agent pod is terminated. |
| tolerations | list | `[]` | Tolerations for node agent pod assignment. |
