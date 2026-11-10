# Feature matrix

Core lifecycle flows go through the **BFF** to the Lifecycle API. Status legend:

| Symbol | Meaning |
|--------|---------|
| ✅ | Implemented with a tested path |
| 🔶 | UI present; depends on cluster/RBAC or partial admin/tenant support |
| 📋 | Planned follow-up PR (not in MVP) |

## MVP — tenant developers

| Feature | Status | BFF | Web |
|---------|--------|-----|-----|
| Tenant key login | ✅ | `/api/auth/tenant` | `/login` |
| Admin token login | ✅ | `/api/auth/admin` | `/login` |
| Sandbox list / filter | ✅ | `GET /api/sandboxes` | `/sandboxes` |
| Sandbox detail | ✅ | `GET /api/sandboxes/{id}` | `/sandboxes/:id` |
| Runtime logs | ✅ | diagnostics logs | detail |
| Create sandbox | ✅ | `POST /api/sandboxes` | `/sandboxes/new` |
| Renew / delete | ✅ | renew / DELETE | detail |
| Endpoint | ✅ | `.../endpoints/{port}` | detail |
| Runtime / TTL summary | ✅ | `runtimeSummary` | list / detail |
| Pause / resume | 🔶 | pause / resume | detail |
| Tenant overview KPI | ✅ | list aggregation | `/` |
| Colored log viewer | ✅ | diagnostics | detail |

## MVP — admin and platform

| Feature | Status | BFF | Web |
|---------|--------|-----|-----|
| Global sandbox list | ✅ | `/api/admin/sandboxes` | `/admin/sandboxes` |
| Runtime stats | ✅ | `/api/admin/stats/runtime` | `/` (admin) |
| Snapshots | ✅ | tenant + admin | `/snapshots` |
| Diagnostics | ✅ | logs / events | `/diagnostics` |
| Pools | ✅ | `/api/pools*` | `/pools` |
| Admin sandbox ops | ✅ | `?tenant=` proxy | `/sandboxes/:id` |
| Component health | 🔶 | K8s deployments + node-agent | `/platform/health` |
| Archive logs | 🔶 | OSS + BFF read | log tab |
| Version | ✅ | `/api/version` | `/platform/version` |
| K8s workloads / events | 🔶 | admin k8s APIs | `/platform/k8s/*` |

## Follow-up PRs (extended scope)

| Feature | Status | Notes |
|---------|--------|-------|
| Sandbox apply history | 📋 | PostgreSQL; server-owned store — [07-sandbox-history.md](./07-sandbox-history.md) |
| Tenant usage / image stats | 📋 | Depends on history store |
| Grafana sandbox monitor | 📋 | BFF embed + platform settings |
| Mirror accel / image catalog UI | 📋 | Static or registry integration TBD |
| Server `store.lifecycle_audit` | 📋 | Not in MVP; no server changes in first PR |

## Known limitations

1. **Pool API** is not tenant-isolated.
2. **K8s platform pages** require BFF ServiceAccount RBAC.
3. **Admin diagnostics** proxy tenant keys; define product policy for your org.
