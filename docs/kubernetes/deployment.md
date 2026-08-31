---
title: Kubernetes Deployment
description: Deploy OpenSandbox components on Kubernetes with Helm charts.
---

# Kubernetes Deployment

This guide covers deploying OpenSandbox on Kubernetes, including the operator, CRDs, and supporting components.

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.x
- `kubectl` configured for your cluster

## Install CRDs and Operator

The OpenSandbox Kubernetes operator manages `BatchSandbox`, `Pool`, and `SandboxSnapshot` custom resources.

For installation instructions and Helm chart values, see the [Kubernetes operator documentation](https://github.com/opensandbox-group/OpenSandbox/tree/main/kubernetes).

## Install the Lifecycle Server

Install the controller and CRDs before the lifecycle server. The server runs in the cluster with a `ServiceAccount` and uses the Kubernetes API to create and manage sandbox resources.

Choose a published `opensandbox-server` chart from [GitHub Releases](https://github.com/opensandbox-group/OpenSandbox/releases?q=helm%2Fopensandbox-server&expanded=true), then set both versions from that release:

```sh
CHART_VERSION="<chart-version>"
APP_VERSION="<app-version>"
CHART_URL="https://github.com/opensandbox-group/OpenSandbox/releases/download/helm/opensandbox-server/${CHART_VERSION}/opensandbox-server-${CHART_VERSION}.tgz"
```

::: info Versioning
The release tag and `.tgz` filename identify the Helm chart version. The server application version is independent and is listed on each GitHub Release.
:::

### Configure API authentication

By default, the server refuses to start without an API key in a non-interactive container. Create both the control-plane namespace and the default sandbox workload namespace, then store the key in a Kubernetes `Secret`:

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

Reference the Secret from a values file:

```yaml
# values-server.yaml
server:
  replicaCount: 2
  env:
    - name: OPENSANDBOX_SERVER_API_KEY
      valueFrom:
        secretKeyRef:
          name: opensandbox-api-key
          key: api-key
```

Use an external secret manager instead of creating the Secret manually in production environments.

The chart installs the server into `opensandbox-system`, while the default `configToml` creates sandbox and pool resources in `opensandbox`. If you change `[kubernetes].namespace` in `configToml`, create that namespace instead of `opensandbox` before submitting workloads.

### Use PostgreSQL for server persistence

Create a Secret containing the PostgreSQL connection string:

```bash
read -s OPENSANDBOX_POSTGRESQL_DSN
kubectl create secret generic opensandbox-postgresql \
  --namespace opensandbox-system \
  --from-literal=dsn="${OPENSANDBOX_POSTGRESQL_DSN}" \
  --dry-run=client -o yaml | kubectl apply -f -
unset OPENSANDBOX_POSTGRESQL_DSN
```

In `values-server.yaml`, set `server.replicaCount` to `1`, add the Secret-backed
environment variable below, and add the shown `[store]` tables to the complete
`configToml` value:

```yaml
server:
  replicaCount: 1
  env:
    - name: OPENSANDBOX_STORE_POSTGRESQL_DSN
      valueFrom:
        secretKeyRef:
          name: opensandbox-postgresql
          key: dsn

configToml: |
  # Keep the rest of the chart's complete server configuration here.
  [store]
  type = "postgresql"

  [store.postgresql]
  min_pool_size = 1
  max_pool_size = 10
```

::: warning
Snapshot recovery is not coordinated across server replicas. Keep
`server.replicaCount: 1` when replicas use the same PostgreSQL database.
:::

### Install and verify

Inspect all available settings before installation:

```sh
helm show values "${CHART_URL}"
```

Install the server from the versioned chart artifact:

```sh
helm install opensandbox-server "${CHART_URL}" \
  --namespace opensandbox-system \
  --set-string server.image.tag="${APP_VERSION}" \
  --values values-server.yaml
```

Wait for the Deployment and verify the API health endpoint:

```sh
kubectl rollout status deployment/opensandbox-server \
  --namespace opensandbox-system \
  --timeout=180s

kubectl port-forward \
  --namespace opensandbox-system \
  service/opensandbox-server 8080:80
```

In another terminal:

```sh
curl --fail http://127.0.0.1:8080/health
```

### Important values

| Value | Purpose | Notes |
|-------|---------|-------|
| `server.image.repository` | Server image registry and repository | Override for a private mirror or custom build. |
| `server.image.tag` | Server image version | The release install command pins it to `APP_VERSION`. |
| `server.replicaCount` | Number of server Pods | Defaults to `2`. |
| `server.env` | Additional container environment variables | Use it with `secretKeyRef` for `OPENSANDBOX_SERVER_API_KEY`. |
| `configToml` | Complete server configuration | Mounted at `/etc/opensandbox/config.toml`; overriding it replaces the complete default TOML, including the workload namespace. |
| `server.gateway.enabled` | Deploy the ingress gateway with the server | Defaults to `false`. |
| `server.service.type` | Service type for the server | Defaults to `ClusterIP`. Use `NodePort` or `LoadBalancer` for access from outside the cluster; pin the port with `server.service.nodePort`. |
| `namespaceOverride` | Namespace used by chart resources | Defaults to `opensandbox-system`. |

The server container and its Service use port `80`. Keep `[server].port = 80` when replacing `configToml` unless the chart templates are also updated to use a different port. The Service is `ClusterIP` by default; set `server.service.type` to reach the server from outside the cluster.

### Configure egress sidecar resources

When a create request includes `networkPolicy`, the lifecycle server adds an egress sidecar to each non-pooled sandbox Pod. Namespace `LimitRange` defaults apply to this container when it does not declare resources, which can reserve substantially more capacity than basic DNS/nft enforcement needs.

Add optional resource settings to the `[egress]` section of `configToml`:

```toml
[egress]
image = "opensandbox/egress:v1.1.7"
requests = { cpu = "25m", memory = "64Mi" }
limits = { cpu = "250m", memory = "256Mi" }
```

You can omit either `requests` or `limits`. Treat these values as a starting point and tune them from observed usage; Credential Vault and transparent mitmproxy generally need more headroom than basic DNS/nft enforcement.

### Upgrade

Select the application and chart versions from the target GitHub Release, update `CHART_URL`, and run:

```sh
helm upgrade opensandbox-server "${CHART_URL}" \
  --namespace opensandbox-system \
  --set-string server.image.tag="${APP_VERSION}" \
  --values values-server.yaml
```

For the complete values reference and local development installation, see the [`opensandbox-server` chart README](https://github.com/opensandbox-group/OpenSandbox/tree/main/kubernetes/charts/opensandbox-server).

## Operator Metrics

The operator (controller-manager) exposes standard [controller-runtime](https://book.kubebuilder.io/reference/metrics) Prometheus metrics — reconcile rate and latency (`controller_runtime_reconcile_*`), work-queue depth, client-go request counts, and Go runtime stats. The endpoint is **disabled by default** (`--metrics-bind-address=0`).

Enable it through the `opensandbox-controller` chart values:

| Value | Default | Purpose |
|-------|---------|---------|
| `controller.metrics.enabled` | `false` | Expose the `/metrics` endpoint (sets `--metrics-bind-address`) |
| `controller.metrics.port` | `8080` | Port for the metrics endpoint |
| `controller.metrics.secure` | `false` | Serve over HTTPS with authn/authz (`--metrics-secure`); set `false` for plain HTTP scraping |

```yaml
controller:
  metrics:
    enabled: true
    port: 8080
    secure: false   # plain HTTP, e.g. for a PodMonitoring/ServiceMonitor scrape
```

- With `secure: false` the endpoint is plain HTTP and can be scraped directly (no TLS or bearer token).
- With `secure: true` the controller-runtime filter authenticates and authorizes each scrape via `TokenReview`/`SubjectAccessReview`. The chart then provisions two `ClusterRole`s automatically:
  - `opensandbox-metrics-auth-role` (bound to the manager) — lets the controller run the auth checks.
  - `opensandbox-metrics-reader` (**not** bound by the chart) — grants `get` on the `/metrics` non-resource URL. Bind it to your scraper's `ServiceAccount` (e.g. Prometheus) and have the scraper present that account's bearer token.

Point your Prometheus stack at the `metrics` container port (for example via a `ServiceMonitor` or `PodMonitoring`).

## Configure the Server for Kubernetes

Generate a Kubernetes-oriented server config:

```bash
opensandbox-server init-config ~/.sandbox.toml --example k8s
```

Key Kubernetes-specific configuration sections:

| Section | Purpose |
|---------|---------|
| `[kubernetes]` | Workload provider, BatchSandbox template file |
| `[agent_sandbox]` | Agent sandbox settings |
| `[ingress]` | Ingress gateway for sandbox traffic routing |
| `[secure_runtime]` | Secure container runtime (gVisor, Kata) |

See [Configuration](/getting-started/configuration) for the full reference.

## Components on Kubernetes

| Component | Deployment | Purpose |
|-----------|-----------|---------|
| Server | Deployment | Lifecycle control plane |
| Operator | Deployment | Manages BatchSandbox/Pool CRDs |
| Ingress | DaemonSet/Deployment | Routes traffic to sandboxes |
| Egress | Sidecar | Per-sandbox egress policy enforcement |
| Execd | Built into sandbox images | In-sandbox execution |

## Related

- [Kubernetes Overview](/kubernetes/) — Operator features and CRDs
- [Pause & Resume](/guides/pause-resume) — Snapshot-based pause/resume on Kubernetes
- [Secure Container](/guides/secure-container) — gVisor and Kata on Kubernetes
- [Network Isolation](/architecture/network-isolation) — Egress policy design for Kubernetes
