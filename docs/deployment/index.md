---
title: Kubernetes Deployment
description: Deploy OpenSandbox on Kubernetes — CRDs, controller, lifecycle server, and optional components, with Helm deployment order and configuration.
---

# Kubernetes Deployment

This guide covers deploying OpenSandbox on Kubernetes: the cluster foundation
(CRDs), the controller, the lifecycle server, and the optional components
(ingress gateway, node agent, fast-sandbox runtime).

Charts are versioned sources in the [OpenSandbox repository](https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts).
They are installed from a checkout of a `release-X.Y.Z` tag — standalone chart
packages are not published.

## Prerequisites

- Kubernetes 1.21.1+
- Helm 3.x
- `kubectl` configured for your cluster

## Deployment Order

Install the charts in this order:

```text
base → opensandbox-controller → fast-sandbox* → opensandbox-server → optional components
```

\* `fast-sandbox` is optional, but when used it must be installed **before** the
server.

| Step | Chart | Why it comes here |
|------|-------|-------------------|
| 1 | `base` | Owns the `sandbox.opensandbox.io` CRDs, the fast-sandbox CRDs (`sandbox.fast.io`), and their RBAC. Everything else depends on these objects existing. Install once per cluster. |
| 2 | `opensandbox-controller` | Reconciles `BatchSandbox`, `Pool`, and `SandboxSnapshot` objects created by you and by the server. Requires the CRDs from `base`. |
| 3 | `fast-sandbox` (optional) | Firecracker runtime (`sandbox.fast.io`). Must be installed **before the server**: the server's `[runtime]`/fsb configuration points at the FastPath gRPC endpoint (`fast-sandbox-fastpath...svc:9090`) and watches `sandbox.fast.io` objects at startup. Also consumes the ServiceAccounts from `base` — keep the namespace values in sync with it. Skip if you only use the default Kubernetes runtime. |
| 4 | `opensandbox-server` | The lifecycle REST API that creates and deletes sandboxes. Requires the CRDs from `base`, a running controller, and — when serving sandboxes through the fsb runtime — the FastPath endpoint from the previous step. |
| 5 | `ingress-gateway` / `opensandbox-node-agent` | Optional, order-independent. The gateway is announced by the server through `server.gateway.*`; the node agent collects sandbox data. |

If you use the umbrella chart, this order is handled for you in a single release.

## Install

Check out the version you want to deploy:

```bash
git clone https://github.com/opensandbox-group/OpenSandbox.git
cd OpenSandbox
git checkout release-1.1.0   # or main for development
```

### Option 1: Umbrella chart (recommended)

One release installs every component with the correct ordering:

```bash
cd manifests/charts

# Package the sub-charts (charts/ is git-ignored, rebuilt every time)
helm dependency build opensandbox

helm install opensandbox opensandbox \
  --namespace opensandbox-system \
  --create-namespace
```

Optional components default to off; enable what you need:

```bash
helm install opensandbox opensandbox \
  --namespace opensandbox-system \
  --create-namespace \
  --set ingress-gateway.enabled=true
```

### Option 2: Per-component releases

```bash
# 1. Cluster foundation: CRDs + RBAC
helm install base manifests/charts/base

# 2. Controller
helm install opensandbox-controller manifests/charts/controller \
  --namespace opensandbox-system \
  --create-namespace

# 3. Lifecycle server
helm install opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --create-namespace
```

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
  replicaCount: 1
  env:
    - name: OPENSANDBOX_SERVER_API_KEY
      valueFrom:
        secretKeyRef:
          name: opensandbox-api-key
          key: api-key
```

Use an external secret manager instead of creating the Secret manually in production environments.

The chart installs the server into `opensandbox-system`, while the default `configToml` creates sandbox and pool resources in `opensandbox`. If you change `[kubernetes].namespace` in `configToml`, create that namespace instead of `opensandbox` before submitting workloads.

::: warning Single-active Server default
The chart defaults to `server.replicaCount: 1`. Keep one active Lifecycle Server
unless you deliberately use the PostgreSQL-backed Kubernetes public snapshot
topology documented below. That exception coordinates public snapshots only; it
does not provide general multi-replica Server HA. The Server Deployment uses the
`Recreate` strategy so an upgrade stops the active Server before starting its
replacement; expect a brief API interruption during upgrades.
:::

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

In `values-server.yaml`, keep the default `server.replicaCount` at `1`, add the
Secret-backed environment variable below, and add the shown `[store]` tables to
the complete `configToml` value:

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
  snapshot_recovery_interval_seconds = 15
```

