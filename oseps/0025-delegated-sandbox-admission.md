---
title: Admission and Idempotency for Delegated Sandbox Creation
authors:
  - "@hpliStartAgain"
creation-date: 2026-09-30
last-updated: 2026-09-30
status: draft
---

# OSEP-0025: Admission and Idempotency for Delegated Sandbox Creation

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
  - [Trusted Principal and Resource Scope](#trusted-principal-and-resource-scope)
  - [Create Intent and Operation Identity](#create-intent-and-operation-identity)
  - [AdmissionProvider Contract](#admissionprovider-contract)
  - [Reservation and Recovery State Machine](#reservation-and-recovery-state-machine)
  - [Budget Accounting](#budget-accounting)
  - [API and Error Semantics](#api-and-error-semantics)
  - [First Implementation and Review Decisions](#first-implementation-and-review-decisions)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Add an optional lifecycle admission extension for delegated callers within an
existing tenant. A trusted principal, a durable create-operation identity, and
atomic resource reservations let a platform enforce per-principal sandbox budgets
and recover a lost create response without starting another sandbox. This is a
draft design for review; it does not change runtime behavior or imply maintainer
acceptance of the extension.

## Motivation

An agent may need additional execution environments while it is running. For
example, a coordinator can delegate parallel build jobs to several workers, or a
hosted workflow service can run independent user sessions within one tenant.
Giving each worker the tenant's lifecycle API key exposes tenant-wide authority.
Putting the same key behind a gateway protects the credential, but does not by
itself establish per-worker accounting at the point where resources are created.

Consider two principals in the same tenant. Principal A may hold two sandboxes,
while principal B has a separate allowance. Concurrent creates must not both pass
a stale "one slot remains" check. If A's successful create response is lost, a
retry must identify that operation rather than spend another slot. If a worker
crashes, deletion or expiry must eventually return its reservation without
waiting for that worker to run a callback.

Existing mechanisms address adjacent problems:

- [OSEP-0014](0014-multi-tenancy.md) supplies API-key-to-tenant-to-namespace
  isolation and deliberately leaves server quotas and rate limiting out of
  scope. Kubernetes ResourceQuota remains the hard namespace capacity boundary;
  it does not distinguish delegated principals within that namespace.
- The current [TenantEntry](../server/opensandbox_server/tenants/models.py)
  contains tenant identity, namespace, and keys. The
  [HTTP tenant provider](../server/opensandbox_server/tenants/http_provider.py)
  caches authentication lookups with TTL and bounded stale fallback. That cache
  is unsuitable for atomic, changing resource reservations.
- [OSEP-0006](0006-developer-console.md) proposes human Console principals,
  roles, and resource scopes. Those concepts can inform a shared principal
  representation, but do not define delegated create budgets and recovery.
- [OSEP-0007](0007-fast-sandbox-runtime-support.md) describes durable intent and
  a runtime-specific `request_id` in fast-sandbox. The public lifecycle
  [create route](../server/opensandbox_server/api/lifecycle.py) still treats
  `X-Request-ID` as tracing; the
  [Kubernetes service](../server/opensandbox_server/services/k8s/kubernetes_service.py)
  generates a new sandbox ID for a new create call. Runtime-local recovery is
  not a caller-bound lifecycle idempotency contract.
- [OSEP-0017](0017-resilient-sdk-transport.md) explicitly excludes a server-side
  idempotency store and wire protocol. Transport retry alone cannot resolve an
  ambiguous create outcome.

[Issue #1570](https://github.com/opensandbox-group/OpenSandbox/issues/1570)
records the original problem. This proposal makes the optional core-extension
choice concrete so that its scope and costs can be reviewed directly.

### Goals

- Reuse tenant and namespace isolation while distinguishing trusted delegated
  principals inside a tenant.
- Enforce concurrent sandbox-count, CPU, and memory allowances atomically before
  starting resource creation.
- Give one logical create a stable identity through retries, process restarts,
  and uncertain runtime responses.
- Reclaim charges after confirmed cleanup, including asynchronous expiry.
- Keep existing deployments unchanged unless an operator enables admission.

### Non-Goals

- A built-in identity provider, token issuer, IAM/RBAC database, billing system,
  or dynamic tenant-management API.
- Replacing Kubernetes ResourceQuota, LimitRange, scheduling, or ingress rate
  limiting; guaranteed fair scheduling or a waiting queue.
- Parent/child ownership graphs, inherited budgets, cascade deletion, ownership
  transfer, or automatic cancellation when a parent exits.
- General lifecycle-server high availability, exactly-once workload execution,
  or deduplicating commands executed inside a sandbox.
- In the first implementation: Docker, fast-sandbox, pool allocation, snapshot
  restore, template builds, storage/GPU budgets, or changing resource sizes.

## Requirements

1. Admission is default-off and explicitly enabled for selected tenants. Within
   an enabled tenant, missing identity, operation identity, or provider support
   cannot fall back to legacy creation. Every resource-creating entry point must
   pass the gate or be rejected as unsupported.
2. Tenant authentication and delegated-principal verification precede admission.
   Caller metadata or an unverified header never establishes authority.
3. Claiming an operation and reserving all of its budget dimensions is atomic.
   Duplicate requests cannot spend twice; rejected or conflicting requests cannot
   invoke the runtime.
4. Operation identity and immutable create intent must be durable before any
   resource-creating side effect. A timeout, lost response, or expired worker
   lease alone is never proof that no resource exists.
5. Authentication, ownership, policy, reservation, and runtime failures remain
   distinguishable. Provider failure is fail-closed for new work, and no stale
   allow decision can authorize another reservation.
6. Recovery and accounting must survive the supported deployment's restart
   model. Unsupported modes are rejected explicitly, not advertised as covered.

## Proposal

Keep `TenantProvider` responsible for tenant resolution. Add a separate
`AdmissionProvider` contract used by a lifecycle admission coordinator, after
identity verification and request validation and before any create side effects.
The provider owns atomic operation registration, policy evaluation, reservations,
and their durable state transitions. The lifecycle coordinator owns runtime
calls and observation-based reconciliation.

```text
Agent -> trusted platform gateway -> tenant authentication
                                    -> verified principal + action scope
                                    -> validate and normalize create intent
                                    -> AdmissionProvider: claim + reserve
                                    -> runtime: create under reserved identity
                                    -> commit binding / reconcile uncertainty

Runtime deletion or expiry -> lifecycle reconciliation -> release reservation
```

The gateway retains tenant credentials and authenticates delegated callers.
Platforms may implement policy and accounting in an external service through a
versioned HTTP provider. OpenSandbox supplies the mandatory enforcement and
lifecycle hooks, rather than embedding a platform's tenant-management database.
A purely advisory allow/deny webhook is insufficient for the promised accounting.

### Notes/Constraints/Caveats

The current [server architecture](../docs/architecture/control-plane/server.md)
and [deployment guidance](../docs/deployment/index.md) describe a single-active
lifecycle server. PostgreSQL currently enables a limited public-snapshot case,
not general lifecycle HA. The first admission implementation retains that
single-active constraint. Its atomic provider contract must tolerate concurrent
requests and repeated reconciliation; a future multi-active rollout additionally
requires shared storage, coordinated runtime ownership, and separate lifecycle
HA validation. A shared quota counter alone does not provide those properties.

Admission is an authorization/accounting boundary for lifecycle requests. It does
not constrain Kubernetes administrators or workloads with direct permission to
create resources. Deployment permissions and network policy must prevent those
bypasses. It also does not change the security properties of data-plane endpoints.

### Risks and Mitigations

| Risk | Required mitigation |
| --- | --- |
| Caller impersonates another principal or chooses a larger budget | Verified issuer/subject and tenant binding; server-selected policy scope; reject caller-written ownership fields |
| Check-then-create race overspends | One atomic operation claim and multidimensional reservation, never a cached check followed by a separate increment |
| Runtime succeeds while accounting update fails | Retain reservation, persist/recover the same runtime binding, retry the idempotent commit |
| Timed-out create is released and later becomes live | Keep uncertain operations charged until the creator is fenced and absence/cleanup is established |
| Provider outage blocks the platform | Bounded calls, explicit 503 responses, metrics, and recovery; no fail-open create path |
| Stuck cleanup permanently consumes allowance | Reconciliation and operator-visible unresolved records; never "fix" availability by guessing that resources are gone |
| Alternate create route or metadata edit bypasses policy | Central service boundary, immutable ownership, and route/provider coverage tests |

## Design Details

### Trusted Principal and Resource Scope

The internal context includes `(tenant_id, issuer, subject)`, allowed lifecycle
actions, and any verified workflow/session scope. The tenant comes from existing
authentication; a delegated assertion must be bound to that same tenant and to the
lifecycle service audience. Stable issuer/subject identity survives key rotation.
Raw API keys must not become operation-owner IDs or be stored in the ledger.

The recommended initial integration is a trusted gateway with a server-verified
assertion or an authenticated, exclusive gateway-to-server channel. The verifier
checks issuer, audience, expiry, tenant binding, and permitted actions. A gateway
header is trusted only when the deployment authenticates its provenance, strips
client copies, and prevents direct ingress around the gateway. Merely configuring
a header name, trusting a source string, or holding a tenant key is insufficient.
The concrete assertion profile is an implementation prerequisite, not something
this proposal declares already available in the current auth middleware.

The provider maps verified identity to configured budget scopes. A caller cannot
choose its own budget group. V1 supports a per-principal allowance and an optional
tenant aggregate allowance, reserved together. Workflow/session IDs may be audit
context; hierarchical or overlapping group budgets are deferred.

Persist owner identity in server-controlled records and reserved runtime metadata.
Apply owner/action checks to operation lookup and sandbox get/list/delete/renew,
endpoint retrieval, and proxy access. Filter lists before pagination and do not
leak cross-principal records through errors. Administrative cross-owner access
requires an explicit operator-granted action scope, never a caller-set flag.
Other delegated lifecycle mutations remain denied until covered. Ordinary
metadata updates cannot change owner, operation identity, or reservation binding.

This does not require handing credentials to a sandbox. If a platform already
has Console identity, an adapter may normalize it to this context; acceptance or
implementation of OSEP-0006 is not assumed as a dependency.

### Create Intent and Operation Identity

For enabled tenants, `POST /sandboxes` requires `Idempotency-Key`. It is scoped by
`(tenant_id, issuer, subject, action=create, key)`. Identical keys from different
principals do not recover each other's resources. A shared logical budget does
not imply shared operation ownership. Clients persist the key and request before
sending; `X-Request-ID` remains a per-attempt trace identifier.

On first admission, store a versioned normalized intent, its digest, an opaque
operation ID, a preallocated sandbox ID, resolved namespace/provider, effective
resource charge, creation time, and fixed absolute expiry. Include every accepted
behavior-affecting field (image, entrypoint, environment, network, volumes,
platform, lifecycle, resource settings, and supported extensions); preserve
order where meaningful, normalize resource units and map order, and distinguish
semantically different omitted/null values. Unknown fields must not disappear
silently from the comparison. Sensitive create data needs encrypted storage and
redacted logs; providers should receive a digest and the minimum policy/accounting
fields rather than raw environment or registry credentials.

Retain both normalized caller intent and resolved execution values. Compare a
retry against the recorded normalization version and caller intent, not freshly
resolved defaults. Retries reuse the original expiry, namespace, runtime identity,
and pinned template/config inputs; changing defaults or policy does not mutate an
already admitted request. Current authentication and action authorization still
apply before revealing or continuing an operation. Tenant remapping cannot move
an existing operation into a different namespace.

A matching retry returns the existing operation; changed intent returns 409.
Admission denials that created no operation may be retried with the same key after
backoff. Once an operation is registered, even a definitive creation failure is
terminal for that key: a deliberate new attempt requires a new key. Key size,
request size, per-principal operation rate, and retained-record capacity are
bounded; exhaustion rejects new claims without evicting active records.

Keep nonterminal records and records with unreleased charges until reconciled.
Retain terminal records for at least 24 hours after final release, with a documented
retention deadline exposed to clients. The guarantee ends when a record is
purged: an opaque key cannot prove that a forgotten operation never existed.
Clients must never reuse keys or automatically retry after the retention deadline;
unknown/expired recovery needs explicit investigation. A timestamped key format
that rejects stale first use is an alternative to settle before finalizing the
wire contract if stronger post-retention rejection is required.

### AdmissionProvider Contract

The names below describe required semantics, not an accepted HTTP schema:

| Operation | Required behavior |
| --- | --- |
| `reserve(owner, key, intent_digest, charge, binding)` | Atomically find an existing operation, reject conflicting intent, or evaluate policy and persist one operation plus all charges; return a durable revision/token |
| `lookup(owner, key)` | Return only the authorized owner's record, including recovery state and retention information |
| `begin_create(operation, revision)` | Persist the transition to creating and the sole runtime attempt's identity before side effects; reject obsolete ownership |
| `commit(operation, revision, runtime_binding)` | Idempotently bind the observed resource without adding a second charge |
| `release(operation, revision, evidence)` | Idempotently close charges after definitive no-create or confirmed final cleanup; never release by elapsed time alone |
| `list_unsettled(cursor)` | Supply bounded reconciliation work to the trusted server, with tenant/namespace and state revisions |

Operation claim, reservation, and record persistence share one transaction or an
equivalent linearizable primitive. Independent stores with a best-effort sequence
of writes do not meet the contract. The coordinator may store encrypted full
intent separately only if the atomic record references an already durable,
immutable intent and missing data cannot cause another runtime attempt.

Provider RPC retries use the same operation identity and revision; an HTTP timeout
may mean the provider committed the write. Re-read or repeat the idempotent call,
never generate a second identity. Replaying `begin_create` must not grant a second
runtime dispatch. Only the uniquely acknowledged transition winner may dispatch
once; observing `creating` or receiving an already-applied response is not a new
permission to call the runtime. If that acknowledgement is ambiguous, reconcile
or retain uncertainty instead of dispatching from a recovered record. Provider endpoints are operator-configured,
authenticated, bounded, and TLS-protected across trust boundaries. The provider
cannot redirect creation to a caller-selected tenant, namespace, or runtime.

### Reservation and Recovery State Machine

```text
unseen -> reserved -> creating -> committed -> releasing -> released
              |          |
              |          +-> uncertain -> reconcile -> committed / releasing
              +-> failed (no side effects) -> released
```

Every transition is conditional on the recorded revision. `reserved`, `creating`,
`uncertain`, `committed`, and `releasing` all retain the same charge. Registration
of one operation and return of a reservation precede runtime work:

1. Authenticate, authorize, validate, and normalize without resource creation.
2. Atomically claim the key and reserve the count/CPU/memory vector. Persist the
   assigned sandbox ID and intent before responding or calling the runtime.
3. Record `creating`, then create only under that identity. Runtime adapters must
   propagate an immutable operation marker and support observation of the exact
   namespaced resource and its UID. Existing helpers that allocate another UUID
   on each call must be adapted; this is new work.
4. On observed runtime acceptance, persist the binding through `commit` and return
   the existing create response. A response lost after commit is recovered from
   the operation record. Acceptance does not assert that the workload is healthy.
5. A definitive pre-side-effect rejection releases once. A runtime error after
   side effects requires verified cleanup first. Timeout, disconnect, cancellation,
   or a lost commit response produces an uncertain operation, retaining its charge.
6. Reconcile uncertain records and live bindings after restart and periodically.
   Commit matching observed resources; release only after definitive no-create or
   confirmed final removal. Preserve a terminal record for subsequent retries.

The ledger and Kubernetes are not one transaction. A deterministic name helps
observe a create, but does not fence a late request after deletion or prevent an
ABA reuse of that name. V1 therefore must not automatically reissue an ambiguous
runtime create, use a new sandbox ID, or release merely because a GET returned
404. Recovery must first establish that the original creator cannot still take
effect, using a runtime-enforced fencing/cancellation contract or a definitive
completion/cleanup result. If that cannot be established, leave the operation
uncertain and require operator reconciliation. Reservation/worker lease expiry
only schedules investigation; it cannot revoke a runtime operation by itself.
Stopping or losing contact with a local worker also does not fence a request
already sent to the runtime.

A crash after reservation but before the `creating` transition can be recovered
with a conditional transition and the saved intent. A crash after `creating` but
before the runtime call is intentionally indistinguishable from a lost runtime
response without additional fencing. Conservative temporary underutilization is
preferable to duplicate creation or unaccounted resources. These boundaries must
be tested before advertising restart recovery for a provider.

Deletion acknowledgement, TTL expiry time, a failed readiness probe, or loss from
an informer cache is not final cleanup evidence. Reconciliation checks the
workload and its dependent resource lifetime, including terminating workloads.
If the provider is unavailable during cleanup, keep the charge and retry release;
never delay safe authorized resource deletion solely to preserve availability of
the accounting service. Durable local/runtime evidence must make a later release
recoverable. Explicit administrative overrides are audited and must fence the
creator and establish cleanup; they cannot erase uncertainty by dropping a row.

### Budget Accounting

V1 charges a conservative vector per outstanding sandbox allocation:

- **Count:** one from reservation until confirmed release, including pending,
  creating, uncertain, running, paused, failed-but-not-cleaned, and terminating.
- **CPU/memory:** the normalized effective sandbox `resourceLimits`, in integer
  milli-CPU and bytes, held for the same lifetime. `resourceRequests` are separately
  validated for runtime scheduling; they do not reduce this charge. This measures
  allocated sandbox limits, not observed usage or billing consumption.
- Operator-injected sidecars, init containers, and other platform overhead remain
  covered by namespace ResourceQuota/LimitRange. This first budget is explicitly
  a sandbox-level entitlement, not a claim to equal total Pod/node capacity.

Pause does not refund the charge, so resume cannot bypass a new reservation.
Renewal keeps the same charge but rechecks authorization and configured lifetime
limits; resource resizing and new allocation modes are rejected in V1. Quota
reductions do not kill existing resources: new reservations fail while usage is
over the new limit. Policy changes do not invalidate an already committed charge.

All supported creation modes in an enabled tenant must use these rules. Unsupported
pool/restore/template/fast-sandbox paths fail before side effects, including routes
outside `POST /sandboxes`. Kubernetes quota rejection still follows its existing
runtime error contract and triggers release only when no residual resources remain.
No provider callback replaces the Kubernetes hard quota boundary.

### API and Error Semantics

The proposed public surface keeps the successful create response intact and adds
an authenticated operation lookup, illustratively
`GET /sandbox-operations/{idempotencyKey}`, scoped to the current principal.
The client knows the key before sending, so lookup remains possible even when
no create response or server-generated operation ID reaches it. Actual path,
key encoding, headers, and schema must be added to `specs/sandbox-lifecycle.yml`
and generated SDK clients in the implementation, not in this documentation PR.

| Condition in enabled mode | Proposed response |
| --- | --- |
| Invalid or missing tenant/delegated authentication | 401; verifier unavailable is 503, never anonymous fallback |
| Valid identity without action scope or policy permission | 403 with a stable admission-specific code |
| Missing/invalid key or unsupported create mode | 400 with a specific code; no side effects |
| Same owner/key with changed intent | 409 `ADMISSION_INTENT_CONFLICT` |
| Budget exhausted before an operation is claimed | 429 `ADMISSION_BUDGET_EXCEEDED`, bounded `Retry-After` guidance |
| Matching operation still creating | 409 `ADMISSION_OPERATION_IN_PROGRESS`, operation lookup location and `Retry-After`; no second create |
| Runtime outcome uncertain | 503 `ADMISSION_OUTCOME_UNKNOWN`, lookup location and operation ID when available; reservation retained |
| Admission provider unavailable, including ambiguous reserve result | 503 `ADMISSION_PROVIDER_UNAVAILABLE`; retry only the same key/request |
| Matching committed operation | Existing 202 create response for the same sandbox, plus operation/recovery metadata |
| Operation lookup | 200 with state, authorized binding, terminal error if any, and retention information; 404 when absent or not visible |

Use structured error codes to distinguish these cases from existing pool-capacity
429 and Kubernetes-quota 403 responses. A lookup 404 does not authorize switching
to a new key after a possibly delivered create: an in-flight reserve may still
commit. An explicit retry of the same key converges through atomic registration.
Authentication and authorization are checked on every retry and read.

A committed operation whose resource has since been deleted returns a terminal
operation result (proposed 410 on create replay), never recreates it. Recovery
responses contain no credentials or refreshed endpoint tokens; endpoint access
is separately authorized. Operation state is separate from sandbox readiness.
Clients cannot infer absence of resources from 5xx, or treat all 409s as terminal
intent conflicts. Responses and logs must avoid exposing another principal's
existence, secrets, or policy details.

SDK recovery is explicit and capability-gated, with an operation handle containing
the saved key and immutable request. It does not broaden OSEP-0017's default
non-idempotent transport retry rules or silently retry a legacy server. Legacy
clients are upgraded before a tenant is opted in; opted-out tenants retain their
current API and SDK behavior.

### First Implementation and Review Decisions

Recommended first slice:

1. Single-active lifecycle server, existing Kubernetes tenant isolation, standard
   non-pool image creation through one explicitly supported workload provider.
2. One trusted gateway identity profile; one durable admission provider with a
   conformance suite; active-count/CPU/memory budgets and owner-scoped recovery.
3. Runtime identity propagation, persistent reconciliation, deletion/expiry
   integration, and explicit SDK operation recovery before enabling enforcement.

The core hook must include the recovery/release contract from the beginning;
shipping only `allow()` would give a misleading budget guarantee. More runtimes
can opt in only after passing the same failure-boundary tests.

Before moving to `implementable`, reviewers should settle:

- Is the optional core enforcement/coordinator boundary acceptable, with platform
  policy outside the server? This proposal recommends it over a broker-only
  contract because lifecycle completion and expiry occur inside OpenSandbox.
- Which existing Kubernetes workload provider can meet the create-fencing and
  observation requirements? Until demonstrated, retain uncertain records rather
  than promise automatic recovery in every crash window.
- Which gateway assertion profile and durable provider/storage implementation
  should form the reference integration? Keep these separate from tenant lookup.
- Is a documented finite opaque-key recovery window sufficient, or should the
  first wire contract require time-bounded keys that reject post-retention replay?

These are review decisions, not claims that prerequisites are already implemented.

## Test Plan

The implementation must exercise the following at the provider, HTTP/service,
and real Kubernetes integration layers as applicable:

| Area | Required cases and invariant |
| --- | --- |
| Compatibility | Disabled mode unchanged; enabled mode rejects missing key/identity and every unsupported allocation path |
| Identity and scope | Forged/expired assertions, wrong issuer/audience/tenant, direct gateway bypass, key rotation, cross-principal key collision, owner-filtered pagination and endpoint/proxy denial |
| Intent | Equivalent resource units/map order match; changed environment/image/volume/expiry conflicts; default changes and upgrades preserve saved intent; reserved metadata cannot be overwritten |
| Atomic admission | Parallel distinct keys at the limit cannot overspend any dimension; duplicate keys create one reservation; rollback does not leave partial vector charges |
| Provider failures | Timeout before/after each provider write, duplicate RPCs, stale revisions, unavailable lookup, capacity exhaustion; no cached allow or duplicate charge |
| Runtime failures | Inject crashes before/after reservation, begin-create, runtime acceptance, commit, response, deletion, and release; assert both resource count and ledger charge |
| Ambiguity | Delayed create after timeout/404/lease expiry, concurrent lookup/retry, UID mismatch, name reuse, lost response; no blind release or second create |
| Reconciliation | Server restart, missed/duplicate/out-of-order events, unavailable Kubernetes, TTL expiry, external deletion, finalizer delay, failed cleanup, paused resources, provider outage during deletion |
| Lifecycle policy | Policy decrease below usage, renewal limits, unsupported resizing, no parent cascade, existing Kubernetes quota rejection and eventual release |
| SDK | Persist-before-send, explicit recovery, pending/unknown/terminal results, capability mismatch, expired retention, no legacy fallback or automatic fresh-key retry |

Run deterministic concurrent tests against the real reference store, not just a
mock or per-process lock. Use a fault-injecting runtime and delayed network replies
to test the uncertain windows; verify at least one real cluster provider end to
end before enabling it. Measure admission latency, reservation contention,
unsettled-record age, and cleanup lag; set bounded timeouts and storage limits
from those results. Any later multi-active support adds shared-store concurrent
writers, stale-owner fencing, failover, and lifecycle HA tests as a separate gate.

For this proposal-only PR, validate template/frontmatter, TOC and repository links,
both proposal indexes, the docs build, and whitespace. Runtime tests cannot verify
an unimplemented contract.

## Drawbacks

This introduces durable state and reconciliation into an otherwise straightforward
create request. Admission availability becomes a dependency for enabled tenants,
and fail-closed uncertainty can temporarily strand capacity. Principal-scoped
access checks also touch more than the create route; an implementation limited to
one middleware callback would be incomplete.

A provider interface has ongoing compatibility and operational costs. The narrow
first slice excludes useful pool and fast-sandbox modes, and keeping paused
resources fully charged is conservative. These are reasons to keep the extension
optional and review the contract before implementing it.

## Alternatives

1. **External Sandbox Broker only.** A broker can own credentials, principal
   verification, policy, and all lifecycle access. This is a valid simpler choice
   for deployments that can enforce exclusive broker access. It still needs
   durable recovery and resource reconciliation; polling cannot safely infer the
   outcome of a lost create by itself. Core operation identity and lifecycle
   hooks avoid each broker reconstructing deletion/expiry and runtime error
   boundaries. The proposed provider can itself be broker-backed.
2. **Kubernetes ResourceQuota and ingress limits only.** Keep them as hard tenant
   capacity and request-rate defenses. Neither establishes per-principal active
   allocations or identifies a retried logical create within a shared tenant.
3. **Add quotas to TenantProvider.** Reuses an extension point but couples cached
   authentication with transactional accounting. Stale tenant-cache fallback must
   not become stale permission to allocate; use a separate contract.
4. **Local counters plus caller cleanup.** Lower initial cost, but races across
   processes, loses state at restart, and leaks or undercounts when callers die.
   It cannot implement the stated guarantee.
5. **Idempotency without admission.** Useful independently, but duplicate prevention
   alone does not constrain an agent issuing many distinct valid operations. The
   two concerns share a durable operation boundary here; they need not become a
   general policy platform.

## Infrastructure Needed

An enabled deployment needs a trusted identity integration, a durable transactional
admission provider/store, and a reconciliation worker with access to the relevant
tenant namespaces. A remote provider adds authenticated connectivity, migrations,
backups, bounded retention, and availability monitoring. No new infrastructure is
required for deployments leaving the extension disabled. Storage and credential
handling must be specified before implementation approval; this draft does not
select a new database dependency or reuse the snapshot repository by assumption.

Audit operation transitions with tenant, verified principal, operation ID, policy
revision, charge, runtime binding, and reason. Do not log credentials, raw keys, or
full create payloads. Use bounded-cardinality metrics for denials, provider errors,
unsettled age, and release lag; keep high-cardinality identities in access-controlled
audit records.

## Upgrade & Migration Strategy

1. Land the reviewed OSEP first. Implementation changes to authentication, lifecycle
   specs, server, SDKs, and operators' docs follow only after design approval.
2. Deploy provider/identity integrations and upgraded recovery-capable clients
   while enforcement remains off. Dry-run observations may inform policy but
   cannot be represented as reserved capacity or security enforcement.
3. Enable only supported tenants/runtimes after existing sandboxes are either
   drained or explicitly imported with trusted owner and resource bindings.
   Unattributed resources block enablement; do not start accounting from zero.
4. Persist an enforcement marker for opted-in tenants. Missing/unavailable provider
   configuration on restart is an error, not a return to legacy creation. Rolling
   upgrades cannot leave an older ungated server handling the same tenant.
5. To disable or downgrade, stop new delegated creates, drain or reconcile all
   outstanding operations, and preserve recovery records through their advertised
   retention. Removing provider configuration or downgrading the server while live
   reservations exist is not a safe rollback.

Proposal numbering is provisional until merge because other open OSEPs may land
first. No existing tenant behavior changes merely by merging this document.
