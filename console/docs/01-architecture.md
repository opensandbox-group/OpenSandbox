# Architecture

## Components

```mermaid
flowchart LR
  Browser[Browser SPA]
  BFF[Console BFF]
  Life[Lifecycle Server /v1]
  K8s[K8s API read-only]
  Browser --> BFF
  BFF -->|tenant key proxy| Life
  BFF -->|admin optional| K8s
```

- **SPA**: static assets (nginx or same-origin reverse proxy). No Lifecycle API key in the bundle.
- **BFF**: sessions, tenant resolution, admin multi-tenant list aggregation, `runtimeSummary`.
- **Server**: unchanged Lifecycle behavior; multi-tenant isolation via `[tenants]` and API keys.

## Dual login

| Role | Login | BFF session | Data scope |
|------|-------|-------------|------------|
| Developer | `POST /api/auth/tenant` + tenant API key | tenant session (tenant name only) | single tenant / namespace |
| Admin | `POST /api/auth/admin` + `BFF_ADMIN_TOKEN` | admin session | all tenants via server-side key rotation |

Admin mutations proxy to the owning tenant key where implemented.

## Shared `tenants.toml`

Server and BFF should mount the **same** `tenants.toml` (ConfigMap or Secret):

- Server validates keys and injects tenant context.
- BFF validates keys for login; admin aggregation uses keys **only on the server**, never returned to the browser.

## Platform constraints (do not mislead in UI)

- **Pool API (`/v1/pools`)** is not tenant-isolated.
- With `[tenants]` enabled, there is no global `server.api_key`.
- Proxy routes still require an API key (BFF can proxy on behalf of the tenant).
