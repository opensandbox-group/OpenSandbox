---
title: Developer Console
description: Optional React console and FastAPI BFF for sandbox lifecycle operations, deployed beside the lifecycle server.
---

# Developer Console

OpenSandbox includes an **optional** developer console under [`console/`](https://github.com/opensandbox-group/OpenSandbox/tree/main/console) in the monorepo. It provides a web UI for common lifecycle tasks (list, create, renew, delete, endpoints, diagnostics) without putting API keys in the browser.

This guide is the **published entry point**. Detailed BFF configuration, API notes, and Kubernetes examples live next to the code:

| Topic | Location |
|-------|----------|
| Quick start | [console/README.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/README.md) |
| Architecture | [console/docs/01-architecture.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/01-architecture.md) |
| BFF env / secrets | [console/docs/02-bff-config-and-secrets.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/02-bff-config-and-secrets.md) |
| Build & deploy | [console/docs/06-build-and-deploy.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/06-build-and-deploy.md) |
| Feature status | [console/docs/05-feature-matrix.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/05-feature-matrix.md) |

## Relation to OSEP-0006

[OSEP-0006](../community/oseps.md) describes a phased console and server-side auth. The in-tree implementation is a **BFF adjunct**:

- Tenant **API keys** and **`BFF_ADMIN_TOKEN`** stay on the server/BFF; the SPA uses **httpOnly cookies**.
- **Sandbox history / usage analytics** (PostgreSQL) are planned for a later release; the MVP console uses the Lifecycle API only. See [console/docs/07-sandbox-history.md](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/07-sandbox-history.md).

## High-level layout

```text
Browser → Console Web (static) → Console BFF (/api) → Lifecycle API (/v1)
                                      ↓
                              tenants.toml (same as server)
```

## Minimum deployment

1. Deploy the lifecycle server (see [Kubernetes deployment](../deployment/)).
2. Provide **`opensandbox-tenants`** ConfigMap (multi-tenant) or equivalent `tenants.toml` for the BFF.
3. Build and apply manifests under `console/k8s/` (see [deployment checklist](https://github.com/opensandbox-group/OpenSandbox/blob/main/console/docs/04-deployment-checklist.md)).

The console is **not** yet bundled in the umbrella Helm chart; run it as separate Deployments.

## CI

Pull requests touching `console/` run the **Console Tests** workflow (`console/web` build and BFF compile).
