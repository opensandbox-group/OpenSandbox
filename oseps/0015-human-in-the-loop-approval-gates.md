---
title: Human-in-the-Loop Approval Gates
authors:
  - "@GodBlf"
creation-date: 2026-07-17
last-updated: 2026-07-21
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
  - [Reviewer Registry](#reviewer-registry)
  - [Lifecycle Policy](#lifecycle-policy)
  - [Command Matching and Execution Flow](#command-matching-and-execution-flow)
  - [New-Domain Egress Flow](#new-domain-egress-flow)
  - [Approval State Machine](#approval-state-machine)
  - [Broker Protocol](#broker-protocol)
  - [Reviewer Grants](#reviewer-grants)
  - [Reviewer API and Review Page](#reviewer-api-and-review-page)
  - [Events and Notifications](#events-and-notifications)
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

This proposal introduces an optional, fail-closed approval gate that pauses
selected sandbox actions until a distinct, authorized human reviewer allows or
denies them. The first phase covers execd commands selected by policy and egress
requests to new FQDNs. It gives autonomous agents a supervised boundary without
changing sandboxes that do not configure an approval policy.

Execd and egress retain the pending action and enforce the final decision. The
lifecycle server provides the reviewer control plane: it authenticates a
pre-registered reviewer, sends a scoped review link through a configured
webhook, serves a minimal review page, and brokers the decision to the owning
component. Agent endpoint credentials and reviewer credentials are separate and
cannot be substituted for one another.

This proposal addresses [issue #1288](https://github.com/opensandbox-group/OpenSandbox/issues/1288).

## Motivation

OpenSandbox currently applies policy before a sandbox starts or rejects an
action immediately at runtime. Egress allow and deny rules are static until a
caller updates them, and the egress deny webhook reports a request after it has
already been denied. Execd does not have an approval concept.

That model is suitable for fixed policy but not for supervising autonomous
agents. An operator may want an agent to continue independently until it
attempts a selected command or reaches a destination that has not yet been
reviewed. Rejecting every such action forces the agent to restart it manually,
while allowing it unconditionally removes the review boundary.

A human-in-the-loop gate requires more than a remotely unblockable pause. The
reviewer must be a principal distinct from the action initiator, must receive a
notification and usable entry point, must hold a credential scoped only to the
pending action, and must be recorded on the decision. This proposal makes those
properties part of the first-phase contract.

### Goals

1. Let users opt into approval gates at sandbox creation without changing
   existing sandbox behavior by default.
2. Pause selected execd commands before process creation and resume the same
   request after an allow decision.
3. Pause DNS handling for a new FQDN before the egress sidecar makes the
   destination reachable.
4. Require at least one operator-registered reviewer whose identity is distinct
   from the agent or sandbox endpoint caller.
5. Notify reviewers through a configured webhook carrying a short-lived,
   single-approval review link.
6. Provide a minimal browser review page plus grant-authenticated API, SDK, and
   CLI decision paths.
7. Attribute every allow or deny decision to the verified reviewer identity.
8. Fail closed on timeout, component shutdown, sandbox deletion, capacity
   exhaustion, notification failure, or broker failure.
9. Keep Docker and Kubernetes user-visible semantics aligned.
10. Preserve operator and explicit egress rule precedence.
11. Bound approval state and avoid leaking commands, request secrets, reviewer
    grants, or webhook credentials through logs, metrics, or error text.

### Non-Goals

1. Command patterns are not a security isolation boundary. An allowed command,
   code execution request, or child process may perform equivalent work through
   another mechanism.
2. The first phase does not inspect code execution, child processes created by
   an allowed command, file API operations, diffs, or generic "destructive"
   behavior.
3. The first phase does not gate direct-IP egress or individual TCP/HTTP
   connections. Domain approval occurs at DNS resolution time.
4. Credential Vault use approval is deferred. A future `credential_use` kind
   may reuse the reviewer model but requires separate context and policy design.
5. A global cross-sandbox inbox, mobile application, email provider, and native
   integrations for specific chat systems are deferred. Webhook delivery and
   the built-in review page form the first-phase entry point.
6. Approval history is not a durable audit ledger. Terminal records are retained
   for a bounded period by the owning component.
7. Reviewer self-service enrollment, OIDC federation, and runtime reviewer CRUD
   are deferred. Operators register reviewer identities in server configuration.
8. Reviewers cannot edit or replace the command, domain, or other pending action
   context through the decision API.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | No configured approval policy preserves current behavior | Must Have |
| R2 | Matching commands are held before process creation | Must Have |
| R3 | New FQDNs are held before DNS resolution makes them reachable | Must Have |
| R4 | Timeout and runtime or broker failure deny the held action | Must Have |
| R5 | Operator and explicit deny rules cannot be overridden by approval | Must Have |
| R6 | Execd and egress expose a common broker-only approval contract | Must Have |
| R7 | Normal execd, egress, sandbox, and agent credentials cannot read or decide approvals | Must Have |
| R8 | Every decision is authenticated as a pre-registered reviewer and records that reviewer ID | Must Have |
| R9 | Reviewer grants are signed, short-lived, and scoped to one sandbox and one approval | Must Have |
| R10 | A pending action triggers an authenticated webhook notification with a browser review entry point | Must Have |
| R11 | Decisions are atomic, terminal, and idempotent only for the same reviewer replaying the same decision | Must Have |
| R12 | Pending, terminal, and remembered state is bounded and sandbox-local | Must Have |
| R13 | Logs, metrics, errors, and notifications expose no more context than their documented purpose requires | Must Have |
| R14 | Docker and Kubernetes implement the same public behavior | Must Have |
| R15 | Concurrent DNS requests for one normalized FQDN share one pending approval | Must Have |
| R16 | Allow decisions may remember an exact command or FQDN for a bounded TTL | Should Have |

## Proposal

Add an optional `approvalPolicy` to sandbox creation. A policy references one or
more reviewer IDs that the operator has registered in server configuration. The
server rejects unknown or disabled reviewers, generates a per-sandbox broker
token, and delivers component-specific policy and broker configuration to execd
and egress.

Execd evaluates commands before process creation. Egress evaluates a normalized
DNS question after fixed rules. When an action requires review, the owning
component creates a local approval record and notifies the lifecycle server over
an authenticated internal callback. The server signs one grant per configured
reviewer and sends each reviewer a webhook containing a review URL. The grant is
bound to the reviewer, sandbox, component, approval, scopes, and expiration.

The browser review page keeps the grant in the URL fragment and uses it as a
Bearer credential for the public reviewer API. After validation, the server
uses the sandbox's separate broker token to read or decide the component-local
approval. The component records the verified reviewer ID and wakes the blocked
action. A normal action client never receives the reviewer grant or broker
token and cannot approve its own request.

```
 Operator config                     Lifecycle server
 reviewer ID + webhook       +--------------------------------+
            ---------------->| reviewer registry              |
                             | grant signer + review page     |
                             | reviewer API + broker proxy    |
                             +-------+----------------+-------+
                                     ^                |
                    broker callback  |                | reviewer webhook
                    + broker token   |                | scoped review URL
                                     |                v
                              +------+-----+      +-----------+
 Agent command ------------->| execd      |      | Reviewer  |
                              | local wait |      | browser   |
                              +------+-----+      +-----+-----+
                                     ^                  |
                                     | broker decision  | grant-authenticated
                                     +------------------+ server API

 Sandbox DNS -------------> egress follows the same callback and decision flow.
```

Approval state remains data-plane local. The lifecycle server authenticates and
routes reviewer operations but does not persist pending actions or become the
wait primitive. This keeps process and DNS resumption atomic with component
state while providing an identity-aware control plane.

### Notes/Constraints/Caveats

1. The command matcher sees the exact command string accepted by the execd API;
   it does not parse shell syntax or infer semantic equivalence.
2. DNS approval controls when the sidecar resolves and admits a domain. Once an
   answer has been returned, DNS interception cannot provide per-connection or
   one-packet approval semantics.
3. Egress approval requires an egress sidecar and therefore `networkPolicy`.
4. Approval cannot override `/var/egress/rules/deny.always` or an explicit user
   deny rule. Such requests are denied immediately.
5. Approval state is lost on execd or egress restart. Waiting actions are denied
   before graceful shutdown where possible and never implicitly allowed after
   an abrupt restart.
6. Reviewer IDs are operator-controlled stable identifiers. A sandbox creator
   selects registered reviewers but cannot submit a new identity, webhook URL,
   webhook secret, signing key, reviewer grant, or broker token.
7. Anyone holding a review link is acting as the reviewer named by its grant.
   HTTPS, short expiry, single-approval scope, fragment transport, and careful
   webhook access control are therefore mandatory.
8. The lifecycle server is trusted to authenticate reviewers and broker their
   decisions. Compromise of the control plane is outside the sandbox threat
   boundary.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Agent approves its own action with its endpoint token | Human boundary is bypassed | Approval routes require a non-empty, per-sandbox broker token; only server-issued reviewer grants reach the public reviewer API |
| Review link is leaked | Attacker acts as the named reviewer | Put grant in URL fragment, require HTTPS, bind it to one approval, cap it at pending expiry, suppress referrers, and never log it |
| Reviewer registry is misconfigured | Wrong or unreachable reviewer is selected | Validate reviewer IDs and enabled state at sandbox creation; reject policy when none remain |
| Webhook delivery fails | Reviewer does not learn about the action | Retry with bounded backoff and stable event ID; action remains pending and expires deny |
| Duplicate webhooks cause repeated decisions | Confusing reviewer experience | Idempotency key is derived from sandbox, approval, and reviewer; terminal decision handling is atomic |
| Glob patterns are bypassed by alternate spelling | Risky command does not pause | Document supervision rather than isolation; match the complete raw string deterministically; retain static controls |
| Commands contain secrets | Secret appears in notification or UI | Default webhook to summary metadata; full bounded context is fetched only through the grant-authenticated API |
| Pending requests exhaust resources | Runtime availability degrades | Enforce hard caps and reject excess actions immediately |
| DNS cache outlives an approval TTL | Traffic continues after grant intent expires | Cap DNS and temporary nft lifetimes to the remembered allow TTL |
| Multiple reviewers race | Attribution or result is ambiguous | First terminal decision wins; only the same reviewer replaying the same decision is idempotent; all other attempts return `409` |
| Broker token is omitted or confused with normal auth | Approval route becomes unprotected | Broker middleware always fails closed when its token is empty and ignores normal endpoint credentials |
| Component APIs drift | Server routes kinds differently | Define common schemas and conformance tests in execd and egress specs |

## Design Details

### Terminology

- **Reviewer**: One operator-registered, accountable human principal with a
  stable ID and webhook destination. Shared team identities are not reviewers
  in the first phase.
- **Approval**: A component-local resource representing one held command or
  normalized FQDN.
- **Reviewer grant**: A server-signed Bearer credential scoped to one reviewer,
  sandbox, source component, approval, permission set, and expiration.
- **Broker token**: A random per-sandbox secret shared only between the server
  and owning components for internal callback, read, and decision operations.
- **Decision**: An immutable terminal `allow` or `deny` operation attributed to
  the verified reviewer.
- **Remembered allow**: A temporary component-local allow cache entry for one
  exact command or normalized FQDN.

### Architecture and Ownership

Each owning component implements the same logical store:

```
Create(kind, context, expiresAt)          -> Approval
Get(id)                                   -> Approval | not found
Decide(id, decision, ttl, reviewerId)     -> Approval | conflict | expired
Expire(now)
Close()                                   -> expire all pending waiters
```

The component owns synchronization between request handlers, timers, and the
blocked action. A broker-authenticated decision commits the terminal record,
including `decidedBy`, before waking the waiter. Closing the store wakes every
waiter without allowing it.

The lifecycle server owns:

1. reviewer registration and webhook credentials;
2. approval signing keys;
3. lifecycle policy validation and reviewer selection;
4. per-sandbox broker token creation and runtime delivery;
5. pending-event authentication and webhook delivery;
6. reviewer grant validation and minimal review UI; and
7. broker-authenticated routing to execd or egress.

The server does not store pending or terminal approval state. It resolves the
current owning component from sandbox runtime metadata for every reviewer
request. Pending notifications may be retried without creating server state.

### Reviewer Registry

Reviewers are registered in server configuration, conceptually:

```toml
[approval]
active_signing_key = "a"

[[approval.signing_keys]]
key_id = "a"
secret = "env:OPENSANDBOX_APPROVAL_SIGNING_KEY_A"

[[approval.reviewers]]
id = "alice"
enabled = true
webhook_url = "https://review-notify.example/hooks/alice"
webhook_authorization = "env:OPENSANDBOX_ALICE_WEBHOOK_TOKEN"
```

Exact secret-provider syntax should follow the server's established config
conventions when implemented. The contract requires that signing and webhook
secrets can be supplied without embedding them in lifecycle requests, workload
metadata, logs, or generated examples.

Reviewer IDs are unique and immutable within one server configuration. Startup
rejects duplicate IDs, missing active keys, malformed HTTPS webhook URLs, or
missing referenced secrets. Disabled reviewers remain valid historical
identities but cannot be selected for a new sandbox.

Each entry represents one human who is accountable for decisions under that ID.
Operators must not register a shared team or service identity as a reviewer. A
team workflow registers its human decision makers separately, even if their
notifications pass through the same integration service.

Signing keys are server-only and support an active key plus verification keys
for rotation. They are separate from OSEP-0011 ingress route keys because a
reviewer grant conveys a different authority and must not be verifiable or
mintable by ingress.

### Lifecycle Policy

The additive lifecycle shape is:

```yaml
approvalPolicy:
  reviewers:
    - alice
  timeoutSeconds: 300
  commands:
    patterns:
      - "rm -rf*"
      - "curl*"
  egress:
    mode: new_domains
```

| Field | Semantics |
|-------|-----------|
| `reviewers` | Required non-empty list of unique, enabled reviewer IDs from server configuration |
| `timeoutSeconds` | Positive pending lifetime; default `300`; bounded by the server maximum |
| `commands.patterns` | Non-empty shell-style glob patterns matched against the complete command string |
| `egress.mode` | First phase accepts only `new_domains`; omission disables domain approval |

The server rejects an empty policy, unknown or disabled reviewer, invalid glob,
invalid timeout, egress approval without `networkPolicy`, unavailable signing
configuration, or a runtime that cannot establish the broker channel. It never
silently drops an invalid reviewer or approval capability.

Pattern matching is case-sensitive and anchored to the full command string.
`*`, `?`, bracket expressions, and escaping follow the selected cross-component
glob contract. Empty patterns and malformed bracket expressions are rejected.

Capacity limits remain operator configuration rather than lifecycle fields.
Initial defaults are 128 pending approvals and 1,024 remembered allows per
component, subject to documented hard maximums.

### Command Matching and Execution Flow

The command gate applies to regular foreground and background commands, bash
session commands, and isolated-session commands.

1. Execd authenticates and validates the action request using normal endpoint
   credentials.
2. It checks an unexpired remembered allow and then the configured globs.
3. A miss follows the existing execution path unchanged.
4. A match creates a unique `command` approval before allocating or starting a
   child process.
5. Execd sends one broker-authenticated pending event to the server and retries
   retryable delivery failures with a stable event ID.
6. Foreground/session SSE emits `approval_required`; background status exposes
   `pending_approval`. Neither surface contains reviewer credentials.
7. A broker-authenticated allow resumes the original in-memory action. Deny or
   expiry returns a typed error without starting a process.
8. Omitted `ttlSeconds` or explicit `0` permits only this command invocation. A
   positive TTL also remembers the exact byte-for-byte command, never its glob.

Every command invocation has a distinct approval. Interrupt, session deletion,
or action cancellation expires the record, and a later decision cannot start
the canceled command.

### New-Domain Egress Flow

Domain approval uses this precedence:

1. operator `deny.always`: deny immediately;
2. operator `allow.always`: allow immediately;
3. first matching user rule: apply its allow or deny immediately;
4. unexpired remembered approval for the normalized FQDN: allow temporarily;
5. `new_domains`: create or join a pending approval;
6. no approval policy: apply the existing default action.

This retains the existing static overlay order. The cache key uses the same
normalization as policy matching: lowercase the DNS wire name and remove one
trailing dot.

Concurrent questions for one normalized FQDN join one pending record. Deny or
expiry returns DNS refusal and adds no temporary nft state. Allow resumes the
held resolution and updates the dynamic address set when required.

For egress allow, omitted `ttlSeconds` uses the smaller of the approval timeout
and upstream DNS TTL. A positive value is bounded by the operator maximum and
the upstream DNS TTL. Explicit `ttlSeconds: 0` is invalid and returns `400`
because DNS cannot provide useful single-request reachability with a zero
lifetime. Deny requests reject `ttlSeconds` entirely.

The effective DNS TTL and temporary address-set lifetime never exceed the
remembered allow. Direct-IP traffic, reverse DNS questions, and non-address
questions are not turned into approval requests in the first phase.

### Approval State Machine

```
                         reviewer allow
                    +--------------------> allowed
                    |
created ----------> pending
                    |
                    +--------------------> denied
                         reviewer deny
                    |
                    +--------------------> expired
                         timeout / cancellation / shutdown
```

`allowed`, `denied`, and `expired` are terminal. Terminal records include
`decidedAt`, `decision`, and `decidedBy` for reviewer decisions; expiration has
an internal reason and no reviewer.

The same reviewer replaying the same decision returns `200` with the terminal
record. An opposite decision, or any decision from another reviewer after a
terminal reviewer decision, returns `409 Conflict` and the sanitized existing
decision metadata. Expired returns `410 Gone`; unknown or evicted returns `404`.

Terminal records remain available for a bounded diagnostic window. Retention
does not extend a remembered allow and is not a durable audit guarantee.

### Broker Protocol

The server generates a cryptographically random broker token for every sandbox
with approval enabled. It stores the token using the same protected runtime
metadata pattern as other internal endpoint tokens and injects it only into
execd and egress. It also injects an internal server callback URL that works for
the selected Docker or Kubernetes runtime.

The broker token is never returned by lifecycle responses, endpoint discovery,
diagnostics, logs, or SDK models and is never mounted into the sandbox workload.

The components expose equivalent broker-only operations:

| Method and path | Behavior |
|-----------------|----------|
| `GET /internal/approvals/{approvalId}` | Read one local approval for the server proxy |
| `POST /internal/approvals/{approvalId}/decision` | Apply a server-verified reviewer decision |

Components call:

| Method and path | Behavior |
|-----------------|----------|
| `POST /internal/sandboxes/{sandboxId}/approval-events` | Notify the server that an approval is pending |

All three operations require the per-sandbox broker token in a dedicated
header. Broker middleware is independent of normal component authentication,
uses constant-time comparison, and rejects an empty configured token. Supplying
a normal execd/egress access token, sandbox API key, reviewer grant, or signed
endpoint credential never authorizes a broker route.

The pending event contains `eventId`, approval ID, source, kind, bounded summary
context, `createdAt`, and `expiresAt`. The server verifies that token, sandbox,
source, ID prefix, and runtime metadata agree before notifying reviewers. Event
retries reuse an ID derived from sandbox, approval, and event type.

The server decision request to a component includes the verified `reviewerId`.
Components accept that field only on a broker-authenticated request and record
it as `decidedBy`.

### Reviewer Grants

The server signs a separate grant for every pending approval and selected
reviewer. The compact, URL-safe token contains at least:

```json
{
  "version": 1,
  "keyId": "a",
  "reviewerId": "alice",
  "sandboxId": "sandbox-123",
  "approvalId": "execd_apr_01J...",
  "source": "execd",
  "scopes": ["approval:read", "approval:decide"],
  "expiresAt": 1784606700
}
```

The signature covers a canonical encoding of all claims and uses a server-only
HMAC-SHA256 key selected by `keyId`. The exact wire encoding must be documented
and shared by grant issue and verify code. `expiresAt` is never later than the
pending approval expiration.

Grant verification requires a valid signature and key, current time not past
expiry, both required scopes, exact path approval ID, matching source prefix,
and current sandbox ownership of the approval. A valid grant cannot access a
different approval, sandbox, or component. Once the approval is terminal,
deleted, or expired, the grant cannot authorize another action. No first-phase
revocation list is needed.

Reviewer identity comes exclusively from the verified grant. The decision body
cannot supply or override `reviewerId`.

### Reviewer API and Review Page

The lifecycle server exposes:

| Method and path | Behavior |
|-----------------|----------|
| `GET /v1/reviewer/approvals/{approvalId}` | Read the single approval named by the grant |
| `POST /v1/reviewer/approvals/{approvalId}/decision` | Allow or deny that approval |
| `GET /approvals/review` | Serve the minimal static browser reviewer UI |

Reviewer API calls use `Authorization: Bearer <reviewer-grant>`. A lifecycle API
key or normal sandbox credential is not an alternative authorization path. The
server validates the grant, resolves the owning component from the ID prefix and
sandbox metadata, and calls the broker-only data-plane API.

The webhook review URL is:

```text
https://server.example/approvals/review#grant=<url-encoded-grant>
```

Fragments are not sent in the initial HTTP request. The page uses local script
to extract the grant and sends it only in the Authorization header. It renders
the verified reviewer ID, kind, bounded context, creation and expiry time,
terminal state, allow/deny controls, and a TTL control appropriate to the kind.
It cannot edit action context or navigate to other approvals.

The review page ships no third-party script, uses a restrictive Content Security
Policy, sets `Referrer-Policy: no-referrer`, does not persist the grant in local
storage or cookies, clears it from memory after a terminal response, and avoids
analytics and error reporting that could capture it.

The public approval response includes:

```json
{
  "id": "execd_apr_01J...",
  "source": "execd",
  "kind": "command",
  "status": "pending",
  "context": {"command": "rm -rf ./build", "truncated": false},
  "createdAt": "2026-07-21T03:00:00Z",
  "expiresAt": "2026-07-21T03:05:00Z"
}
```

Terminal responses additionally include `decision`, `decidedAt`, and
`decidedBy`. The decision request remains `{decision, ttlSeconds?}`. Command
allows accept omission, zero, or a positive bounded TTL. Egress allows accept
omission or a positive bounded TTL and return `400` for zero. Deny rejects TTL.

### Events and Notifications

Execd SSE emits `approval_required` with approval ID, kind, creation time, and
expiry. Background status exposes `pending_approval`. These agent-facing events
do not include reviewer IDs, review links, grants, broker tokens, webhook
destinations, or full secret-bearing context.

After validating a pending callback, the server sends one webhook per selected
reviewer. The payload includes a stable event ID, reviewer ID, approval ID,
source, kind, safe summary, expiry, and review URL. It omits full commands by
default; the reviewer fetches authoritative context through the grant.

Webhook delivery requires HTTPS, uses the reviewer registry's authorization
secret, applies bounded timeout and retry with backoff, and treats any non-2xx
response as failure. Retries preserve event ID and review grant. The webhook
response cannot decide an approval. Failure is observable but never allows the
action; the pending action eventually expires.

The existing egress deny webhook keeps its current already-denied semantics and
does not carry reviewer grants.

### SDK and CLI

Reviewer clients are separate from normal sandbox clients so endpoint headers
cannot be accidentally reused. Supported SDKs expose a reviewer approval client
constructed from the lifecycle server URL and reviewer grant:

```
reviewer_approvals.get(approval_id, grant)
reviewer_approvals.allow(approval_id, grant, ttl = ...)
reviewer_approvals.deny(approval_id, grant)
```

The client sends the grant only to the lifecycle reviewer API. It never resolves
or calls execd/egress endpoints directly. Public duration types follow each
language's established convention; wire TTL remains seconds.

The first-phase CLI surface is:

```text
OPENSANDBOX_REVIEWER_GRANT=... osb approvals get <approval-id>
OPENSANDBOX_REVIEWER_GRANT=... osb approvals allow <approval-id> [--ttl SECONDS]
OPENSANDBOX_REVIEWER_GRANT=... osb approvals deny <approval-id>
```

The environment variable is preferred over a command-line grant to avoid shell
history and process-list exposure. CLI commands call the Python reviewer client,
reject a missing grant locally, and do not fall back to the normal sandbox API
key. The browser link remains the primary human entry point.

### Authentication and Sensitive Data

The design has three non-interchangeable credentials:

| Credential | Holder | Authority |
|------------|--------|-----------|
| Normal execd/egress token | Agent or sandbox client | Existing action APIs only; never approval read or decision |
| Per-sandbox broker token | Lifecycle server and runtime components | Internal pending callback and component approval proxy only |
| Per-reviewer grant | Notified reviewer | Read and decide one named approval until expiry |

Command strings can contain tokens, URLs, and inline secrets. Authenticated
reviewer responses may return bounded context, but logs, traces, metrics,
generic errors, agent events, and default webhook payloads omit the full value.
Truncated context reports original length and a digest so the UI never implies
that the visible prefix is the complete action.

Reviewer grants, signing secrets, broker tokens, webhook authorization, URL
fragments, request Authorization headers, URL queries, environment variables,
and Credential Vault material are never logged or used as telemetry attributes.
Approval IDs are unpredictable but are not authorization credentials.

### Capacity, Cleanup, and Observability

Each component enforces separate caps for pending records, retained terminal
records, and remembered allows. Exceeding the pending cap rejects the new action
without evicting an existing waiter.

Expiration uses a deadline-aware timer or heap rather than one unbounded
goroutine per retained record. Terminal records and remembered allows are
removed after their deadlines. Sandbox or component shutdown closes the store,
wakes waiters, and clears remembered state.

Metrics cover approvals created, decisions by kind and result, pending count,
decision latency, expiry reason, capacity rejection, broker callback outcomes,
webhook delivery outcomes, and remembered-allow hits. Labels never contain
approval ID, reviewer ID, command, FQDN, sandbox ID, webhook URL, or secrets.

Audit-oriented structured logs may include approval ID, sandbox ID, source,
kind, terminal status, and verified reviewer ID, but never action context or any
credential. Reviewer ID is bounded operator-controlled data and is not a metric
label.

### Runtime Delivery

The server serializes validated component policy, broker callback URL, and
broker token into component-specific startup configuration. Docker uses a
server-reachable host address; Kubernetes uses the server Service address.
Reviewer registry entries, webhook URLs and secrets, signing keys, and reviewer
grants are never injected into the sandbox workload.

Both runtimes apply the same defaults, validation, broker authorization,
capacity behavior, and fail-closed semantics. Runtime-specific endpoint
discovery is hidden behind the server broker proxy. Public lifecycle, reviewer,
execd, and egress contracts remain sources of truth and generated consumers are
regenerated during implementation.

## Test Plan

**Contract and unit tests:**

- Lifecycle validation accepts valid command-only, egress-only, and combined
  policies and rejects empty policies, no reviewers, duplicate reviewers,
  unknown or disabled reviewers, invalid globs/timeouts, egress approval without
  `networkPolicy`, and missing signing or broker configuration.
- Reviewer registry rejects duplicate IDs, non-HTTPS webhooks, missing secrets,
  and invalid signing-key rotation state.
- Grant tests cover canonical signing, valid verification, tampering, unknown
  key, expiry, missing scope, path mismatch, source mismatch, cross-sandbox use,
  and cross-approval use.
- State transitions cover allow, deny, timeout, cancellation, shutdown, same
  reviewer replay, opposite replay, different reviewer race, eviction, and
  attribution.
- Normal agent, execd, egress, lifecycle, and signed-endpoint credentials receive
  `401` or `403` on reviewer and broker operations.
- Empty broker-token configuration fails closed; valid broker comparison is
  constant time.
- Command omission and zero TTL are single-use; egress omission uses the bounded
  DNS default; egress zero returns `400`; deny with TTL returns `400`.
- FQDN normalization, static rule precedence, remembered expiry, DNS TTL cap,
  concurrency deduplication, and temporary nft cleanup are verified.
- Capacity tests reject new actions without evicting waiters, leaking goroutines,
  or exposing sensitive context.

**Reviewer entry and notification tests:**

- Pending callback validation checks broker token, sandbox, source, ID prefix,
  timestamps, and replayed event IDs.
- Webhook delivery signs one grant per approval/reviewer, preserves event ID and
  grant across retries, authenticates the request, and never treats a response
  as a decision.
- Delivery failure leaves the action pending until default deny timeout.
- Review page tests verify fragment-only grant transport, no Referer, restrictive
  CSP, no third-party resources, no storage/cookies, correct controls by kind,
  and removal of grant state after a terminal response.
- Browser and API decisions record the reviewer ID from the grant, not request
  input, and the server forwards it only through broker authentication.

**Execd and egress integration tests:**

- Foreground, background, bash-session, and isolated commands pause before
  process creation and resume only after brokered reviewer allow.
- Deny, timeout, callback failure, interrupt, disconnect, session deletion, and
  shutdown never start a canceled or denied command.
- A new domain stays unresolved while pending; allow resumes resolution, while
  deny, zero-TTL error, and timeout do not leak traffic.
- Static allow bypasses approval; explicit and always deny cannot be overridden;
  concurrent DNS questions share one approval.

**SDK, CLI, and end-to-end tests:**

- Generated and handwritten reviewer clients send grants only to lifecycle
  reviewer APIs and map `400`, `401`, `403`, `409`, `410`, and component errors
  consistently across languages.
- CLI help, missing-grant behavior, environment-based grant loading, output
  formats, TTL validation, and exit codes are covered.
- Docker and Kubernetes each create a sandbox with configured reviewers, receive
  a webhook, open the review page, decide command and domain approvals, verify
  reviewer attribution, and clean up at sandbox deletion.
- Existing create, command, egress policy, deny webhook, and SDK flows run
  without `approvalPolicy` to demonstrate compatibility.

## Drawbacks

1. The server gains reviewer configuration, signing, a small browser surface,
   internal callbacks, and a broker proxy.
2. Two local approval stores still cannot provide a durable global inbox, and
   state disappears on component restart.
3. Webhook availability affects whether a reviewer learns about a pending action,
   although failure remains safe by default.
4. Bearer review links require careful operational control even with short,
   single-approval scope.
5. Holding SSE requests and DNS questions consumes bounded runtime resources and
   adds review latency.
6. Raw-command globs cannot classify semantic shell risk.
7. DNS approval authorizes temporary domain reachability, not one application
   request.

## Alternatives

**Reuse normal runtime endpoint authentication.** This lets an autonomous client
approve its own action and cannot attribute decisions. It is rejected because
it does not establish a human review boundary.

**Runtime pause/resume primitive only.** Renaming the feature would accurately
describe the original unauthenticated reviewer model, but it would not satisfy
issue #1288's human supervision goal. The first phase instead includes the
minimum identity, entry point, notification, and authority separation required
to retain the human-in-the-loop name.

**Persist every approval in the lifecycle server.** A fully central coordinator
could provide a global inbox, but it duplicates live component state and adds
distributed persistence and multi-replica consistency to action resumption. The
selected server broker authenticates reviewers while components remain the
source of truth for held actions.

**External approval broker or OIDC provider.** These can offer enterprise
identity and workflow integration but make an external dependency mandatory.
The operator registry and signed grant are a self-contained first phase; future
providers can replace registration without changing component stores.

**Operator-supplied reviewer public keys in each create request.** This is more
tenant-dynamic but complicates enrollment, webhook secret ownership, rotation,
and cross-language signing. Server registration gives identities stable meaning
and keeps secrets away from the agent-facing request.

**HTTP interception for all egress approval.** Transparent MITM can show richer
request context, but excludes non-HTTP traffic and adds CA and privacy concerns.
DNS-level new-domain approval matches the first phase.

**Semantic command parsing.** Shell AST or model classification is richer but
not deterministic across dialects, expansion, scripts, and indirection.

## Infrastructure Needed

No new OpenSandbox-managed service or approval database is required. Operators
need an HTTPS webhook receiver capable of delivering the review URL to the named
reviewer. Implementation adds server approval signing and reviewer
configuration, a small static review page, internal broker connectivity,
additive lifecycle/execd/egress contracts, regenerated SDK clients,
component-local stores, and Docker and Kubernetes end-to-end coverage.

## Upgrade & Migration Strategy

The feature is additive and opt-in:

1. Operators configure approval signing keys and reviewers before accepting an
   `approvalPolicy`.
2. Lifecycle clients add optional approval policy and reviewer references.
3. The server accepts approval policy only when selected runtime images support
   the broker protocol; mixed versions fail sandbox creation rather than ignore
   policy.
4. Execd and egress add broker-only internal routes without changing existing
   action routes or their credentials.
5. The reviewer API, review page, SDK client, and CLI commands are additive.
6. A missing approval policy follows existing command and egress paths without
   grants, callbacks, notifications, or approval state.
7. Existing `OPENSANDBOX_EGRESS_DENY_WEBHOOK`, static policies, `deny.always`,
   OSEP-0011 signed endpoints, and Credential Vault behavior remain unchanged.
8. Signing-key rotation keeps old verification keys until all issued grants have
   expired. Reviewer disablement blocks new selection and webhook delivery; an
   already issued grant remains bounded by its single approval and expiry.
9. Rollback omits `approvalPolicy` and uses runtime images without broker routes;
   no stored approval migration is required.

The proposal must reach `implementable` before public contracts or runtime code
are changed. Credential-use approvals, durable audit storage, global inboxes,
OIDC reviewer enrollment, and native notification providers require follow-up
design review.
