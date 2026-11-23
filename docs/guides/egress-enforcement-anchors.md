---
title: Egress Enforcement Anchors
description: Operator guide to enabling, tuning, and alerting on egress enforcement-state anchors — periodic verification that the kernel tables enforcing sandbox egress policy still exist and still match what the sidecar installed.
---

# Egress Enforcement Anchors

This guide is written for **cluster administrators** and platform operators. It explains how to turn on enforcement-state anchors for sandbox egress, which deployments they cover, and what to alert on. The internals — what is anchored, capture and restore invariants, backend scope — live in the [Egress Enforcement-State Anchors architecture page](/architecture/network/egress-enforcement-anchors); this page focuses on what you configure and what it buys you.

## What it buys you

The egress installs its enforcement tables once and, without this feature, never re-reads them while the daemon runs. A table deleted or altered from outside the process — a host-level `nft flush ruleset`, another nftables user, a cleanup script, a privileged pod in the same network namespace — leaves the sidecar healthy while sandbox traffic bypasses policy. That is the silent failure mode: health checks stay green and denials stop being enforced.

With enforcement-state anchors enabled, a periodic loop re-reads the live ruleset for every table the egress owns natively, restores tables that drifted, and reports per-table enforcement state through metrics and the egress APIs. You then alert on the state instead of assuming the daemon's health implies kernel enforcement.

::: warning Coverage requires the native nft redirect backend
Only redirects installed as egress-owned nftables tables can be anchored. Set `redirect_backend = "nft"` (server config) or `OPENSANDBOX_EGRESS_REDIRECT_BACKEND=nft` (fast-sandbox workload) — on the default `auto`/`iptables` backend the redirects are rules in the shared `nat OUTPUT` chain, where there is no owned table to verify, and nothing is recorded.
:::

## Enabling it

### Pod sandbox (server-generated sidecar)

Docker and Kubernetes sandboxes get the egress sidecar from the server, so enable the feature in the server configuration. Per-request environment cannot set these controls — the same variable names in a sandbox create request are rejected, because they toggle a security control and the kernel backend.

```toml
[egress]
image = "opensandbox/egress:v1.1.7"
mode = "dns+nft"
redirect_backend = "nft"   # required for anchor coverage

[egress.enforcement]
reconcile = true
# reconcile_interval_seconds = 60   # 1-3600
# max_restore_failures = 3          # 1-100
```

| Key | Type | Default | Purpose |
|-----|------|---------|---------|
| `egress.redirect_backend` | string \| omitted | `null` (component default `auto`) | `auto`, `iptables`, or `nft`. `nft` is required for anchor coverage. |
| `egress.enforcement.reconcile` | bool | `false` | Enable the anchor ledger and reconcile loop. |
| `egress.enforcement.reconcile_interval_seconds` | integer | `60` | Reconcile cadence, 1–3600 seconds. Each tick jitters by ±10% across the fleet. |
| `egress.enforcement.max_restore_failures` | integer | `3` | Consecutive failed restorations before a table escalates as unrestorable, 1–100. |

The server injects these as `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE`, `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE_INTERVAL_SEC`, `OPENSANDBOX_EGRESS_ENFORCEMENT_MAX_RESTORE_FAILURES`, and `OPENSANDBOX_EGRESS_REDIRECT_BACKEND` on every generated sidecar. Changes affect newly created sandboxes only. See the [server configuration reference](https://github.com/opensandbox-group/OpenSandbox/blob/main/server/configuration.md#egress) for the full `[egress]` table.

### Fast Sandbox

The fast-sandbox profile runs one egress control plane per Fastlet pod, deployed by the platform rather than generated per sandbox. Set the same environment variables on that egress workload (the Fastlet pod's egress container):

| Variable | Required | Purpose | Default |
|------|----------|---------|---------|
| `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE` | No | Enable the anchor ledger and the reconcile loop (`1/true/on`, etc.) | Disabled |
| `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE_INTERVAL_SEC` | No | Reconcile interval in seconds (1–3600) | `60` |
| `OPENSANDBOX_EGRESS_ENFORCEMENT_MAX_RESTORE_FAILURES` | No | Consecutive failed restorations after which a table is escalated as unrestorable (1–100) | `3` |
| `OPENSANDBOX_EGRESS_REDIRECT_BACKEND` | No | Set to `nft` — required for coverage in both profiles | `auto` |

## What to alert on

The feature exports `egress.nftables.enforcement.state` (by `family` and `table`: `0` unknown, `1` drifted, `2` confirmed), `egress.nftables.enforcement.consecutive_failures`, `egress.nftables.enforcement.drift_total`, and `egress.nftables.enforcement.restore_failed_total`. The series are absent while the feature is disabled.

- **Gate placement or alerting on `state = 2` (confirmed).** Treat `0` (unknown) as a delay, not a denial: it means the observation failed, so it says nothing about the kernel state. Transient `nft` errors do not flap state.
- **Drift is actionable** (`drift_total` increments only for well-formed observations that differ): restoration was attempted. A sustained `restore_failed_total` or a rising `consecutive_failures` means the table cannot be brought back — escalate to the host, since something outside the egress is likely fighting it.
- The egress APIs expose the same data for spot checks: `GET /enforcement` on the sidecar policy server, and the additive `enforcement` array on the fast-sandbox `/healthz`. Both are read-only.

See [Telemetry](/guides/telemetry) for the full egress metric reference and scrape setup.

## Operational notes

- The reconcile loop costs one or two `nft` invocations per anchored table per tick, and a re-anchor after each policy mutation costs two additional table reads. Both were measured negligible at pool densities of a few thousand sandboxes; the interval is the knob if a host is unusually busy.
- The policy table and the fast-sandbox per-subject policy tables are not anchored in this increment: DNS-learned set elements expire on nft timers, so a static comparison would report lease expiry as drift. Redirect-table coverage is the deliberate first step.
- Restores read the caller's current desired state and are epoch-guarded, so a restore racing a normal lifecycle change (a sandbox registering or unregistering) converges on the newest state instead of overwriting it; that churn does not raise drift alerts.
- Anchors are in-process. A daemon restart re-installs the tables at startup and rebuilds the ledger from the fresh installs — no persistence is required or provided.
