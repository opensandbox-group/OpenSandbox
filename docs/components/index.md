---
title: Components
description: Overview of OpenSandbox system components — server, execd, ingress, and egress.
---

# Components

OpenSandbox is composed of several system components that work together to provide sandbox lifecycle management, in-sandbox execution, and network control.

## Architecture

```
┌─────────────┐     ┌─────────────┐
│   SDK/CLI   │────▶│   Ingress   │──┐
└─────────────┘     └─────────────┘  │
                                     ▼
┌─────────────┐     ┌─────────────────────────┐
│   Server    │────▶│      Sandbox            │
│ (Control    │     │  ┌───────┐  ┌─────────┐ │
│  Plane)     │     │  │ execd │  │ egress  │ │
└─────────────┘     │  └───────┘  └─────────┘ │
                    └─────────────────────────┘
```

## Components

| Component | Description | Details |
|-----------|-------------|---------|
| [Server](/components/server) | FastAPI-based lifecycle control plane. Creates, monitors, and terminates sandboxes across Docker and Kubernetes. | Python, REST API |
| [Execd](/components/execd) | In-sandbox execution daemon. Provides HTTP APIs for shell commands, file operations, PTY sessions, and code interpreters. | Go, Gin framework |
| [Ingress](/components/ingress) | HTTP/WebSocket reverse proxy for Kubernetes sandbox routing. Routes traffic to sandbox instances via header or URI mode. | Go |
| [Egress](/components/egress) | Per-sandbox FQDN-based egress control sidecar. Enforces allowlists, credential injection, and network policy. | Go |

## Related

- [Architecture Overview](/architecture/) — How components fit together
- [Kubernetes Deployment](/kubernetes/) — Deploying components on Kubernetes
- [API Specs](/api/) — OpenAPI specifications
