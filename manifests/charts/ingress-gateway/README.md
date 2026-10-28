# ingress-gateway

![Version: 1.2.0](https://img.shields.io/badge/Version-1.2.0-informational?style=flat-square) ![AppVersion: 1.2.0](https://img.shields.io/badge/AppVersion-1.2.0-informational?style=flat-square)

Helm chart for deploying the OpenSandbox ingress gateway (components/ingress)

**Homepage:** <https://github.com/opensandbox-group/OpenSandbox>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| OpenSandbox Team |  |  |

## Source Code

* <https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts/ingress-gateway>
* <https://github.com/opensandbox-group/OpenSandbox/tree/main/components/ingress>

## Requirements

Kubernetes: `>=1.21.1-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| fullnameOverride | string | `"opensandbox-ingress-gateway"` | Resource names and app.kubernetes.io/name are fixed to this value, independent of release name (keeps names stable across releases). |
| gateway.activity.enabled | bool | `false` | Enable activity recording. |
| gateway.activity.minInterval | string | `"5s"` | Min interval between activity writes for the same sandbox. |
| gateway.activity.redisDsn | string | `"redis://127.0.0.1:6379/0"` | Redis DSN; must match the server's [automation.idle_pause] redis.dsn. |
| gateway.activity.ttlSeconds | int | `1800` | TTL for activity keys; keep equal to the server's activity_ttl_seconds. |
| gateway.affinity | object | `{}` | Affinity for the gateway pod. |
| gateway.containerSecurityContext | object | `{}` | Container-level security context for the gateway container. |
| gateway.dataplaneNamespace | string | `"opensandbox"` | Namespace where the gateway dataplane workloads run. |
| gateway.env | list | `[]` | Additional environment variables for the gateway container (e.g. OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_SERVICE_NAME for OTLP metrics). |
| gateway.fastpathEndpoint | string | `""` | FastPath gRPC endpoint for the fast-sandbox provider (--fastpath-endpoint). Required when providerType is "fast-sandbox"; ignored otherwise. Example: fast-sandbox-fastpath.opensandbox-system.svc:9090 |
| gateway.gatewayRouteMode | string | `"header"` | Gateway route mode: header or uri. Must match server.gateway.gatewayRouteMode. |
| gateway.image | object | `{"pullPolicy":"IfNotPresent","repository":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress","tag":""}` | Gateway image configuration. |
| gateway.image.pullPolicy | string | `"IfNotPresent"` | Gateway image pull policy. |
| gateway.image.repository | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress"` | Gateway image repository. |
| gateway.image.tag | string | `""` | Gateway image tag. Empty uses the release-<appVersion> image tag published for this chart version. |
| gateway.logLevel | string | `"info"` | Gateway log level. |
| gateway.nodeSelector | object | `{}` | Node selector for the gateway pod. |
| gateway.podAnnotations | object | `{}` | Extra annotations for the gateway pod. |
| gateway.podLabels | object | `{}` | Extra labels for the gateway pod. |
| gateway.podSecurityContext | object | `{}` | Pod-level security context for the gateway pod. |
| gateway.port | int | `28888` | Gateway container port. |
| gateway.priorityClassName | string | `""` | Priority class name for the gateway pod. |
| gateway.providerType | string | `"batchsandbox"` | Gateway provider type (e.g. batchsandbox). |
| gateway.replicaCount | int | `2` | Number of gateway replicas. |
| gateway.resources | object | `{"limits":{"cpu":"2","memory":"8Gi"},"requests":{"cpu":"1","memory":"4Gi"}}` | Resource requests and limits for the gateway. |
| gateway.secureAccess.existingSecret | string | `""` | Name of an existing Secret holding the signing key ring. Mutually exclusive with keys. The Secret must carry a `keys` entry containing the ring, "a=<base64-secret>[,b=<base64-secret>...]". The chart wires it into the `--secure-access-keys` arg via $(...) env expansion, so key material never appears in values or pod args. |
| gateway.secureAccess.keys | list | `[]` | List of signing keys. Each entry: { key_id: "a", key: "<base64-secret>" }. key_id must be exactly one character in [0-9a-z]. Mutually exclusive with existingSecret. |
| gateway.service | object | `{"nodePort":"","port":80,"type":"ClusterIP"}` | Service settings for the gateway. |
| gateway.service.nodePort | string | `""` | Static nodePort (used when the service type is NodePort or LoadBalancer). |
| gateway.service.port | int | `80` | Service port. The targetPort always points to gateway.port. |
| gateway.service.type | string | `"ClusterIP"` | Service type for the gateway. |
| gateway.tolerations | list | `[]` | Tolerations for the gateway pod. |
| gateway.topologySpreadConstraints | list | `[]` | Topology spread constraints for the gateway pod. |
| gateway.wake.enabled | bool | `false` | Enable wake-on-access. |
| gateway.wake.maxFlightLifetime | string | `"5m"` | Max lifetime of one wake flight; expiry releases the restore permit while the resume converges server-side. |
| gateway.wake.parkBudget | string | `"5s"` | Park budget per wake flight (must exceed the FastPath wait timeout plus one GetSandbox RPC). |
| gateway.wake.parkMax | int | `1024` | Parking lot capacity: max concurrently parked requests per replica. |
| gateway.wake.restoreMax | int | `256` | Concurrent restore permits per replica (must be <= parkMax); a flight holds its permit until the restore reaches a terminal outcome. |
| gateway.wake.retryFactor | string | `"1.3"` | Exponential growth factor of the wake poll interval. |
| gateway.wake.retryInterval | string | `"50ms"` | First readiness-poll interval of a wake flight. |
| gateway.wake.retryJitter | string | `"0.1"` | Relative jitter applied to the wake poll interval. |
| imagePullSecrets | list | `[]` | Image pull secrets for the gateway deployment. Each entry: {name: <secret-name>}. |
| nameOverride | string | `""` | Override the name of the chart |
| namespaceOverride | string | `""` | Override the namespace (default: opensandbox-system) |
| rbac.create | bool | `true` | Specifies whether the gateway RBAC resources (ClusterRole + binding for reading pods/services and BatchSandboxes) should be created. |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
