---
title: Egress Enforcement-State Anchors
description: How the egress records the kernel state it installs, re-verifies it on a loop, and restores drifted tables — the design, invariants, and coverage limits of enforcement-state anchors (OSEP-0029).
---

# Egress Enforcement-State Anchors

Every enforcement table the egress installs into the kernel is written once — at startup, or on a policy mutation — and was never read back while the daemon ran. A table deleted or altered by anything outside the egress process (a host-level `nft flush ruleset`, another nftables user, a cleanup script) left the daemon healthy while sandbox traffic silently bypassed policy. For a security control, silent is the worst failure mode.

Enforcement-state anchors close that window ([issue #2718](https://github.com/opensandbox-group/OpenSandbox/issues/2718), formalized in [OSEP-0029, pull request #2740](https://github.com/opensandbox-group/OpenSandbox/pull/2740)): at apply time each applier records the exact kernel state it just installed, and a periodic reconcile loop re-reads the live ruleset through the same shared verifier the quarantine fence uses — restoring drifted tables idempotently and surfacing per-table enforcement state instead of conflating daemon health with kernel enforcement. For how to enable and operate the feature, see the [Egress Enforcement Anchors guide](/guides/egress-enforcement-anchors).

## What is anchored

The reconcile covers the redirect tables the egress owns natively, keyed by (address family, table name):

| Table | Families | Profile |
|---|---|---|
| `opensandbox_dns_redirect` | `ip`, `ip6` | sidecar |
| `opensandbox_mitm_redirect` | `ip`, `ip6` | sidecar (transparent MITM) |
| `opensandbox_gateway_dns` | `inet` | fast-sandbox |
| `opensandbox_gateway_mitm` | `inet` | fast-sandbox |

The anchor records both the structured JSON ruleset (chains and ordered rules) and the exact normalized textual snapshot of the table, captured immediately after the install's nft transaction commits. Verification requires both to match, so a broken `nft list` is never mistaken for a missing rule.

## Capture invariants

The capture is the install's first verification, and it is deliberately strict:

- **The snapshot must describe one kernel state.** The JSON listing and the textual table read are separate `nft` invocations, so a privileged writer rewriting the table between them would pair JSON from one version with text from another — an impossible state no later restore could confirm. The capture therefore finishes with a fresh verification of the assembled snapshot through the shared verifier and rejects a capture whose two views no longer agree. A rejected capture is not fatal: the install stays, the table is simply left unanchored until the next successful capture.
- **A shape the verifier cannot express is rejected at capture.** Unknown object kinds or unexpected properties inside the owned table fail the capture with their cause attached, rather than anchoring an expectation that could never verify.
- **An absent table is a capture error.** There is nothing to anchor.

## Drift response

Each tick verifies every anchored table once:

- A match records `confirmed`.
- A well-formed observation that differs — a missing rule, an extra rule, a changed rule, foreign content inside the table, or a positively absent table — is drift: a drift counter and a warning log are emitted, then the expected state is re-applied with the same idempotent whole-transaction install the applier used, and re-verified.
- A failed `nft list` is `unknown`: it asserts neither absence nor success, and nothing is re-applied on the basis of it. Transient observation failures therefore do not flap state.

The restore path carries three invariants:

- **Epoch guard.** Every anchor carries a monotonic epoch. A verification started against a superseded anchor is discarded rather than overwriting the newer record, and the epoch is re-checked before drift signals are emitted — ordinary lifecycle churn (a subject registering or unregistering re-anchors the tables mid-read) must not surface as a security alert.
- **Restore reads current desired state.** The re-apply closure restores the caller's authoritative desired state, never a captured snapshot — so a restore racing a lifecycle change reinstalls the newest set, and a stale snapshot can never overwrite a legitimate newer policy.
- **Bounded failure escalation.** After N consecutive failed restorations a table escalates through a loud error and a consecutive-failure gauge. Containment escalation (quarantining the sandbox) is deliberately deferred until the threshold is tuned with production data.

A mutation whose re-anchor capture fails drops the table's anchor instead of retaining a superseded snapshot: because restore closures read current desired state, a stale anchor would make every later pass judge the correctly installed table as drift. An intentional removal (for example, the gateway DNS table when its last subject unregisters) drops the anchor, so the loop never restores a table the daemon deliberately deleted.

## Backend scope

Deletion detection is native-nft-only. With `OPENSANDBOX_EGRESS_REDIRECT_BACKEND=nft` the redirects live in owned tables that can be read back and restored. On the default `auto`/`iptables` backend the redirects are rules appended to the shared `nat OUTPUT` chain, where there is no owned table to anchor — nothing is recorded there, and deleting such a rule stays invisible until a rule-scoped iptables reconciliation design exists (an open OSEP-0029 question).

Deliberately out of scope for this first increment:

- **The `inet opensandbox` policy table and the per-subject fast-sandbox policy tables.** Their DNS-learned set elements expire on nft timeouts without a policy-apply event, so a static anchor comparison would report lease expiry as drift; coverage extends there once expiry-aware desired state exists (OSEP-0029).
- **The mitmdump crash-restart path.** It still re-publishes readiness without re-installing the OUTPUT redirect; the reconcile loop covers the table from the loop, not from the restart path, until the restart install is verified idempotent per backend.
- **Persisted anchors across daemon restarts.** Anchors are in-process; a restart re-installs the tables at startup and the ledger is rebuilt from the fresh installs.

## Surfacing

The reconcile exposes what it learned instead of asserting health:

- **Metrics** (see [the telemetry guide](/guides/telemetry)): `egress.nftables.enforcement.state` (`0` unknown / `1` drifted / `2` confirmed, by `family` and `table`), `egress.nftables.enforcement.consecutive_failures`, `egress.nftables.enforcement.drift_total`, `egress.nftables.enforcement.restore_failed_total`. Gate placement or alerting on `state = 2`; treat `unknown` as a delay, not a denial.
- **Sidecar**: `GET /enforcement` on the policy server returns `{"enabled": bool, "tables": [{family, table, state, epoch, consecutiveFailures, lastVerified, lastError}]}`. Read-only and additive; `/healthz` is unchanged.
- **Fast-sandbox**: `/healthz` gains an additive `enforcement` array with the same per-table state.

## Verification

- Unit tests cover the ledger (confirm, drift detection, idempotent restore, unknown never re-applies, stale-epoch discard and drift suppression, drop-on-removal, escalation once per streak) and the capture (exact state, absent table, shapes the verifier cannot express, and a table rewritten between the capture's two reads).
- `TestEnforcementAnchorKernelPrivateNS` (opt-in kernel regression, run in CI under `scripts/ci-nft-go-test.sh`) exercises the real kernel in a private network namespace: single-family deletion restored independently, an external `flush ruleset` restoring every anchored table, and foreign tables never judged, restored, or mutated.
