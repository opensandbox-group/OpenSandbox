# Sandbox lifecycle history (deferred)

Persistent sandbox **apply history**, **tenant usage**, and **image run statistics** are **not part of the initial upstream console MVP**. They depend on a future server-side persistence design (PostgreSQL audit or catalog, K8s watch vs Docker poll) and possible **major-version** Lifecycle API semantics for deleted sandboxes.

Track design discussion in a follow-up OSEP/PR series. Until then:

- Console reads **live Lifecycle API** data only (`GET /sandboxes`, etc.).
- Do not enable server `[store.lifecycle_audit]` or BFF `BFF_HISTORY_*` in the MVP PR.

When implemented, expect:

- **Server-owned schema** under `[store]` (same pattern as snapshots), not BFF `CREATE TABLE`.
- **Audit-first model**: PostgreSQL as a queryable copy; runtime remains source of truth for existence; `GET` after delete stays **404** unless a future major version defines tombstones.

See [05-feature-matrix.md](./05-feature-matrix.md) for what ships in MVP vs follow-up PRs.
