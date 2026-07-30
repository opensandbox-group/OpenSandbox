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
