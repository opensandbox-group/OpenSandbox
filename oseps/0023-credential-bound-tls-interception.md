---
title: Credential-Bound TLS Interception
authors:
  - "@hpliStartAgain"
creation-date: 2026-09-04
last-updated: 2026-10-09
status: implementing
---

# OSEP-0023: Credential-Bound TLS Interception

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
  - [Effective Mode Verification](#effective-mode-verification)
  - [Static Pass-Through Overlap](#static-pass-through-overlap)
  - [TLS Decision Contract](#tls-decision-contract)
  - [Authoritative Decision Snapshot](#authoritative-decision-snapshot)
  - [Revision Transaction Protocol](#revision-transaction-protocol)
  - [Lifecycle and Revision Semantics](#lifecycle-and-revision-semantics)
  - [Connection Transition Semantics](#connection-transition-semantics)
  - [Concurrency](#concurrency)
  - [Failure Semantics](#failure-semantics)
  - [Sidecar and Fast Sandbox Profiles](#sidecar-and-fast-sandbox-profiles)
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
in the fast-sandbox profile.

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
   fast-sandbox profile.
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
| R4 | In credential-bound mode, an unknown fast-sandbox identity always denies; for a known identity, ECH/no-SNI/static-ignore traffic passes early, while other SNI-bearing traffic requires an installed acknowledged snapshot and denies only while authoritative state is unknown; first creation installs an empty snapshot before readiness | Must Have |
| R5 | Vault and effective-policy mutations are serialized per sandbox/subject and acknowledged only after the proxy installs the new decision revision | Must Have |
| R6 | Host-add acknowledgement makes the new revision effective for subsequent TLS decisions; existing opaque connections remain uncredentialed until clients reconnect | Must Have |
| R7 | Removing the final binding for a host fences new requests from the retired revision before acknowledgement; previously admitted requests may drain for a bounded interval while new connections use pass-through | Must Have |
| R8 | The request-admission linearization point and prior-revision completion semantics are explicit for HTTP/1.1 and HTTP/2 | Must Have |
| R9 | Sidecar and fast-sandbox profiles expose the same user-visible behavior | Must Have |
| R10 | Static pass-through keeps precedence, and overlap with binding selectors is rejected using a sound shared host-selector algebra | Must Have |
| R11 | Metrics and logs contain no credentials and avoid unbounded hostname labels | Must Have |
| R12 | Phase 1 covers canonical HTTPS port 443; other TLS ports require explicit follow-up support | Must Have |
| R13 | HTTP port 80 behavior remains unchanged in the first implementation | Must Have |
| R14 | Operators can observe what the new mode would decide before enabling it | Should Have |
| R15 | Interception mode is immutable for a sandbox/subject lifetime | Must Have |
| R16 | TLS host selectors include only bindings eligible for HTTPS on canonical port 443 | Must Have |
| R17 | Every Credential Vault PATCH in credential-bound mode requires `expectedRevision` | Must Have |
| R18 | Clients verify the runtime-confirmed effective mode after create and fail explicitly on missing or mismatched values | Must Have |
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
| Binding added while an opaque TLS connection is already open | Reused connections receive no injected credential | Apply the binding to new TLS decisions and document client reconnect requirements; do not promise immediate injection on existing opaque sessions |
| Binding removed while a decrypted HTTP/2 connection remains open | Unrelated future streams remain visible to the proxy | Atomically retire the binding revision, prevent new streams, send GOAWAY/close, and bound drain time |
| Missing snapshot is mistaken for authoritative empty | Traffic passes through when the proxy cannot determine whether credentials are required | Represent active-empty separately from bootstrapping; deny without an installed snapshot, retain an installed snapshot on pre-commit update failure, and reconcile post-commit readback loss |
| Concurrent policy and vault mutations validate against different states | A binding becomes active against stale policy or vice versa | Use one per-sandbox/subject mutation barrier and validate the complete post-mutation policy/vault pair |
| SNI and HTTP authority disagree | Wrong host is decrypted or a credential is injected to the wrong destination | Require the existing destination-identity hardening before enabling the new mode; SNI selection never substitutes for HTTP binding validation |
| Fast Sandbox subject cache leaks decisions between sandboxes | One subject's host set changes another subject's decryption | Key snapshots and connections by fenced subject identity, not hostname alone |
| Operators lose L7 addon visibility | Monitoring or custom addons no longer see unbound HTTPS | Keep `all` as the default and document the visibility change on opt-in |
| Per-ClientHello decision adds latency | Higher TLS connection setup cost | Match against the installed immutable snapshot in-process; do not perform a control-socket fetch per connection |
| Hostname metric labels expose destinations or create high cardinality | Privacy and telemetry cost | Use bounded `mode`, `decision`, `reason`, and `transition` attributes; keep hostname out of metrics |
| HTTP/2 origin coalescing carries a bound authority over an unbound SNI connection | The request remains opaque and receives no credential | Define the feature as SNI-connection scoped, reject cross-authority requests on decrypted connections, and document that credentialed clients must connect with the bound host as SNI |
| Decrypted-connection registry exhausts its budget | New bound TLS cannot be safely tracked | Deny only new decrypt decisions, preserve opaque pass-through, and alert operators with budget/occupancy telemetry |
| An older server ignores the requested mode | Unrelated TLS may be decrypted before the SDK detects the mismatch | Verify the runtime-confirmed response field, report failure, and require coordinated version upgrades; detection cannot undo startup traffic |
| A wildcard binding overlaps only one exact hostname matched by static pass-through | Probe-based validation misses the overlap, so credential traffic passes through without injection | Reject legacy regex configuration in the new mode and compare exact/wildcard selector languages directly |

## Design Details

### Current Behavior

The current sidecar profile installs an `OUTPUT` redirect for every non-mitm
UID TCP connection to configured destination ports, normally `80,443`. The
fast-sandbox profile installs per-subject prerouting DNAT rules for the same ports.
Both routes send all matching destinations to mitmdump.

The system addon selects a credential binding in `requestheaders`, which runs
after the TLS ClientHello and TLS termination. A request outside binding scope
returns from the addon unchanged, but HTTPS was already decrypted. The active
vault is pulled from a private Unix socket with a per-request conditional ETag
check; the full snapshot is reused only when its tag is confirmed. Runtime
vault writes update the Go store, while the addon observes the update on a
subsequent request. This is not the push-install acknowledgement protocol
proposed below.

Static `ignore_hosts` works earlier at ClientHello time and is therefore able
to preserve opaque TLS, but it is process-wide static configuration rather than
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
  added per-subject fast-sandbox MITM dispatch and is the basis for subject-local
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
servers may ignore unknown nested fields. Clients use the existing
`POST /v1/sandboxes` route and verify the returned effective mode as described
below. Old clients continue sending only `enabled`.

The server delivers the chosen create-time mode to the egress runtime as
operator-owned configuration. Sandbox request `env` cannot override it. Fast Sandbox
runtimes carry the mode in the fenced subject binding input rather than a
process-global environment variable. A runtime that cannot provide the full
sidecar/fast-sandbox, HTTP/1.1, and HTTP/2 contract rejects `credential-bound` at
admission instead of advertising partial support.

### Effective Mode Verification

Keep the existing create route. Add `credentialProxy.interceptionMode` to
create and sandbox-get responses as a runtime-confirmed effective value, not a
copy of the request. The server obtains it from the egress runtime after mode
initialization and readiness; unsupported or unconfirmed runtime configuration
must fail creation rather than echo the requested value.

When requesting `credential-bound`, SDKs must verify the create response
contains exactly that effective value before returning a usable sandbox handle.
Missing, unknown, or mismatched values raise `INTERCEPTION_MODE_MISMATCH`.
Never default a missing response field to the requested mode or silently retry
with `all`. Apply the same verification when connecting to an existing sandbox
with an explicit expected mode. Raw API callers have the same responsibility.

The mismatch error includes the sandbox ID when available so callers can clean
up the already-created resource. Cleanup is an explicit caller/SDK option;
failure to delete must retain the ID and report the cleanup failure.

This is post-creation detection. An older server may start the entrypoint and
decrypt HTTPS before the SDK receives the response. Reporting an error or
deleting the sandbox cannot undo that traffic. Document minimum compatible
server, egress, and SDK versions and require coordinated upgrades of every
backend serving this mode. Deployments requiring a guarantee before any
workload traffic must use a verified compatible deployment and keep application
work gated externally; response verification alone does not provide that
guarantee.

Implement strict unknown-field rejection on capable servers using
`extra = "forbid"` for `CredentialProxyConfig`. Although the OpenAPI schema
already declares `additionalProperties: false`, existing runtime validation
ignores extras. Enforcing rejection is an observable compatibility tightening:
previously tolerated unknown fields now fail validation. Document it explicitly.

Generic capability negotiation and dedicated per-feature create routes are
deferred. They require broader motivation and a separate API design.

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

1. Resolve the fenced sandbox or fast-sandbox subject identity. If identity is
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
7. If the selector does not match, including active-empty, pass through with
   `reason=no_binding_host`. Do not add the opaque connection to an OSEP
   decision registry; mitmproxy still owns its normal transport lifecycle.
8. For a matching selector, atomically register the connection and its selected
   decision epoch before decryption. Revalidate the epoch if a mutation raced
   with admission. Deny with `reason=registry_exhausted` if the decrypted-
   connection budget is full; never downgrade a bound connection to opaque TLS.

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

The local transaction wire splits this conceptual object at the only
non-circular boundary. Its established revision envelope has six fields:
`controlGeneration` (the wire name for conceptual
`controlPlaneGeneration`), `subjectGeneration`, the coordinator-allocated
`decisionEpoch`, `vaultRevision`, `policyEpoch`, and `digest` of the exact
payload bytes. The versioned canonical payload carries `vaultRevision`,
`effectivePolicyEpoch`, `interceptionMode`, `state`,
`tlsBindingHostSelectors`, `fullRenderedBindings`, and `redactions`. Payload
`vaultRevision` must equal envelope `vaultRevision`; payload
`effectivePolicyEpoch` is the semantic alias of and must equal envelope
`policyEpoch`. Together the envelope and payload form the complete snapshot.

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
2. **Commit** atomically swaps the active request/TLS decision snapshot.
   Subsequent TLS decision admissions use the new epoch. For a removed host,
   the new-request fence on tracked decrypted connections becomes active in
   the same commit. Host additions do not enumerate or close opaque sockets.
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
| `active-empty` | Authoritative empty binding set; select pass-through without decision-registry admission |

On first creation of a new sandbox or subject, the trusted control plane knows
that no vault has yet been configured. It automatically installs and
acknowledges an explicit empty decision snapshot before reporting readiness or
releasing the workload. HTTPS then passes through without requiring the caller
to create a vault. Failed initialization keeps readiness false.

This empty decision snapshot is internal metadata with no credentials. It does
not set the public vault store's `exists` flag or consume public revision 1.
`GET /credential-vault` still returns not found until the caller's first POST;
that POST creates revision 1 and transitions the decision snapshot to active
(or active-empty for an intentionally empty vault). Startup installation and
POST use the same mutation barrier: a delayed startup operation must never
overwrite a caller-installed revision.

Deleting a vault commits an acknowledged empty tombstone before returning
`204`. Public GET then returns not found, and a later POST can create a new
vault. The internal decision epoch and runtime generation prevent confusion
between separate public vault lifetimes.

`bootstrapping` denotes unknown state during initialization or recovery, not
absence of caller action. On mitmdump restart, the surviving Go control plane
reinstalls its authoritative snapshot. After Go/sidecar replacement, pause/resume,
or fast-sandbox replay, missing local vault data alone is not proof of emptiness.
The trusted recovery owner must restore the intended revision or explicitly
confirm empty state for the new generation before readiness returns. A replay
timeout or missing record must not silently install an empty snapshot.
Deployments must retain that recovery intent outside the process; this OSEP
adds no credential persistence. First creation and recovery are distinct
lifecycle events, even when recovery uses a newly allocated Pod.

### Connection Transition Semantics

Let `covers_old(sni)` and `covers_new(sni)` be semantic HTTPS/443
coverage predicates, rather than raw selector-set subtraction. Replacing
`*.example.com` with `api.example.com` keeps that exact host covered while
uncovering `docs.example.com`.

**Newly covered hosts**

- Atomically install the new decision epoch before acknowledging the mutation.
- TLS decision admissions after cutover use the new coverage. A handshake
  admitted before cutover may finish opaque, even if it completes after ACK.
- Existing opaque connections remain opaque and uncredentialed. Clients must
  reconnect or recycle their connection pools to receive injection. There is
  no bounded wait until such a connection becomes credentialed.
- This has the same injection-availability limitation as HTTP/2 coalescing over
  an unbound-SNI connection. Neither case exposes a vault credential.
- Request-level binding selection on already decrypted connections still uses
  the current acknowledged revision.

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
affect effective policy, fast-sandbox binding replay, and subject unload share one
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

The fast-sandbox profile additionally fences every snapshot with the subject runtime
generation. A delayed push, acknowledgement, or connection-close event from an
old generation cannot affect the replacement subject.

### Failure Semantics

| Condition | Result in credential-bound mode |
|---|---|
| Authoritative active-empty snapshot | Pass through without decision-registry admission |
| SNI does not match a bound host | Pass through without decision-registry admission |
| Validated static pass-through selector match | Pass through; semantic overlap with a binding is rejected before activation |
| ECH hides the actual SNI | Pass through as opaque TLS; no credential injection |
| No SNI | Preserve current secure no-SNI pass-through behavior; no credential injection |
| Decrypted-connection registry budget reached | Immediately close the newly accepted bound TCP connection before TLS termination; no hang or opaque fallback. Unbound pass-through remains available |
| Prepare/install failure before commit | Reject the candidate; keep the prior installed snapshot. If no snapshot is installed, deny |
| Commit readback timeout or lost acknowledgement | Enter `CREDENTIAL_REVISION_INDETERMINATE`; reject vault reads/writes until active-tuple readback reconciles the outcome |
| Unknown fast-sandbox source identity | Deny and emit a bounded dispatch-miss signal |
| Snapshot revision/generation mismatch | Deny until reconciled |
| Candidate install or connection fence failure | Reject mutation; retain prior acknowledged revision |
| Sidecar restart or subject rebind before replay | For a known identity, early ECH/no-SNI/static-ignore pass-through remains; all other SNI-bearing TLS is denied until replay |
| Go control-plane incarnation disappears | Discard generation-bound snapshot after the liveness bound, enter bootstrapping deny, and restart |

This proposal depends on the fail-closed active-vault lookup and destination-
identity invariants tracked by the related work above. It must not be
implemented by returning pass-through on every lookup exception or by trusting
HTTP authority independently of SNI/original destination.

### Sidecar and Fast Sandbox Profiles

The sidecar profile owns one decision snapshot and one connection registry.

The fast-sandbox profile owns one snapshot per subject. Client source IP is only the
dispatch key into a fenced subject identity; it is not the durable identity.
The same SNI may decrypt for subject A and pass through for subject B when only
subject A has a matching binding. Subject registration starts deny-first,
subject unload removes its snapshot and closes its tracked connections, and a
new runtime generation never inherits the old subject's cache. Teardown must
also terminate opaque transports using mitmproxy's existing connection ownership
or network attachment teardown before source identity is reused. Removing the
extra host-add registry does not authorize cross-generation transport reuse.

The decision registry contains only live decrypted connections, including those
draining after binding removal. Opaque traffic retains normal mitmproxy
transport state but no extra SNI index, revision membership, or host-add scan.
Remove entries on close; never evict a live decrypted entry to admit another.

Replace the fixed 65,536/1,024 ceilings with operator-configured finite process
and per-subject budgets. Both may be raised or lowered within the provisioned
memory budget. Before selecting release defaults, benchmarks must measure
incremental bytes per entry, peak handshake/drain overhead, expected bound
connections per subject, and process memory headroom. The global budget must
cover the intended concurrency: 4,096 subjects at 32 bound connections require
at least 131,072 entries plus measured headroom. Any oversubscription must be
explicit; a per-subject limit is not a reserved allocation.

On exhaustion, immediately close a new bound connection before TLS termination;
the client observes connection termination, not an indefinite timeout. Existing
tracked connections and new opaque connections continue. Emit
`registry_exhausted`, occupancy, and configured-budget metrics using bounded
labels. Sustained exhaustion is a paging-worthy bound-HTTPS availability signal;
operators increase provisioned budget or reduce workload concurrency. Limits
and defaults are deployment controls, not a new sandbox-user API.

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

User-facing guides and SDK examples must make the connection-pool behavior
explicit: install bindings before starting credential-dependent requests, and
reconnect or recycle the relevant connection pool after adding a binding for a
previously unbound host. A new HTTP/2 stream on an existing opaque TLS
connection is not a new TLS connection and cannot receive injection. Binding
acknowledgement does not imply that existing pooled connections are credentialed.

IP-literal credential binding remains invalid. DNS and nftables policy remain
the network-reachability authority, including for pass-through traffic.

### Security and Privacy Model

This mode reduces the plaintext visible to the trusted egress proxy and its
operator-controlled addons. It does not make the egress sidecar untrusted, hide
destinations from network policy, or provide end-to-end TLS for bound hosts.

For unbound HTTPS, HTTP inspection addons cannot read headers, URL paths, or
bodies, perform content-based data loss prevention (DLP), or generate
request/response-level application audit records. DNS and network-policy
enforcement remain in place, but network metadata and TLS-decision telemetry
do not replace application-level inspection or auditing. Documentation must
distinguish this intentional loss of visibility from an addon malfunction.

Deployments that require such inspection should select `interceptionMode: all`
and configure the trusted platform admission layer to reject incompatible mode
requests where inspection is mandatory. The caller-selected enum alone is not
an enforcement mechanism for an operator's mandatory inspection policy. The
implementation documentation must identify how each supported deployment
enforces that restriction before recommending it for this purpose.

`all` remains subject to its interception scope and protocol support; it does
not guarantee universal coverage of no-SNI, ECH, QUIC, or explicitly bypassed
traffic. Operators must evaluate these exceptions against their inspection
requirements. This OSEP does not add a DLP engine or fill those coverage gaps.

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
| `egress.mitm.tls.active_connections` | `mode`, `decision` | Aggregate transport counters; do not build an opaque SNI registry for this metric |
| `egress.mitm.registry.entries` / `egress.mitm.registry.capacity` | `scope` (process or subject aggregate) | Observe decrypted-entry occupancy and configured limits without per-subject UID labels |
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

The Go transaction coordinator is also an isolated in-memory foundation. It
allocates decision epochs, checks exact acknowledgements, and blocks mutations
while an operation is unresolved. A failed prepare remains inert, so the prior
revision is still readable while abort acknowledgement is retried; reads are
blocked only after commit may have reached the receiver. Reconciliation uses
metadata-only readback or exact commit/abort retries. Its transport is injected;
an unused Go adapter now implements its strict JSON contract over a
caller-provisioned private Unix socket, presents a high-entropy per-session
bearer token for receiver-side authentication, and rejects malformed, oversized,
or credential-bearing error responses. A matching unused Python endpoint now
authenticates the bearer token before reading bounded request bodies, strictly
decodes the envelope, and exposes only fixed errors and metadata
acknowledgements. The always-loaded system addon now owns that endpoint only
when the Go launcher supplies a complete internal per-process session bundle;
missing configuration keeps it disabled, partial configuration fails startup,
and addon shutdown fences the receiver and removes its owned socket. The Go
launcher strips inherited bundle values and can hand off a validated bundle.
Behind an internal development-only gate, the sidecar assembly now gives every
initial or restarted mitmdump process a fresh session bundle and keeps health
not-ready until the current in-memory Vault snapshot, or the authoritative
initial empty state, is exactly acknowledged. Fast Sandbox still passes no
bundle, and the gate defaults off. Public Vault writes are rejected while the
internal gate is enabled until mutation acknowledgement is wired. The Go
process-session owner creates a private per-process receiver directory,
high-entropy control generation and token, matching launcher bundle, Unix
transport, and coordinator. It accepts readiness only from an authenticated
fresh receiver with no active revision. Directory operations stay anchored to a
caller-owned stable non-writable parent, verify the child UID/GID and mode, and
require the target identity to have directory search permission. Cleanup refuses
a replaced directory identity. The session owner can now bootstrap one
authoritative empty or restored `ActiveSnapshot`: it first marshals the canonical
decision payload, requires an authenticated fresh receiver, applies the
prepare/commit transaction, and returns only after the coordinator confirms the
exact identity. It can also reconcile an indeterminate bootstrap through
metadata-only readback and exact commit/abort retries: a confirmed identity
completes bootstrap, while a confirmed non-activation returns to an idle state
that permits a new candidate. When a candidate allocated by `Apply` has an
indeterminate prepare/abort or commit outcome, the call returns that exact
attempt identity with `ErrIndeterminate`; that identity is not proof of
activation, and the caller must compare it exactly with a later reconciliation
result before publishing. Attempt identity is now also retained by the private
ProcessSession API, but public mutation and atomic public-store finalization,
along with connection fencing, remain unwired. Durable recovery intent after a
complete sidecar replacement remains integration work.

ProcessSession now also exposes an internal post-bootstrap `Update` primitive
and exact-attempt `ReconcileUpdate`. A future caller must keep its Vault
candidate unpublished while holding the shared mutation barrier. A successful
`Update` confirms its exact identity and permits finalization. After an
indeterminate update, the session retains the exact attempt and the exact
previous confirmed identity. `ReconcileUpdate` accepts only that outstanding
attempt: it permits finalization only when it confirms that attempt active, and
permits discarding only when it confirms the frozen previous identity remains
active. Other attempts are rejected without transport activity; reconciliation
errors retain the attempt, and a concurrent update remains blocked without
transport activity.

The sidecar now has an internal generation-pinned callback that holds the live
process/session lifecycle read lock for the callback's full duration. This is
only an ownership primitive: public mutation handlers, Vault Store candidate
finalization, and connection fences are still not connected to it, so no
public mutation acknowledgement is live. The callback accepts only the narrow
update/reconcile session interface and requires a bounded context with a
deadline canceled when sidecar shutdown begins. Callbacks must pass that same
context to session operations and return promptly on cancellation; shutdown
waits for a running callback to release the lifecycle read lock, and the helper
cannot terminate a callback that ignores cancellation.

`ErrClosed` and `ErrTransportUnavailable`, including a local parent-path fence
failure after the receiver committed, are terminal session failures rather
than reconcilable mutation outcomes. They return no attempt identity and never
authorize candidate finalization. The future owner must stop the exact child,
close the session, discard the unpublished candidate, and latch sticky
unknown-outcome recovery — the external effects cannot be confirmed, so the
same sidecar must never bootstrap a fresh session from the possibly-stale
prior public state; recovery requires replacing the sidecar. This primitive
does not connect public mutation handlers, finalize the Vault store, or
install connection fences; selective TLS decisions remain disabled. (This is
the current implementation-owner behavior; the proposal itself is unchanged.)

The Go Vault store can now prepare unpublished create, patch, and delete
candidates. A candidate freezes its rendered `ActiveSnapshot` before commit,
publishes at most once, and uses a private mutation tag to reject concurrent
changes and delete/recreate ABA even when the public Vault revision repeats.
This is only the store-side prerequisite: the public handlers still return
`503` under the internal gate, and ProcessSession update acknowledgement and
connection fencing remain unwired. The sidecar's existing policy mutex now
serializes effective-policy reads plus Vault create/patch/delete with `/policy`
updates. Periodic `deny.always` / `allow.always` reload now uses this same
barrier: it parses a candidate pair and, when an nft applier is configured,
applies the corresponding static policy before publishing the loader and proxy
rules. An `ApplyStatic` error preserves the active in-memory rules and leaves
the candidate eligible for a later retry; parse errors do the same. This is a
scoped nft-first staging boundary, not a revision transaction, and it makes no
claim that an external nft apply error has no side effects. Vault binding
revalidation, ProcessSession update acknowledgement, and connection fencing
remain unconnected; no selective TLS decision is enabled by this change.

The proxy-side transaction receiver validates
generation/epoch/digest identities, stages immutable bytes, and implements
commit, abort, and metadata-only readback. Its authenticated IPC endpoint is
conditionally attached to the live addon as described above; only the gated
sidecar startup/restart path supplies a session. The Go builder emits
the versioned canonical decision payload from a rendered Vault snapshot and
policy epoch. It derives and sorts HTTPS selectors from the same canonical
bindings, preserves redaction order, and rejects non-canonical revisions,
selectors, or rendered credential/redaction coverage. A matching unused Python
validator now strictly decodes those exact bytes, checks envelope vault/policy
agreement, recomputes active state and HTTPS selectors from the full bindings,
and rejects incomplete redaction coverage with a fixed sanitized error. The
next integration must place public policy/Vault mutations and revision
installation under the shared mutation barrier, then add connection fences
before acknowledging those mutations. Existing request processing continues to
use the conditional ETag lookup, and no selective TLS decision is enabled yet.

Implementation has started with the internal host-selector algebra and shared
Go/Python conformance vectors. The control plane owns non-transitional UTS #46
normalization; the addon consumes canonical ASCII selectors and matches ASCII
wire SNI. An opt-in request-level shadow observer now reuses the existing Vault
lookup to project host coverage and exports a separate request-sample counter.
It performs no ClientHello lookup and does not enable selective interception;
opaque connections and failed handshakes are outside its observation set. The
public interception mode remains unavailable until the later phases pass.

The Python side now also has a pure ClientHello decision foundation. It builds
an immutable TLS selector view only after strict validation of a real canonical
revision snapshot, retaining revision metadata and parsed host selectors while
discarding payload and credential-bearing bindings. Classification follows the
early identity, ECH, no-SNI, invalid-SNI, static-ignore, snapshot-generation,
and binding-host order, and reports only closed action/reason values. A bound
host returns `needs_registry`; this is not a decrypt instruction. This step
fails malformed ECH/static-selector arguments closed with `reason=invalid_input`
while preserving early identity, ECH, and no-SNI ordering. It does not connect
the classifier to the system addon or receiver commit path,
and does not change Go, public configuration, or live traffic. Selective TLS
remains disabled.

An unused sidecar-only connection-registry foundation now consumes the pure
classification result under one lock with bounded admission. It records only
bound, admitted connections with their generation and decision epoch; capacity
exhaustion denies new bound admission rather than making it opaque, while
unbound pass-through consumes no entry. One Registry instance accepts only one
sidecar generation; a replacement process creates a fresh instance. Deactivation
denies later non-exempt SNI-bearing decisions and returns existing memberships
for the future owner to close, but does not close transports itself. The
joint-publication owner described below remains separate from mitmproxy hooks,
fast-sandbox budgets, and live traffic.

The unused sidecar registry now also reports which still-tracked decrypted
connections become newly uncovered when a validated decision snapshot is
activated. It compares the old and new selector coverage of each admitted SNI
under the same lock as new TLS admissions, so overlapping wildcard and exact
selectors are evaluated semantically rather than by raw set subtraction.
Already-uncovered entries are not reported again on consecutive uncovered
views, but remain tracked until released. Remove/readd/remove can report the
same token again; a future transport owner must not reset its first retirement
deadline on repeated notifications.

The unused registry now permanently fences those tokens from its internal
request-admission primitive. A Registry may bind one Receiver at construction;
request admission without one denies. Under the Registry lock, `acquire_request`
checks exact live token ownership, generation, and the monotonic connection
fence, then acquires the Receiver snapshot and compares the complete revision.
Only a coherent immutable Snapshot is returned. The sole nested lock order is
Registry -> Receiver; successful admission linearizes when Receiver.acquire
pins that snapshot. Neither later publication nor teardown revokes an already
admitted request's bytes. Credential or request-selector updates retain the
connection token, but new requests use the new snapshot once both views agree.
Removed/readded hosts require a new connection; releasing a connection removes
its fence without reusing its serial or accumulating tombstones.

Standalone Receiver/Registry publication remains independent: either order
fails closed while revisions disagree, with no old credential fallback. The
internal joint-publication owner below removes that mismatch window for its
owned pair. Live hooks and public mutation ACKs still require integration with
the public mutation barrier and transport owner. Request admission is not full
binding or destination authorization. No live HTTP request/stream uses this primitive yet; transport
closure, HTTP/2 GOAWAY and request drain, including credential-only rotation
drain, remain unimplemented. Holders must retain one snapshot through response
redaction; this primitive provides neither live deadline enforcement nor zeroization.

Successful internal request admission now also registers an exact, immutable
request handle before returning, with the same pinned Snapshot as the result.
Request records have a separate global capacity; the compatibility default is
the connection capacity, not a production HTTP/2 sizing recommendation. An
adapter must choose its budget explicitly. Exhaustion denies without waiting,
eviction or pass-through, and one connection can consume the entire request
budget; this is not per-connection fairness or tenant isolation.

The future adapter must call `finish_request` on completion, cancellation and
error paths. Exact terminal connection `release` also removes that connection's
request records; copied tokens cannot release a real connection or its requests.
Release is only for confirmed transport termination, not the start of drain.
Host removal and Registry deactivation retain admitted requests for completion
or terminal cleanup, and Receiver close does not revoke their immutable bytes.
Empty connection indexes are removed and serials are never reused. Registration
failures roll back partial indexes and expose only a fixed error.

`pending_requests` exposes bounded, serial-ordered metadata pages containing
only request serial, connection serial and complete revision, with optional
exact-connection and revision filters. It exposes no Snapshot or finish handle.
Each page is lock-consistent, but completion and new admission can change later
pages; there is no frozen query view or high-watermark drain protocol. An empty
page and `request_count` describe only Registry bookkeeping, not network drain,
mutation ACK readiness, external Snapshot references or credential zeroization.
The unused registry now records monotonic retirement deadlines at activation
under its admission lock. Newly uncovered connections receive a deadline even
when idle; unfinished requests pinned to older revisions receive independent
deadlines, including on credential-only, request-scope or policy updates. The
internal constructor accepts an integer timeout of `1..300` seconds, default
`30`; the operator environment setting remains unwired. Repeated publication,
further rotations and remove/readd/remove never extend the first deadline.
Exact request completion removes its deadline; exact terminal connection
release removes all corresponding deadlines. Deactivation preserves existing
deadlines and returns all transports for shutdown without starting a new grace
period.

`expired_connections` returns bounded, serial-ordered pages of exact live
connection tokens whose connection deadline or at least one unfinished retired
request deadline has expired. Multiple expired requests yield one target. A
completed old request no longer causes expiry on a still-covered connection;
newer requests alone do not retire that connection. Pages are observations,
not closure commands: completion or release may invalidate a returned target,
and each new scan must restart at zero because lower serials can expire later.
Expiry neither releases bookkeeping nor cancels work, revokes external
Snapshots, fences new requests on still-covered connections, or proves ACK
readiness. A future transport owner must inspect promptly and close expired
targets, potentially interrupting newer requests sharing the same transport.
There is still no live timer, transport closure or HTTP/2 GOAWAY owner, or
public mutation ACK integration.

An internal `RevisionPublisher` now exclusively owns a fresh Receiver
and TLS Registry for one generation. Prepare validates the bounded immutable
bytes and compiles their credential-free selector view outside both state locks,
then stages both under the existing Registry -> Receiver lock order. Concurrent
abort, close or another receiver transition invalidates delayed preparation;
exact active/pending retries preserve newer prepared work. The Receiver's
lifetime abort budget and historical exact-abort retries remain unchanged.

Commit rechecks the exact prepared revision while holding both locks. It plans
coverage changes, permanent request fences and retirement deadlines against the
connections and requests present at commit, including those admitted after
prepare. All fallible planning completes before either active view changes;
failed planning preserves active state and the candidate for retry. Both views
and all retirement bookkeeping are then published before either lock is released.
Connection/request admissions therefore observe the old or new coherent state,
without the standalone publication mismatch window. Credential-only rotation
keeps old request bytes and deadlines while later requests pin the new snapshot.
Exact active commit retries return no newly uncovered transports, do not extend
deadlines, and do not discard a newer prepared candidate.

The owner's close fences both components together, clears prepared state, and
retains memberships, pinned handles and existing deadlines for terminal cleanup.
Independent Receiver mutations and Registry activation/deactivation are rejected
for this owned pair; standalone instances keep their existing APIs. Readback
remains metadata-only, and commit returns newly uncovered connection tokens for
a future drain owner, not a public mutation ACK. `RevisionPublisher` is not a
Receiver and is not accepted directly by the exact-type IPC server.

An internal `InstallationReceiver` now adapts a fresh Publisher to the existing
authenticated IPC and experimental sidecar addon lifecycle. It exposes only the
Receiver-shaped install/readback API and permanently disables connection
admission before the owner escapes construction. No connection memberships or
request handles can be created through this owner, so commit and close have no
transport obligations to discard. Standalone Receiver and Publisher APIs remain
available with their existing behavior. Commit returns the exact requested
revision metadata, even if another commit installs a successor before the reply;
active readback resolves a lost acknowledgement. Both active views are fenced
on startup failure or addon shutdown.

This backend confirms coherent installation only, never transport drain or a
public mutation ACK. It adds no TLS/request hooks, live timer, transport closure,
HTTP/2 GOAWAY, public configuration or selective TLS activation. The experimental
gate still blocks public Vault writes; a future transport-aware owner must
consume Publisher obligations before enabling live admissions.

An internal Go Vault mutation owner now composes candidate preparation/rendering,
exact process-session Update/Reconcile, and local Store finalization under the
shared policy/Vault barrier and an exclusive live-generation lease. It requires
a deadline context that its future caller must cancel on sidecar shutdown. The
owner sends each candidate once, reconciles only the exact attempt, and checks
the complete returned identity against the pinned generation, rendered digest,
Vault revision and bootstrap-reserved policy epoch zero before finalization.
Confirmed success finalizes even if cancellation arrives with the confirmation;
an exact confirmed abort discards without replacing the live generation.

An unresolved deadline, terminal session failure, inconsistent acknowledgement
or failed local finalization after acknowledgement fences readiness and detaches
the exact child/session. Both locks remain held while that child is stopped and
reaped, the session is closed, and the unpublished candidate is discarded. This
prevents shutdown from claiming the same child twice and prevents the existing
restart path from reading recovery state before cleanup. Session-close failure
never reports success or restores readiness. IPC reconciliation is bounded by
the context; existing process stop/reap does not promise a hard cleanup deadline.

The owner integration tests run the real Go Store, ProcessSession, coordinator
and Unix client against a Python subprocess using the production authenticated
IPC endpoint and installation-only publisher. Response-boundary faults cover
lost prepare/commit replies, withheld then failed readback, deadline cleanup,
and stale local finalization, followed by fresh-session bootstrap from public
Store state. The test child is actually signaled and reaped through the owner's
stop seam; this does not exercise the production mitmdump launcher,
`GracefulShutdown`, restart watcher, TLS hooks, or transport draining. These tests
require Python 3 and permission to create Unix sockets; Egress CI supplies
Python before running Go tests. Socket setup failures fail rather than skip.

This owner is not wired into public HTTP handlers. Public Vault writes remain
blocked by the experimental gate, policy mutations do not yet participate in
revision installation, and installation confirmation is not transport drain or
public mutation completion. Selective TLS, live admissions and request hooks
remain disabled for the installation-only backend.

An internal, HTTP-unwired effective-policy candidate now freezes explicit user,
ordered always-deny/allow, and resolved telemetry rules without loader callbacks.
The independent immutable policy-base handle and authoritative Store must both
be supplied when validating a candidate under the shared mutation barrier.
Replacing the base handle invalidates older candidates even if policy content
and epoch repeat; the Store's private mutation identity rejects Vault changes,
including absent/create/delete and delete/recreate public-revision ABA.
The experimental sidecar now tracks the live current base through a shared
recovery/readiness owner, described below. Active policy epochs remain zero;
policy-only candidate installation does not publish an active policy epoch.

The recovery/readiness owner holds the authoritative immutable effective-policy
base, Store identity, a private non-reusable bootstrap invalidation identity and
a sticky recovery reason under the shared policy/Vault barrier. Capturing a
bootstrap ticket freezes the already-rendered Vault snapshot without resolving
credential sources again. The ticket is private, is not sent over IPC, and does
not use the decision digest as proof of complete policy identity. Replacing the
base invalidates prior candidates and bootstraps even if rules and epoch repeat;
Vault mutation identity also rejects public-revision ABA.

Initial startup and the revision layer's child-restart path share one protected publication
step, taking the policy barrier before the process-lifecycle lock. Slow launch,
bootstrap IPC and listener checks run outside these locks. The final step checks
the exact ticket, current base/Store/Vault identity, recovery state, caller
cancellation, shutdown and exact pending child/session generation before
transferring ownership and setting health ready. Successful installation and
listener availability alone do not permit publication. Rejected attempts retain
their own resources for exact child stop/reap followed by session close; an old
exit notification cannot detach a newer generation. A rejected launch permits
a fresh capture only while recovery has not been required.

Experimental policy and always-rule paths invalidate in-flight bootstrap tickets
before attempting external effects, then replace the authoritative base on
successful legacy publication. They use explicitly frozen loader/telemetry
inputs and do not turn policy success responses into revision transaction ACKs.
An uncertain failure after an attempted policy-file or nft effect, or an
unconfirmed session cleanup, latches `recovery-required`. Pure parse/validation
failures before effects do not. Once latched, health remains not-ready, internal
policy candidate preparation and Vault mutation ownership reject work, and
ordinary child restart or successful IPC readback cannot clear it. Shutdown and
teardown remain available subject to the fault-containment boundary below.
There is no reset API; the first recovery reason is
sticky only within the same Go process incarnation.

For the ordinary sidecar's experimental revision runtime, recovery also
synchronously quiesces the same nft Manager. A terminal static-apply error with
an `Unknown` kernel effect sets a one-way frozen state under the Manager lock
before returning, including the first startup apply; an `Unchanged` error or
successful missing-table fallback does not add a freeze.
The option is set by the existing experimental switch at Manager construction,
before startup enforcement. The recovery owner requires `Quiesce()` on its
internal nft interface and calls it for policy persistence, nft apply and
session-cleanup failures after publishing ticket invalidation and the sticky
health restriction. Health probes do not acquire the policy or Manager lock,
so they can return 503 while an admitted writer finishes. Lock order remains
policy/Vault barrier, any already-held process-lifecycle lock, then Manager lock;
the Manager does not call back into the owner.

`ApplyStatic` retains its error interface: nil means `Committed`, and a typed
error/helper carries `Unchanged` or `Unknown`. Ruleset construction failure,
pre-start cancellation and process Start failure are known not to execute any
kernel operation. The production runner separates Start from Wait and preserves
command output. Every post-Start error, including timeout, signal, nonzero exit,
wait or output-copy failure, is `Unknown`; unclassified runner errors also default
to `Unknown`. Neither stderr nor nonzero exit proves an unchanged kernel.
The existing missing-table fallback makes at most one retry. A failed pair keeps
both errors and cannot downgrade an `Unknown` first attempt.

The owner still stages file Save, then nft, then in-memory publication. With no
policy file, `Unchanged` preserves the old base and permits retry without adding
recovery. After Save, it permits this only if exact durable Restore of the previous
bytes/metadata or previous absence succeeds. Any Restore failure latches recovery,
even `FileUnchanged`, which may mean the newly saved file is still present.
`Unknown` latches health/bootstrap/writer restrictions before best-effort Restore;
successful Restore does not clear recovery. Failed always reload keeps old
loader/proxy/base, and prior bootstrap invalidation, recovery and quiescence
remain irreversible. Initial setup fails startup for every apply error.

The atomic write-admission gate covers static replacement, `AddResolvedIPs`,
`AddResolvedDomain`, late `applyDomainRefresh` results, active TCP lease renewal,
and `AddUpstreamProxyIPs`. Quiescence closes this gate before waiting for the
Manager lock; queued and new writers check it after acquiring the lock. A writer
that passed the gate before it closed may complete its nft update and associated
state publication. Quiescence returns only after that admitted operation drains.
An in-flight DNS query may return later, but
cannot write nft or republish old authorization tracking. Upstream proxy lease
renewal also stops: DNS-learned addresses may expire and later proxy connections
may fail. Existing traffic and established connections can still be permitted.
Stale bootstrap tickets and successful policy publication do not themselves
freeze the Manager. Legacy sidecar, Fast Sandbox and dns-only behavior
keep their existing boundaries.

The Manager API still permits explicit `RemoveEnforcement` without unfreezing
it. The experimental runtime owner retains enforcement at shutdown as described
below; legacy shutdown continues to call that teardown API. There is no reset or
unfreeze API, durable frozen state, rollback of unknown effects,
or new hard shutdown deadline. The Manager alone supplies no packet fence;
runtime fault containment is a separate owner described below.
Deterministic owner tests use real Manager write
admission and injected effect failures to cover recovery sources and health
while an admitted writer drains; these tests do not establish kernel behavior.

`TestNftQuiescenceAfterCommittedStaticError` uses a separate Linux network
namespace and the real nft runner. After seeding old dynamic and upstream
addresses, it commits a new static ruleset and injects a result error only after
nft succeeds. All six subsequent runtime paths must leave the committed policy
and empty dynamic/upstream sets unchanged. The privileged egress CI explicitly
selects this test, `TestDynamicElementRenewal`,
`TestNftStartFailurePreservesKernelAndRetry`, `TestNftRealMissingTableFallback`,
and `TestRevisionNftEffectsRealFileAndKernel` with `OPENSANDBOX_NFT_TEST=1`.
The new tests verify actual unchanged kernel readback and retry after production
Start failure, real missing-table fallback, and real owner Save/Restore of prior
bytes or absence coupled with both unchanged and committed-but-unknown nft
outcomes. The dedicated nft CI runner requires exact discovery plus RUN/PASS
for each selected test, and rejects SKIP. Linux, nftables, `unshare`, and namespace/nft permissions
are required; missing prerequisites fail enabled runs. Ordinary Go suite runs
skip this kernel validation when it is not enabled. Neither a skip nor a setup
failure establishes kernel behavior, and the test does not prove a packet fence.

Deterministic lifecycle tests cover both readiness entry points, staged recovery
transitions, exact cleanup and publication races. A real Go→Unix→Python test uses
the production authenticated IPC endpoint and installation-only receiver to
pause after installation and reject publication after a policy-base change,
Vault ABA or recovery latch, with clean recapture and sticky-state controls. That
test requires permission to create Unix sockets: a setup failure is a test
failure, not evidence that stale-publication assertions ran. The child stop seam
signals and reaps a real subprocess; it does not exercise the production
mitmdump launcher, listener, `GracefulShutdown`, TLS or transport drain.

The latch is neither a network fence nor durable recovery intent. This increment
does not provide atomic policy-file/nft rollback, restore dynamic DNS state, or
survive a whole Go/sidecar crash. Restarting the entire sidecar loses the latch
and is not a proven safe recovery procedure. External effects transactions,
durable intent, active policy epochs, public mutation ACKs, selective TLS,
request hooks, live admissions and Fast Sandbox integration remain future work.

The runtime-only fault-containment addition supplements that latch and Manager
freeze with an independent packet fence for the Linux `dns+nft` sidecar. It uses the existing experimental
gate without adding lifecycle-server wiring, a deployment topology or new
configuration. Ordinary policy, always-rule and internal Vault updates do not
install a whole-IP fence. Containment follows the `requireRecovery` path,
including unknown external effects. A known `prepareRejected` or
definitive `committed=false` result does not by itself introduce permanent
isolation. Successful updates retain their existing behavior unless recovery
was already required.

Unexpected child death enters recovery only if the observation still belongs to
the exact currently owned, running child generation. Stale, already detached or
normally stopping generations do not trigger a new fence. Normal shutdown adds
no fence. Experimental shutdown stops services but always retains ordinary
policy/redirect rules, preventing late drain or session-cleanup failures from
being followed by enforcement removal. Legacy teardown is unchanged.

The original network namespace is pinned only for the current Go process's
lifetime. `QuarantineConfirmed` requires both confirmation of that target and
exact readback of the independent `inet opensandbox_quarantine` table. A target
validation, installation or readback failure leaves isolation
`QuarantineUnknown`. Readiness, writer quiescence and packet isolation are
independent: neither a 503 response nor process exit proves containment.
Confirmed containment blocks IPv4/IPv6 input, output and forwarding, including
loopback, established traffic, UDP, marks, the MITM UID and DNS/upstream-proxy
exceptions; health/control HTTP and execd IP traffic are interrupted too. This
assumes no workload NET_ADMIN/NET_RAW, hostile privileged namespace writer or
L2/offloaded bypass. There is no zero-leakage guarantee before fault detection
and fence confirmation, and failed installation does not guarantee workload
termination.

Ordinary policy-table or redirect cleanup cannot remove this independent fence,
and the runtime-fault path does not automatically remove it or restore service.
A residual kernel table does not constitute a restart protocol. The
process-lifetime namespace pin supplies no Go-process/container-restart
guarantee, durable intent, replay or pause/resume recovery. Those lifecycle and
recovery boundaries remain future work; this addition must not be presented as
durable quarantine or safe automatic restart. Legacy prestart can remove
ordinary redirects while a residual fence still blocks startup; the supported
operational response is sandbox destruction and recreation, not verified restart
recovery. See the
[runtime fault-containment boundary](../docs/architecture/network/egress.md#experimental-runtime-fault-containment).

Preparation revalidates every Vault binding, including HTTP-only bindings, and
adds a conservative ordered whole-selector coverage proof for wildcard hosts.
An exact or nested wildcard deny is rejected unless an earlier allow covers
its overlap; nameserver nft allowances cannot authorize credential bindings.
The helper preserves first-match policy semantics and does not alter legacy
public Vault validation. Coverage is fail-closed: it does not prove that a union
of narrower selectors exhausts a maximum-length wildcard's finite DNS names.

The Store captures its existing pinned rendered snapshot under the same lock as
binding validation and mutation identity. Bound Vaults created by legacy
Create/Patch without a committed rendered candidate are rejected rather than
re-resolving credentials. An empty Vault preserves its positive revision and
exists state; an absent Vault remains absent. Policy-only preparation changes
neither public Vault revision nor rendered credential bytes. Canonical decision
bytes and digest use prospective policy epoch `base + 1`, including identical
inputs; repeated preparation does not consume epochs. No-op selection remains
the future publication owner's responsibility. The digest authenticates the
decision payload (Vault and epoch), not the frozen policy inputs; different
candidates from the same base can share it. The future effect owner must bind
external effects to the exact candidate, not infer policy identity from digest.

The candidate integration test reuses the real Go-to-Python IPC fixture to
check prospective epoch/digest installation with admissions disabled and
unchanged control-plane base/Vault state. It requires Unix socket permission
and fails on setup errors. This slice supplies no disk/nft effects, live policy
publication, rollback/crash durability, public mutation acknowledgement, or
TLS/request/fast-sandbox activation. A later effect owner must establish those
boundaries before wiring the candidate into public mutations.

The live sidecar credential-bound loop is now wired for the Docker sidecar
experiment (canonical HTTPS/443, exact hostnames, inline credentials, HTTP/1.1
keepalive). The launcher hands the mitmdump child the live admission bundle
(decrypted-connection capacity, admitted-request capacity, drain timeout)
alongside the revision session; the addon then builds a `LiveReceiver` whose
registry admits real connections instead of the installation-only receiver.
`tls_clienthello` classifies visible SNI against the installed immutable
snapshot: an unbound or no-SNI ClientHello passes through opaquely, as does an
ECH-hidden connection detected from the actual `encrypted_client_hello`
extension (0xfe0d) in the parsed extension list; a bound host is decrypted
only with a registry admission, and every deny outcome (bootstrapping,
generation mismatch, invalid SNI, malformed ClientHello data, exhausted
registry) closes the connection instead of decrypting or tunneling unknown
state. A decrypted connection without an admission is rejected at
`requestheaders`, where each request acquires a revision-pinned snapshot,
the strict HTTP/1 request authority (exactly one well-formed `Host`, port
omitted or 443, matching absolute-form target when present) must equal both
the connection SNI and the admission token SNI, `ssl_insecure` is rejected
under the live bundle, terminal outcomes with unknown external effects latch
sticky recovery rather than restarting from prior state, and the rendered
bindings drive the existing binding/path/method match, injection and
redaction path. The task-level acceptance record for this loop is kept in
[live-vault-acceptance.md](../docs/guides/live-vault-acceptance.md).
Public `POST`/`PATCH`/`DELETE /credential-vault` now run the Go mutation
transaction under the shared policy/Vault barrier: the candidate is installed
and acknowledged on the receiver before the Store is finalized, a stale
`expectedRevision` conflicts, an indeterminate or failed outcome detaches
readiness instead of publishing, and prepare failures cross the boundary only
as fixed public error classes. Removed hosts fence new requests on tracked
connections immediately and the drain timeout force-closes the transport at
expiry, so remove/re-add cannot revive a retired connection. HTTP/2,
fast-sandbox subjects, dynamic policy, always-rule reload, and cross-process
recovery remain out of scope for this slice.

1. **Decision telemetry and red tests**
   - Add fail-closed tests that distinguish authoritative empty from lookup
     failure.
   - Add dry-run decision counters while traffic still uses `all`.
   - Land shared normalized host matching and destination-identity prerequisites.
2. **Immutable revision acknowledgement**
   - Replace time-only cache correctness with explicit snapshot install and
     invalidation.
   - Serialize policy/vault changes and fence sidecar/fast-sandbox generations.
3. **Opt-in sidecar mode for HTTPS/443**
   - Keep the mode behind an internal experimental gate; do not yet accept the
     public lifecycle field.
   - Implement ClientHello selection and HTTP/1.1 connection transitions.
4. **HTTP/2 and fast-sandbox parity**
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
- A legacy create response with no effective mode causes an explicit SDK error
  containing the created sandbox ID; mismatched/unknown modes also fail.
- The server echoes only the mode confirmed by the runtime, never the request.
- Cleanup success and failure preserve the mismatch result and resource identity.
- Unknown request fields are rejected by capable servers; omitted mode retains
  `all`. Test the previously tolerated-extra-field compatibility change.
- Credential-bound active-empty and unbound SNI select pass-through.
- First-create auto-empty leaves the public vault absent and first POST creates
  revision 1. Serialize startup/POST races so startup cannot overwrite a vault.
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
  timeout, 5xx, malformed JSON, dispatch miss, and generation mismatch. Dynamic
  pass-through requires an installed snapshot with no matching TLS selector.
- Cache invalidation makes an acknowledged revision visible immediately.
- Prepare, commit, abort, and readback are idempotent; lost acknowledgement is
  resolved by active-revision readback.
- Indeterminate reconciliation blocks vault, policy, always-rule, and replay
  mutations while still permitting forced subject/sandbox teardown.
- Concurrent policy and vault mutations cannot commit an inconsistent pair.
- `expectedRevision` is required for every credential-bound PATCH and rejects
  stale mutations.
- Old fast-sandbox generations cannot install snapshots or close new-generation
  connections.
- Registry exhaustion immediately terminates new bound connections without
  evicting live entries or denying new unbound TLS.
- Telemetry uses only the documented bounded attributes.

### Integration Tests

- A newly ready sandbox that never creates a vault can immediately use HTTPS
  pass-through. A failed auto-empty install prevents readiness.
- First Vault POST succeeds after startup; recovery without confirmed intent
  remains not-ready and cannot replace a lost bound revision with empty state.

- An unbound HTTPS server with a certificate trusted by the sandbox succeeds
  without trusting the OpenSandbox CA and observes end-to-end TLS.
- A bound HTTPS server requires the OpenSandbox CA and receives the expected
  injected credential only on matching requests.
- Adding the first binding leaves an existing opaque connection uncredentialed;
  after ACK a fresh TLS decision decrypts and injects on matching requests.
  Test handshake-admission races across the cutover.
- Reuse an opaque HTTP/2 connection after host-add ACK: new streams remain
  uncredentialed, while reconnecting with bound SNI enables injection. Exercise
  the connection-pool recycling procedure documented in the SDK examples.
- With an inspection test addon, verify unbound HTTPS exposes no HTTP headers,
  paths, bodies, or request/response audit events in credential-bound mode.
  Compare with `all` for supported, non-bypassed TLS and verify network-policy
  denial still applies in both modes. Test the documented platform restriction
  against incompatible caller-selected modes where inspection is mandatory.
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
- Exercise new and legacy server responses through the existing create route.
  Verify mismatch detection and cleanup, and demonstrate that entrypoint traffic
  may precede detection so no pre-creation safety guarantee is claimed.
- The model API is decrypted and credentialed; the unrelated endpoint is
  pass-through and does not require the OpenSandbox CA.
- In fast-sandbox mode, the same destination decrypts for a subject with a binding and
  passes through for another subject without one.
- Runtime add, replace, delete, restart, subject unload/rebind, and replay
  preserve the revision and failure contracts.
- Egress network deny rules still block pass-through destinations.
- Logs, metrics, diagnostics, and API responses contain no credential value.

### Performance Tests

- Compare TLS handshake latency and throughput for `all`, credential-bound
  decrypt, and credential-bound pass-through paths.
- Exercise at least 4,096 fast-sandbox subjects without unbounded per-host or
  per-source cache growth.
- Measure binding-revision transition latency with active HTTP/1.1 and HTTP/2
  connections.
- Run sustained connect/close churn and burst storms for mostly unbound,
  mostly bound, and mixed traffic, including concurrent host-add/remove.
  Measure CPU, RSS, allocations, p95/p99 admission latency, registry cleanup,
  and rejection rates against `all`; verify unbound traffic creates no extra
  decision-registry entries and returns to baseline after churn.
- Publish per-entry memory measurements and validate global/per-subject budget
  arithmetic at the advertised fast-sandbox scale; test raised budgets and exhaustion.

## Drawbacks

- The proxy must maintain connection state and revision fences, which is more
  complex than time-based snapshot polling.
- A runtime binding addition does not upgrade existing opaque connections;
  pooled clients must reconnect to receive credentials.
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
does not follow sandbox-local runtime mutations, cannot vary per fast-sandbox subject,
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

Credential-bound interception may be a suitable future default for deployments
whose primary purpose is credential injection. Any default change requires
implementation and performance results, addon compatibility evidence, explicit
operator communication, and a migration plan that prevents silent loss of
required inspection coverage. Preserve an explicit `all` option even if the
default changes. This OSEP keeps the current default and leaves that decision
to a future proposal.

### Track Every Opaque Connection and Close on Host Add

This provides stronger host-add injection availability but requires registry
churn for all visible-SNI TLS and turns a global cap into a broad HTTPS outage.
The selected bound-only design accepts existing opaque sessions just as it
accepts coalescing: no credential is injected until the client reconnects.
Revision acknowledgement remains immediate for new decisions and for request
evaluation on decrypted connections.

### Bound Opaque Connection Age

A 60–120 second maximum age could bound stale opaque sessions, but still needs
timers and per-connection lifecycle work, interrupts legitimate long-lived
traffic, and cannot make an opaque connection credentialed in place. Defer this
option until a concrete workload needs bounded reconnection latency.

### Time-Based Vault Polling Alone

Retaining the 0.5-second snapshot cache would also delay new TLS decisions and
request-level revocation on decrypted connections. That is distinct from
accepting existing opaque sessions. Keep explicit revision acknowledgement and
the decrypted-request fence.

## Infrastructure Needed

No new external service is required. The implementation needs an internal
revision-install/acknowledgement path between the Go egress control plane and
the mitmproxy system addon, plus a bounded connection registry. The exact local
IPC mechanism is an implementation detail as long as it is private to the
sidecar, fenced by subject generation, and meets the acknowledgement contract.

CI needs Linux integration coverage with mitmproxy 11.0.2, local TLS servers,
HTTP/2, network namespaces, and the existing Docker/Kubernetes/fast-sandbox egress test
paths.

## Upgrade & Migration Strategy

1. Implement and validate the internal decision/transition protocol without a
   public lifecycle field.
2. Ship dry-run telemetry and acknowledgement prerequisites without changing
   traffic.
3. After sidecar, fast-sandbox, HTTP/1.1, and HTTP/2 parity passes, align the lifecycle
   request/response schema, server runtime confirmation, and all SDKs on the
   existing create route. Document minimum versions, coordinated upgrades,
   effective-mode verification, and its post-creation detection limitation.
4. Enable `credential-bound` only when explicitly requested and only on
   runtimes that implement the complete contract; reject unsupported
   extra-port or pool/fast-sandbox combinations instead of degrading silently.
5. The runtime automatically installs an internal empty decision snapshot before
   first-create readiness. The caller's first public Vault POST remains valid.
   Recovery requires replay or explicit trusted confirmation of empty intent.
6. Document that every `PATCH /credential-vault` request in this mode must
   include the existing `expectedRevision`; callers can obtain it from `GET
   /credential-vault`. SDK documentation must explicitly distinguish this
   requirement from optional preconditions in `all` mode.
7. Keep current regex `ignore_hosts`, CA setup, and `all` behavior unchanged for
   existing deployments. Before enabling `credential-bound`, migrate intended
   static bypass entries to the exact/wildcard
   `OPENSANDBOX_EGRESS_MITMPROXY_PASSTHROUGH_HOSTS` list; the new mode rejects a
   non-empty legacy regex list.
8. Release notes must call out strict unknown-field rejection as a runtime
   compatibility tightening, even though the OpenAPI schema was already closed.
9. Do not change the default in this OSEP. A future default change requires
   usage data, addon compatibility evidence, and a separate migration plan.
