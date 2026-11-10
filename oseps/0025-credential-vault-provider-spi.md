---
title: Credential Vault Exchange and Delegated Header Resolution
authors:
  - "@Pangjiping"
creation-date: 2026-10-28
last-updated: 2026-11-06
status: implementing
---

# OSEP-0025: Credential Vault Exchange and Delegated Header Resolution

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
  - [Follow-ups (declared, non-blocking)](#follow-ups-declared-non-blocking)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Credential and Binding Model](#credential-and-binding-model)
  - [API](#api)
  - [Usage Demos](#usage-demos)
  - [Request Handling](#request-handling)
  - [Resolver Contract](#resolver-contract)
  - [Resolver Caller Authentication](#resolver-caller-authentication)
  - [Example Instantiations](#example-instantiations)
  - [Fast Sandbox Profile](#fast-sandbox-profile)
  - [Observability](#observability)
  - [Relationship to Prior OSEPs](#relationship-to-prior-oseps)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Credential Vault (OSEP-0012) brokers static credentials: callers deliver inline values, and the vault injects headers rendered once per revision. This proposal adds one dynamic building block. An **exchange credential** declares a header-resolver service — a local Unix socket or a remote HTTPS endpoint — and a binding whose auth references it resolves matched requests through that service. When a bound request arrives, the egress asks the resolver for fresh headers and injects the response. The resolver is a black box: the vault does not know how it is started, what identity scheme it implements, or how it caches. Exchange traffic inherits every OSEP-0012 policy invariant unchanged: binding match, egress alignment, ambiguity fail-closed, revisions, audit, redaction.

## Motivation

Two gaps remain after OSEP-0012.

First, identity-derived credentials (SPIFFE-compatible SVIDs, STS tokens, OAuth2 client-credentials grants) are short-lived and computed from request context — an audience derived from the destination, or a signature bound to the request's method, path, and headers. They cannot be delivered as a static value at vault creation and cannot be pre-rendered at revision time.

Second, sandboxes that need such identities today use ad-hoc mitm addons and per-sandbox signing daemons. Those paths bypass binding validation, egress policy alignment, ambiguity checks, audit, and fail-closed semantics, and they require the sandbox application to cooperate (for example by setting an opt-in header). Credential policy effectively leaks into application code.

### Goals

1. Add an `exchange` credential source: a declarative pointer to a header-resolver service, addressed by a URI (`unix://` socket or `https://` endpoint).
2. Add an exchange auth form on bindings — an auth that omits `type` and only references an exchange credential — which routes matched requests to that credential's resolver for per-request header resolution.
3. One transport-agnostic resolver contract; the egress proxies each request to the resolver and injects the returned headers.
4. Fail closed on resolution error or timeout; redact per request; never expose or persist exchanged material.
5. The subject is supplied by the egress on every resolution, so one resolver can serve many sandboxes; identical model in sidecar and fast-sandbox profiles.
6. Zero behavior change for vaults that use only `inline` credentials.

### Non-Goals

1. Becoming a general secret manager (unchanged from OSEP-0012).
2. Dictating resolver internals — how it is started, deployed, what identity scheme it implements, how it caches or rotates. The vault contract starts and ends at the resolver endpoint.
3. Putting credential scope or audience policy into the vault API; a resolver derives scope from request context itself.
4. Injecting at connection level (mTLS/X509-SVID); injection remains HTTP header substitution, as in OSEP-0012's MVP.
5. Adding new sandbox-facing lifecycle APIs; callers continue to use the existing egress Credential Vault API.

### Follow-ups (declared, non-blocking)

Declared out of v1 by the scope note; none of them changes the contracts in this document:

1. **Soak and performance matrices** — long-run stability (leaks, unbounded growth) and latency characterization of the per-request resolver hop across transports, profiles, and envelope bounds.
2. **Body signing** — extending resolver proofs from headers to request bodies needs a contract change (body digest in the resolve request) and different timing (the body must be hashed before resolution completes); header substitution remains the v1 surface (Non-Goal 4).
3. **Full OSEP-0023 integration depth and recovery/drain hardening** — exchange support in the live revision runtime (today rejected by the version-skew gate) and graceful drain of in-flight resolutions on shutdown; tracked in issue #2562 with explicit unblock conditions.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | A new `exchange` credential source carries a resolver endpoint URI (`unix://`, `https://`, or `http://` loopback-only); `inline` behavior is unchanged; unknown types are rejected | Must Have |
| R2 | Binding auth that omits `type` and references an exchange credential resolves headers per request through that credential's resolver | Must Have |
| R3 | Auth with a static `type` binds only to static sources; typeless auth binds only to exchange sources; cross-references are rejected at validation | Must Have |
| R4 | Resolution runs only inside the egress process trust boundary; sandboxes must not be able to obtain credential material by calling the resolver directly | Must Have |
| R5 | Resolution failure or timeout fails closed: 407 for non-streamed requests, kill for streamed requests | Must Have |
| R6 | The subject is derived by egress from connection observables (client IP) and passed to the resolver; no protocol surface accepts a client-declared subject | Must Have |
| R7 | Behavior is identical in sidecar and fast-sandbox profiles; the resolver contract carries the subject | Must Have |
| R8 | Vault reads, diagnostics, and logs never expose exchanged credential material | Must Have |
| R9 | Every header returned by a resolver is redacted per request — by name, by value, and by rendered variants — in response headers; resolver-supplied redaction hints are additive only, never authoritative | Must Have |
| R10 | Audit events record the credential (resolver) name, decision, and target host; never credential values | Must Have |
| R11 | The vault enforces only a resolution timeout; resolver-internal caching and refresh are invisible to the contract | Must Have |
| R12 | Exchanged material is never persisted to disk; restart recovery requires only policy | Must Have |
| R13 | Exchange binding hosts join the credential-bound TLS decrypt set (OSEP-0023) in both profiles | Should Have |
| R14 | The egress enforces a bounded resolver response envelope — total response bytes, header count, per-value length, and redaction count — before decoding; exceedance is a per-request denial | Must Have |

## Proposal

### Notes/Constraints/Caveats

1. **Additive by construction.** Sandboxes that use only `inline` credentials observe zero behavior change; static credential behavior is untouched.
2. **Resolver reachability is part of the trust model.** A sandbox that can mint credentials by calling the resolver directly bypasses the entire policy plane. `unix://` resolvers are scoped by socket permissions; `https://` caller authentication is **deferred** (see the resolver contract below) — until it lands there is **no egress-only boundary** for `https://` resolvers in the sidecar profile: transparent interception re-originates sandbox resolver dials from the same mitm uid as the egress's own, so a sandbox process that disables TLS verification can call the resolver and mint credentials for any subject (known R4 gap). `unix://` is the only resolver transport with a real trust boundary today.
3. **The subject is an opaque routing key, not a delegation.** The vault performs no identity delegation; the subject→identity authorization mapping belongs to the resolver's operator domain, which configures and audits it.
4. **Resolver outage means denial, never bypass.** An unreachable or slow resolver is indistinguishable from a policy denial for exchange bindings.
5. **Per-request latency is bounded.** The vault applies a fixed resolution timeout; whether the resolver served a cached token or performed a fresh signature is invisible to the contract.
6. **Version skew fails closed.** A runtime component that does not support exchange must reject a vault containing exchange bindings outright; a dynamic binding must never silently degrade into an empty static binding or a missing vault.
7. **One slow resolver affects only its matched requests.** Resolution is per request with a bounded timeout; unrelated traffic through the same egress is never blocked waiting on a resolver.
8. **Resolver output is bounded before decoding (R14).** The timeout alone is not a memory bound: a caller-operated resolver can answer immediately with an arbitrarily large body. The egress therefore enforces a response envelope budget — total response bytes, header count, per-value length, and redaction count — and treats exceedance as a per-request denial; a single subject cannot exhaust a shared egress through a resolver response.
9. **Resolver reachability under sidecar default-deny is an operator constraint, not an egress behavior.** The egress does not install exceptions for its own resolver dials. In the sidecar profile under `dns+nft` default-deny, locally re-originated dials traverse the OUTPUT policy, so the operator must provision resolver reachability for the egress process only — for example via the existing operator-provisioned UID-scoped exclusion mechanism used for upstream proxies, or equivalent host-level rules. Fast Sandbox imposes no OUTPUT enforcement for local dials, so once reachability is provisioned, behavior is identical in both profiles (R7). Resolver hosts must never be opened to the sandbox itself (never via the sandbox NetworkPolicy), which would violate R4.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Sandbox calls the resolver directly and mints credentials | Full policy bypass | `unix://` socket permissions; for `https://`, no boundary until caller authentication lands (deferred — known R4 gap under transparent interception) |
| Client forges subject identity | Cross-subject credential use | Subject never accepted from the addon protocol; resolved from client IP with the existing authoritative mapping |
| Exchanged values leak through reads or diagnostics | Credential exposure | Resolution output exists per request only; redacted by name and value; never persisted |
| Resolver outage breaks exchange-bound traffic | Availability regression | Documented as fail-closed semantics; operators monitor resolver health; static bindings are unaffected |
| Malicious or broken resolver returns oversized responses | Shared-egress memory exhaustion across subjects | The egress enforces a bounded response envelope and denies on exceedance (R14) |

## Design Details

### Credential and Binding Model

The OSEP-0012 split is preserved: a **credential** is a service definition (what to exchange with), a **binding** is a policy (when to exchange). One exchange credential can serve many bindings.

```mermaid
flowchart LR
    subgraph C["🗂️ Credential — service definition"]
        E["source.type: exchange<br/>endpoint: unix:///run/spi/spi.sock<br/>or https://resolver.internal"]
    end
    subgraph B["📋 Binding — policy"]
        M["match: hosts · paths · methods<br/>auth → credential (no type = exchange)"]
    end
    M -->|"per request, resolved by egress"| E
    classDef cred fill:#E6E6FA,stroke:#333,stroke-width:2px,color:darkblue
    classDef binding fill:#90EE90,stroke:#333,stroke-width:2px,color:darkgreen
    class C cred
    class B binding
```

### API

Endpoints are unchanged from OSEP-0012. Only schemas grow.

| Operation | Behavior |
|---|---|
| `POST /credential-vault` | Create the vault with credentials and bindings; activates the initial revision |
| `GET /credential-vault` (+ `/credentials`, `/bindings`) | Sanitized metadata; never returns values |
| `PATCH /credential-vault` | Atomic runtime mutation with optional `expectedRevision` |
| `DELETE /credential-vault` | Revoke the vault and all runtime material |

Schema delta:

```jsonc
{
  "credentials": [
    { "name": "gh-token", "source": { "type": "inline", "value": "..." } },       // static, unchanged
    { "name": "spi-signer",
      "source": { "type": "exchange", "endpoint": "unix:///run/spi/spi.sock" } }, // local resolver
    { "name": "corp-resolver",
      "source": { "type": "exchange", "endpoint": "https://header-resolver.internal" } }
  ],
  "bindings": [
    { "name": "github-read",
      "match": { "hosts": ["api.github.com"], "methods": ["GET"], "paths": ["/repos/*"] },
      "auth": { "type": "bearer", "credential": "gh-token" } },
    { "name": "internal-api",
      "match": { "hosts": ["*.internal.example.com"], "paths": ["/*"] },
      "auth": { "credential": "spi-signer" } }
  ]
}
```

Validation rules:

- Auth objects that declare a `type` (`bearer`, `basic`, `apiKey`, `customHeaders`) must reference static sources; auth objects without `type` are exchange references and must reference exchange sources. Cross-references fail with 400. `type` is omitted for exchange because the resolver produces the complete header set — there is no render shape to select, and the referenced credential's source already determines the behavior.
- The endpoint URI scheme must be `unix://`, `https://`, or `http://`; an `http://` endpoint must name a loopback authority (`localhost`, `127.0.0.1`, `[::1]`) and fails with 400 otherwise. Other schemes fail with 400.
- Header names returned by a resolver pass the same hop-by-hop rejection set as static injection.
- All existing rules apply unchanged: egress allow coverage, ambiguity fail-closed, `expectedRevision` optimistic concurrency, sanitized read responses.

Field semantics — the user-facing reading of the model:

| Field | Meaning |
|---|---|
| `source.type: "exchange"` | This credential is a resolver service definition, not a static value; validation cross-locks it against static auth types |
| `source.endpoint` | Where to exchange, per request. The URI scheme selects the transport: `unix://` is HTTP over a Unix socket, `https://` is TLS |
| `match` | When to exchange: the hosts, paths, and methods that trigger resolution |
| `auth.credential` (no `type`) | Which resolver to use when triggered; "when" and "where" are decoupled so one resolver can serve many bindings. No `type` field: the resolver produces the complete header set, so there is no render shape to declare |
| binding `name` | Audit identifier surfaced in audit logs only; never a metric dimension (tenant-provided names are unbounded) |

In short: `match` declares which requests, `endpoint` declares where to exchange, and the credential name welds the two together.

### Usage Demos

**Demo 1 — local signing resolver (UDS).** A SPIFFE-compatible signing service runs next to the egress and exposes a socket. One `POST /credential-vault` wires it up:

```jsonc
POST /credential-vault
{
  "credentials": [
    { "name": "spi-signer",
      "source": { "type": "exchange", "endpoint": "unix:///run/spi/spi.sock" } }
  ],
  "bindings": [
    { "name": "internal-api",
      "match": { "hosts": ["*.internal.example.com"], "paths": ["/*"] },
      "auth": { "credential": "spi-signer" } }
  ]
}
```

Sandbox code stays unchanged:

```python
requests.get("https://api.internal.example.com/v1/foo")
```

The request arrives at the upstream with fresh `Authorization` and signature headers. The sandbox process never sees or holds them. If the resolver is down, the request is denied (407) — never forwarded uncredentialed.

**Demo 2 — shared HTTPS resolver, one credential serving many bindings.** A centrally operated resolver behind TLS. The same credential backs different policies:

```jsonc
POST /credential-vault
{
  "credentials": [
    { "name": "corp-resolver",
      "source": { "type": "exchange", "endpoint": "https://header-resolver.internal" } }
  ],
  "bindings": [
    { "name": "team-a",
      "match": { "hosts": ["api.team-a.internal"], "paths": ["/*"] },
      "auth": { "credential": "corp-resolver" } },
    { "name": "team-b",
      "match": { "hosts": ["*.team-b.internal"], "methods": ["GET", "POST"], "paths": ["/v1/*"] },
      "auth": { "credential": "corp-resolver" } }
  ]
}
```

Both teams' hosts exchange through one service; per-sandbox identity selection happens inside the resolver, keyed on the subject the egress supplies.

**Demo 3 — runtime mutation.** Bindings can be added to a running sandbox atomically:

```jsonc
PATCH /credential-vault
{
  "expectedRevision": 7,
  "bindings": {
    "add": [
      { "name": "registry",
        "match": { "hosts": ["registry.internal"], "paths": ["/v2/*"] },
        "auth": { "credential": "corp-resolver" } }
    ]
  }
}
```

The response carries the acknowledged revision (8) and sanitized metadata. Reads never include values.

### Request Handling

The interaction view below is the complete implementation story: state reads (snapshot) and per-request resolution are separate interactions; exchange bindings carry only a marker until a request actually arrives.

```mermaid
flowchart TD
    A(["🚀 bound request arrives"]) --> C{"binding match"}
    C -->|"no match"| Z(["✅ ordinary egress, untouched"])
    C -->|"static (bearer, apiKey, …)"| S["inject pre-rendered headers<br/>(today's path, unchanged)"]
    C -->|"exchange"| R["resolve via credential endpoint"]
    R --> OK{"resolve ok?"}
    OK -->|"yes"| I["inject returned headers<br/>+ per-request redactions"]
    I --> FWD(["✅ forward"])
    S --> FWD
    OK -->|"no: error / timeout / captured binding invalidated"| F(["❌ fail closed:<br/>407 non-streamed · kill streamed"])
    classDef ok fill:#90EE90,stroke:#333,stroke-width:2px,color:darkgreen
    classDef proc fill:#87CEEB,stroke:#333,stroke-width:2px,color:darkblue
    classDef err fill:#FFB6C1,stroke:#DC143C,stroke-width:2px,color:black
    class Z,FWD ok
    class A,C,S,R,I proc
    class F err
```

```mermaid
sequenceDiagram
    autonumber
    participant App as 🧩 Sandbox app
    participant Egr as ⚙️ egress (vault + proxy)
    participant Res as 🔀 resolver (UDS or HTTPS)
    participant Up as 🌐 upstream

    App->>Egr: outbound HTTPS (transparent intercept)
    Egr->>Egr: binding match → exchange → subject from client IP
    Egr->>Res: POST {subject, credential, binding, request}
    Note over Res: identity scheme, signing, caching — all resolver-internal
    Res-->>Egr: 200 {headers, redactions}
    Egr->>Up: forward with exchanged headers
    Up-->>Egr: response
    Egr-->>App: response (exchanged values redacted by name and value)

    alt non-200 / timeout / captured binding invalidated
        Egr--xApp: non-streamed → 407 · streamed → kill (fail closed, never pass-through)
    end
```

Failure semantics: non-200 and timeout are per-request denials. Invalidation is scoped, not global. Each resolution is pinned to the opaque snapshot generation and subject under which it was captured, following the immutable-snapshot semantics of OSEP-0012 and OSEP-0023: an unrelated vault mutation — any patch that does not touch the captured binding — never denies an in-flight or already-completed resolution. A result is discarded and never injected when its captured binding is deleted or replaced, when the vault is deleted, or when its subject is replaced, including by a successor that reuses the same revision number. Denials never degrade to pass-through and never poison subsequent requests.

### Resolver Contract

The same JSON-over-HTTP contract applies to all transports: HTTP over a Unix socket for `unix://`, plain HTTPS for `https://`, and plain HTTP over loopback for `http://` (loopback authority only). This is the only surface a resolver implements.

HTTP framing: all transports issue `POST` with `Content-Type: application/json`. The request target is fixed at `/resolve`; an `https://` or `http://` endpoint URI may carry a path prefix (for example `https://resolver.internal/egress`), in which case the request target is that prefix joined with `/resolve` and `Host` is the endpoint authority. For `unix://` endpoints the request target is `/resolve` and `Host` is the literal `localhost`, which resolvers ignore. This removes route ambiguity between independent egress and resolver implementations.

```jsonc
// egress → resolver
{
  "subject": "<sandboxId>",    // authoritative, filled by egress; the sandbox's external ID, identical in both profiles
  "credential": "spi-signer",
  "binding": "internal-api",
  "destination": {             // validated outbound destination, not the client-controlled Host
    "scheme": "https",
    "authority": "api.internal.example.com:443",
    "sni": "api.internal.example.com"
  },
  "request": { "method": "GET", "host": "api.internal.example.com",
               "path": "/v1/foo", "headers": { } }
}

// 200 — "redactions" is optional and additive: the egress redacts every
// returned header (name, value, rendered variants) regardless of this field
{
  "headers": { "authorization": "Bearer ...", "x-signature": "..." },
  "redactions": { "authorization": ["Bearer ..."] }
}
```

The `destination` block reflects the validated outbound destination as verified by the egress (scheme, authority including port, and SNI), so a resolver can bind a proof to the connection identity and distinguish `http://host` from `https://host`. `request.path` carries the full request target — path plus query string (`/v1/foo?a=1`), exactly as the egress admitted it — so a resolver can bind signatures to the complete target; it is never a normalized or query-stripped path. Header names in the response are validated before injection.

**Subject identity.** The wire value is the sandbox's `sandboxId` — the external, lifecycle-level identifier — identical in both profiles, so a resolver operator provisions exactly one identity namespace keyed on the identifier callers already use. `subject` is an egress-internal concept; it never crosses the resolver boundary. The egress fills the `sandboxId` authoritatively: the sidecar egress holds the identity of the sandbox it serves; the fast-sandbox egress maps the connection's source IP through its internal subject registry, which is seeded from the same `sandboxId`. The raw client IP is never forwarded, and no protocol surface accepts a client-declared subject (R6).

Hard requirements on a resolver endpoint, none of which dictate an implementation:

| Requirement | Rationale |
|---|---|
| Reachable only by the egress process (socket permissions, or caller authentication for HTTPS) | trust boundary; prevents sandbox-side credential minting (R4) |
| Concurrent-safe, read-only | the egress issues parallel resolves |
| Responds within the vault resolution timeout | the vault fails closed beyond it |
| Stays within the egress-enforced response envelope (bounded bytes, header count, per-value length, redaction count) | shared-process memory safety; one subject cannot exhaust the egress through a resolver response (R14) |
| Per-request semantics | input includes request context; output differs per request |
| Absent/unreachable = denial | no fallback to uncredentialed pass-through |

Authentication material for `https://` resolvers is provisioned through operator configuration of the egress — origin-scoped, so a token is only ever presented to the endpoint it was configured for — never through the vault API and never into the sandbox. **Status: designed, not yet implemented** — the adopted design (an Ed25519 JWT minted by the egress and verified offline against a server-published JWKS) is specified in [Resolver Caller Authentication](#resolver-caller-authentication); until it lands, resolver dials carry no caller credential and the egress exposes no auth configuration. Interim posture: `unix://` endpoints (socket permissions) are the only resolver transport with an egress-only trust boundary; network-level scoping of `https://` resolvers (UID-scoped exclusions) unblocks the egress's own dials under default-deny but does **not** isolate the resolver from the sandbox, because transparent interception re-originates sandbox dials from the same uid. Treat `https://` resolvers as sandbox-reachable until caller authentication lands.

### Resolver Caller Authentication

Caller authentication for `https://` resolvers is a self-contained Ed25519 JWT minted by the egress and verified offline against a JWKS endpoint published by the lifecycle server. The credential proves "this resolve call originated from the egress process" without distributing any secret material to resolvers, applies uniformly to both profiles, and closes the R4 gap for `https://` resolvers at the application layer. `unix://` dials are unchanged: socket permissions remain their boundary. `http://` is accepted only for loopback authorities and follows the `https://` rule — the credential is attached to every TCP dial. This matters most in the sidecar profile, where the sandbox shares the egress's network namespace and a loopback resolver is sandbox-reachable: there the caller credential is the resolver's only boundary. In fast-sandbox the egress pod's loopback is not sandbox-reachable and the credential is defense in depth.

Every TCP resolve dial carries one header, `OpenSandbox-Egress-Credential: <compact JWT>`. Resolvers verify with standard JWT libraries and enforce two bindings: `aud` must name the resolver's own origin, so a credential observed by one resolver cannot be replayed to another; and JWT `sub` must equal the resolve document's `subject` — on mismatch the request is rejected with 401, and the attested `sub` is the sole authoritative subject for identity selection, never the body value alone. A resolver-side SDK utility wraps key fetch and caching so that integration is a few lines.

#### Credential Format

```jsonc
// header — only these parameters are legal; anything else is rejected
{ "alg": "EdDSA", "typ": "JWT", "kid": "pool-a:v2" }
// kid: sandboxId (+incarnation) in the sidecar profile, <pool>:<version> in fast-sandbox

// payload — six claims, no more
{
  "iss": "opensandbox",                            // platform identifier, matched against SDK config
  "aud": "credential-resolver@resolver.internal",  // valid at exactly this resolver origin
  "sub": "sbx-7f3a2c",                             // egress-authoritative sandboxId
  "iat": 1790000000,
  "exp": 1790003600,                               // iat + 3600 at most
  "jti": "kX9sT2vZ8qLm4NpR6wYb3A"                  // fresh 128-bit nonce on every mint
}
```

| Field | Rule |
|---|---|
| `alg` | pinned to `EdDSA`; the SDK hardcodes it and rejects anything else (including `none` and HMAC confusion) — no algorithm negotiation |
| `typ` | not required; if present it must be `JWT` |
| `kid` | must match the platform identifier grammar; off-grammar kids are rejected locally with no fetch |
| `iss` | must equal the SDK's configured issuer |
| `aud` | must equal the verifying resolver's own origin — cross-resolver replay dies here |
| `sub` | must equal the resolve document's `subject`; the attested claim is the sole authoritative identity |
| `iat`/`exp` | `exp - iat ≤ 3600`; `now ≤ exp + 30s`; `iat` more than the leeway in the future is rejected |
| `jti` | no resolver-side dedup — one credential is legitimately reused across its whole window; `jti` distinguishes mint windows inside the 12-minute rotation overlap |
| extras | unknown header parameters are rejected; unknown payload claims are ignored |

Deliberate omissions, recorded so nobody "helpfully" adds them later: no `nbf` (`iat` + `exp` + leeway already bound the window — one fewer clock comparison); no `scope`/`role`/tenant claims (authorization is the resolver operator's domain keyed on `sub`, per Non-Goals — roles in the token would move policy into the credential); and `jti` is not a replay defense (replay is bounded by `exp` and `aud`).

Serial form is the compact JWS: three base64url segments, ~250–300 bytes total (the Ed25519 signature is a fixed 64 bytes), carried in the single header. The resolver SDK needs exactly three configuration values: `jwks_base`, `expected_issuer`, and `expected_aud` (its own origin).

Rejected alternatives, recorded with reasons: per-origin static bearer tokens (secret distribution to resolvers — the original sketch); HMAC over the egress token verified by server introspection (per-credential online dependency on the control plane plus hand-rolled caching glue); credentials embedded in the endpoint URI (tenant-readable through vault read-back; breaks R4 and R6); and reuse of `OPENSANDBOX_EGRESS_TOKEN` or revision session tokens (both grant policy-plane powers far beyond resolver authentication — capability confusion).

#### Key Provisioning: Server Cooperation per Profile

| | Sidecar | Fast Sandbox |
|---|---|---|
| Signer granularity | one keypair per sandbox; dies with the sandbox | one keypair per pool; shared by the pool's egress instances |
| Generation | server, at sandbox creation | ops-runbook, as a versioned Secret `opensandbox-egress-signing-<pool>-<v>` (pool identity in the name — two pools in one namespace never collide) |
| Private-key delivery | env injection — the same channel as `OPENSANDBOX_EGRESS_TOKEN` | read-only Secret volume projected into the egress container (`signing.key` entry only; `OPENSANDBOX_EGRESS_RESOLVER_SIGNING_KEY_DIR`) |
| Public-key registration | sibling workload annotation / container label, written next to the egress-token entry | server reads `signing.pub` from the same Secret through an independent `secretRef` |
| JWKS shape | per-kid lookup; `kid` = sandboxId (+incarnation) | flat JWKS of pool keys; `kid` = `<pool>:<version>`, globally unique across pools and path-safe for the per-kid lookup |
| Key retention | server discards the private key after injection | server never touches any private key |

The fast-sandbox signing Secret is a separate object from the MITM CA seed (`opensandbox-mitm-ca-<v>`): independent version lines, rotation cadences, reader sets, and failure domains — entries are never bundled and no reference covers both. A missing or unloadable key fails only the exchange gate (vaults containing exchange bindings are rejected at write time); inline-only vaults, static bindings, and MITM keep serving in both profiles.

Provisioning is create-time only: the design adds zero new runtime interaction between server and egress. The only new runtime edge in the system is resolver → server JWKS fetch, once per key lifetime.

#### Interaction

```mermaid
sequenceDiagram
    autonumber
    participant Srv as 🖥️ lifecycle server
    participant Egr as ⚙️ egress (sidecar / fast-sandbox)
    participant Res as 🔀 resolver (https://)
    participant SDK as 🧰 resolver SDK (JWKS cache)

    Note over Srv,Egr: Provisioning — create time only, no runtime channel.<br/>Sidecar: server generates the per-sandbox keypair, injects the private key<br/>through the OPENSANDBOX_EGRESS_TOKEN env channel, records the public key<br/>in the workload annotation / container label, and discards the private key.
    Note over Srv,Egr: Fast Sandbox: ops-runbook versioned Secret,<br/>egress mounts signing.key read-only, server reads signing.pub via secretRef.

    Note over Egr: Background timer mints one JWT per<br/>subject and origin at startup and again at<br/>80% of the TTL — no request meets an expired credential.

    Egr->>Res: POST /resolve<br/>OpenSandbox-Egress-Credential: JWT
    Res->>SDK: decode unverified header, take kid
    alt kid cached
        SDK->>SDK: verify Ed25519 signature, iss/aud, exp (±30s leeway)
    else unknown kid
        SDK->>Srv: GET /jwks/{kid}
        Srv-->>SDK: JWK — public key only
        SDK->>SDK: verify and cache the key
    end
    Note over Res: sub is the egress-attested subject and must<br/>equal the body subject — per-sandbox authorization happens here.
    Res-->>Egr: 200 {headers, redactions}

    alt missing / invalid / expired credential
        Res--xEgr: 401
        Note over Egr: a 401 is a resolver rejection: per-request denial<br/>(407 non-streamed · kill streamed), never pass-through.
    end
```

#### JWKS Endpoint Security

The JWKS edge needs authentication in exactly one direction. The resolver does not authenticate itself to the server: JWKS serves public keys only, so the endpoint is public by design — the same posture as every OIDC provider's `/.well-known/jwks.json` — and requires only rate limiting. Requiring resolver registration here would reintroduce the static credential this design eliminates.

The direction that matters is the resolver authenticating the server, because a forged JWKS is a key-injection attack: feed a resolver your public key, then mint the matching JWTs. Five rules, all mandatory for SDK implementations:

- JWKS fetch runs over HTTPS with standard server-certificate verification, never disabled; deployments may additionally pin the platform CA.
- The JWKS base URL comes from operator configuration only, never from token contents; per-kid URLs are constructed solely from `kid`, which is a platform-controlled identifier (sandboxId, or `<pool>:<version>` — the delimiter is path-safe, so `kid` substitutes into the lookup path without escaping ambiguity). A `kid` that does not match the platform identifier grammar is rejected locally, before any fetch.
- The JWT header carries only `alg` and `kid`; `jku`, `x5u`, and embedded `jwk` header parameters are rejected on sight. Keys come from the configured endpoint and nowhere else.
- Unknown-but-well-formed kids get a bounded negative cache with per-peer fetch rate limiting: an unauthenticated caller spraying random kids must not amplify into lifecycle-server lookups.
- The positive cache is bounded too: cached keys are re-validated at most hourly; a key absent on refresh rejects that kid until it reappears. Without this, rotation could not remediate a compromised signer.

| Attack | Defense |
|---|---|
| MITM feeds the resolver a forged JWKS | TLS server verification |
| Token carries `jku` pointing at an attacker key service | header restricted to `alg` + `kid` |
| Enumerating other sandboxes' public keys | per-kid lookup requires the identifier, and public keys are not sensitive |
| JWKS endpoint flooding | rate limiting plus the server's internal cache |
| Spraying random `kid` values to force JWKS lookups | `kid` grammar rejected locally, negative cache, per-peer fetch limits |

#### Performance

- The resolve hot path adds one offline Ed25519 verification — tens of microseconds, zero network hops.
- Key fetch is cached with a bounded max age: a JWK is fetched on first use and re-fetched at most once per hour (aligned with credential TTL + leeway), which is what makes rotation an effective revocation — a retired kid disappears from JWKS and the next refresh rejects its credentials. Steady-state cost stays trivial: 10k active subjects ≈ a few refresh reads per second fleet-wide.
- The server is never on the resolve path; an internal 30–60s cache in front of annotation/label reads absorbs first-contact bursts.
- Server unavailability does not deny resolution for known keys — verification is offline. Only first-contact fetches fail, fail-closed for those subjects alone.
- Minting is per (subject, resolver origin), not per signer: the sidecar signer is one-to-one with its sandbox, and a fast-sandbox pool mints and caches one JWT per active subject per origin — lazily on first resolve, refreshed at 80% of the TTL. A vault may reference several resolver origins, and the origin-bound `aud` makes their credentials distinct. A pool hosting N subjects across R origins performs N×R signatures every ~48 minutes, still µs-scale work.

#### Rotation

- Credential: TTL 1h; the egress re-mints at 80% of the window (48min) so no request meets an expired credential. The superseded credential stays valid until its own `exp` (the 12min overlap bounds the replay surface of a captured credential); an egress restart mints a fresh `jti` immediately.
- Sidecar keys: sandbox recreation replaces the keypair; the annotation/label — and with it the JWKS entry — disappears with the workload. There is no cleanup path to forget.
- Fast-sandbox keys: rotation is the versioned-Secret runbook — create `-(v+1)`, add it to the server's signing-key reference list, update the template reference, roll the pool. The server references the current and previous versions concurrently and serves both kids in JWKS; the previous version is retired only after the rollout completes and `TTL + leeway` has passed, so credentials minted by not-yet-rolled instances stay verifiable throughout.
- TTL bounds replay of a captured credential (≤1h). Signer-key compromise is remediated by rotation plus the bounded key-cache refresh: acceptance of a retired kid ends within the refresh window. Resolver 401 spikes should page whoever owns the keys.

#### Security Properties

- R4 for `https://` resolvers closes at the application layer in both profiles: a sandbox process dialing the resolver directly cannot mint a credential and receives 401. The egress-only boundary no longer rests on network scoping alone.
- R6 unchanged: `sub` is the egress-derived sandboxId — attested by the signature instead of merely asserted by the protocol.
- No secret material reaches a resolver (public keys only); resolvers gain no platform capability; the vault API carries no authentication material, unchanged from OSEP-0012 invariants.

#### Test Additions

- Happy path on both profiles (annotation-backed and Secret-backed keys); unknown-kid fetch and cache; expired credential denied with leeway honored.
- Recreated sandbox invalidates the old kid (sidecar); dual-kid grace window across a version bump (fast-sandbox), with the previous key retained until rollout plus `TTL + leeway`.
- Binding rules: a credential minted for resolver A's origin is rejected at resolver B (`aud` origin binding); JWT `sub` not equal to the resolve document's `subject` is a 401; a `kid` off the platform grammar is rejected locally with no JWKS fetch; sprayed unknown kids hit the negative cache and per-peer fetch limits.
- Key cache: a cached JWK older than the max age is re-fetched on next use; a retired `kid` rejects credentials once its refresh window passes — rotation revokes within the bounded window.
- Format matrix: `alg` other than EdDSA, a wrong `typ`, off-grammar `kid`, and unknown header parameters are rejected; unknown payload claims are ignored.
- Missing or unloadable signing key rejects exchange vaults at write time while inline-only vaults, static bindings, and MITM keep serving.
- A sandbox process dialing the resolver directly receives 401 (R4 closure).
- Independence assertions: the signing Secret is distinct from `opensandbox-mitm-ca-*`, with no cross-mounts.

### Example Instantiations

A SPIFFE-compatible signing service behind a local socket is the first target deployment: the credential points at its socket, the service fetches SVIDs and signs proof headers per request. The same contract is served unchanged by a remote `https://` resolver, which allows one centrally operated signing service shared by many egress instances and sandboxes.

### Fast Sandbox Profile

- Behavior is identical to the sidecar profile; the subject lets one shared resolver distinguish sandboxes.
- Exchanged material is never written to disk; after an egress restart, bindings and resolver references reactivate from persisted policy and material re-resolves on demand.
- Exchange binding hosts join the credential-bound TLS decrypt set (OSEP-0023).

### Observability

Audit events gain fields, never values: `credential` (resolver name), `resolution` (`resolved | denied | timeout`), `target_host`, `subject`. Metrics use only closed outcome attributes (`resolution` outcome, latency buckets) — credential and binding names are tenant-provided and must never become metric dimensions, keeping time-series cardinality bounded per OSEP-0010; they live in audit logs only.

### Relationship to Prior OSEPs

- **OSEP-0012**: this proposal adds one source discriminator and a typeless auth form. Every policy invariant is inherited, not restated; the static path is untouched.
- **OSEP-0023**: exchange binding hosts feed the credential-bound TLS decrypt set; resolution failures deny at the HTTP layer exactly as static denials do.

## Test Plan

### Integration Tests

- Static path regression: inline-only vaults behave byte-for-byte as before.
- Exchange happy path with both transports (UDS and HTTPS): resolve → inject → upstream sees fresh headers.
- Fail-closed matrix: resolver down, timeout, and captured-binding invalidation each deny without pass-through.
- In-flight stability: an unrelated binding patch does not deny an admitted resolution; deleting or replacing the captured binding does.
- Response bounds: oversized and over-count resolver responses are denied per request while sibling subjects and unrelated traffic are unaffected (R14).
- Subject isolation: no protocol surface accepts a subject string; the resolver always receives the egress-derived subject, and the wire value is the sandbox's `sandboxId`, identical across profiles.
- Resolver access control: a sandbox process cannot obtain credential material by calling the resolver directly.
- Sidecar resolver reachability: with an operator-provisioned reachability exclusion, the egress-originated HTTPS resolver dial succeeds while a sandbox process dialing the same endpoint is denied.
- Redaction: injected dynamic values are removed from response headers by name and value; logs contain no values.
- Restart recovery: exchange bindings reactivate from persisted policy; no material on disk before or after.

### E2E Tests

- A SPIFFE-compatible signing service behind a UDS endpoint: sandbox process calls a bound host with no credential material and the request arrives with valid fresh headers.
- A remote HTTPS resolver shared across sandboxes: subject-derived identity selection works per sandbox.
- A sandbox that cannot reach the resolver receives denials for exchange bindings while static bindings keep working.

## Drawbacks

- Exchange bindings add a per-request resolver hop (bounded by the resolution timeout; resolvers cache internally).
- Exchange-bound availability now depends on resolver health; this is inherent to brokered identity but is a real operational change.
- HTTPS resolver caller authentication is new operator configuration to get right (deferred; not implemented yet).

## Alternatives

### App-Driven Header Signing (status quo for identity workloads)

Per-sandbox signing daemons plus mitm addons that the application opts into by header. Rejected as the general mechanism: no policy plane, no audit, application cooperation required, identity scope uncontrolled.

### Pre-Rendering Exchanged Material at Revision Time

Rejected: request-derived scope makes the material set unbounded before the request exists; per-request signatures cannot be computed pre-request; reuse of pre-rendered output would break proof-of-possession properties (the signature is bound to one request, so reuse is invalid by construction) and weaken fail-closed (stale material keeps flowing after resolver failure until TTL). A fixed-scope, non-signing token could be modeled as a static-with-rotation source — an allowed optimization for resolvers that choose it, not a replacement for resolution.

### Addon-Direct Resolver Calls

The addon would resolve directly against the signing service, fragmenting validation, revision consistency, redaction, and audit across processes, with no subject dimension. Rejected.

### Treat Identity Tokens as Inline Values with External Refreshers

Callers would run their own refresh loops and re-POST values. This reintroduces the plaintext-delivery path OSEP-0012 removed and loses per-request binding. Rejected.

## Infrastructure Needed

- Egress: `exchange` source type, typeless auth form, resolver proxy (UDS and HTTPS clients), and response envelope enforcement — no new component or image.
- Lifecycle server and spec: extend the `Credential.source` schema (today fixed to inline) and the binding auth union (today requires `type`) for the fast-sandbox relay path, including sanitization, server-side merge, and regenerated SDKs.
- Operator configuration surface for the resolution timeout, resolver caller-authentication key provisioning and JWKS publication ([Resolver Caller Authentication](#resolver-caller-authentication)), and the egress-side exchange capability gate.
- Documentation and one example per transport.

No new required external service is introduced by this proposal.

## Upgrade & Migration Strategy

The proposal is additive and opt-in. Sandboxes that use only `inline` credentials are unaffected.

1. Land the schema and resolver proxy behind an explicit egress-side exchange gate: the egress accepts vaults containing exchange bindings only when its resolver operator configuration (resolution timeout; caller-authentication table for HTTPS endpoints) is provisioned, and rejects them outright otherwise (fail closed). The lifecycle server needs no separate gate — it relays the extended schema, which also covers the fast-sandbox profile, which does not consult `credentialProxy.enabled`.
2. Extend the lifecycle server path (source/auth schema, sanitization, merge, regenerated SDKs) so exchange requests survive the fast-sandbox relay round-trip instead of failing server-side validation.
3. Validate against a local UDS signing service.
4. Migrate operator templates from bespoke per-sandbox signing addons to exchange credentials and retire those addons.
5. Document operator guidance: narrow binding scopes, resolver health monitoring, and sidecar resolver reachability provisioning under default-deny (egress-process-only access; never exposed to the sandbox).

Scope note: the first implementation targets the Fast Sandbox use case with one publicly testable resolver and explicitly rejects unsupported combinations (per the version-skew caveat above). Full OSEP-0023 integration depth, recovery/drain hardening, body signing, and soak/performance matrices are follow-up work and do not block this proposal.
