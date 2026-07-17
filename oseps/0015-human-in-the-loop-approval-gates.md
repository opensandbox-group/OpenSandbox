---
title: Human-in-the-Loop Approval Gates
authors:
  - "@GodBlf"
creation-date: 2026-07-17
last-updated: 2026-07-17
status: draft
---

# OSEP-0015: Human-in-the-Loop Approval Gates

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
  - [Terminology](#terminology)
  - [Architecture and Ownership](#architecture-and-ownership)
  - [Lifecycle Policy](#lifecycle-policy)
  - [Command Matching and Execution Flow](#command-matching-and-execution-flow)
  - [New-Domain Egress Flow](#new-domain-egress-flow)
  - [Approval State Machine](#approval-state-machine)
  - [Runtime Approval APIs](#runtime-approval-apis)
  - [Events and Reviewer Notification](#events-and-reviewer-notification)
  - [SDK and CLI](#sdk-and-cli)
  - [Authentication and Sensitive Data](#authentication-and-sensitive-data)
  - [Capacity, Cleanup, and Observability](#capacity-cleanup-and-observability)
  - [Runtime Delivery](#runtime-delivery)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal introduces an optional, fail-closed approval gate that can pause
selected sandbox actions until an authorized reviewer allows or denies them.
The first phase covers execd commands selected by policy and egress requests to
new FQDNs. It gives autonomous agents a supervised boundary without changing
the behavior of sandboxes that do not configure an approval policy.

Approval state remains in the data-plane component that owns the blocked
action. Execd owns command approvals and egress owns domain approvals. Both
components expose the same approval resource model, while SDK and CLI facades
present a sandbox-scoped view across the two endpoints.

This proposal addresses [issue #1288](https://github.com/opensandbox-group/OpenSandbox/issues/1288).

## Motivation

OpenSandbox currently applies policy before a sandbox starts or rejects an
action immediately at runtime. Egress allow and deny rules are static until a
caller updates them, and the egress deny webhook only reports a request after
it has already been denied. Execd does not have an approval concept.

That model is suitable for fixed policy but not for supervising autonomous
agents. An operator may want an agent to continue independently until it
attempts a selected command or reaches a destination that has not yet been
reviewed. Rejecting every such action forces the agent to restart it manually,
while allowing it unconditionally removes the review boundary.

A blocking approval gate fills this gap: the runtime holds the action, exposes
sanitized context to an authorized reviewer, and resumes only after an explicit
allow decision. Missing decisions, component failures, and timeouts deny the
action.

### Goals

1. Let users opt into approval gates at sandbox creation time without changing
   existing sandbox behavior by default.
2. Pause selected execd commands before process creation and resume the same
   request after an allow decision.
3. Pause DNS handling for a new FQDN before the egress sidecar makes the
   destination reachable.
4. Provide consistent list, get, allow, and deny operations through runtime
   APIs, supported SDKs, and `osb`.
5. Fail closed on timeout, component shutdown, sandbox deletion, capacity
   exhaustion, or decision-channel failure.
6. Keep Docker and Kubernetes user-visible semantics aligned.
7. Preserve the precedence of operator and explicit deny policy.
8. Bound approval state and avoid leaking command or request secrets through
   logs, metrics, webhooks, or API error text.

### Non-Goals

1. This proposal does not turn command patterns into a security isolation
   boundary. An allowed command, code execution request, or child process can
   perform equivalent work through another mechanism.
2. The first phase does not inspect code execution, child processes created by
   an allowed command, file API operations, diffs, or generic "destructive"
   behavior.
3. The first phase does not gate direct-IP egress or individual TCP/HTTP
   connections. Domain approval occurs at DNS resolution time.
4. Credential Vault use approval is deferred. The design leaves room for a
   future `credential_use` approval kind but does not define its policy or API
   context here.
5. Browser review pages, scoped share links, push notification services, and a
   global cross-sandbox approval inbox are deferred.
6. Approval history is not a durable audit ledger. Terminal records are held
   only for a bounded time in the owning component.
7. Reviewers cannot edit or replace the command, domain, or other pending
   action context through the decision API.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | No configured approval policy preserves current behavior | Must Have |
| R2 | Matching commands are held before process creation | Must Have |
| R3 | New FQDNs are held before DNS resolution makes them reachable | Must Have |
| R4 | Timeout and runtime failure deny the held action | Must Have |
| R5 | Operator and explicit deny rules cannot be overridden by approval | Must Have |
| R6 | Execd and egress expose a common approval resource and decision contract | Must Have |
| R7 | Decisions are atomic, terminal, and idempotent for identical retries | Must Have |
| R8 | Pending and remembered state is bounded and sandbox-local | Must Have |
| R9 | Existing execd and egress endpoint authentication protects approval APIs | Must Have |
| R10 | Logs, metrics, errors, and notifications expose no more action context than their documented reviewer purpose requires | Must Have |
| R11 | Docker and Kubernetes implement the same public behavior | Must Have |
| R12 | SDK and CLI users can review and decide approvals for one sandbox | Must Have |
| R13 | Concurrent DNS requests for one normalized FQDN share one pending approval | Must Have |
| R14 | Allow decisions may remember an exact command or FQDN for a bounded TTL | Should Have |

## Proposal

Add an optional `approvalPolicy` to sandbox creation. The server validates and
delivers the relevant policy subset to execd and egress when it creates the
sandbox. It does not store pending approvals or participate in their state
transitions.

Execd evaluates each command immediately after request validation and before
starting a process. Egress evaluates a normalized DNS question after fixed deny
rules and existing allow rules. When an action needs review, the owning
component creates a local approval record and waits for a terminal decision.

Reviewers resolve the sandbox's existing execd and egress endpoints and use the
approval APIs protected by those endpoints' existing authentication. SDKs and
the CLI hide endpoint discovery and combine records from both components.

```
                                create sandbox
Client --------------------------------------------------------> Server
  |                                                               |
  |                                                    validates and splits
  |                                                    approvalPolicy
  |                                                               |
  |                                      +------------------------+------------------+
  |                                      |                                           |
  |                                      v                                           v
  |                           +---------------------+                      +---------------------+
  | command request           | execd               |                      | egress              |
  +-------------------------->| - command matcher   |                      | - DNS policy        |
  |                           | - approval store    |                      | - approval store    |
  |                           +----------+----------+                      +----------+----------+
  |                                      |                                            ^
  |                                      | wait                                       | DNS request
  |                                      v                                            | from sandbox
  |                           +---------------------+                                  |
  | approvals facade         | pending command     |                       sandbox ---+
  +------------------------->| allow/deny/expire   |
  |                           +---------------------+
  |
  +-------------------------------> egress approval APIs
```

Approval decisions are data-plane local by design. This avoids a reverse
dependency from sandbox components to the lifecycle server, avoids giving
components a server-wide credential, and avoids introducing distributed
persistence or multi-replica coordination into the lifecycle control plane.

### Notes/Constraints/Caveats

1. The command matcher sees the exact command string accepted by the execd API;
   it does not parse shell syntax or infer semantic equivalence.
2. DNS approval controls when the sidecar resolves and admits a domain. Once an
   answer has been returned, the runtime cannot provide per-connection or
   one-packet approval semantics through DNS interception alone.
3. Egress approval requires an egress sidecar and therefore requires
   `networkPolicy`. A create request that configures egress approval without
   `networkPolicy` is invalid.
4. Approval cannot override `/var/egress/rules/deny.always` or an explicit user
   deny rule. Such requests are denied immediately and retain existing blocked
   event behavior.
5. Approval state is lost on execd or egress restart. Waiting actions are
   denied before shutdown where possible; after an abrupt restart they fail by
   connection loss or DNS failure and are never implicitly allowed.
6. A reviewer with approval access can see action context that may itself be
   sensitive. Approval access is equivalent to existing authenticated access
   to the corresponding sandbox runtime endpoint and must not be exposed
   through an unauthenticated share link in the first phase.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Glob patterns are bypassed by alternate spelling or indirection | A risky command does not pause | Document this as supervision, not isolation; match the complete raw string predictably; recommend broad patterns and static sandbox controls for enforcement |
| Reviewer approves misleading context | Unintended action runs | Do not permit context mutation; display exact bounded command or normalized FQDN; include kind and timestamps |
| Commands contain secrets | Secrets appear in logs or notifications | Return context only from authenticated get/list APIs; omit full context from logs, metrics, errors, and webhooks |
| Pending requests exhaust memory or goroutines | Runtime availability degrades | Enforce configurable hard caps and reject excess actions immediately |
| Runtime restart loses an allow decision | Agent action fails and may be retried | Keep state local and fail closed; expose restart/expiration outcomes clearly; do not claim durable approvals |
| Runtime restart loses a deny decision | A retry creates another review request | Deny decisions are not durable; callers that need permanent denial must update static policy |
| Approval overrides an operator restriction | Security policy is weakened | Evaluate always-deny and explicit deny before the approval layer and never create an approval for them |
| DNS cache outlives an approval TTL | Traffic continues after remembered approval expires | Cap returned DNS TTL to the remaining approval TTL and remove temporary nft state at expiry where applicable |
| Multiple DNS questions race | Duplicate prompts or inconsistent results | Normalize the FQDN and use single-flight pending records per domain |
| Webhook is mistaken for an authorization channel | Spoofed callback affects execution | Webhooks are notification-only; decisions require authenticated runtime API calls |
| Component APIs drift | SDK and CLI behavior differs by approval kind | Define common schemas and conformance tests in both OpenAPI contracts |

## Design Details

### Terminology

- **Approval policy**: Optional sandbox creation policy that selects actions for
  human review.
- **Approval**: A runtime resource representing one held command or normalized
  FQDN.
- **Decision**: An immutable terminal `allow` or `deny` operation applied to an
  approval.
- **Remembered decision**: A temporary in-memory allow cache entry for one exact
  command string or normalized FQDN.
- **Owning component**: Execd for command approvals and egress for domain
  approvals.

### Architecture and Ownership

Each owning component implements the same logical `ApprovalStore` behavior:

```
Create(kind, context, expiresAt) -> Approval
List(status?)                    -> []Approval
Get(id)                          -> Approval | not found
Decide(id, decision, ttl?)       -> Approval | conflict | expired
Expire(now)
Close()                          -> deny all pending waiters
```

The store owns synchronization between request handlers, timers, and blocked
runtime operations. A decision updates the record and wakes waiters atomically.
An allow is observed by the held operation only after the terminal state is
committed. Closing the store wakes every waiter with deny.

The lifecycle server remains responsible only for:

1. validating the public create policy;
2. rejecting egress approval without `networkPolicy`;
3. serializing the command subset into execd startup configuration;
4. serializing the egress subset into egress startup configuration; and
5. configuring component limits consistently for Docker and Kubernetes.

### Lifecycle Policy

The proposed additive lifecycle shape is:

```yaml
approvalPolicy:
  timeoutSeconds: 300
  commands:
    patterns:
      - "rm -rf*"
      - "curl*"
  egress:
    mode: new_domains
```

`approvalPolicy` is optional. Its fields have these semantics:

| Field | Semantics |
|-------|-----------|
| `timeoutSeconds` | Time a new approval may remain pending; default `300`; must be positive and is bounded by an implementation-defined server maximum |
| `commands.patterns` | Non-empty shell-style glob patterns matched against the complete command string; any match requires approval |
| `egress.mode` | The first phase accepts only `new_domains`; omitted egress configuration disables domain approval |

Pattern matching is case-sensitive and anchored to the complete command string.
`*`, `?`, and bracket expressions follow the selected standard-library glob
implementation; a backslash escapes the next metacharacter. The specification
and each SDK must use examples to make this behavior explicit. Empty patterns
and invalid bracket expressions are rejected at sandbox creation.

Unknown fields follow the existing lifecycle API validation policy. An empty
`approvalPolicy` is rejected because it cannot gate an action and is likely a
configuration mistake.

Component capacity limits are operator configuration, not per-sandbox public
API fields in the first phase. Initial defaults are 128 pending records and
1,024 remembered decisions per component. Operators may lower these values;
raising them must remain subject to documented hard maximums.

### Command Matching and Execution Flow

The command gate applies to:

- regular foreground and background command execution;
- commands run in an existing bash session; and
- commands run in an isolated execution session.

The flow is:

1. Execd authenticates and validates the request as it does today.
2. Execd checks the exact command against unexpired remembered allows.
3. If there is no remembered allow, execd evaluates every configured glob.
4. A miss proceeds through the current execution path unchanged.
5. A match creates a unique `command` approval before allocating or starting
   the child process.
6. Foreground/session SSE emits `approval_required` and remains connected.
   Background execution exposes `pending_approval` through command status.
7. Allow resumes the original in-memory request once. Deny or expiry returns a
   typed approval-denied or approval-expired execution error without starting
   the process.
8. A positive decision `ttlSeconds` also remembers the exact, byte-for-byte
   command string. It does not remember the glob or authorize other commands
   matched by that glob.

Each matching command invocation gets its own approval. This preserves the
request's session, working directory, environment, and cancellation context and
prevents one prompt from authorizing multiple concurrent executions. Interrupt
and request cancellation remove the waiter and make its approval terminal;
later allow attempts cannot start the canceled command.

### New-Domain Egress Flow

Domain approval is inserted into the DNS policy path with this precedence:

1. operator `deny.always` match: deny immediately;
2. operator `allow.always` match: allow immediately;
3. the first matching explicit user rule: apply its allow or deny immediately;
4. unexpired remembered approval for the normalized FQDN: allow temporarily;
5. `new_domains` enabled: create or join a pending approval;
6. no approval policy: apply the current default action unchanged.

This retains the existing `deny.always`, `allow.always`, then user-rule overlay
order. In particular, the approval layer never changes which static rule wins.
The egress component normalizes the cache key exactly as current policy matching
does: lowercase the DNS wire name and remove one trailing dot. Policy target and
DNS-question normalization must stay shared so equivalent names cannot create
separate decisions.

Concurrent questions for the same normalized FQDN join one pending record and
receive the same result. Deny or expiry returns a DNS refusal and does not add
temporary nft state. Allow resumes the held resolution, updates the dynamic
address set as required by the current egress mode, and remembers the domain
for the decision TTL. When `ttlSeconds` is omitted, the domain is remembered
for the smaller of the configured approval timeout and the upstream DNS TTL.

The effective DNS TTL and temporary address-set lifetime must not exceed the
remaining remembered-decision TTL. Direct-IP traffic, reverse DNS questions,
and non-address DNS record types are not turned into approval requests in the
first phase and continue through existing policy behavior.

### Approval State Machine

```
                        allow
                    +-----------> allowed
                    |
created ----------> pending
                    |             denied
                    +-----------> denied
                    |
                    | timeout / cancellation / shutdown
                    +-----------> expired
```

`allowed`, `denied`, and `expired` are terminal. The API uses `expired` for
timeout, request cancellation, and owning-component shutdown because in every
case the original action is no longer available to resume. An optional terminal
`reason` distinguishes those cases without exposing sensitive context.

Submitting the same decision again returns `200` and the existing terminal
record. Submitting the opposite decision to an allowed or denied record returns
`409 Conflict`. Deciding an expired record returns `410 Gone`. An unknown or
evicted identifier returns `404 Not Found`.

Terminal records remain listable for a bounded retention window to make retries
and immediate diagnosis possible. Retention does not extend a remembered allow
and is not an audit guarantee.

### Runtime Approval APIs

Execd and egress add equivalent operations to their respective public specs:

| Method and path | Behavior |
|-----------------|----------|
| `GET /approvals?status=pending` | List local approvals, optionally filtered by one status |
| `GET /approvals/{approvalId}` | Return one local approval |
| `POST /approvals/{approvalId}/decision` | Atomically allow or deny one pending approval |

The common response shape is:

```json
{
  "id": "execd_apr_01J...",
  "source": "execd",
  "kind": "command",
  "status": "pending",
  "context": {
    "command": "rm -rf ./build"
  },
  "createdAt": "2026-07-17T08:00:00Z",
  "expiresAt": "2026-07-17T08:05:00Z"
}
```

`source` is `execd` or `egress`, and IDs use the corresponding
`execd_apr_` or `egress_apr_` namespace. This makes an ID globally routable
within a sandbox without querying or submitting a decision to both components.
The unpredictable suffix, not the prefix, supplies collision resistance.

`kind` is `command` or `egress_domain` in the first phase. Context is a tagged
union: command records include a length-bounded command string; domain records
include only the normalized FQDN. Implementations report truncation explicitly
and never silently substitute a different value for the action being approved.
The original full value remains internal to the blocked operation.

Terminal responses additionally contain `decidedAt`, `decision`, and optional
`reason`. They do not identify the reviewer in the first phase because runtime
endpoint authentication does not currently provide a stable reviewer identity.

The decision request is:

```json
{
  "decision": "allow",
  "ttlSeconds": 600
}
```

`decision` is required and is `allow` or `deny`. `ttlSeconds` is optional,
non-negative, and bounded by an operator maximum. Zero or omission is a
single-use command decision. For an egress allow, omission uses the bounded DNS
default described above. `ttlSeconds` on deny is rejected in the first phase;
durable or remembered denial belongs in static policy.

List responses use the component's existing pagination conventions where they
exist; otherwise the new specs define a stable cursor and bounded page size.
Ordering is newest creation time first with `id` as a deterministic tie-breaker.

### Events and Reviewer Notification

Execd adds an `approval_required` SSE event containing the approval ID, kind,
creation time, and expiry time. It does not duplicate the full command in event
logs. Existing completion and error events remain the source of the final
execution result after allow, deny, or expiry.

Background command status adds `pending_approval` and the approval ID without
changing the meaning of existing terminal statuses.

Egress extends the existing asynchronous blocked-event infrastructure with an
approval-pending notification. The notification contains the approval ID,
normalized FQDN, and expiry time and is emitted once per pending record, not
once per joined DNS question. The webhook is notification-only: response bodies
and status codes cannot allow or deny the request. Existing deny webhooks keep
their current payload and already-denied semantics.

### SDK and CLI

Sandbox SDKs expose a sandbox-scoped approval facade with equivalent operations
in each supported language:

```
sandbox.approvals.list(status = pending)
sandbox.approvals.get(approval_id)
sandbox.approvals.allow(approval_id, ttl = ...)
sandbox.approvals.deny(approval_id)
```

For list operations, the facade resolves both execd and egress endpoints,
queries them concurrently, and merges results by creation time. Get and decision
operations route directly from the component namespace in the approval ID; an
unknown prefix is rejected locally. Public durations use each language's
established native duration convention while the wire format remains seconds.

The stable CLI surface is:

```text
osb approvals list <sandbox-id> [--status pending] [-o table|json|yaml]
osb approvals get <sandbox-id> <approval-id> [-o table|json|yaml]
osb approvals allow <sandbox-id> <approval-id> [--ttl SECONDS] [-o table|json|yaml]
osb approvals deny <sandbox-id> <approval-id> [-o table|json|yaml]
```

CLI commands call the Python SDK facade rather than reconstructing endpoint
paths. If only one component exists or is reachable, list returns its records
and reports the unavailable source through the SDK's established partial-error
model. Get and decision operations fail with the owning endpoint's error and
never fall back to the other component. The final SDK design must make this
partial-result behavior explicit and consistent across languages before the
OSEP becomes `implementable`.

### Authentication and Sensitive Data

Approval APIs use the current authentication for the owning runtime endpoint.
This proposal does not add an unauthenticated decision route. Lifecycle server
proxy and signed-endpoint access must preserve the same scope as direct execd
or egress access and must not turn a sandbox application credential into a
reviewer credential.

Command strings are inherently capable of containing tokens, URLs, and inline
secrets. Therefore:

- authenticated get/list responses may return a bounded command because a
  reviewer needs the action context;
- logs, traces, metrics, generic error messages, and capacity warnings contain
  only approval ID, kind, state, and timing data;
- notification webhooks must support an operator option to omit command context
  and default to omission;
- URL query strings, HTTP headers, DNS answers, environment variables, and
  Credential Vault material are never approval context in the first phase; and
- OpenTelemetry attributes use low-cardinality kind/status/reason labels only.

Approval ID suffixes are unpredictable but IDs are not authorization secrets.
All API access still requires endpoint authentication.

### Capacity, Cleanup, and Observability

Each component enforces separate caps for pending records, retained terminal
records, and remembered allows. A request that would exceed the pending cap is
denied immediately with a typed capacity error. It is not queued outside the
store and does not evict an existing pending action.

Expiration uses a deadline-aware timer or heap rather than one unbounded
goroutine per retained record. Terminal records and remembered decisions are
removed after their respective deadlines. Sandbox or component shutdown closes
the store, wakes waiters, and clears remembered state.

The initial metrics are counters and gauges for:

- approvals created by kind;
- terminal decisions by kind, decision, and reason;
- pending approval count;
- decision latency;
- capacity rejections; and
- remembered-decision hits and expirations.

No metric label contains approval ID, command, FQDN, sandbox ID, or reviewer
input. Structured logs follow the same restriction.

### Runtime Delivery

The server serializes validated policy as component-specific startup
configuration. Docker passes it when creating the execd and egress containers.
Kubernetes places it in the corresponding container environment or mounted
configuration using the existing workload-building path. Policy is immutable
for the sandbox lifetime in the first phase.

Both runtimes must apply the same defaults, validation, capacity behavior, and
failure semantics. Runtime-specific endpoint discovery and token delivery are
implementation details hidden by the SDK. The public lifecycle request and
runtime approval schemas remain the sources of truth and generated consumers
must be regenerated from them during implementation.

## Test Plan

**Contract and unit tests:**

- Lifecycle schema accepts valid command-only, egress-only, and combined
  policies and rejects empty policies, invalid globs, invalid timeouts, and
  egress approval without `networkPolicy`.
- Glob matching is anchored, case-sensitive, deterministic, and covers escaping
  and invalid bracket expressions.
- FQDN normalization, invalid-name rejection, explicit deny, `deny.always`,
  explicit allow, and remembered-allow precedence are verified.
- State transitions cover allow, deny, timeout, cancellation, shutdown,
  identical retry, conflicting retry, expired record, unknown record, and
  terminal-record eviction.
- TTL caching covers exact-command isolation, FQDN expiry, DNS TTL capping, and
  temporary nft cleanup.
- Concurrent questions for one FQDN create one approval; concurrent commands
  create independent approvals.
- Capacity limits reject new actions without evicting waiters, leaking
  goroutines, or logging sensitive context.

**Execd integration tests:**

- Foreground, background, bash-session, and isolated-session commands pause
  before process creation and resume only after allow.
- Deny and timeout produce typed execution failures and never start a process.
- SSE disconnect, interrupt, session deletion, and execd shutdown wake or clean
  up waiters without later execution.
- Commands that do not match and commands covered by remembered exact allows
  retain expected streaming and status behavior.

**Egress integration tests:**

- A new domain remains unresolved while pending; allow resumes resolution and
  traffic, while deny and timeout return failure without leaking packets.
- Static allow rules bypass approval, and explicit/always deny rules cannot be
  overridden.
- Concurrent DNS requests share a prompt and all waiters receive one result.
- Existing deny webhook behavior is unchanged and the pending webhook cannot
  decide an approval.

**SDK and CLI tests:**

- Generated models match both specs and handwritten facades route decisions to
  the owning component.
- Cross-language duration conversion, error mapping, pagination, ordering, and
  partial endpoint failure are consistent.
- CLI help, SDK call arguments, table/JSON/YAML output, and allow/deny exit codes
  are covered with mocked SDK calls.

**End-to-end tests:**

- Docker and Kubernetes each create a sandbox with combined approval policy,
  exercise command and domain approvals through SDK and CLI, and verify cleanup
  at sandbox deletion.
- Existing create, command, egress policy, deny webhook, and SDK flows are run
  without `approvalPolicy` to demonstrate backward compatibility.

## Drawbacks

1. Two local approval stores require common schemas and conformance tests and
   cannot provide a naturally global inbox.
2. Approval records and remembered decisions disappear on component restart.
3. Holding SSE requests and DNS questions consumes bounded runtime resources and
   adds latency at review boundaries.
4. Raw-command glob matching is understandable but cannot reliably classify the
   semantic risk of arbitrary shell behavior.
5. DNS-level approval is coarse: it approves temporary reachability to a domain,
   not one application-layer request.
6. SDK aggregation must represent partial failures when only one component is
   reachable.

## Alternatives

**Central lifecycle-server coordinator.** The server could store every approval
and expose one global API. This simplifies listing but requires runtime
components to call back to the server, distributes a new service credential,
and introduces persistence and multi-replica consistency into the blocking
path. Data-plane ownership is smaller and fails closed naturally.

**External approval broker or synchronous webhook.** An external service could
own decisions. This is useful for enterprise integration but makes a new
dependency part of the action hot path and leaves standalone OpenSandbox
without a complete workflow. A future adapter can mirror local pending events
to an external system without making it the core state owner.

**Update static policy and retry the action.** A reviewer could edit egress
policy or command configuration after denial. This does not hold and resume the
original action, is prone to races, and broadens a one-time approval into a
configuration change.

**HTTP interception for all egress approval.** Transparent MITM can approve
individual HTTP requests and show richer context, but it excludes non-HTTP
traffic and adds CA, encryption, privacy, and protocol concerns. DNS-level new
domain approval matches the first-phase requirement with the current egress
architecture.

**Semantic command parsing.** Parsing shell ASTs or classifying commands with a
model may produce richer prompts, but shell dialects, expansion, scripts, and
indirection make it unsuitable as the deterministic first policy primitive.

## Infrastructure Needed

No new external service or persistent database is required. Implementation will
require additive lifecycle, execd, and egress OpenAPI changes; regenerated SDK
clients; component-local in-memory stores; and Docker and Kubernetes end-to-end
coverage. Existing endpoint authentication, SSE, egress DNS policy, and webhook
infrastructure are reused.

## Upgrade & Migration Strategy

The feature is additive and opt-in:

1. Servers and generated clients add the optional `approvalPolicy` field.
2. Execd and egress add approval resources without changing existing paths.
3. A missing policy runs the existing command and egress paths without creating
   approval state or events.
4. Existing `OPENSANDBOX_EGRESS_DENY_WEBHOOK`, static network policies,
   `deny.always`, and Credential Vault behavior remain unchanged.
5. During a mixed-version rollout, the server must not accept an approval policy
   unless the selected runtime images advertise support; it fails sandbox
   creation rather than silently ignoring the policy.
6. SDK and CLI additions do not remove or rename existing methods or commands.
7. Rollback is performed by omitting `approvalPolicy` and using runtime images
   without the additive endpoints; no stored approval data needs migration.

The proposal must reach `implementable` before public contracts or runtime code
are changed. Credential-use approvals, share links, a durable audit store, and a
global approval service require follow-up design review rather than being
silently added to the first implementation.
