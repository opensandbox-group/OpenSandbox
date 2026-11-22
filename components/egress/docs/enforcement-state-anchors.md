# Egress Enforcement-State Anchors

Every enforcement table the egress installs into the kernel is written once — at startup, or on a policy mutation — and, until now, was never read back while the daemon ran. A table deleted or altered by anything outside the egress process (a host-level `nft flush ruleset`, another nftables user, a cleanup script) left the daemon healthy while sandbox traffic silently bypassed policy. For a security control, silent is the worst failure mode.

Enforcement-state anchors close that window (issue #2718, formalized in [OSEP-0029, pull request #2740](https://github.com/opensandbox-group/OpenSandbox/pull/2740)): at apply time each applier records the exact kernel state it just installed, and a periodic reconcile loop re-reads the live ruleset through the same shared verifier the quarantine fence uses — restoring drifted tables idempotently and surfacing per-table enforcement state instead of conflating daemon health with kernel enforcement.

## What is anchored

The reconcile covers the redirect tables the egress owns natively, keyed by (address family, table name):

| Table | Families | Profile |
|---|---|---|
| `opensandbox_dns_redirect` | `ip`, `ip6` | sidecar |
| `opensandbox_mitm_redirect` | `ip`, `ip6` | sidecar (transparent MITM) |
| `opensandbox_gateway_dns` | `inet` | fast-sandbox |
| `opensandbox_gateway_mitm` | `inet` | fast-sandbox |

The anchor records both the structured JSON ruleset (chains and ordered rules) and the exact normalized textual snapshot of the table, captured immediately after the install's nft transaction commits. Verification requires both to match, so a broken `nft list` is never mistaken for a missing rule.

Deliberately out of scope for this first increment:

- **The `inet opensandbox` policy table and the per-subject fast-sandbox policy tables.** Their DNS-learned set elements expire on nft timeouts without a policy-apply event, so a static anchor comparison would report lease expiry as drift; coverage extends there once expiry-aware desired state exists (OSEP-0029).
- **The mitmdump crash-restart path.** It still re-publishes readiness without re-installing the OUTPUT redirect; the reconcile loop covers the table from the loop, not from the restart path, until the restart install is verified idempotent per backend.
- **Persisted anchors across daemon restarts.** Anchors are in-process; a restart re-installs the tables at startup and the ledger is rebuilt from the fresh installs.

## Backend scope

Deletion detection is native-nft-only. With `OPENSANDBOX_EGRESS_REDIRECT_BACKEND=nft` the redirects live in owned tables that can be read back and restored. On the default `auto`/`iptables` backend the redirects are rules appended to the shared `nat OUTPUT` chain, where there is no owned table to anchor — nothing is recorded there, and deleting such a rule stays invisible until a rule-scoped iptables reconciliation design exists (an open OSEP-0029 question).

## Configuration

| Variable | Required | Purpose | Default |
|------|----------|------|--------|
| `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE` | No | Enable the anchor ledger and the reconcile loop (`1/true/on`, etc.) | Disabled |
| `OPENSANDBOX_EGRESS_ENFORCEMENT_RECONCILE_INTERVAL_SEC` | No | Reconcile interval in seconds (1–3600) | `60` |
| `OPENSANDBOX_EGRESS_ENFORCEMENT_MAX_RESTORE_FAILURES` | No | Consecutive failed restorations after which a table is escalated as unrestorable (1–100) | `3` |

Behavior when enabled:

- Each tick verifies every anchored table once. A match records `confirmed`. A well-formed observation that differs — a missing rule, an extra rule, a changed rule, foreign content inside the table, or a positively absent table — is drift: a drift counter and a warning log are emitted, then the expected state is re-applied with the same idempotent whole-transaction install the applier used, and re-verified.
- A failed `nft list` is `unknown`: it asserts neither absence nor success, and nothing is re-applied on the basis of it. Transient observation failures therefore do not flap state.
- After `N` consecutive failed restorations a table escalates through a loud error and the consecutive-failure gauge. Containment escalation (quarantining the sandbox) is deliberately deferred until the threshold is tuned with production data.
- Every policy mutation re-records the anchor for the tables it touches, so the expected state never goes stale relative to current intent; a mutation whose re-anchor capture fails drops the table's anchor instead of retaining a superseded snapshot — restore closures read the caller's current desired state, so a stale anchor would make every later pass judge the correctly installed table as drift. An intentional removal (for example, the gateway DNS table when its last subject unregisters) drops the anchor, so the loop never restores a table the daemon deliberately deleted. Recording a fresh anchor costs two additional `nft` reads of the table the mutation just replaced (one JSON ruleset listing plus the exact textual table), on top of the rebuild itself — the fast-sandbox redirects are rebuilt wholesale on subject lifecycle changes, so this is a constant-factor addition to work those paths already do.
- Restores race a concurrent policy mutation safely: each anchor carries a monotonic epoch, and a verification started against a superseded anchor is discarded rather than overwriting the newer record. Restore closures read the caller's authoritative desired state, so a restore racing a lifecycle change reinstalls the newest set, never a captured stale one.

## Surfacing

- **Metrics** (see [the telemetry guide](../../../docs/guides/telemetry.md#egress-sidecar)): `egress.nftables.enforcement.state` (`0` unknown / `1` drifted / `2` confirmed, by `family` and `table`), `egress.nftables.enforcement.consecutive_failures`, `egress.nftables.enforcement.drift_total`, `egress.nftables.enforcement.restore_failed_total`. Gate placement or alerting on `state = 2`; treat `unknown` as a delay, not a denial.
- **Sidecar**: `GET /enforcement` on the policy server returns `{"enabled": bool, "tables": [{family, table, state, epoch, consecutiveFailures, lastVerified, lastError}]}`. Read-only and additive; `/healthz` is unchanged.
- **Fast-sandbox**: `/healthz` gains an additive `enforcement` array with the same per-table state.

## Verification

- Unit tests cover the ledger (confirm, drift detection, idempotent restore, unknown never re-applies, stale-epoch discard, drop-on-removal, escalation once per streak) and the capture (exact state, absent table, shapes the verifier cannot express).
- `TestEnforcementAnchorKernelPrivateNS` (opt-in kernel regression, run in CI under `scripts/ci-nft-go-test.sh`) exercises the real kernel in a private network namespace: single-family deletion restored independently, an external `flush ruleset` restoring every anchored table, and foreign tables never judged, restored, or mutated.