::: info
The chart default remains one Server replica. You may explicitly set
`server.replicaCount: 2` for multi-active public snapshot handling only when
both replicas use the same PostgreSQL database and the Kubernetes runtime.
SQLite and Docker snapshot execution do not support this multi-active topology.
:::

### Install the server with the API key and verify

Install the server referencing your values file:

```bash
helm install opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --create-namespace \
  --values values-server.yaml
```

Wait for the Deployment and verify the API health endpoint:

```bash
kubectl rollout status deployment/opensandbox-server \
  --namespace opensandbox-system \
  --timeout=180s

kubectl port-forward \
  --namespace opensandbox-system \
  service/opensandbox-server 8080:80
```

In another terminal:

```bash
curl --fail http://127.0.0.1:8080/health
```

### Important values

| Value | Purpose | Notes |
|-------|---------|-------|
| `server.image.repository` | Server image registry and repository | Override for a private mirror or custom build. |
| `server.image.tag` | Server image version | Defaults to the `release-<appVersion>` image published with the chart version; override only for custom builds. |
| `server.replicaCount` | Number of server Pods | Defaults to `1`; general multi-replica Server HA is not supported yet. |
| `server.env` | Additional container environment variables | Use it with `secretKeyRef` for `OPENSANDBOX_SERVER_API_KEY`. |
| `configToml` | Complete server configuration | Mounted at `/etc/opensandbox/config.toml`; overriding it replaces the complete default TOML, including the workload namespace. |
| `server.gateway.enabled` | Announce an ingress gateway to clients | Defaults to `false`. The gateway itself is deployed by the [ingress-gateway chart](https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts/ingress-gateway). |
| `server.service.type` | Service type for the server | Defaults to `ClusterIP`. Use `NodePort` or `LoadBalancer` for access from outside the cluster; pin the port with `server.service.nodePort`. |
| `namespaceOverride` | Namespace used by chart resources | Defaults to `opensandbox-system`. |

The server container and its Service use port `80`. Keep `[server].port = 80` when replacing `configToml` unless the chart templates are also updated to use a different port. The Service is `ClusterIP` by default; set `server.service.type` to reach the server from outside the cluster.

### Configure egress sidecar resources

When a create request includes `networkPolicy`, the lifecycle server adds an egress sidecar to each non-pooled sandbox Pod. Namespace `LimitRange` defaults apply to this container when it does not declare resources, which can reserve substantially more capacity than basic DNS/nft enforcement needs.

Add optional resource settings to the `[egress]` section of `configToml`:

```toml
[egress]
image = "opensandbox/egress:release-1.1.0"
requests = { cpu = "25m", memory = "64Mi" }
limits = { cpu = "250m", memory = "256Mi" }
```

You can omit either `requests` or `limits`. Treat these values as a starting point and tune them from observed usage; Credential Vault and transparent mitmproxy generally need more headroom than basic DNS/nft enforcement.

### Optional: ingress gateway

Deploy the gateway chart, then announce it from the server so clients receive the gateway address:

```bash
helm install ingress-gateway manifests/charts/ingress-gateway \
  --namespace opensandbox-system

helm upgrade opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --set server.gateway.enabled=true \
  --set server.gateway.host=gateway.example.com \
  --values values-server.yaml
```

