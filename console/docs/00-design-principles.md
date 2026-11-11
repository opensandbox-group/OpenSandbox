# Design principles

## Goals

- Ship the console under `console/` with **minimal coupling** to lifecycle server internals; prefer a **BFF** over browser-held API keys.
- **Tenant developers** use a tenant API key → see sandboxes in their namespace only.
- **Platform admins** use a separate BFF admin token; the BFF aggregates Lifecycle calls **server-side** with per-tenant keys (optional read-only Kubernetes RBAC for platform pages).

## Avoid (large cross-cutting PRs)

| Avoid | Why |
|-------|-----|
| Full server auth overhaul in the same PR as the UI | High review cost and conflict with `main` |
| Browser storage of `OPEN-SANDBOX-API-KEY` | Use httpOnly BFF session instead |
| Runtime-specific diagnostics in core UI | Stay on Diagnostics API |
| Everything in one merge (history DB, Grafana, image catalog, K8s ops) | Split PRs; see [05-feature-matrix.md](./05-feature-matrix.md) |

## Do

| Principle | Approach |
|-----------|----------|
| Decoupled delivery | `console/` + standalone Deployments (optional Helm subchart later) |
| Single auth path to Lifecycle | Tenant key → BFF session; admin → `BFF_ADMIN_TOKEN` |
| Runtime summary | BFF computes `runtimeSummary` from `createdAt` / `expiresAt` / status |
| Pool | Call existing `/v1/pools`; UI states **platform-level, not tenant-isolated** |
| Phased delivery | P0 sandbox CRUD; P1 snapshots/diagnostics; P2 admin + K8s views |

## Phasing

```text
P0  Tenant: login, list/detail/create/renew/delete/endpoint, runtimeSummary
P1  Snapshots, diagnostics, pools (admin)
P2  Admin cross-tenant list/stats, K8s workloads/events, component health
P3  Optional: PostgreSQL usage/history, OIDC (likely still at BFF layer)
```

## References

- [OSEP-0006: Developer Console](../../../oseps/0006-developer-console.md) — capability reference; this implementation uses **BFF session auth**, not OSEP Phase 1 server RBAC.
- Optional server-side rows: `[store.lifecycle_audit]` in [server configuration](../../server/configuration.md).
