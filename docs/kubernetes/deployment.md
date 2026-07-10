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