Keep `server.gateway.gatewayRouteMode` in sync with `gateway.gatewayRouteMode`
of the gateway chart. For signed, expiring sandbox routes, configure the
shared secure-access key ring on both charts — see [secure-access keys](https://github.com/opensandbox-group/OpenSandbox/blob/main/manifests/HELM-DEPLOYMENT.md#secure-access-keys-osep-0011).

### Optional: fast-sandbox runtime

The `fast-sandbox` chart adds the Firecracker (`sandbox.fast.io`) runtime.
Install it **before the lifecycle server** (see [Deployment Order](#deployment-order)):
the server's `[runtime]`/fsb configuration points at the FastPath gRPC endpoint
this chart creates. It also requires `base` first, KVM-capable nodes, and
companion images built from a pinned upstream commit — see the [fast-sandbox runtime deployment guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/manifests/HELM-DEPLOYMENT.md#fast-sandbox-runtime-firecracker).

## Upgrade

Upgrade the umbrella release from a newer checkout:

```bash
git fetch --tags
git checkout release-1.2.0

cd manifests/charts
helm dependency build opensandbox
helm upgrade opensandbox opensandbox --namespace opensandbox-system
```

For a per-component server release:

```bash
git checkout release-1.2.0
helm upgrade opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --values values-server.yaml
```

For the complete values reference and local development installation, see the
[`opensandbox-server` chart README](https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts/server).

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

### Business capacity metrics

The elected controller also exports low-cardinality business capacity metrics over OTLP/HTTP when `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` or `OTEL_EXPORTER_OTLP_ENDPOINT` is set. This is independent of the controller-runtime Prometheus endpoint and remains disabled when neither variable is configured.

```yaml
extraEnv:
  - name: OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
    value: http://otel-collector.observability:4318/v1/metrics
```

| Metric | Unit | Attributes | Description |
|--------|------|------------|-------------|
| `controller.pool.pods` | `{pod}` | `namespace`, `pool_name`, `state` | Current Pool Pods, where `state` is `total`, `allocated`, `available`, or `updated` |
| `controller.pool.cpu.requested` | `{cpu}` | `namespace`, `pool_name`, `state` | Scheduler-equivalent CPU requests represented by total, allocated, or available Pool Pods |
| `controller.pool.memory.requested` | `By` | `namespace`, `pool_name`, `state` | Scheduler-equivalent memory requests represented by total, allocated, or available Pool Pods |
| `controller.batchsandbox.count` | `{batchsandbox}` | `namespace`, `phase`, `allocation_mode` | Current BatchSandbox objects by lifecycle phase and pool/direct mode |
| `controller.batchsandbox.pods` | `{pod}` | `namespace`, `state`, `allocation_mode` | Desired, current, allocated, and ready BatchSandbox Pod counts |
| `controller.capacity.collect.duration` | `s` | None | Time spent reading cached objects and collecting one capacity snapshot |

The metrics deliberately omit sandbox, BatchSandbox, and Pod identifiers. Only the leader exports them, so multiple controller replicas do not duplicate cluster totals. An unset initial BatchSandbox phase is exported as `Unknown`. Derive Pool utilization from `allocated / total` and calculate peak, valley, or percentile capacity in the telemetry backend. Actual CPU and memory usage remains available from kubelet/cAdvisor rather than being duplicated here.

Each collection reads all Pools and BatchSandboxes from the controller manager's informer cache, then performs one cached, owner-UID-indexed Pod list for every non-deleting Pool. Collection CPU and memory therefore grow linearly with the number of cached Pools, BatchSandboxes, and Pool-owned Pods, without issuing one API-server list request per Pool. The OpenTelemetry periodic reader exports every 60 seconds by default; `OTEL_METRIC_EXPORT_INTERVAL` can change the interval in milliseconds. Monitor `controller.capacity.collect.duration` and validate the target cluster scale before shortening that interval. If OTLP setup fails after an endpoint is configured, the controller continues reconciling and logs the failed setup stage together with the endpoint environment variable and a credential-stripped endpoint.

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
| CRDs + RBAC (`base`) | Cluster-scoped | `BatchSandbox`, `Pool`, `SandboxSnapshot` and `sandbox.fast.io` API types |
| Server | Deployment | Lifecycle control plane |
| Controller (operator) | Deployment | Manages BatchSandbox/Pool CRDs |
| Ingress gateway | Deployment | Routes traffic to sandboxes |
| Egress | Sidecar | Per-sandbox egress policy enforcement |
| Execd | Built into sandbox images | In-sandbox execution |
| Node agent | DaemonSet | Optional node-level sandbox data collection |
| fast-sandbox | Deployment + DaemonSet | Optional Firecracker control plane and node runtime |

## Uninstall

```bash
helm uninstall opensandbox -n opensandbox-system
```

CRDs carry the `helm.sh/resource-policy: keep` annotation and are retained
across uninstalls; the `opensandbox-dataplane` namespace (created by `base`
for fast-sandbox) is kept as well. Delete them manually once their data is no
longer needed:

```bash
kubectl delete crd batchsandboxes.sandbox.opensandbox.io \
  pools.sandbox.opensandbox.io \
  sandboxsnapshots.sandbox.opensandbox.io
kubectl delete namespace opensandbox-dataplane
```

## Related

- [Kubernetes Overview](/architecture/control-plane/operator) — Operator features and CRDs
- [Pause & Resume](/guides/pause-resume) — Snapshot-based pause/resume on Kubernetes
- [Secure Container](/guides/secure-container) — gVisor and Kata on Kubernetes
- [Network Isolation](/architecture/network/network-isolation) — Egress policy design for Kubernetes
- [Helm charts source](https://github.com/opensandbox-group/OpenSandbox/tree/main/manifests/charts) — Chart sources and values reference
