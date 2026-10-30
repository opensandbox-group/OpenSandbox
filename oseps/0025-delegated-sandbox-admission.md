---
title: Broker Integration for Delegated Sandbox Creation
authors:
  - "@hpliStartAgain"
creation-date: 2026-09-30
last-updated: 2026-10-13
status: draft
---

# OSEP-0025: Broker Integration for Delegated Sandbox Creation

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
  - [Existing Capabilities](#existing-capabilities)
  - [Caller-Supplied Sandbox ID](#caller-supplied-sandbox-id)
  - [Broker Integration](#broker-integration)
  - [Recovery Boundaries](#recovery-boundaries)
  - [Delivery Scope](#delivery-scope)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Recommend an external broker/frontend for principal-specific access and budgets
within a shared tenant. Propose an optional, constrained caller-supplied sandbox
ID as the core addition, allowing exact lookup after a lost create response.
Existing metadata passthrough supplies correlation. This ID supports positive
recovery; it does not make create safely retryable after deletion or uncertainty.

This replaces the earlier core `AdmissionProvider` proposal. Policy and accounting
stay with the platform; OpenSandbox supplies lifecycle mechanisms. For stable
principals using Kubernetes object-count quotas, namespace-per-principal may be a
simpler starting point than shared-namespace budget accounting.

## Motivation

Agents and workflow sessions sharing a tenant may need different sandbox
allowances. The platform knows each principal and its policy, while OpenSandbox
knows the sandbox lifecycle. A broker can connect those responsibilities without
introducing platform-specific admission semantics into every runtime and SDK.

A lost create response exposes a smaller mechanism gap: the caller may not know
the server-generated sandbox ID. Metadata search can find an observed resource,
but a caller-known ID enables exact GET without searching a changing list.
Neither approach proves that an unobserved request cannot still take effect.

[Issue #1570](https://github.com/opensandbox-group/OpenSandbox/issues/1570) records
the use case. [OSEP-0014](0014-multi-tenancy.md) leaves quotas and rate limiting to
deployment infrastructure. [OSEP-0017](0017-resilient-sdk-transport.md) distinguishes
safe transport retries from ambiguous POST outcomes and excludes server-side
idempotency from its scope. This proposal follows those boundaries.

### Goals

- Recommend a platform-owned integration for delegated creation and access.
- Add a generally useful caller-known identity for positive resource recovery.
- Reuse metadata, tenant isolation, lifecycle APIs, and Kubernetes quotas.
- State recovery limits without claiming exactly-once creation or immediate
  physical-resource reclamation.

### Non-Goals

- Core principal verification, policy evaluation, quota ledgers, or admission
  callbacks; no `reserve/commit/release` provider protocol.
- Standardizing a broker database, state machine, or hosted service.
- Durable create idempotency, automatic resolution of every crash window, or
  general lifecycle-server high availability.
- Hierarchical budgets, billing, or simultaneous adoption by every creation mode.

## Requirements

The proposed core identity capability must:

1. Preserve server-generated IDs when the new field is omitted.
2. Validate canonical form, routing compatibility, and collision behavior before
   resource-creating side effects; never silently replace a supplied identity.
3. Preserve tenant isolation and define uniqueness for all routes that resolve a
   sandbox by ID without tenant context.
4. Reject unsupported uses explicitly before side effects and document its
   supported providers and creation modes.
5. Distinguish resource identification from durable replay suppression. No new
   authorization meaning is assigned to caller metadata or sandbox IDs.

## Proposal

| Component | Responsibility |
| --- | --- |
| Platform broker | Principal authentication, authorization, budgets, request correlation, and accounting |
| OpenSandbox | Tenant authentication, lifecycle operations, runtime integration, and constrained caller-supplied identity |
| Deployment infrastructure | Namespace quotas, workload isolation, credential protection, and network access controls |

The broker calls ordinary lifecycle APIs. OpenSandbox has no broker callback or
availability dependency. The integration below is reference guidance, not a new
broker conformance protocol.

### Notes/Constraints/Caveats

A conservative integration can use existing APIs today. Uncertain operations may
retain budget until operator investigation establishes their outcome. Adding a
known ID improves lookup but does not eliminate this availability trade-off.

An initial example should target ordinary Kubernetes creation with one verified
provider and count budgets. It does not imply coverage of pools, templates,
snapshot restore, fast-sandbox, or all lifecycle-server deployment models.

### Risks and Mitigations

| Risk | Integration boundary |
| --- | --- |
| Caller bypasses policy | Broker-exclusive credentials and controlled lifecycle/data-plane access |
| Concurrent budget checks overspend | Platform-owned atomic accounting and durable request registration |
| Create response is lost | Retain uncertainty and observe the original resource; no blind replacement |
| Metadata is changed or forged | Broker records establish ownership; metadata only assists correlation |
| Deletion is accepted before cleanup finishes | Separate logical accounting from physical-resource and quota observations |

## Design Details

### Existing Capabilities

The API already accepts create metadata, returns it on GET, supports
metadata-filtered paginated lists and PATCH, and exposes deletion and expiration.
Authenticated Kubernetes lifecycle API calls use the tenant namespace; some
background and proxy resolution paths also resolve IDs without tenant context.
`X-Request-ID` is tracing, not caller-visible idempotency.

Kubernetes and Docker creation generate sandbox IDs. Fast-sandbox uses a
runtime-specific request identity, but that identity is generated for a public
create call; its recovery does not establish cross-request caller idempotency.

Metadata is mutable, has reserved keys and provider-specific format limits, and
PATCH has no optimistic locking. Existing passthrough should be documented and
tested rather than presented as a new API. Only demonstrated compatibility gaps
should require additional core changes.

### Caller-Supplied Sandbox ID

The recommended core direction is an optional canonical caller-supplied ID for
`POST /sandboxes`, usable with existing GET. The caller can persist it before
sending. The precise field, accepted format, and first provider remain design
review decisions; they belong in the public OpenAPI contract before implementation.

Prefer a constrained, unpredictable identifier format over arbitrary names.
Current backend routing uses prefixes such as `fsb-`, and providers normalize
names and support legacy aliases. Canonical input must avoid routing ambiguity
and distinct inputs mapping to the same underlying resource. Uniqueness must
cover the service's ID-only routing domain, not merely one namespace.

A collision must not create another resource, modify the existing one, or extend
its lifetime. It is not automatic evidence that the request succeeded: the caller
retrieves and verifies the original binding. Unsupported paths reject the new
field rather than ignoring it. Calls omitting it retain existing behavior.

A resource name does not retain a create result after deletion. Once a name is
available again, replay can create another resource. The reference broker never
reuses IDs or automatically resends an ambiguous create, even with the same ID.
Supporting safe retries would require the separate token contract discussed in
Alternatives; it is not implied by this proposal.

### Broker Integration

A minimal reference flow authenticates a principal, registers its logical request
and count charge atomically in platform-owned durable storage, then calls create
with broker-controlled correlation metadata. With the proposed capability, it
also records the chosen ID before dispatch. Repeated client requests resolve the
same broker operation rather than causing another dispatch. The platform chooses
its storage and recovery implementation; detailed transaction and retention
procedures belong in integration guides.

A broker can crash after recording dispatch but before receiving a response.
Its database cannot atomically commit an OpenSandbox create. Retaining that
uncertain charge is safer than treating restart as permission to dispatch again.
The broker's retry-history policy also must not turn an old request into a new
create merely because a record was discarded.

A deployment claiming principal isolation needs an explicit route allowlist and
protected data-plane access. Unimplemented operations, including alternate create
paths and administrative routes, remain inaccessible to delegated callers. Tenant
credentials alone do not provide per-principal authorization. Single-tenant proxy
paths bypass normal API-key authentication, so lifecycle ingress restrictions alone
are insufficient: use network isolation or an authenticated mediation boundary.
For directly exposed Kubernetes gateway endpoints, use `secureAccess` with
controlled token distribution; tokens do not replace broker ownership checks.

The example should enforce bounded lifetime and control renewal, including
access-triggered renewal. The public API permits an omitted timeout, subject to
provider restrictions. Resource access and metadata changes are mediated by the
broker, which filters results before exposing pagination or details.

Verify SDK retry configuration as well as broker logic. For example, the Go SDK's
opt-in retry configuration can retry POST; it is disabled by default. Do not enable
post-send create retries merely because the broker has persisted an operation ID.

### Recovery Boundaries

| Observation | Safe interpretation |
| --- | --- |
| Matching resource is observed | Record its binding; this does not prove the original create finished successfully |
| Timeout, ambiguous error, or restart during dispatch | Keep the operation unresolved and its charge retained; do not dispatch a replacement |
| GET returns `404` or list has no match | Absence at that observation does not exclude delayed creation |
| Verified rejection before all side effects | Release the charge once; establish this from the specific path, not an HTTP status class |
| DELETE returns or requested expiration passes | Continue cleanup observation; neither alone proves physical-resource release |

Current Kubernetes creation can create a resource, wait for readiness, and then
roll it back on failure. Cleanup can fail while the original error still returns.
Consequently, neither a generic error nor every quota response proves there were
no side effects. A resource found during an in-flight create can subsequently be
removed by rollback; disappearance alone does not establish whether creation
failed, deletion occurred, or expiration took effect.

A synchronous handler finishing is not a fence against an uncertain downstream
create taking effect. Resource absence supports release only when potentially
late creation is excluded and the relevant cleanup is established. Where current
APIs cannot establish that, retain uncertainty and provide operator investigation;
this proposal adds no fencing framework or operation-completion endpoint.

A logical sandbox slot and physical CPU/memory capacity need different evidence.
Lifecycle removal can end logical accounting under a platform's stated policy,
while dependent Pods or volumes may still exist. ResourceQuota continues to apply
its own resource accounting independently. TTL triggers cleanup; it is not a
reclamation deadline. CPU/memory charge models and cleanup runbooks are outside
the first count-budget example.

### Delivery Scope

First, document the integration and a small existing-API example, including its
unresolved-operation behavior. Architecture guidance, usage instructions, and
runnable examples should follow the repository's existing content ownership rules.

Separately review and implement the proposed ID capability for one ordinary
Kubernetes create path. Settle canonical format, routing-wide uniqueness,
collision behavior, and provider mapping before exposing it. Expand coverage only
with explicit tests; do not require all runtimes to adopt admission semantics.
The OSEP remains `draft` pending review of this direction and its open details.

## Test Plan

For the reference integration, test concurrent count admission, repeated client
requests, and restart before/during/after dispatch. Verify metadata recovery,
pagination changes, blocked unsupported routes, endpoint authorization, and renewal
policy. Inject lost responses, delayed creation followed by `404`, rollback
failure, and delayed cleanup. None should cause an unaccounted replacement.

For the ID capability, test omitted-field compatibility, invalid IDs, aliases,
concurrent collisions, cross-tenant routing, and exact lookup after response loss.
Verify no mutation or lifetime extension on collision, explicit rejection by
unsupported paths, metadata coverage, and deletion/name-reuse limits. Keep the
spec, implementation, affected SDKs, and documentation aligned.

These are planned implementation tests, not runtime validation by this document.

## Drawbacks

A shared-namespace broker needs durable accounting and operational ownership.
Conservative uncertainty handling can strand allowance. Caller-supplied IDs require
careful routing and collision analysis while providing less than durable
idempotency. Recovery and quota observations remain provider-dependent.

## Alternatives

### Namespace per Principal with ResourceQuota

For stable principals using agent-sandbox, one namespace and OpenSandbox tenant
per principal may be the simpler first choice. ResourceQuota can limit
`count/sandboxes.agents.x-k8s.io` directly, avoiding a separate broker ledger for
that object-count ceiling. This counts existing custom resources, not portable
sandbox execution or physical cleanup across all providers.

Operators provision namespaces, credentials, quotas, RBAC, and network policy.
Short-lived principals increase namespace/configuration churn; aggregate budgets
across namespaces still need platform policy. Current `/pools` management uses the
default namespace rather than tenant scoping, so pool and runtime resource
placement must be checked before claiming quota coverage.

Quota rejection also depends on what is counted and where admission occurs.
Agent-sandbox supports fail-fast reporting of resource quota failures; batchsandbox
can surface a readiness timeout for Pod-level quota denial. This is not a claim
that every batchsandbox quota rejection is a timeout. Object-count admission and
Pod resource admission are distinct. Quotas do not independently recover a lost
create response.

### Broker with Existing APIs Only

This requires no core changes. Metadata search can recover observed resources,
but missing matches leave uncertainty. It is suitable when the platform accepts
that limitation; the proposed ID improves direct lookup.

### Request-Scoped Idempotency Token

A token could support stronger retry semantics, but needs defined request
comparison, concurrent duplicate handling, original expiration, durable results,
and retention across deletion and restart. Expired history also needs explicit
behavior. Existing database infrastructure does not itself supply this contract.
Revisit it if integrations require safe automatic retry rather than positive lookup.

### Core AdmissionProvider

A core reservation protocol would couple lifecycle paths and clients to platform
policy. The proposed broker boundary avoids that cost. Reconsider only if concrete
integrations demonstrate a shared mechanism smaller primitives cannot provide.

## Infrastructure Needed

The reference broker uses platform-owned storage and reconciliation alongside
existing Kubernetes controls. No database product, hosted broker, or additional
infrastructure is mandated for other OpenSandbox deployments.

## Upgrade & Migration Strategy

The integration pattern changes no existing server behavior. Operators introducing
it need to close bypass routes and establish ownership/accounting for existing
resources before claiming enforcement; arbitrary metadata cannot establish that.

The future identity field is additive. Brokers verify provider/server support and
never downgrade an uncertain operation to a fresh create. Existing clients keep
server-generated IDs. Supported modes and recovery limits must accompany rollout.
