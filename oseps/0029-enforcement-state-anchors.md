---
title: Enforcement-State Anchors for Egress Policy
authors:
  - "@AdityaxDeore"
creation-date: 2026-11-11
last-updated: 2026-11-11
status: draft
---

# OSEP-0029: Enforcement-State Anchors for Egress Policy

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal adds enforcement-state anchors to the egress data plane: at apply time, each applier records the expected kernel state for the tables it owns; a periodic reconcile loop reads the live ruleset back with structured verification, re-applies on drift, and surfaces per-table enforcement state (`confirmed` / `drifted` / `unknown`) via healthz and metrics. This is a draft proposal for discussion: the direction is not yet maintainer-approved, and it formalizes issue #2718 (reported by yjjnls, who operates OpenSandbox in production and hit exactly this class of failure), which offered to split into an OSEP if maintainers prefer that process. The first increment of the shared read-back primitive this proposal builds on — a table-scoped verifier shared between the quarantine fence and the nftables package — is proposed separately in PR #2735.

## Motivation

All egress enforcement state in the kernel — the transparent MITM redirect tables, the gateway DNS/MITM redirect tables, the per-subject fast-sandbox table, and the sidecar `inet opensandbox` policy table — is installed once (at process startup, or on a mutation event) and is never re-verified while the daemon runs. If that state is lost or altered by anything outside the egress process, the daemon keeps running healthy and sandbox traffic silently bypasses policy. For a security control, "silent" is the worst failure mode. The issue documents five concrete gaps, verified against `main` at commit 97d51cb8: external ruleset changes while the daemon is alive are never detected (another host-level nftables user can `nft flush ruleset` or delete an opensandbox table); ambiguous apply outcomes are modeled but never resolved (`ApplyEffect` in `pkg/nftables/apply_effect.go` explicitly documents that `ApplyUnknown` "does not imply rollback, atomicity, or that any particular rule was installed", and for the main policy tables nothing ever follows up to resolve it); a mitmdump crash-restart does not re-verify the redirect (`SetupTransparentHTTP` is called exactly once, in the `startInitial` path, and the restart path brings the proxy back and re-publishes readiness without re-installing or re-checking the OUTPUT redirect table); the only recurring nft-related loop, `StartConnectionRefresh` (30s), renews DNS-learned dynamic-set timeouts from conntrack and never checks that the table, its base chains, or its dispatch rules still exist; and no health signal distinguishes "intent loaded" from "kernel currently enforcing" (readiness reflects process/proxy state, and OSEP-0007 even says the policy API response "does not assert live enforcement" — today there is no source that could). The reporter hit exactly this failure in production: the redirect table was lost while the egress daemon stayed healthy, sandbox traffic silently bypassed policy until externally noticed, and a periodic read-back reconciler they added afterwards restores enforcement within one reconcile interval and has caught real drift since. CI corroborates the fragility from the other side: the smoke-test nft assertions failed nondeterministically across branches that touched no nft code (#2541), because a broken `nft list` was indistinguishable from a missing rule, and #2543 hardened the helpers to surface errors — but CI now checks enforcement state once during a test run while the production daemon still never checks it again after startup. The tree already contains the pattern that proves the fix works: `pkg/quarantine/fence.go` does install plus exact read-back verification for its own table, but it exists only for the quarantine fence, runs only at containment time, and is documented as "not a restart/recovery protocol" — this proposal generalizes that pattern to every table the egress owns and runs it on a loop.

### Goals

1. Detect loss or alteration of any opensandbox-owned enforcement table within one reconcile interval.
2. Restore enforcement automatically by re-applying the expected state on drift, idempotently, without touching foreign tables.
3. Never mistake an observation failure for absence or success: a failed `nft list` is `unknown`, and an `ApplyUnknown` outcome is resolved by read-back, never assumed.
4. Surface per-table enforcement state (`confirmed` / `drifted` / `unknown`) via healthz and a metric, so a platform can gate sandbox placement or alerting on confirmed enforcement rather than process liveness, and give OSEP-0007's "does not assert live enforcement" caveat a concrete remedy.
5. Give CI and the running daemon one shared source of truth: the smoke-test helpers verify kernel state through the same read-back code path the daemon uses.
6. Ship the first increment small and reviewable: anchors for the 2-3 tables that carry the security-critical redirects, a ~60s loop, metric plus re-apply, default-off until baked.

### Non-Goals

1. No persisted anchors across daemon restarts in the first increment; in-process anchors are sufficient to start, and an optional persisted snapshot can later extend coverage across restarts.
2. No redesign of OSEP-0022's replay, which recovers intent at process start; anchors cover kernel state while the process lives and complement it.
3. No netlink subscription to nftables events, no external watchdog or supervisor process, and no new service or storage.
4. No reconciling of every table at once: the first increment covers the security-critical redirect tables, and coverage extends table by table.
5. No change to the egress policy model itself (what is enforced); this proposal is about whether what is enforced is actually in the kernel.

## Requirements

1. Only opensandbox-owned tables are anchored, verified, and reconciled; foreign ruleset content is never judged and never mutated.
2. Verification is structured: `nft -j list ruleset` plus an exact normalized textual snapshot, compared against the anchor, scoped to one table at a time; never stderr-guessing and never textual grep of human-formatted output.
3. A failed `nft list` is an error (`unknown`), never proof of absence; an `ApplyUnknown` outcome is resolved only by a successful read-back of the complete expected table.
4. Re-application is idempotent and table-scoped, using the fence's whole-transaction install style, so a reconcile that races a concurrent apply or a concurrent nftables user converges instead of corrupting.
5. The reconcile loop runs on a configurable interval (default on the order of a minute, with jitter) and is default-off behind an env flag until baked.
6. Drift is observable: a metric plus a log on every drift detection and re-apply, and per-table state exposed via healthz.
7. After N consecutive failures to restore a table, the daemon escalates to existing containment (the quarantine fence) and/or degraded health rather than continuing silently.
8. Every policy mutation updates the anchor for the tables it touches, so the anchor never goes stale relative to current intent.

## Proposal

Generalize the quarantine fence's install-plus-read-back pattern to every table the egress owns and run it on a loop, in three parts. First, an anchor: at apply time, each applier records the expected kernel state for the tables it owns — the canonical table/chain/rule set it just installed, plus a snapshot hash for cheap comparison and a monotonic epoch so a stale read-back can never be mistaken for current state. In-process storage is sufficient to start; an optional persisted snapshot can later extend coverage across daemon restarts. Second, structured read-back and periodic reconcile: a loop (interval configurable, default on the order of a minute, with jitter) compares the live ruleset against the anchor using the shared table-scoped verifier (`pkg/nftables.VerifyTable` / `InspectTableJSON`, extracted from the fence's verification in PR #2735 so the fence, the nftables package, and the smoke-test helpers all verify kernel state identically). On drift it emits a metric plus a log, then re-applies the expected state using the fence's idempotent whole-transaction install, with bounded retries; after N consecutive failures it escalates to existing containment (the quarantine fence) and/or degraded health rather than continuing silently. Third, health surfacing: per-table enforcement state (`confirmed` / `drifted` / `unknown`) is exposed via healthz and a metric, so a platform can gate sandbox placement or alerting on confirmed enforcement. The first increment is deliberately small: anchor ledger plus periodic reconcile for the 2-3 security-critical redirect tables (the transparent MITM redirect, the gateway DNS/MITM redirects, and the per-subject fast-sandbox table), a ~60s loop, metric plus re-apply, default-off. This complements OSEP-0022's replay (which recovers intent at process start) and #1594's policy/vault persistence (restart-time intent recovery): those cover intent, anchors cover kernel state while the process lives.

### Notes/Constraints/Caveats

- The mitmdump crash-restart path needs a check-then-install approach, not a blind re-install: `SetupTransparentHTTP` is not idempotent on the default iptables backend, so re-running it unconditionally on restart is not safe, and any re-install on that path must first be verified idempotent per backend (or guarded by a read-back that skips the install when the redirect is already present).
- The anchor must be updated on every policy mutation (binding, vault, or policy events that change a table), or the reconciler will "restore" a stale expected state over a legitimate new one; the mutation paths and the anchor update must be wired together, not left as a convention.
- Whether the egress tables are exclusively owned by opensandbox or host tooling may legitimately manage rules inside them is an open maintainer question: the proposal assumes exclusive ownership of the named tables and never touches foreign tables, but if host tooling is expected to add rules inside an opensandbox table, the anchor model needs a notion of tolerated foreign rules before reconciliation can be safe.
- The persisted-anchor extension (coverage across daemon restarts) is deferred: until it exists, a daemon restart relies on OSEP-0022's replay for intent and on the startup install for kernel state, and the reconcile loop only starts once the anchor is rebuilt.
- A failed `nft list` must never be read as absence — this is the same trap that made the CI smoke-test assertions fail nondeterministically (#2541), and the shared verifier preserves the fence's strictness on it.
- The N for consecutive-failure escalation, the default interval, and the jitter bounds are proposed starting points for discussion, not decided values.

### Risks and Mitigations

- Reconciler fighting a concurrent nftables user: mitigated by table-scoped, idempotent whole-transaction installs and by scoping verification strictly to opensandbox-owned tables, so foreign content is never judged or touched.
- Stale anchors overwriting legitimate policy changes: mitigated by requiring every mutation path to update the anchor, by the monotonic epoch on the anchor, and by tests that mutate policy and then verify the reconciler converges to the new state rather than the old one.
- Observation failure mistaken for drift or absence: mitigated by the three-state model — a failed list is `unknown`, not drift and not absence — and by never re-applying on the basis of an unknown read.
- `ApplyUnknown` resolved as success: mitigated by the fence's rule, preserved in the shared verifier: only a successful read-back of the complete expected table resolves the uncertainty as success.
- MITM restart re-install breaking the redirect: mitigated by the check-then-install constraint above; the restart path is excluded from the first increment until its idempotency is verified per backend.
- Reconcile load on the kernel: mitigated by the ~60s interval with jitter and by the snapshot-hash fast path (compare hashes first, parse only on mismatch); the loop is default-off until baked, so the cost is opt-in.
- Escalation to containment being too aggressive or too lax: mitigated by making N configurable and by surfacing the consecutive-failure count as a metric before any containment action is taken in the first increment.

## Design Details

- Anchor ledger: a per-table record holding the canonical expected table/chain/rule set (or its snapshot hash for comparison), a monotonic epoch bumped on every mutation, and the last verification result with its timestamp. Stored in-process in the first increment; the record is keyed by table name (`opensandbox`, `opensandbox_dns_redirect`, `opensandbox_mitm_redirect`, `opensandbox_gateway_dns`, `opensandbox_gateway_mitm`, `opensandbox-fast-sandbox`, and the sidecar `inet opensandbox` policy table as coverage extends).
- Shared verifier: `pkg/nftables.VerifyTable` / `InspectTableJSON` (PR #2735) — JSON parse of `nft -j list ruleset` plus the exact normalized textual table match, with exact nftables envelope and trailing-data rejection, exact table/chain/rule match scoped to one table, foreign tables ignored, and a failed `nft list` returned as an error (unknown). The textual snapshot stays required alongside the JSON read-back so older nft versions that omit table comments in JSON stay covered. The quarantine fence is rewired to use it with no behavior change (PR #2735), and the smoke-test helpers (`nft_has` et al.) are pointed at the same code path so tests and runtime verify identically.
- Reconcile loop: a daemon-side loop, default interval on the order of a minute with jitter, that for each anchored table runs the shared verifier against the anchor and acts on the outcome: match → record `confirmed` with the current epoch; mismatch → emit a drift metric plus a log, re-apply the expected state with the fence's idempotent whole-transaction install, then verify again and record `confirmed` or `drifted`; observation failure → record `unknown` and do not re-apply. Bounded retries per cycle; after N consecutive failures for a table, escalate to existing containment (the quarantine fence) and/or degraded health.
- Profiles: applies uniformly to the sidecar profile (`dns+nft`, transparent MITM) and the fast-sandbox/gateway profile; the first increment covers the 2-3 security-critical redirect tables and extends table by table.
- Configuration: default-off behind an env flag (e.g. `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE` plus an interval flag) so behavior is opt-in until baked, then the default flips. No API changes; the healthz/metrics addition is additive.
- Health and metrics: per-table enforcement state (`confirmed` / `drifted` / `unknown`) exposed via healthz and a metric, plus a drift counter and a consecutive-failure count per table. This gives OSEP-0007's "does not assert live enforcement" caveat a concrete remedy: a platform can now ask for confirmed enforcement rather than process liveness.
- MITM restart path: out of the first increment. Before it is included, the restart path (`restartWithBackoffUsing` → `launchAndListen`) must be changed to check-then-install the OUTPUT redirect table, with the install's idempotency verified per backend; until then the reconciler covers the table from the loop, not from the restart path.

## Test Plan

- Unit tests on the anchor ledger: anchor creation at apply time records the expected state, epoch, and snapshot hash; every mutation path updates the anchor; a stale epoch is never treated as current.
- Unit tests on the shared verifier (PR #2735, nftables 101 passing): exact match confirms; a missing rule, an extra rule, or a changed rule is drift; a failed `nft list` is an error (unknown), never proof of absence; foreign tables are ignored; older nft JSON without table comments is still covered by the textual snapshot.
- Reconcile-loop tests with a fake runner: drift detection followed by idempotent re-apply converges to the anchor; foreign tables are never touched; a failed list yields `unknown` and no re-apply; an `ApplyUnknown` outcome is resolved only by a successful read-back; idempotent re-apply on an already-correct table is a no-op; concurrent reconcile attempts converge without corruption; context cancellation stops the loop cleanly; N consecutive failures escalate to containment/degraded health and the count is visible as a metric.
- Wrong-selector/ownership tests: rules added by a simulated host tool inside a foreign table are ignored; rules added inside an opensandbox-owned table are treated as drift (until the ownership question above is settled with maintainers).
- Integration tests in a network namespace (`unshare -rn`) with real nft: install a table, delete a rule externally, and verify the reconciler restores it within one interval; `nft flush ruleset` externally and verify all anchored tables are restored and the health state transitions `confirmed` → `drifted` → `confirmed`.
- Live-daemon tests that need CI, not a developer VM: a running egress daemon with a table deleted externally, verifying restoration within one interval and the healthz transition; the mitmdump crash-restart path once its check-then-install design is settled.

## Drawbacks

- A reconcile loop adds periodic `nft list` traffic and a small amount of daemon complexity; the interval and jitter bound the cost, and the feature is default-off until baked.
- The guarantee is only as good as the anchor: if a mutation path forgets to update the anchor, the reconciler will fight legitimate policy changes, so every new mutation path must be wired to the anchor update — a maintenance obligation, not a one-time fix.
- The first increment deliberately leaves the mitmdump restart path and the persisted-anchor extension out, so those two loss windows stay open until follow-up work lands.
- Escalation to containment on N consecutive failures is a sharp tool: if N is too small or the failure is environmental (e.g. a transient nft error), the daemon may contain sandboxes that are actually fine; N must be tuned with production data and the count must be visible before containment is enabled.

## Alternatives

- Restart-only recovery: rejected; a container restart re-runs the startup install, but the loss window is while the daemon lives — and it is silent — so it is not sufficient for a security control.
- Blind periodic re-apply (no read-back): rejected; it fights concurrent nftables users and cannot resolve the `ApplyUnknown` case, while read-back-then-reconcile is strictly safer and produces a signal.
- Netlink subscription to nftables events: rejected for now; more precise in theory, but it adds a dependency, does not cover changes originating outside the namespace, and still needs read-back as the source of truth.
- External watchdog/supervisor process: rejected; it verifies process liveness, not kernel state, and adds more moving parts for the same gap.
- Fail-closed at apply failure: already done at startup (`log.Fatalf` in `setupNft`) — necessary, but it only covers t=0.

## Infrastructure Needed

None. The proposal uses the existing nftables tooling, the existing quarantine fence, and the existing healthz/metrics plumbing; no new services, storage, or third-party dependencies are required.

## Upgrade & Migration Strategy

Fully backward compatible and additive: the reconcile loop is default-off behind an env flag, existing installs, APIs, and health endpoints are unchanged for operators who do not opt in, and no migration is needed for existing deployments. Operators opt in by setting the flag (e.g. `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE` plus the interval), observe the per-table health and drift metrics, and the default flips to on after a bake-in period once maintainers are satisfied with the production data. The persisted-anchor extension, when it lands, is likewise additive: in-process anchors keep working, and persisted snapshots only add coverage across restarts.
