---
title: Credential-Bound TLS Interception
authors:
  - "@hpliStartAgain"
creation-date: 2026-09-04
last-updated: 2026-09-04
status: draft
---

# OSEP-0024: Credential-Bound TLS Interception

Tracking issue: [#1713](https://github.com/opensandbox-group/OpenSandbox/issues/1713)

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
  - [Current Behavior](#current-behavior)
  - [Relationship to Existing Work](#relationship-to-existing-work)
  - [Public Configuration](#public-configuration)
  - [Capability Negotiation](#capability-negotiation)
  - [Static Pass-Through Overlap](#static-pass-through-overlap)
  - [TLS Decision Contract](#tls-decision-contract)
  - [Authoritative Decision Snapshot](#authoritative-decision-snapshot)
  - [Revision Transaction Protocol](#revision-transaction-protocol)
  - [Lifecycle and Revision Semantics](#lifecycle-and-revision-semantics)
  - [Connection Transition Semantics](#connection-transition-semantics)
  - [Concurrency](#concurrency)
  - [Failure Semantics](#failure-semantics)
  - [Sidecar and Fleet Profiles](#sidecar-and-fleet-profiles)
  - [SNI, ECH, and Destination Identity](#sni-ech-and-destination-identity)
  - [Security and Privacy Model](#security-and-privacy-model)
  - [Observability](#observability)
  - [Phased Implementation](#phased-implementation)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal adds an opt-in Credential Proxy interception mode that decrypts
TLS only when the connection's SNI hostname matches at least one binding in the
active, acknowledged Credential Vault revision. TLS for other hosts remains
inside the transparent proxy path for network routing, but is forwarded as
opaque bytes without presenting an OpenSandbox-generated certificate.

The existing intercept-all behavior remains the default. The new mode reduces
the privacy, compatibility, and certificate-trust impact of Credential Vault
for unrelated HTTPS destinations while preserving full binding validation and
credential injection for credential-bound requests.

## Motivation

Credential Proxy currently redirects every outbound connection on its
configured HTTP(S) ports into mitmproxy. For TLS on port 443, mitmproxy
terminates TLS before the system addon knows whether any credential binding
matches the eventual HTTP request. If no binding matches, the request is
forwarded unchanged, but its TLS session has already been decrypted.

This is broader than Credential Vault needs. A sandbox that injects a credential
for one model API may also call package registries, documentation sites, source
hosts, or other public services. Those unrelated services currently require the
sandbox to trust the dynamic mitmproxy CA and lose end-to-end TLS visibility to
the local proxy even though Credential Proxy never injects a credential into
their requests.

The repository already supports static pass-through with mitmproxy
`ignore_hosts`, including an SNI-aware addon check. Static configuration is an
operator-owned image or ConfigMap setting, however. It cannot follow
sandbox-local bindings, runtime binding mutations, or per-subject binding sets
in the fleet profile.

### Goals

1. Add an explicit, backward-compatible mode that decrypts TLS only for hosts
   covered by an active Credential Vault binding.
2. Preserve OSEP-0012's complete scheme, host, method, path, and authentication
   checks after a connection is selected for decryption.
3. Define exact lifecycle, concurrency, revision acknowledgement, revocation,
   and connection-drain semantics for runtime binding mutations.
4. Fail closed when the authoritative binding decision is unavailable or a
   revision transition cannot be completed safely.
5. Keep decisions sandbox-local in the sidecar profile and subject-local in the
   fleet profile.
6. Provide bounded-cardinality telemetry for decrypt, pass-through, deny, and
   revision-transition decisions without recording credential values.
7. Stage implementation behind an opt-in so current users and operator addons
   keep existing behavior.

### Non-Goals

1. Removing transparent proxying or the mitmproxy process from Credential
   Proxy.
2. Eliminating CA installation. Credential-bound TLS hosts still require the
   sandbox to trust the OpenSandbox MITM CA.
3. Replacing DNS or nftables egress enforcement. Pass-through traffic remains
   subject to the same L3/L4 egress policy.
4. Injecting credentials into no-SNI, ECH-hidden, IP-literal, non-HTTP, or QUIC
   traffic.
5. Making static `ignore_hosts` sandbox-user configurable.
6. Changing credential binding selection, request-header selectors, HTTP
   credential sources, vault persistence, DNS behavior, or NSS/JDK CA import.
7. Treating SNI as authorization to inject a credential. SNI selects whether
   decryption is needed; the full HTTP binding match still authorizes injection.
8. Guaranteeing that a request whose HTTP authority names a bound host will use
   a TLS connection opened with that same SNI. HTTP/2 origin coalescing is a
   client transport behavior and is addressed as a documented limitation.

## Requirements

| ID | Requirement | Priority |
|---|---|---|
| R1 | Omitting the new option preserves the current intercept-all behavior | Must Have |
| R2 | In credential-bound mode, TLS is decrypted only for SNI hosts covered by the active acknowledged binding host set | Must Have |
| R3 | A binding host match at TLS time never bypasses the existing full HTTP request binding match | Must Have |
| R4 | In credential-bound mode, an unknown fleet identity always denies; for a known identity, ECH/no-SNI/static-ignore traffic passes early, while other SNI-bearing traffic requires an installed acknowledged snapshot and denies during bootstrapping | Must Have |
| R5 | Vault and effective-policy mutations are serialized per sandbox/subject and acknowledged only after the proxy installs the new decision revision | Must Have |
| R6 | Adding a bound host closes matching pre-existing pass-through connections before the mutation is acknowledged | Must Have |
| R7 | Removing the final binding for a host fences new requests from the retired revision before acknowledgement; previously admitted requests may drain for a bounded interval while new connections use pass-through | Must Have |
| R8 | The request-admission linearization point and prior-revision completion semantics are explicit for HTTP/1.1 and HTTP/2 | Must Have |
| R9 | Sidecar and fleet profiles expose the same user-visible behavior | Must Have |
| R10 | Static pass-through keeps precedence, and overlap with binding selectors is rejected using a sound shared host-selector algebra | Must Have |
| R11 | Metrics and logs contain no credentials and avoid unbounded hostname labels | Must Have |
| R12 | Phase 1 covers canonical HTTPS port 443; other TLS ports require explicit follow-up support | Must Have |
| R13 | HTTP port 80 behavior remains unchanged in the first implementation | Must Have |
| R14 | Operators can observe what the new mode would decide before enabling it | Should Have |
| R15 | Interception mode is immutable for a sandbox/subject lifetime | Must Have |
| R16 | TLS host selectors include only bindings eligible for HTTPS on canonical port 443 | Must Have |
| R17 | Every Credential Vault PATCH in credential-bound mode requires `expectedRevision` | Must Have |
| R18 | The non-default mode uses a capability-gated create route that does not exist on older servers, so unsupported backends cannot silently create an `all`-mode sandbox | Must Have |
| R19 | Credential-bound mode rejects legacy arbitrary-regex `ignore_hosts`; operators must use analyzable exact/wildcard pass-through selectors | Must Have |

## Proposal

Extend `credentialProxy` with an interception mode:

```json
{
  "credentialProxy": {
    "enabled": true,
    "interceptionMode": "credential-bound"
  }
}
```

The public modes are:

| Mode | Behavior |
|---|---|
| `all` | Current behavior. Decrypt TLS on configured transparent interception ports unless static `ignore_hosts` or existing no-SNI handling selects pass-through. |
| `credential-bound` | On canonical HTTPS port 443, decrypt only when normalized SNI matches an active binding host in the acknowledged decision snapshot. Pass other TLS through without decryption. |

`interceptionMode` defaults to `all`. Setting it requires
`credentialProxy.enabled: true`; otherwise the lifecycle API rejects the
request. The initial implementation rejects `credential-bound` when
non-canonical extra interception ports are enabled, rather than silently
applying mixed semantics.

The mode is selected at sandbox creation and is immutable for that runtime
generation. Changing modes requires replacing the sandbox/subject, so runtime
mode transitions never have ambiguous connection semantics.

At the TLS ClientHello boundary, the proxy evaluates only information available
before decryption: sandbox or subject identity, active decision revision,
normalized SNI, static ignore rules, and the configured mode. If any binding's
HTTPS/443-eligible host selector matches SNI, the connection is decrypted. An
HTTP-only or non-canonical-port binding never expands the TLS decryption set.
The existing HTTP request matcher then evaluates scheme, canonical port, host,
method, path, and any future binding selectors before injecting a credential.

### Notes/Constraints/Caveats

- An unbound host can still traverse the mitmproxy process in pass-through
  mode. The guarantee is no TLS termination or HTTP visibility, not a kernel-
  level bypass around the proxy process.
- Host-level selection is intentionally broader than request-level binding
  scope. If a host has one bound path, TLS to other paths on that host is still
  decrypted because path and method are unavailable before the handshake.
- Certificate-pinned clients work without the OpenSandbox CA only for hosts
  that remain unbound. Bound hosts continue to require CA trust and may remain
  incompatible with pinning.
- Static operator pass-through remains higher precedence than dynamic
  selection, but credential-bound mode accepts only the analyzable exact and
  leftmost-wildcard selector list defined below. Existing arbitrary-regex
  `ignore_hosts` remains supported in `all` mode and must be migrated before
  enabling `credential-bound`.
- Network allow/deny behavior does not change. A pass-through decision does not
  imply that egress policy allows the destination.
- Selection is defined on the TLS connection's visible SNI, not on every HTTP
  authority that a client might later coalesce onto that connection. A bound
  authority carried over an opaque connection with different unbound SNI is not
  visible to Credential Proxy and receives no credential. A decrypted
  connection rejects cross-authority requests rather than injecting into them.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| Binding added while an opaque TLS connection is already open | Requests continue without injection | Track and close matching pass-through connections before acknowledging the host-set addition |
| Binding removed while a decrypted HTTP/2 connection remains open | Unrelated future streams remain visible to the proxy | Atomically retire the binding revision, prevent new streams, send GOAWAY/close, and bound drain time |
| Missing snapshot is mistaken for authoritative empty | Traffic passes through when the proxy cannot determine whether credentials are required | Represent active-empty separately from bootstrapping; deny without an installed snapshot, retain an installed snapshot on pre-commit update failure, and reconcile post-commit readback loss |
| Concurrent policy and vault mutations validate against different states | A binding becomes active against stale policy or vice versa | Use one per-sandbox/subject mutation barrier and validate the complete post-mutation policy/vault pair |
| SNI and HTTP authority disagree | Wrong host is decrypted or a credential is injected to the wrong destination | Require the existing destination-identity hardening before enabling the new mode; SNI selection never substitutes for HTTP binding validation |
| Fleet subject cache leaks decisions between sandboxes | One subject's host set changes another subject's decryption | Key snapshots and connections by fenced subject identity, not hostname alone |
| Operators lose L7 addon visibility | Monitoring or custom addons no longer see unbound HTTPS | Keep `all` as the default and document the visibility change on opt-in |
| Per-ClientHello decision adds latency | Higher TLS connection setup cost | Match against the installed immutable snapshot in-process; do not perform a control-socket fetch per connection |
| Hostname metric labels expose destinations or create high cardinality | Privacy and telemetry cost | Use bounded `mode`, `decision`, `reason`, and `transition` attributes; keep hostname out of metrics |
| HTTP/2 origin coalescing carries a bound authority over an unbound SNI connection | The request remains opaque and receives no credential | Define the feature as SNI-connection scoped, reject cross-authority requests on decrypted connections, and document that credentialed clients must connect with the bound host as SNI |
| Live connection tracking exhausts memory | Revision fencing becomes unreliable | Enforce global and per-subject live-connection caps; deny new tracked TLS connections on exhaustion |
| A new client sends `interceptionMode` to an older server that ignores unknown nested fields | The request silently runs in `all` mode and decrypts unrelated TLS | Never send the new mode to the legacy create route; use a capability-gated route that older servers cannot match |
| A wildcard binding overlaps only one exact hostname matched by static pass-through | Probe-based validation misses the overlap, so credential traffic passes through without injection | Reject legacy regex configuration in the new mode and compare exact/wildcard selector languages directly |

## Design Details

### Current Behavior

The current sidecar profile installs an `OUTPUT` redirect for every non-mitm
UID TCP connection to configured destination ports, normally `80,443`. The
fleet profile installs per-subject prerouting DNAT rules for the same ports.
Both routes send all matching destinations to mitmdump.

The system addon selects a credential binding in `requestheaders`, which runs
after the TLS ClientHello and TLS termination. A request outside binding scope
returns from the addon unchanged, but HTTPS was already decrypted. The active
vault is pulled from a private Unix socket and cached for 0.5 seconds. Runtime
vault writes update the Go store and return before that time-based cache is
explicitly invalidated.

Static `ignore_hosts` works earlier at ClientHello time and is therefore able
to preserve opaque TLS, but it is fleet-wide static configuration rather than
an acknowledged sandbox-local or subject-local binding revision.

### Relationship to Existing Work

- [PR #615](https://github.com/opensandbox-group/OpenSandbox/pull/615)
  introduced transparent mitmproxy and configured-port interception.
- [PR #975](https://github.com/opensandbox-group/OpenSandbox/pull/975)
  made `ignore_hosts` a static YAML extension point.
- [OSEP-0012 / PR #955](https://github.com/opensandbox-group/OpenSandbox/pull/955)
  defined Credential Vault, runtime mutation, revision acknowledgement, and
  pass-through incompatibility for bound hosts; [PR #1009](https://github.com/opensandbox-group/OpenSandbox/pull/1009)
  implemented the first runtime path.
- [PR #1188](https://github.com/opensandbox-group/OpenSandbox/pull/1188)
  proposes dynamic HTTP credential sources. Source resolution is separate from
  the TLS decryption decision and is not changed here.
- [PR #1469](https://github.com/opensandbox-group/OpenSandbox/pull/1469)
  added no-SNI TLS pass-through, which this proposal preserves.
- [PR #1633](https://github.com/opensandbox-group/OpenSandbox/pull/1633)
  added per-subject fleet MITM dispatch and is the basis for subject-local
  decision snapshots.
- [PR #1636](https://github.com/opensandbox-group/OpenSandbox/pull/1636)
  proposes fail-closed behavior for active-vault lookup failures. That
  distinction between authoritative absence and operational failure is a
  prerequisite here.
- [PR #1193](https://github.com/opensandbox-group/OpenSandbox/pull/1193)
  proposes destination-identity hardening against HTTP authority confusion.
  Credential-bound interception must not ship before an equivalent invariant
  is in place.

None of these changes derives a TLS decrypt/pass-through decision from the
active sandbox-local or subject-local binding host set.

### Public Configuration

The lifecycle OpenAPI schema adds:

```yaml
CredentialProxyConfig:
  type: object
  properties:
    enabled:
      type: boolean
      default: false
    interceptionMode:
      type: string
      enum: [all, credential-bound]
      default: all
  additionalProperties: false
```

Handwritten SDK models and generated clients expose the same enum using their
language naming conventions. SDKs omit `interceptionMode` when the caller does
not explicitly select it, including when the effective value is `all`. Existing
servers may ignore an unknown nested field in `credentialProxy`, so a minimum
version string alone is not a safe negotiation mechanism. Callers opting in
must complete the capability handshake below before sending the create request.
Old clients continue sending only `enabled` and retain intercept-all behavior.

The server delivers the chosen create-time mode to the egress runtime as
operator-owned configuration. Sandbox request `env` cannot override it. Fleet
runtimes carry the mode in the fenced subject binding input rather than a
process-global environment variable. A runtime that cannot provide the full
sidecar/fleet, HTTP/1.1, and HTTP/2 contract rejects `credential-bound` at
admission instead of advertising partial support.

### Capability Negotiation

Add a read-only server capability endpoint:

```http
GET /v1/capabilities
```

```json
{
  "capabilities": {
    "credential-proxy-interception-mode": {
      "revision": "v1",
      "values": ["all", "credential-bound"],
      "createPath": "/v1/sandboxes:credential-bound-v1"
    }
  }
}
```

The capability key and revision are protocol identifiers, not display strings.
A server advertises `credential-proxy-interception-mode/v1` only when its
server, egress image, runtime provider, sidecar/fleet path, HTTP/1.1 handling,
and HTTP/2 handling implement the complete contract in this OSEP.

The advertised path is a fixed protocol constant, not an arbitrary server-
selected URL. It accepts the same create body as `POST /v1/sandboxes`, requires
`credentialProxy.enabled=true` and
`credentialProxy.interceptionMode=credential-bound`, and rejects any other
mode. Its implementation may delegate internally to the normal create service,
but it is a distinct HTTP route. Older servers do not define the route, so a
request routed to an old backend returns 404/405 and cannot create a sandbox.
There is no fallback from this route to `POST /v1/sandboxes`.

A gateway or load-balanced deployment advertises the capability only when it
can route `/v1/sandboxes:credential-bound-v1` exclusively to capable backends.
During mixed-version rollout or rollback it either keeps that routing guarantee
or returns 404/503 from the dedicated route; it must never rewrite the request
to the legacy collection route.

Before sending `interceptionMode: credential-bound`, every SDK and raw API
caller must query the same API origin and require:

- HTTP 200 from `/v1/capabilities`;
- capability key `credential-proxy-interception-mode`;
- exact revision `v1`; and
- value `credential-bound`; and
- exact create path `/v1/sandboxes:credential-bound-v1`.

A 404, unavailable endpoint, malformed response, missing key, different
revision, or missing value fails locally with a capability-not-supported error.
The client submits the request only to the dedicated path and must not retry it
against `POST /v1/sandboxes`. Capability results are scoped to the exact API
origin and may be cached for at most 60 seconds. A stale cache can produce a
404/503 after rollback, but cannot produce a silently downgraded sandbox because
the old backend has no matching create route.

Capable servers reject `interceptionMode=credential-bound` on the legacy
`POST /v1/sandboxes` route with an actionable `USE_CAPABILITY_CREATE_ROUTE`
error. This prevents capable callers from accidentally bypassing the route
contract; the dedicated route remains the old-server safety boundary.

Capable servers also set `extra = "forbid"` on `CredentialProxyConfig` and
reject unknown nested fields. This catches typos and future fields when a
caller reaches a capable server, but it is defense in depth rather than the
old-server negotiation mechanism.

### Static Pass-Through Overlap

The current mitmproxy `ignore_hosts` option is an arbitrary Python regular-
expression list. The existing Go validation cannot soundly prove that such a
regex is disjoint from a Credential Vault wildcard by testing the literal
selector and one synthetic `probe.` hostname. For example,
`^api\.example\.com$` overlaps `*.example.com` even though neither probe is a
complete proof of the two languages' intersection.

Credential-bound mode therefore introduces an operator-owned selector list:

```bash
OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS='["status.example.com","*.logs.example.com"]'
```

The list uses exactly the same normalized host grammar as Credential Vault
bindings:

- exact IDNA-normalized FQDN, such as `status.example.com`; or
- leftmost-label wildcard, such as `*.logs.example.com`, matching one or more
  subdomain labels but not the apex.

When `interceptionMode=credential-bound`, startup fails closed if the baked-in
mitmproxy `ignore_hosts` list is non-empty. The error identifies the migration
requirement but does not echo sensitive configuration. Operators move intended
entries to `OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS`. The sidecar
compiles those selectors to anchored mitmproxy regexes only after validation;
callers cannot provide raw regex. The legacy regex option remains unchanged for
`all` mode.

Vault create and PATCH compare every HTTPS/443 binding host selector against
every static pass-through selector using semantic intersection:

| Binding selector | Static selector | Overlap rule |
|---|---|---|
| exact | exact | normalized hosts are equal |
| exact | wildcard | the exact host is a non-apex member of the wildcard |
| wildcard | exact | the exact host is a non-apex member of the wildcard |
| wildcard | wildcard | their suffix languages are nested or equal; one normalized base is equal to or a subdomain of the other |

Any overlap rejects the complete candidate revision before prepare/commit. The
same shared selector parser, normalizer, matcher, and intersection function are
used by Go validation and the addon decision snapshot; probe-based overlap
checks are not part of credential-bound correctness. The static selector list
is immutable for the egress process lifetime. A future hot-reload design must
join the same per-subject mutation barrier and revalidate every active binding
before activation.

### TLS Decision Contract

`all` mode keeps the current decision path and does not require a Credential
Vault decision snapshot before TLS interception: no SNI passes through when
secure upstream verification is enabled, static `ignore_hosts` passes through,
and other configured-port TLS is decrypted.

For each TLS ClientHello on port 443 in `credential-bound` mode, apply this
order:

1. Resolve the fenced sandbox or fleet subject identity. If identity is
   unavailable, deny the connection.
2. If the ClientHello advertises ECH such that the actual server name is not
   available to the proxy, pass through without credentials and record
   `reason=ech`.
3. If SNI is absent, pass through without credentials and record
   `reason=no_sni`. Credential-bound runtimes already reject
   `ssl_insecure=true`, so there is no insecure no-SNI MITM exception.
4. If the validated static pass-through selector list matches normalized SNI,
   pass through and record
   `reason=static_ignore`.
5. Load the installed active acknowledged decision snapshot for that identity.
   If it is unavailable or bootstrapping, deny the connection.
6. Match normalized SNI against `tlsBindingHostSelectors`, the union of host
   selectors from bindings whose scheme includes HTTPS and whose effective
   canonical port is 443.
7. Before applying either dynamic decision, register the live connection under
   `(controlPlaneGeneration, subjectGeneration, normalizedSNI)`. If the global
   or per-subject registry cap is exhausted, deny with
   `reason=registry_exhausted`; do not pass through an untracked visible-SNI
   connection.
8. Apply the registered decision:
   - selector match: decrypt and record `reason=binding_host`;
   - no match, including active-empty: pass through and record
     `reason=no_binding_host`.

Exact and leftmost-label wildcard semantics, lowercase normalization, trailing
dot handling, and IDNA normalization must be shared with Credential Vault
binding validation. The TLS decision path must not introduce a second subtly
different hostname matcher.

### Authoritative Decision Snapshot

Each sandbox or subject has one immutable internal snapshot:

```text
DecisionSnapshot {
  controlPlaneGeneration
  subjectGeneration
  decisionEpoch
  vaultRevision
  effectivePolicyEpoch
  interceptionMode
  state: bootstrapping | active | active-empty
  tlsBindingHostSelectors[]
  fullRenderedBindings[]
  redactions[]
}
```

`tlsBindingHostSelectors` is derived from the HTTPS/443-eligible portion of the
same complete binding set as
`fullRenderedBindings`; callers cannot update it independently. The proxy
installs the whole candidate snapshot atomically. An API response may report a
new vault revision only after the proxy acknowledges that exact snapshot and
any required connection fence has been installed.

An installed snapshot has no data TTL. It remains authoritative until it is
explicitly replaced, the subject generation changes, the proxy process loses
it, or the owning Go control-plane incarnation disappears. Vault create,
patch, delete, policy change, subject unload, and generation change actively
install or retire snapshots.

Every egress start creates a random `controlPlaneGeneration`; it is never
derived from a counter that can reset and collide. The addon binds snapshots to
that generation and monitors parent/control-channel liveness. Normal process
supervision terminates the child when Go exits; as a fallback, missing
generation keepalive for 15 seconds makes the addon discard the snapshot,
enter bootstrapping deny, and exit for supervisor restart. A new Go incarnation
must reinstall the current active or explicit empty revision before readiness
returns. The keepalive is an incarnation fence, not snapshot-data expiry.

If mitmdump restarts and loses the snapshot, health remains not-ready and
credential-bound TLS returns to bootstrapping deny until the Go control plane
reinstalls and reads back the current snapshot.

The existing 0.5-second pull cache is therefore not used as a correctness or
acknowledgement mechanism in this mode. A transient control-socket failure does
not expire an already installed snapshot; it prevents a new revision from being
acknowledged.

### Revision Transaction Protocol

Every candidate uses `(controlPlaneGeneration, subjectGeneration,
decisionEpoch, vaultRevision, contentDigest)` as its idempotent internal
identity.

1. **Prepare** validates the complete policy/vault pair, constructs an inert
   snapshot, reserves required connection-registry capacity, and installs any
   reversible deny fence. It does not expose the candidate as active.
2. **Commit** atomically swaps the active request/TLS decision snapshot. For an
   added-host transition, matching opaque connections must be confirmed closed
   before commit returns. For a removed host, the new-request fence becomes
   active in the same commit.
3. **Acknowledge/readback**: a successful commit response containing the exact
   tuple and digest is the proxy's acknowledgement. The egress API may then
   report the new vault revision. Readback returns the same active tuple and
   digest and is required only when the commit response is lost or ambiguous.
4. **Abort** removes a prepared fence and candidate. Connection closes already
   completed during prepare are not reversible, but they affect availability
   only; the previous snapshot remains active and reconnects use it.

Prepare, commit, abort, and readback are idempotent for the same tuple and
digest. A conflicting digest for an existing tuple is rejected. If the API
response is lost after a successful commit, the committed candidate is the
proxy's internally authoritative revision, but the Go control plane cannot yet
report it publicly. Go enters an internal `reconciling` state and rejects all
snapshot-affecting mutations and vault reads/writes with
`503 CREDENTIAL_REVISION_INDETERMINATE` until readback establishes which tuple
is active. It then finalizes that tuple in Go. Subject unload, sandbox deletion,
and process shutdown remain unconditional escape paths: they force-close
connections and discard both active and prepared state without waiting for
reconciliation. The proxy may continue using the fully validated internally
authoritative candidate that it already committed; readback loss does not claim
that the prior snapshot was restored. Once reconciled, callers read `GET
/credential-vault` to resolve the outcome. A retry with stale
`expectedRevision` conflicts rather than applying the candidate twice.

### Lifecycle and Revision Semantics

Credential-bound mode uses these states:

| State | TLS behavior |
|---|---|
| `bootstrapping` | For a known identity, preserve early ECH/no-SNI/static-ignore pass-through; deny other SNI-bearing TLS until the control plane installs an active or explicit empty revision |
| `active` | Decide decrypt/pass-through from the acknowledged non-empty binding set |
| `active-empty` | Authoritative empty binding set; select pass-through after live-connection registry admission |

This distinguishes "the authoritative binding set is empty" from "no vault
revision has been installed" and "the proxy cannot obtain the binding set." A
caller that intentionally wants no bindings may create an empty vault. Deleting
the vault installs an acknowledged empty tombstone for the current runtime
generation before returning `204`; it does not turn a missing or failed lookup
into implicit dynamic pass-through. The legacy `all` mode keeps its current
no-vault behavior because this state machine gates only the opt-in mode.

On sidecar restart or fleet subject generation change, the state returns to
`bootstrapping`. Fleet replay or the trusted caller must install either a vault
revision or an explicit empty revision. This aligns selective decisions with
the existing memory-only vault model without pretending that lost state is an
authoritative empty configuration.

### Connection Transition Semantics

Let `covers_old(sni)` and `covers_new(sni)` be the semantic HTTPS/443 host-
coverage predicates for consecutive revisions. Transitions are evaluated for
the observed normalized SNI of each live connection, not by subtracting raw
selector strings. For example, replacing `*.example.com` with
`api.example.com` does not treat a live `api.example.com` connection as newly
added or removed, while `docs.example.com` becomes removed.

**Newly covered connections (`!covers_old(sni) && covers_new(sni)`)**

- Install a fence that rejects new pass-through connections for the added host.
- Close existing opaque pass-through TLS connections whose observed SNI matches
  the added host. They cannot be converted into MITM connections in place.
- Atomically activate the new decision snapshot.
- Acknowledge the mutation only after matching live opaque sockets are
  confirmed closed and the proxy returns the exact successful commit
  acknowledgement, or matching readback resolves an ambiguous commit. Clients
  reconnect through the decrypt path.

**Newly uncovered connections (`covers_old(sni) && !covers_new(sni)`)**

- Atomically activate the new snapshot so no new request can receive a
  credential from the retired revision.
- Stop accepting new HTTP/1.1 requests or HTTP/2 streams on decrypted
  connections for removed hosts; send connection close or HTTP/2 GOAWAY.
- A request is admitted when the system addon's `requestheaders` hook binds it
  to an immutable revision. For HTTP/2 this is per stream; for pipelined
  HTTP/1.1 it is per parsed request. Requests admitted before the cutover may
  finish under the old snapshot. New HTTP/2 streams and parsed HTTP/1.1
  requests are rejected after the fence.
- Drain uses the operator-owned
  `OPENSANDBOX_EGRESS_CREDENTIAL_TRANSITION_DRAIN_TIMEOUT_SECONDS` setting
  (default `30`, valid range `1..300`). At expiry the proxy force-closes the
  connection. HTTP/2 GOAWAY uses the greatest stream ID admitted before the
  cutover as `last_stream_id`.
- Acknowledge once the new-request fence is active. Subsequent connections use
  pass-through.

**Unchanged host set**

- Credential or path/method-only changes atomically replace the request-level
  snapshot without reconnecting TLS.
- Requests admitted before the cutover may finish under the old revision; new
  requests after acknowledgement use the new revision. Admission is the
  `requestheaders` hook's atomic binding of the request to an immutable
  revision, so an already admitted request may transmit a credential after the
  API acknowledges the new revision if upstream writes were queued or
  backpressured. Such requests use the same operator-owned 30-second drain
  bound and are force-closed at expiry. No request admitted after the cutover
  can use the retired revision. This is consistent with OSEP-0012's rule that
  mutation does not revoke a credential already attached to an in-flight
  request.

If the proxy cannot install the snapshot or required fence, the mutation fails
and the previous acknowledged revision remains active. The API must not expose
the candidate as current.

### Concurrency

Vault create/patch/delete, runtime policy mutation, always-rule changes that
affect effective policy, fleet binding replay, and subject unload share one
per-sandbox or per-subject mutation barrier. Candidate validation observes one
consistent pair of effective policy and vault state.

The internal monotonic `decisionEpoch` serializes this composite state. Vault
callers still use public `expectedRevision` only for vault content; policy
callers use their existing policy API. Any accepted policy or always-rule
change that affects the complete snapshot advances `decisionEpoch` inside the
same barrier. Conflict responses return the current public vault revision when
the request was a vault mutation; `decisionEpoch` remains internal.

`expectedRevision` retains its OSEP-0012 optimistic concurrency semantics. In
credential-bound mode every `PATCH /credential-vault` request requires it,
including credential-only changes; omission or mismatch returns a conflict
before preparing the candidate. This makes retry after a lost response safe:
either the original revision is still current and the patch can commit once,
or the revision advanced and the retry conflicts. Create and delete retain
their naturally checkable exists/not-found results. All writes remain
serialized.

The fleet profile additionally fences every snapshot with the subject runtime
generation. A delayed push, acknowledgement, or connection-close event from an
old generation cannot affect the replacement subject.

### Failure Semantics

| Condition | Result in credential-bound mode |
|---|---|
| Authoritative active-empty snapshot | Select pass-through after live-connection registry admission |
| SNI does not match a bound host | Select pass-through after live-connection registry admission |
| Validated static pass-through selector match | Pass through; semantic overlap with a binding is rejected before activation |
| ECH hides the actual SNI | Pass through as opaque TLS; no credential injection |
| No SNI | Preserve current secure no-SNI pass-through behavior; no credential injection |
| Live connection registry cap reached | Deny; do not create an untracked visible-SNI pass-through or decrypted connection |
| Prepare/install failure before commit | Reject the candidate; keep the prior installed snapshot. If no snapshot is installed, deny |
| Commit readback timeout or lost acknowledgement | Enter `CREDENTIAL_REVISION_INDETERMINATE`; reject vault reads/writes until active-tuple readback reconciles the outcome |
| Unknown fleet source identity | Deny and emit a bounded dispatch-miss signal |
| Snapshot revision/generation mismatch | Deny until reconciled |
| Candidate install or connection fence failure | Reject mutation; retain prior acknowledged revision |
| Sidecar restart or subject rebind before replay | For a known identity, early ECH/no-SNI/static-ignore pass-through remains; all other SNI-bearing TLS is denied until replay |
| Go control-plane incarnation disappears | Discard generation-bound snapshot after the liveness bound, enter bootstrapping deny, and restart |

This proposal depends on the fail-closed active-vault lookup and destination-
identity invariants tracked by the related work above. It must not be
implemented by returning pass-through on every lookup exception or by trusting
HTTP authority independently of SNI/original destination.

### Sidecar and Fleet Profiles

The sidecar profile owns one decision snapshot and one connection registry.

The fleet profile owns one snapshot per subject. Client source IP is only the
dispatch key into a fenced subject identity; it is not the durable identity.
The same SNI may decrypt for subject A and pass through for subject B when only
subject A has a matching binding. Subject registration starts deny-first,
subject unload removes its snapshot and closes its tracked connections, and a
new runtime generation never inherits the old subject's cache.

The registry contains live connections only and removes entries on close. The
initial implementation caps the process at 65,536 tracked TLS connections and
each fleet subject at 1,024; operator configuration may lower these limits but
cannot disable them. When either cap is reached, new credential-bound TLS
connections that require tracking are denied with `reason=registry_exhausted`.
Live entries are never evicted to make room, because eviction would make a
future host-set transition unenforceable.

### SNI, ECH, and Destination Identity

Credential-bound selection requires visible SNI. No-SNI connections keep the
existing pass-through behavior and cannot receive Credential Vault injection.
If the ClientHello advertises ECH and the actual service name is unavailable,
the outer SNI is not used as a binding host. The connection is treated as
opaque and cannot receive credentials in the initial design. Operators that
require Credential Vault for a destination must ensure its client exposes
usable SNI.

SNI is only a decryption hint. Before injection, the proxy must bind the HTTP
authority to the verified upstream destination and reject destination
confusion. The credential-bound implementation therefore depends on completing
the existing destination-identity hardening; it must not expand trust in the
client-controlled `Host` header.

HTTP/2 origin coalescing cannot be detected on an opaque connection. If a
client opens TLS with an unbound SNI and later sends a request for a bound
authority over that connection, the request stays opaque and receives no
credential. On a decrypted connection, the addon rejects an authority that is
not consistent with the verified connection destination/SNI, even when the
certificate covers both names. Clients that require injection must open the
connection with the credential-bound host as SNI.

IP-literal credential binding remains invalid. DNS and nftables policy remain
the network-reachability authority, including for pass-through traffic.

### Security and Privacy Model

This mode reduces the plaintext visible to the trusted egress proxy and its
operator-controlled addons. It does not make the egress sidecar untrusted, hide
destinations from network policy, or provide end-to-end TLS for bound hosts.

Security invariants:

- A pass-through decision never exposes HTTP headers or bodies to addons and
  never injects credentials.
- A decrypt decision does not authorize injection; the full request binding
  selection still applies. Exact-host precedence remains unchanged, and an
  unresolved multiple-binding match fails closed.
- Without an installed authoritative snapshot, operational uncertainty denies
  instead of selecting dynamic pass-through or reverting to intercept-all. A
  failed update keeps the last installed snapshot; a post-commit indeterminate
  result blocks all snapshot-affecting mutations and vault reads/writes until
  readback reconciles it. Teardown remains unconditional.
- Static operator pass-through and dynamic credential binding cannot overlap.
- Candidate revisions, retired revisions, and connection registries contain no
  plaintext credentials beyond the existing active vault requirements.
- Sandbox users cannot set the mode through egress environment variables or
  load untrusted addons into credential-enabled runtimes.

### Observability

Add bounded-cardinality metrics to the `opensandbox/egress` meter:

| Metric | Attributes | Purpose |
|---|---|---|
| `egress.mitm.tls.connections_total` | `mode`, `decision`, `reason` | Count `decrypt`, `passthrough`, and `deny` decisions |
| `egress.mitm.tls.active_connections` | `mode`, `decision` | Observe current decrypted and opaque connections |
| `egress.credential_vault.transitions_total` | `transition`, `result` | Count host-set add/remove, credential-only, empty, replay, and failed transitions |
| `egress.credential_vault.transition.duration` | `transition`, `result` | Measure snapshot install and connection-fence latency |

Allowed attribute values are closed sets. Hostnames, paths, credential names,
credential values, raw errors, sandbox IDs beyond existing shared attributes,
and subject UIDs must not become metric attributes.

Structured logs may include revision, bounded decision/reason, and the existing
sandbox attribution. Host logging follows the existing egress privacy policy
and must never include credentials or rendered headers. A dry-run mode may
compare `all` with the would-be credential-bound decision using counters only;
it does not change traffic.

### Phased Implementation

1. **Decision telemetry and red tests**
   - Add fail-closed tests that distinguish authoritative empty from lookup
     failure.
   - Add dry-run decision counters while traffic still uses `all`.
   - Land shared normalized host matching and destination-identity prerequisites.
2. **Immutable revision acknowledgement**
   - Replace time-only cache correctness with explicit snapshot install and
     invalidation.
   - Serialize policy/vault changes and fence sidecar/fleet generations.
3. **Opt-in sidecar mode for HTTPS/443**
   - Keep the mode behind an internal experimental gate; do not yet accept the
     public lifecycle field.
   - Implement ClientHello selection and HTTP/1.1 connection transitions.
4. **HTTP/2 and fleet parity**
   - Add GOAWAY/drain semantics and per-subject connection registries.
   - Run subject-isolation and replay/restart E2E coverage.
   - Only after these checks pass, add the lifecycle spec, server, SDK, and
     egress public configuration.
5. **Broader port support and graduation**
   - Evaluate extra TLS ports only after binding matching supports them.
   - Consider changing the default only in a future OSEP with compatibility
     evidence; this proposal keeps `all` as the default.

## Test Plan

### Unit Tests

- Exact, wildcard, trailing-dot, case, and IDNA SNI matching share Credential
  Vault normalization.
- `all` mode preserves current decisions.
- An older server without `/v1/capabilities` causes the client to fail before
  create; it never receives `interceptionMode`.
- Capability responses with a missing key, wrong revision, or missing
  `credential-bound` value or exact create path prevent create.
- After a capability response is cached, routing the dedicated create request
  to an older backend returns 404/405 and creates no sandbox; the client never
  retries the legacy route.
- A capable legacy create route rejects `interceptionMode=credential-bound`
  with `USE_CAPABILITY_CREATE_ROUTE`.
- A capable server rejects unknown `CredentialProxyConfig` fields instead of
  dropping them.
- Credential-bound active-empty and unbound SNI select pass-through.
- ECH is detected before outer-SNI matching and selects opaque pass-through.
- Bound SNI selects decryption, then still requires a full HTTP binding match.
- HTTP-only bindings do not add hosts to `tlsBindingHostSelectors`.
- Exact/exact, exact/wildcard, wildcard/exact, and wildcard/wildcard overlap
  checks reject every intersecting binding revision.
- Static exact `api.example.com` rejects binding wildcard `*.example.com`; the
  inverse combination and nested wildcard suffixes are covered as regressions.
- Credential-bound startup rejects any non-empty legacy regex `ignore_hosts`
  list and accepts the equivalent analyzable pass-through selector list.
- Missing/bootstrapping state is distinct from an acknowledged empty revision,
  timeout, 5xx, malformed JSON, dispatch miss, and generation mismatch; only
  the acknowledged empty revision selects dynamic pass-through.
- Cache invalidation makes an acknowledged revision visible immediately.
- Prepare, commit, abort, and readback are idempotent; lost acknowledgement is
  resolved by active-revision readback.
- Indeterminate reconciliation blocks vault, policy, always-rule, and replay
  mutations while still permitting forced subject/sandbox teardown.
- Concurrent policy and vault mutations cannot commit an inconsistent pair.
- `expectedRevision` is required for every credential-bound PATCH and rejects
  stale mutations.
- Old fleet generations cannot install snapshots or close new-generation
  connections.
- Registry exhaustion denies new tracked connections without evicting live
  entries.
- Telemetry uses only the documented bounded attributes.

### Integration Tests

- An unbound HTTPS server with a certificate trusted by the sandbox succeeds
  without trusting the OpenSandbox CA and observes end-to-end TLS.
- A bound HTTPS server requires the OpenSandbox CA and receives the expected
  injected credential only on matching requests.
- Adding the first binding for a host closes an existing pass-through
  connection; a reconnect is decrypted and credentialed.
- Removing the final binding prevents new injection, drains HTTP/1.1 and HTTP/2
  connections, and makes the next connection pass through.
- Credential-only replacement keeps the TLS connection but switches new
  requests to the acknowledged revision.
- Long-running requests admitted before cutover either finish within the drain
  window or are force-closed at the documented deadline.
- A pre-commit transition failure returns an error and leaves the prior
  revision active; a lost post-commit readback blocks vault APIs until
  reconciliation resolves the active tuple.
- Restart preserves early ECH/no-SNI/static-ignore pass-through but denies other
  SNI-bearing TLS until an active or explicit empty revision is replayed.
- No-SNI and ECH-hidden connections receive no credential injection.
- A bound HTTP/2 authority coalesced over an opaque connection receives no
  credential; a cross-authority request on a decrypted connection is rejected.

### E2E Tests

- Docker and Kubernetes sandboxes call one credential-bound model API and one
  unrelated pinned-CA public HTTPS endpoint in the same sandbox.
- SDKs negotiate `credential-proxy-interception-mode/v1` and use only
  `/v1/sandboxes:credential-bound-v1`; a legacy backend creates zero sandboxes
  for the new mode even after a stale capability cache or mixed-version route
  change.
- The model API is decrypted and credentialed; the unrelated endpoint is
  pass-through and does not require the OpenSandbox CA.
- In fleet mode, the same destination decrypts for a subject with a binding and
  passes through for another subject without one.
- Runtime add, replace, delete, restart, subject unload/rebind, and replay
  preserve the revision and failure contracts.
- Egress network deny rules still block pass-through destinations.
- Logs, metrics, diagnostics, and API responses contain no credential value.

### Performance Tests

- Compare TLS handshake latency and throughput for `all`, credential-bound
  decrypt, and credential-bound pass-through paths.
- Exercise at least 4,096 fleet subjects without unbounded per-host or
  per-source cache growth.
- Measure binding-revision transition latency with active HTTP/1.1 and HTTP/2
  connections.

## Drawbacks

- The proxy must maintain connection state and revision fences, which is more
  complex than time-based snapshot polling.
- A runtime binding addition may interrupt an existing otherwise healthy TLS
  connection so the client reconnects through MITM.
- Host-level decryption remains broader than path- or method-level credential
  scope.
- Visible SNI is required; ECH and no-SNI traffic cannot use Credential Vault.
- Operators using custom addons lose L7 visibility for unbound hosts when they
  opt in.
- Bound hosts still require dynamic CA trust, so this reduces rather than
  eliminates CA integration work.

## Alternatives

### Keep Intercept-All

This is the simplest implementation and remains the default, but it preserves
the broad TLS decryption and CA compatibility cost that motivates this
proposal.

### Generate Static `ignore_hosts`

Operators can already rebuild or mount a static mitmproxy configuration. It
does not follow sandbox-local runtime mutations, cannot vary per fleet subject,
and risks drift between the static list and the active vault. Arbitrary regex
also has no sound intersection check against the binding wildcard language, so
credential-bound mode requires migration to the analyzable selector list.

### Bypass mitmproxy in iptables or nftables

Routing only bound host IPs into mitmproxy would avoid even the pass-through
proxy hop, but binding policy is FQDN-based and destination IPs are dynamic,
shared, and learned through DNS. Kernel rules cannot reliably distinguish two
TLS hostnames sharing an IP. ClientHello selection keeps the decision at the
first layer that can observe SNI.

### Decide From HTTP Host After Decryption

That is the current behavior. It can choose whether to inject a credential but
cannot undo TLS decryption that already occurred.

### Make Credential-Bound the New Default Immediately

This would silently remove L7 visibility from operator addons and change
certificate behavior for existing sandboxes. The proposal uses an opt-in and
requires separate evidence before any future default change.

### Allow Eventual Consistency for Runtime Mutations

Keeping the 0.5-second cache and documenting a delay is simpler, but it breaks
OSEP-0012's acknowledgement and revocation contract and leaves already-open
pass-through connections able to bypass newly added bindings. Explicit
revision and connection semantics are required.

## Infrastructure Needed

No new external service is required. The implementation needs an internal
revision-install/acknowledgement path between the Go egress control plane and
the mitmproxy system addon, plus a bounded connection registry. The exact local
IPC mechanism is an implementation detail as long as it is private to the
sidecar, fenced by subject generation, and meets the acknowledgement contract.

CI needs Linux integration coverage with mitmproxy 11.0.2, local TLS servers,
HTTP/2, network namespaces, and the existing Docker/Kubernetes/fleet egress test
paths.

## Upgrade & Migration Strategy

1. Implement and validate the internal decision/transition protocol without a
   public lifecycle field.
2. Ship dry-run telemetry and acknowledgement prerequisites without changing
   traffic.
3. After sidecar, fleet, HTTP/1.1, and HTTP/2 parity passes, add the enum to the
   lifecycle spec, the `/v1/capabilities` contract, and all SDKs. SDKs omit the
   field unless the caller explicitly selects `credential-bound`, and refuse to
   send it until the same API origin advertises
   `credential-proxy-interception-mode/v1` with the fixed
   `/v1/sandboxes:credential-bound-v1` path. They never retry that path against
   legacy `POST /v1/sandboxes`. Release documentation still records minimum
   compatible server, egress, and SDK versions for operators, but version
   documentation does not replace the route-bound handshake.
4. Enable `credential-bound` only when explicitly requested and only on
   runtimes that implement the complete contract; reject unsupported
   extra-port or pool/fleet combinations instead of degrading silently.
5. Document that callers must install an initial vault revision, including an
   explicit empty revision, before TLS egress is released in this mode.
6. Document that every `PATCH /credential-vault` request in this mode must
   include the existing `expectedRevision`; callers can obtain it from `GET
   /credential-vault`.
7. Keep current regex `ignore_hosts`, CA setup, and `all` behavior unchanged for
   existing deployments. Before enabling `credential-bound`, migrate intended
   static bypass entries to the exact/wildcard
   `OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS` list; the new mode rejects a
   non-empty legacy regex list.
8. Do not change the default in this OSEP. A future default change requires
   usage data, addon compatibility evidence, and a separate migration plan.
