# opensandbox-server Helm Chart

OpenSandbox Lifecycle API server: provides sandbox create/delete and other lifecycle APIs, typically used with BatchSandbox/Pool on Kubernetes.

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.0+
- OpenSandbox CRDs installed (deploy opensandbox-controller first)
- A sandbox workload namespace matching `[kubernetes].namespace` in `configToml` (default: `opensandbox`)

## Install from a GitHub Release

Choose a published `opensandbox-server` chart from [GitHub Releases](https://github.com/opensandbox-group/OpenSandbox/releases?q=helm%2Fopensandbox-server&expanded=true). The release tag and package filename use the chart version shown in the release notes; the application version is listed separately.

```bash
CHART_VERSION="<chart-version>"
APP_VERSION="<app-version>"
CHART_URL="https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-server/${CHART_VERSION}/opensandbox-server-${CHART_VERSION}.tgz"

helm show values "${CHART_URL}"
```

By default, the server requires an API key for non-interactive startup. Create a Kubernetes Secret and reference it from a values file:

```bash
kubectl create namespace opensandbox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -

read -s OPENSANDBOX_API_KEY
kubectl create secret generic opensandbox-api-key \
  --namespace opensandbox-system \
  --from-literal=api-key="${OPENSANDBOX_API_KEY}" \
  --dry-run=client -o yaml | kubectl apply -f -
unset OPENSANDBOX_API_KEY
```

```yaml
# values-server.yaml
server:
  env:
    - name: OPENSANDBOX_SERVER_API_KEY
      valueFrom:
        secretKeyRef:
          name: opensandbox-api-key
          key: api-key
```

Install the versioned package:

```bash
helm install opensandbox-server "${CHART_URL}" \
  --namespace opensandbox-system \
  --set-string server.image.tag="${APP_VERSION}" \
  --values values-server.yaml
```

See the [Kubernetes deployment guide](../../../docs/kubernetes/deployment.md) for production configuration, verification, and upgrades.

## Install from local source

```bash
# Create the default sandbox workload namespace
kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -

# Server only (default namespace opensandbox-system)
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --namespace opensandbox-system \
  --create-namespace

# With custom image and config
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --set server.image.repository=your-registry/opensandbox/server \
  --set server.image.tag=v0.1.0 \
  --namespace opensandbox-system \
  --create-namespace
```

### Deploy server and ingress-gateway together

To run both the Lifecycle API server and the ingress gateway (components/ingress) in one release, set `server.gateway.enabled=true`. The chart will deploy the server and the gateway (Deployment, Service, RBAC), and write server config `[ingress] mode = "gateway"` so the server returns the correct gateway address to clients.

```bash
helm install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --namespace opensandbox-system \
  --create-namespace \
  --set server.gateway.enabled=true \
  --set server.gateway.host=gateway.example.com
```

Optional: override gateway image, replicas, or resources (see `server.gateway.*` in Configuration).

### OSEP-0011 secure-access keys

To enable signed, expiring sandbox routes, provide the signing keys either
inline (plaintext in values — fine for local dev only):

```bash
--set server.gateway.secureAccess.activeKey=a \
--set 'server.gateway.secureAccess.keys[0].key_id=a' \
--set 'server.gateway.secureAccess.keys[0].key=<base64-secret>'
```

or from an existing Secret (`server.gateway.secureAccess.existingSecret`) with
two data entries: `keys` (`a=<base64-secret>[,b=...]`) and `active-key` (`a`).
The chart delivers the Secret to the server and gateway containers as
environment variables, so key material stays out of values, the server
ConfigMap, and pod args. The two forms are mutually exclusive.

## Configuration

The following table lists the configurable parameters of the chart and their default values.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| configToml | string | `"[server]\nhost = \"0.0.0.0\"\nport = 80\napi_key = \"\"\n\n[log]\nlevel = \"INFO\"\n\n[runtime]\ntype = \"kubernetes\"\nexecd_image = \"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.1.0\"\n\n[kubernetes]\nkubeconfig_path = \"\"\nnamespace = \"opensandbox\"\ninformer_enabled = true\ninformer_resync_seconds = 300\ninformer_watch_timeout_seconds = 60\nsnapshot_create_timeout_seconds = 900\nworkload_provider = \"batchsandbox\"\nbatchsandbox_template_file = \"/etc/opensandbox/example.batchsandbox-template.yaml\"\n\n[egress]\nimage = \"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/egress:v1.1.7\"\nmode = \"dns+nft\"\n"` | Server config (TOML). Mounted at /etc/opensandbox/config.toml. |
| fullnameOverride | string | `"opensandbox-server"` | Resource names and app.kubernetes.io/name are fixed to this value, independent of release name |
| imagePullSecrets | list | `[]` | Image pull secrets for the server deployment. Each entry: {name: <secret-name>}. |
| nameOverride | string | `""` | Override the name of the chart |
| namespaceOverride | string | `""` | Override the namespace (default: opensandbox-system) |
| server.affinity | object | `{}` | Affinity for the server pod. |
| server.containerSecurityContext | object | `{}` | Container-level security context for the server container. |
| server.env | list | `[]` | Additional environment variables for the server container. |
| server.gateway.affinity | object | `{}` | Affinity for the ingress gateway pod. |
| server.gateway.containerSecurityContext | object | `{}` | Container-level security context for the ingress gateway container. |
| server.gateway.dataplaneNamespace | string | `"opensandbox"` | Namespace where the gateway dataplane workloads run. |
| server.gateway.enabled | bool | `false` | Whether to deploy the ingress gateway alongside the server. |
| server.gateway.env | list | `[]` | Additional environment variables for the ingress-gateway container (e.g. OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_SERVICE_NAME for OTLP metrics). |
| server.gateway.gatewayRouteMode | string | `"header"` | Gateway route mode: header or uri. |
| server.gateway.host | string | `"opensandbox.example.com"` | Gateway host/address returned to clients when the gateway is enabled. |
| server.gateway.image | object | `{"repository":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress","tag":"v1.0.10"}` | Gateway image configuration. |
| server.gateway.image.repository | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/ingress"` | Gateway image repository. |
| server.gateway.image.tag | string | `"v1.0.10"` | Gateway image tag. |
| server.gateway.logLevel | string | `"info"` | Gateway log level. |
| server.gateway.nodeSelector | object | `{}` | Node selector for the ingress gateway pod. |
| server.gateway.podAnnotations | object | `{}` | Extra annotations for the ingress gateway pod. |
| server.gateway.podLabels | object | `{}` | Extra labels for the ingress gateway pod. |
| server.gateway.podSecurityContext | object | `{}` | Pod-level security context for the ingress gateway pod. |
| server.gateway.port | int | `28888` | Gateway service port. |
| server.gateway.priorityClassName | string | `""` | Priority class name for the ingress gateway pod. |
| server.gateway.providerType | string | `"batchsandbox"` | Gateway provider type (e.g. batchsandbox). |
| server.gateway.replicaCount | int | `2` | Number of gateway replicas. |
| server.gateway.resources | object | `{"limits":{"cpu":"2","memory":"8Gi"},"requests":{"cpu":"1","memory":"4Gi"}}` | Resource requests and limits for the gateway. |
| server.gateway.secureAccess.activeKey | string | `""` | Active signing key id, one character in [0-9a-z]. |
| server.gateway.secureAccess.existingSecret | string | `""` | Name of an existing Secret holding the signing keys (keys + active-key), as an alternative to plaintext `keys` above (mutually exclusive). The Secret must carry two entries:   keys:       the key ring, "a=<base64-secret>[,b=<base64-secret>...]"   active-key: the active signing key id, one character in [0-9a-z] The chart wires it into both containers as environment variables (server: OPENSANDBOX_SECURE_ACCESS_*; gateway: $(...) expansion in the `--secure-access-keys` arg), so key material never appears in values, the server ConfigMap, or pod args. Env-sourced Secrets are read once at container start: after updating the Secret in place, `kubectl rollout restart` the server and gateway Deployments (or version the Secret name to get a spec-driven rollout). |
| server.gateway.secureAccess.keys | list | `[]` | List of signing keys. Each entry: { key_id: "a", key: "<base64-secret>" }. key_id must be exactly one character in [0-9a-z]. |
| server.gateway.tolerations | list | `[]` | Tolerations for the ingress gateway pod. |
| server.gateway.topologySpreadConstraints | list | `[]` | Topology spread constraints for the ingress gateway pod. |
| server.image | object | `{"repository":"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server","tag":"v0.2.2"}` | Server image configuration |
| server.image.repository | string | `"sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/server"` | Server image repository. |
| server.image.tag | string | `"v0.2.2"` | Server image tag. Defaults to the chart appVersion when empty. |
| server.nodeSelector | object | `{}` | Node selector for the server pod. |
| server.podAnnotations | object | `{}` | Extra annotations for the server pod. |
| server.podLabels | object | `{}` | Extra labels for the server pod. |
| server.podSecurityContext | object | `{}` | Pod-level security context for the server pod. |
| server.priorityClassName | string | `""` | Priority class name for the server pod. |
| server.replicaCount | int | `2` | Number of server replicas |
| server.resources | object | `{"limits":{"cpu":"2","memory":"8Gi"},"requests":{"cpu":"1","memory":"4Gi"}}` | Resource requests and limits |
| server.service.nodePort | string | `""` | Node port to bind when type is not ClusterIP. Empty lets Kubernetes allocate one from the cluster node-port range. |
| server.service.type | string | `"ClusterIP"` | Service type for the server. Set to NodePort or LoadBalancer to reach the server from outside the cluster. |
| server.tolerations | list | `[]` | Tolerations for the server pod. |
| server.topologySpreadConstraints | list | `[]` | Topology spread constraints for the server pod. |
| server.volumeMounts | list | `[]` | Additional volume mounts for the server container. |
| server.volumes | list | `[]` | Additional volumes for the server pod. |

Versioning note:

- The release install and upgrade examples pin `server.image.tag` to `APP_VERSION` so the selected chart release deploys the matching server image.
- The chart package `version` and the image/app `appVersion` are intentionally
  separate. A server release branch or tag does not automatically imply a new
  Helm chart package version.
- If you want the chart to deploy a specific server release, override
  `server.image.tag` explicitly or consume a Helm package release whose chart
  version was published for that purpose.

**Gateway**: When `server.gateway.enabled=true`, the chart writes `[ingress] mode = "gateway"` in config.toml and deploys **components/ingress** Deployment/Service/RBAC; gateway `--mode` matches config. External access must be configured separately.

Set `[kubernetes].namespace` in config for the sandbox workload namespace and create that namespace before submitting workloads. Configure `OPENSANDBOX_SERVER_API_KEY` from a Secret in production. The container and its Service use port `80`; keep `[server].port = 80` when replacing `configToml`. The Service is `ClusterIP` unless `server.service.type` says otherwise.

## Upgrade and uninstall

```bash
helm upgrade opensandbox-server "${CHART_URL}" \
  --namespace opensandbox-system \
  --set-string server.image.tag="${APP_VERSION}" \
  --values values-server.yaml
helm uninstall opensandbox-server -n opensandbox-system
```

## References

- [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
- [Helm deployment docs](../../docs/HELM-DEPLOYMENT.md)
