# BFF HTTP API (SPA)

Base path: `/api`. Cookie session required except login and health.

## Authentication

### `POST /api/auth/tenant`

```json
{ "apiKey": "..." }
```

- 200: `{ "role": "tenant", "tenant": "...", "namespace": "..." }` + Set-Cookie
- 401: invalid key

### `POST /api/auth/admin`

```json
{ "adminToken": "..." }
```

- 200: `{ "role": "admin" }` + Set-Cookie

### `POST /api/auth/logout`

Clears session.

### `GET /api/auth/me`

`{ "role": "tenant"|"admin", "tenant"?, "namespace"? }`

## Tenant sandboxes

Proxies Lifecycle; responses include BFF **`runtimeSummary`**.

| Method | Path | Lifecycle |
|--------|------|-----------|
| GET | `/api/sandboxes` | `GET /sandboxes` |
| GET | `/api/sandboxes/{id}` | `GET /sandboxes/{id}` |
| POST | `/api/sandboxes` | `POST /sandboxes` |
| DELETE | `/api/sandboxes/{id}` | DELETE |
| POST | `/api/sandboxes/{id}/renew-expiration` | same |
| POST | `/api/sandboxes/{id}/pause` | same (202) |
| POST | `/api/sandboxes/{id}/resume` | same (202) |
| POST | `/api/sandboxes/{id}/snapshots` | same (202) |
| GET | `/api/sandboxes/{id}/diagnostics/logs` | query `scope` required |
| GET | `/api/sandboxes/{id}/diagnostics/events` | same |
| GET | `/api/sandboxes/{id}/endpoints/{port}` | same |

### Snapshots (tenant)

| Method | Path |
|--------|------|
| GET | `/api/snapshots` |
| GET | `/api/snapshots/{id}` |
| DELETE | `/api/snapshots/{id}` |

### `runtimeSummary`

```json
{
  "wallClockSeconds": 3842,
  "remainingSeconds": 900,
  "asOf": "2026-09-28T08:00:00Z",
  "basis": "createdAt"
}
```

## Admin

### `GET /api/admin/sandboxes`

Aggregates per-tenant lists; optional `tenant=` filter. Pagination includes `tenantErrors` for partial failures.

### Admin sandbox ops

Query **`tenant`** required for `{id}` read/write, diagnostics, etc.

### Admin snapshots

`GET/DELETE /api/admin/snapshots[/{id}]?tenant=`

### Admin Kubernetes (read-only)

`GET /api/admin/k8s/workloads`, `GET /api/admin/k8s/events` — requires `BFF_K8S_PROBE_ENABLED` and RBAC.

### `GET /api/admin/stats/runtime`

Aggregated KPIs from admin sandbox list.

### Pools (admin)

Proxies `/v1/pools` (**not tenant-isolated**; BFF uses first tenant key in `tenants.toml`).

### `GET /api/admin/platform/summary`

Server health/version plus component placeholders.

## Health

- `GET /health` → `{ "status": "ok" }`
- `GET /api/version` → proxies server `/version`

## Errors

```json
{ "code": "UNAUTHORIZED", "message": "..." }
```

Lifecycle errors forward `code` / `message` when present.
