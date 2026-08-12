# opensandbox-server Helm Chart

OpenSandbox Lifecycle API server: provides sandbox create/delete and other lifecycle APIs, typically used with BatchSandbox/Pool on Kubernetes.

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.0+
- OpenSandbox CRDs installed (deploy opensandbox-controller first)
- A sandbox workload namespace matching `[kubernetes].namespace` in `configToml` (default: `opensandbox`)

## Install from a GitHub Release

Choose a published `opensandbox-server` chart from [GitHub Releases](https://github.com/opensandbox-group/OpenSandbox/releases?q=helm%2Fopensandbox-server&expanded=true). The release tag uses the application version, while the package filename uses the chart version shown in the release notes.

```bash
APP_VERSION="<app-version>"
CHART_VERSION="<chart-version>"
CHART_URL="https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-server/${APP_VERSION}/opensandbox-server-${CHART_VERSION}.tgz"

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

| Parameter | Description | Default |
|-----------|-------------|---------|
| `server.image.repository` | Server image repository | `sandbox-registry.../opensandbox/server` |
| `server.image.tag` | Server image tag | See `values.yaml` |
| `server.replicaCount` | Server replicas | `2` |
| `server.env` | Additional environment variables, including Secret-backed API key configuration | `[]` |
| `server.resources` | CPU/memory requests and limits | See values.yaml |
| `namespaceOverride` | Deployment namespace | `opensandbox-system` |
| `configToml` | Complete config.toml content, mounted at `/etc/opensandbox/config.toml` | See values.yaml |
| `server.gateway.enabled` | When true: set server config to gateway and deploy components/ingress gateway | `false` |
| `server.gateway.host` | config `gateway.address` (address returned to clients) | `opensandbox.example.com` |
| `server.gateway.gatewayRouteMode` | server config and gateway route mode (header/uri) | `header` |
| `server.gateway.env` | Additional environment variables for the ingress-gateway container (e.g. `OTEL_EXPORTER_OTLP_ENDPOINT`) | `[]` |
| `server.gateway.secureAccess.keys` | OSEP-0011 signing key ring, plaintext in values | `[]` |
| `server.gateway.secureAccess.existingSecret` | Name of a Secret holding `keys` + `active-key`; alternative to plaintext `keys` | `""` |
| `server.gateway.*` | Gateway image, replicas, port, dataplaneNamespace, providerType, resources | See values.yaml |

Versioning note:

- The release install and upgrade examples pin `server.image.tag` to `APP_VERSION` so the selected chart release deploys the matching server image.
- The chart package `version` and the image/app `appVersion` are intentionally
  separate. A server release branch or tag does not automatically imply a new
  Helm chart package version.
- If you want the chart to deploy a specific server release, override
  `server.image.tag` explicitly or consume a Helm package release whose chart
  version was published for that purpose.

**Gateway**: When `server.gateway.enabled=true`, the chart writes `[ingress] mode = "gateway"` in config.toml and deploys **components/ingress** Deployment/Service/RBAC; gateway `--mode` matches config. External access must be configured separately.

Set `[kubernetes].namespace` in config for the sandbox workload namespace and create that namespace before submitting workloads. Configure `OPENSANDBOX_SERVER_API_KEY` from a Secret in production. The container and `ClusterIP` Service use port `80`; keep `[server].port = 80` when replacing `configToml`.

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
