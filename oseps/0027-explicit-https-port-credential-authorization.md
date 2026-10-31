---
title: Explicit HTTPS Port Credential Authorization
description: Experimental authorization of exact HTTPS origin ports with identity, interception, compatibility, and lifecycle boundaries.
authors:
  - "@hpliStartAgain"
creation-date: 2026-10-20
last-updated: 2026-10-20
status: draft
---

# OSEP-0027: Explicit HTTPS Port Credential Authorization

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Maintainer decisions](#maintainer-decisions)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [API semantics and presence](#api-semantics-and-presence)
  - [Interception and network enforcement](#interception-and-network-enforcement)
  - [Origin identity and request routing](#origin-identity-and-request-routing)
  - [Matching and precedence](#matching-and-precedence)
  - [Static passthrough admission](#static-passthrough-admission)
  - [Later live-mode extension](#later-live-mode-extension)
  - [SDK compatibility and replacement guards](#sdk-compatibility-and-replacement-guards)
  - [Errors and observability](#errors-and-observability)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [Implementation Stages](#implementation-stages)
- [References](#references)
<!-- /toc -->

## Summary

Add an experimental `CredentialMatch.httpsPorts` field for exact HTTPS origin-port authorization without changing deprecated `ports`. Injection requires three independent conditions: network policy permits the destination, the runtime actually intercepts the port, and the binding authorizes the verified HTTPS origin. Omitted `httpsPorts` continues to mean HTTPS/443 only.

This is a documentation-only proposal, not implemented behavior or a release commitment. **0027 is a provisional number**, subject to maintainer assignment; `status: draft` does not imply acceptance. The proposed initial release covers `all` mode with Docker and Kubernetes sidecars, direct upstream connections, and HTTP/1.1 and HTTP/2, only after complete identity and network-enforcement acceptance. The later experimental live-mode design below is not a prerequisite for that release.

## Motivation

An internal service can resolve to a private address and expose HTTPS at `https://llm.corp.example:8443`. Extra interception alone must not authorize sending credentials. Users need to authorize a DNS name and exact HTTPS ports with existing method/path constraints, without authorizing another service on that name at 443 or 9443.

Source observations are pinned to [`0b6cdb286d38b145f3a69fecaa57b30046b03303`](https://github.com/opensandbox-group/OpenSandbox/tree/0b6cdb286d38b145f3a69fecaa57b30046b03303):

- Go accepts legacy `ports` values 80/443 and then clears them. They are not authorization filters: `schemes:[https], ports:[80]` still means HTTPS/443. Reinterpreting this input would change authorization. [S1]
- The Python matcher and encoded-path ambiguity protection both restrict requests to standard scheme ports. Both need consistent changes. [S2]
- Interception already uses `80,443 + OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS`. The iptables builder limits the total to 15 ports; the nft integer-list builder does not impose the same total limit. [S3]
- OSEP-0023 decision snapshot v1 derives a host-only TLS set; live request checks restrict ports to 443. Raw inbound H2 capture currently integrates with live, not generally with `all`. [S4, S5, S13]
- OSEP-0023 defers additional TLS ports. Its stated initial extra-port rejection does not establish a complete startup rejection in the baseline code: the live selector is host-only, using origin port for static 443 bypass, so extra-port traffic can reach decryption before request rejection. Actual port-aware selection and unsupported-combination validation are required. [S6, S10]

Historical work motivates, but does not approve, this design. [#1189](https://github.com/opensandbox-group/OpenSandbox/pull/1189) narrowed legacy `ports` and includes [discussion of a future end-to-end extension](https://github.com/opensandbox-group/OpenSandbox/pull/1189#issuecomment-4899427671). [#1379](https://github.com/opensandbox-group/OpenSandbox/pull/1379) added extra interception while leaving credential matcher/spec/SDK work separate. NetworkRule changes in [#2212](https://github.com/opensandbox-group/OpenSandbox/pull/2212), [#2301](https://github.com/opensandbox-group/OpenSandbox/issues/2301), and [#2302](https://github.com/opensandbox-group/OpenSandbox/pull/2302) concern a different contract. Legacy hardening in [#1758](https://github.com/opensandbox-group/OpenSandbox/issues/1758) and [#1759](https://github.com/opensandbox-group/OpenSandbox/pull/1759) needs coordination, not an assumed merge dependency. Upstream CONNECT from [#1816](https://github.com/opensandbox-group/OpenSandbox/pull/1816) needs separate acceptance for custom ports.

### Goals

- Authorize exact HTTPS ports for DNS names and existing wildcard selectors, preserving method/path, authentication, redaction, and streaming semantics.
- Preserve historical omitted-field HTTPS/443 and deprecated `ports`, while requiring strict identity for every new explicit binding, including `[443]`.
- Prove composition of network allowance, interception, and authorization on each advertised profile.
- Define safe creation, replacement, persistence, restore, SDK, and unsupported-profile behavior before exposure.
- Preserve OSEP-0023 atomic publication, exact ACK, revocation, and bounded drain in the later live extension.

### Non-Goals

IP-literal/CIDR credential identities; nonstandard plaintext HTTP ports; QUIC/HTTP3; CONNECT/nested-tunnel credentials; port ranges; automatic policy or interception changes; a NetworkRule domain-plus-ports contract; public capability/generation negotiation; or graduation of experimental OSEP modes. Private IPs may be DNS resolution results, not credential identities.

## Requirements

1. Omission authorizes HTTPS/443 only; explicit nonempty sets replace the default. Legacy `ports` never gains new authority.
2. Validate complete candidates against actual interception, mode/profile, static configuration, and runtime state on mutation, startup, and restore.
3. Non-443 credential requests and explicit non-null `httpsPorts` bindings must pass verified TLS/SNI, raw authority, immutable original destination, and final-route checks before credential resolution/injection/transmission. Failure cannot fall back to legacy matching.
4. Preserve presence in storage, readers, clones, rendering, APIs, SDKs, and persistence. Reject replacement that silently drops an existing explicit set.
5. Reject unsupported combinations before mutation. Not-ready alone does not prove blocked traffic; supported failure paths must demonstrate actual isolation or teardown.
6. Keep `httpsPorts` and the `EXTRA_PORTS` dependency experimental initially. Every advertised H1/H2/profile combination requires real origin-observed and network-enforcement evidence.

## Proposal

Use existing credential CRUD and interception/redirect infrastructure with a confirmed canonical interception set. Add no capabilities endpoint, client feature flag, public generation parameter, separate credential cache protocol, or automatic configuration mutation.

### Maintainer decisions

These choices are proposed, not community consensus:

| Decision | Proposed choice |
|---|---|
| Field | `httpsPorts`; omitted HTTPS defaults to 443; explicit sets replace it |
| Stability | Experimental field, SDK docs, examples, and `EXTRA_PORTS` dependency; graduation separately |
| Null/empty | New-create null means omission; empty fails; replacing explicit bindings requires a concrete nonempty set |
| Static passthrough | Host-wide conflict admission for new explicit bindings, without expanding live wire bypass |
| First release | `all`, Docker/Kubernetes sidecars, direct upstream, H1/H2 after full acceptance |
| Legacy identity | Preserve actual-443 omitted-field behavior; strict new-path checks do not depend on #1759 |
| Public interface | Existing CRUD/errors; no capability or generation negotiation |
| First-release dependencies | Disk vault v2, active reader, matcher, identity gate, interception/enforcement tests; live snapshot v2 later |

### Notes/Constraints/Caveats

| Mode/profile | Initial feature release | Conditions/later work |
|---|---|---|
| `all` + Docker sidecar + direct + H1/H2 | Proposed support | Strict identity, raw H2 capture, route freezing, real interception and port-policy E2E; SDKs released together |
| `all` + Kubernetes sidecar + direct + H1/H2 | Proposed support | Same egress chain plus deployment E2E, not only schema tests |
| `all` + Fast Sandbox | Reject new field initially | Later prove per-subject original port, effective rules, and isolation; preserve legacy 443 |
| Experimental live sidecar | Reject new field initially | Later tuple-aware selection/admission/revocation under existing experimental gates |
| Chained upstream proxy | Reject new field initially | Later prove origin versus proxy socket, CONNECT, certificate verification, and no direct fallback |
| Nonstandard HTTP, IP identities, QUIC | Unsupported | Outside this proposal |

Rejecting the new field in an unsupported combination must not invalidate historical 443 configurations. Maintainers may explicitly choose H1-only with H2 rejected, but this proposal retains H1/H2 as the first complete release target. Existing live H2 checks cannot simply be reused without independently validating raw identity capture and route freezing in `all`.

`all` does not become credential-bound TLS selection and does not gain live ACK/drain guarantees. OSEP-0023 profile parity also does not imply that `all` and live must have identical TLS decryption behavior.

### Risks and Mitigations

- **Cross-origin leakage:** freeze original identity and compare raw authority/SNI/final route; no fallback after failure. Shared certificates do not remove port isolation.
- **Post-redirect policy bypass:** prove original-port enforcement before/after redirect and on proxy-origin paths; reject unprovable combinations.
- **Client field loss:** preserve presence and enforce replacement guards; explicitly disclose that discarded initial-create intent is unrecoverable.
- **Operational interruption:** invalid reduced interception fails closed for the affected runtime/subject, with demonstrated isolation and controlled recovery, not a promise of lossless rollback.
- **Version skew:** coherent Go/system-addon images, startup/restore validation, preserved unknown disk formats, and no field-stripping retry.
- **Scope growth:** separate `all`, live, Fast Sandbox, chaining, and graduation while retaining full stage-specific acceptance.

## Design Details

### API semantics and presence

Example `CredentialMatch`:

```json
{
  "hosts": ["llm.corp.example"],
  "schemes": ["https"],
  "httpsPorts": [8443],
  "methods": ["POST"],
  "paths": ["/v1/*"]
}
```

| Input | Meaning/validation |
|---|---|
| Omitted `httpsPorts`, HTTPS applicable | HTTPS/443 only |
| `[8443]` | Only 8443; do not append 443 |
| `[443,8443]` | Both explicitly authorized |
| New-create null | Omission; no explicit-port marker |
| `[]` | Reject; SDK must not turn it into omission |
| Omitted/null replacing an existing explicit binding | Reject; restoring default requires `[443]` |
| Strings, floats, booleans, 0, negatives, greater than 65535, or 80 | Reject; excluding 80 is an initial scope choice, not a claim TLS cannot use TCP/80 |
| Duplicate/unordered ports | Deduplicate and sort ascending |
| More than 256 raw entries or 15 normalized ports | Reject; independently enforce both bounds |
| HTTP-only schemes and non-null field | Reject even `[443]`; null follows omission |
| HTTP and HTTPS with `[8443]` | HTTP remains 80 only; HTTPS is 8443 only |
| Port suffix or IP literal in `hosts` | Still reject; ports belong in `httpsPorts` |
| Legacy `ports:[80]`, `[443]`, or `[80,443]` alone | Continue accepting then ignoring |
| Nonempty old `ports` plus non-null new field | Reject conflict and instruct removal of old `ports`; generated `ports:[]` can retain old empty-field treatment |

The proposed per-binding 15-port bound is a portable product/resource choice, not an existing nft limit or permission to intercept 15 extra ports. Actual total interception limits include 80/443, and the subset check below further restricts bindings. The independent 256-entry bound prevents duplicates bypassing resource limits.

One helper, `effectiveHTTPSPorts(match)`, returns `{443}` for absent/null input when HTTPS applies, and empty when it does not. Matching and derived TLS views use effective sets, but GET/list must preserve explicitness rather than emit `[443]` for historical omission. Store, clone, render, active reader, and disk all retain presence. Strict gating uses that presence and actual origin port, not an inference from effective ports. SDK defaults must not insert the field into historical requests.

Go writes need `*[]int` or equivalent: nil omits, an empty slice pointer fails validation, a nonempty pointer serializes. `[]int,omitempty` erases empty input and is insufficient. Treat unknown fields differently in read and write models.

### Interception and network enforcement

Define `I = runtimeEffectiveInterceptPorts`: operator configuration normalized by the existing parser and confirmed by successful installation for the current runtime generation. It is not caller input, an unverified environment string, or a set that bindings automatically expand. This is a proposed trust requirement, not a property of the current configuration path.

At the baseline, `ALLOWED_EGRESS_ENV_VARS` includes `OPENSANDBOX_EGRESS_MITMPROXY_EXTRA_PORTS`, and `split_egress_env(request.env)` forwards it into Docker and Kubernetes sidecars when `networkPolicy` creates an egress sidecar. Before a profile can enable this credential-port feature, it must source extra interception ports exclusively from trusted operator deployment/server configuration, propagate that source through both provider paths, and reject request-env overrides, including equal-value overrides, before provisioning. A caller-controlled environment value cannot establish or expand trusted `I` merely because its rules were installed successfully. The operator configuration spelling and transport are implementation decisions; no new public capability protocol is required.

Scope this migration to activation of the custom-port-capable profile. Existing legacy profiles may retain their documented caller-supplied extra-interception behavior, but cannot accept the new explicit credential-port feature until migrated. Do not silently remove that behavior globally: any broader deprecation needs a separate maintainer decision. Migrating operators transfer intended ports to the trusted source, remove the request-env key, and recreate the runtime under the normal candidate/rule validation and isolation procedure. Test both provider paths for attempted caller addition, replacement, and equal-value override; all must reject before side effects in the migrated profile, while legacy behavior outside that profile remains covered.

Startup order: parse configuration; check profile; validate initial/restored candidates; install interception; initialize proxy with the same canonical `I`; confirm rule/subprocess generation coherence; for live install the initial acknowledged snapshot, or for `all` use its paired active-vault reader; then release readiness and workload egress. Authoritative-empty initial state is distinct from lost state.

Require `union(effectiveHTTPSPorts(bindings))` to be a subset of `I`. Create, PATCH, restore, restart replay, and complete-candidate policy validation use one validator. HTTP/80 constraints remain. Unintercepted 8443 bindings fail rather than silently bypassing the proxy.

`explicitAllowCoversHost` checks `pol.Evaluate(host)`, not full `(host,port)` reachability. Creation success does not guarantee DNS, routing, port policy, or certificate success. Do not extend NetworkRule domain-plus-ports here. In particular, #2212 is not proof of enforcement after transparent redirect: explicitly allow the domain but deny 8443 in real tests, exercising pre/post-redirect and proxy-origin paths, proving original-port enforcement and no credential receipt. Reject profile/policy combinations that cannot preserve it.

Initially `I` is fixed within a process/subject generation. Under the existing shared mutation barrier, validate vault/policy candidates against frozen `I`. Failure must not change vault, snapshot, rules, revision, or old drain deadlines. This does not add live ACK/drain to `all`.

Changing `EXTRA_PORTS` requires recreation. Remove dependent bindings through normal APIs before shrinking `I`; restoring into too-small `I` rejects startup/readiness, never silently trims ports. Later Fast Sandbox must confirm each subject's actual rules, not a process-wide maximum.

The fail-closed decision can interrupt the entire affected runtime/subject's egress. **Not-ready or process exit is not evidence of isolation or established-connection revocation.** Do not release initial workload egress before validation/rule confirmation. Restart/running-instance inconsistency requires verified profile-specific network isolation or teardown; without that proof the transition is unsupported.

Recover while isolated: restore previously verified interception and coherent image, normally reload original durable state, validate and confirm rules/proxy/readiness, then release traffic. Alternatively revoke dependent bindings through APIs while the old configuration works and recreate. Never instruct manual secret-file edits or state deletion to bypass validation. If old configuration cannot be restored, use controlled teardown/recreation and authorized credential restoration, reporting interruption without promising lossless rollback. Future hot changes require a coherent network/vault/decision transaction and are out of scope; restoring a config file does not roll back kernel state or connections.

### Origin identity and request routing

Identity is `(https, normalized DNS host, original destination port)`. SNI and certificate names remain DNS names without ports. Obtain trusted original IP/port before redirect/upstream rewrites, freeze `origin_port` in TLS connection context, and bind it to subject/runtime generation and normalized SNI. Never derive it from Host, `flow.request.port`, listener port, server peername, or upstream proxy socket.

Treat `context.server.address` as an acquisition point to validate against pinned mitmproxy semantics, not a universal original-destination API. Direct/transparent-rewrite regressions must prove it for the initial release; existing live H2 use does not prove `all` hooks leave it unchanged. Later DNAT/dispatch must preserve it. Chaining must distinguish origin 8443 from `proxy.example:3128`; unsupported opaque/chained combinations stay rejected. If original destination, final route, or TLS endpoint identity cannot be proved, reject the feature, not guess/default to 443.

Before any credential resolution, injection, or credential-bearing headers/streamed body can be sent:

1. Verify the TLS peer using admission SNI; reject `ssl_insecure`. Operator-trusted internal CAs are allowed, disabled verification is not.
2. Require raw authority DNS name = TLS SNI = frozen connection SNI. Live uses its token; `all` need not adopt the live token protocol.
3. Require authority's effective port = immutable origin port = final route's origin port, not proxy socket port.
4. Feed the same tuple into matcher, encoded-path ambiguity protection, and other non-path checks.

Apply this strict gate to every non-443 HTTPS credential request and every selected binding with explicit non-null `httpsPorts`, including `[443]`. Unknown original port cannot become guessed legacy 443. Decide the path before credential resolution; failure rejects without switching to a historical binding, old matcher, or forwarding an injected request. Only actual-443 omitted-field historical bindings retain legacy behavior and limitations. This does not solve all of #1758 or require #1759 to merge; SNI-in-binding alone is not the full host/port/route invariant. Existing verified `all` no-SNI passthrough does not excuse missing explicit new-path no-SNI validation.

Retain strict ASCII/punycode and existing case/single-trailing-dot normalization. Reject IP literals, whitespace/control bytes, userinfo, malformed/multiple ports, and leading-zero spellings such as `08443`; decimal ports are 1..65535. Do not invent a second Unicode normalization scheme.

- H1 requires exactly one Host. Absolute-form target scheme must be HTTPS and authority equivalent to Host. CONNECT/nested tunnels are unsupported.
- H2 validates raw inbound headers: unique `:scheme=https`, unique `:authority`, valid method; optional Host must be equivalent. Reject CONNECT/extended CONNECT, duplicate/missing pseudoheaders, illegal bytes. Do not use rewritten `pretty_host`.
- **Omitted authority port always means 443**, not the connected port. Bare hostname on 8443 rejects; bare hostname and `:443` on 443 are equivalent.
- H2 coalescing cannot inject across host or port. Bound authorities inside opaque connections remain invisible and receive no credential; request checks cannot repair opaque transport.

Operator addons remain trusted deployment code and `EnvMitmproxyScript` remains supported. Tenants cannot replace system scripts/install arbitrary operator addons. Trust does not remove route consistency: supported mutation hooks must freeze the validated destination, or revalidate immediately before send with no subsequent rewrite possible; otherwise reject the combination. Never inject then let a later addon redirect credentials.

Initial `all + H2` needs independently testable raw inbound capture and final-route validation beyond the live-only `next_layer` integration. Removing `_live_h2_authority`'s 443 check is insufficient. Target-changing, coalescing, and header-hook negative tests gate activation.

### Matching and precedence

Keep host exact/wildcard precedence, method/path/auth, redaction, streaming errors, and existing tie-breaks. Ports filter eligibility; single-port bindings do not outrank multi-port bindings. Go ambiguity/conflict checks must test effective `(scheme,port)` intersection before existing host/conflict rules, allowing otherwise-identical disjoint 443/8443 bindings.

Use one HTTPS membership helper in `_binding_matches`, `_path_is_ambiguous` non-path checks, shadow observation, and other derived standard-port checks. HTTP remains 80 only. Carry the field through active reads, snapshots, rendering, cloning, deletion, and list paths.

### Static passthrough admission

Reject any new explicit HTTPS binding whose host-selector language intersects operator static passthrough, regardless of port, method, or path. A tenant must not bypass operator intent via 8443. Use each mode's safe exact/wildcard intersection rules; live needs provable typed selector algebra, not probe hostnames.

This **host-wide admission rule** does not change wire bypass: `all` currently uses host-based SNI `ignore_hosts`; live typed static early bypass currently applies only at actual 443. Do not extend live bypass to 8443: extra ports still follow ECH/no-SNI/snapshot/tuple selection. Port-aware/all-port static configuration needs a separate semantics/migration decision.

Revalidate complete candidates for explicit-binding modification, whole-vault replace, policy/bootstrap/replay, including `[8443]` to `[443]` or host changes. Do not claim same-host different-port static/binding coexistence. Retain live native `ignore_hosts`/`allow_hosts` and upstream restrictions; add no static port API. Profile parity within a mode does not erase these `all`/live differences.

### Later live-mode extension

#### Decision snapshot v2

Introduce canonical decision snapshot **v2**, e.g. `tlsBindingSelectors:[{host:"*.corp.example",httpsPorts:[443,8443]}]`. Derive/sort/merge exclusively from HTTPS `fullRenderedBindings`; no independent selector mutation. Digest covers effective ports and full bindings; receiver recomputes selectors. Preserve payload/entry/byte bounds and secret-free errors. Upgrade Go sender, Python validator, TLS view, registry, and shared golden vectors together.

Versioning makes old receivers reject rather than ignore semantics. Coordinated upgrades can use v2 without custom ports. Transitional v1 reads, if needed, map hosts to 443 only, never all intercepted ports. Never send v2 to a v1-only receiver then strip fields to retry.

#### TLS selection and lifecycle

Preserve priority among subject/generation, ECH, no-SNI, static bypass, authoritative snapshot, selectors, and registry capacity. Selector evaluation uses trusted `(SNI,origin_port)`. Missing/invalid destination never qualifies for decryption. Known subject with no trustworthy snapshot must not fail open by guessing an unbound port; authoritative-empty is different. Binding 8443 must not decrypt 443 or vice versa. HTTP-only bindings do not enlarge TLS selectors. Registry-full bound tuples deny, not downgrade to opaque.

Tokens/registry retain fenced subject generation, SNI, origin port, and decision epoch; they cannot be reconstructed from client fields. Preserve atomic admission and epoch-race revalidation. Each H1 request/H2 stream obtains a current request handle before identity and complete binding matching.

| Mutation | Required behavior |
|---|---|
| Add 8443 | New TLS decisions decrypt after ACK; existing opaque 8443 stays opaque, requiring reconnect/pool retirement |
| Remove 8443 from `[443,8443]` | Fence retired 8443 new requests at commit, despite retained host; avoid unnecessary 443 retirement; symmetric for removing 443 |
| Restore default | Explicit `[443]` with full validation; omission/null on replacement rejects |
| Delete one binding while tuple still covered | Match against full new revision, not unconditional same-host close |
| Rotate credentials or narrow path/method | New requests use new revision; admitted requests follow existing bounded drain and cannot reacquire under retired revision |
| Remove/re-add tuple | Never revive fenced connections; H2 GOAWAY/new-stream rejection/final close retain validated transport ownership |

Pool keys distinguish subject/generation, scheme, normalized host, origin port, and necessary TLS/route context, not merely SNI or proxy socket. Do not add an unbounded opaque registry.

#### Publication and uncertainty

Publish full bindings and selectors atomically using prepare, commit, exact ACK/readback. Finalize Go state only after exact ACK. Definite prepare failure leaves old revision; uncertain commit reconciles the same attempt. Terminal uncertainty retains not-ready/sticky recovery, not a claimed rollback by restarting old configuration. These guarantees belong to live: `all` retains conditional active-vault reads through its paired reader, without per-revision ACK or decision snapshot v2 dependency.

### SDK compatibility and replacement guards

Synchronize lifecycle/egress OpenAPI, Pydantic, forwarding/runtime validation, and model/codec paths for all five official SDKs: Go, JavaScript/TypeScript, Python, Kotlin/Java, and C#/.NET. Old server `extra=forbid` and egress `DisallowUnknownFields` reject fields that reach them. [S8] They cannot detect intent already dropped by Python converters, Kotlin codecs, Go typed decoding, or JS sanitizers. [S9] Old-SDK dict input is unsupported; use upgraded typed SDKs or raw HTTP to the strict API. Public preflight cannot fix clients that do not call it.

New SDK writes strictly validate values, aliases, and empty arrays. Response reads should tolerate ordinary additive fields: preserve unknown fields with safe write-back constraints or mark unknown-semantic responses as not directly writable. Never silently project a response into a lossy replacement. Old SDKs may incompletely display new bindings and must not modify them.

C# is part of this synchronized rollout, not a deferred client: update the handwritten `CredentialMatch` in `sdks/sandbox/csharp/src/OpenSandbox/Models/Sandboxes.cs`, request/response serialization, SDK documentation, and tests. Its writes and reads must preserve the same omitted/null/empty/explicit distinctions, with normalized response round trips and safe replacement behavior.

The CLI is also a feature-release dependency. Its current `opensandbox>=1.1.1,<2.0.0` dependency permits field-unaware Python SDKs, and `osb credential-vault create/patch` passes JSON/YAML dictionaries through typed SDK conversion. Raise the CLI minimum to the first released Python SDK that preserves `httpsPorts` on all relevant write/read paths, publishing that version only when assigned rather than inventing one here. Before connecting or sending any HTTP request for a payload containing the new field, the updated CLI must check its installed SDK compatibility and reject unsupported versions with upgrade guidance, including environments installed with dependency checks bypassed. Do not strip the field or retry as omission. CLI create/PATCH tests must capture the actual serialized wire body for JSON and YAML, explicit 8443, null, empty arrays, add/replace operations, and read-back/replacement; incompatible-SDK tests must prove rejection before HTTP. Empty input must reach validation or fail locally, never become omitted/default-443 authorization. Previously released CLI/SDK combinations remain unsupported for this feature and require upgrading both.

Server-side, client-independent guards:

1. Freeze pre-mutation state and identify IDs with explicit non-null `httpsPorts`.
2. If such an ID survives in the final candidate and is completely replaced/reconstructed, input must contain a concrete nonempty array. Omitted/null returns existing 400; `[443]` explicitly restores default.
3. Cover whole-vault replace, `bindings.replace`, full-object PATCH, and same-ID delete/add within one atomic mutation. An earlier delete step cannot erase frozen presence.
4. Genuine partial updates not replacing match preserve the explicit set. Classify actual operation semantics, not a full replacement labeled partial.
5. Deletion alone is revocation. Separate delete and recreate requests, including whole-vault deletion, cannot recover stripped initial-create intent; disclose that unsupported flow, without sticky feature flags.

Responses normalize ports and preserve presence. Client read-back is diagnostic, not a repair for wrong authorization. Live success requires exact ACK; `all` does not gain those revocation semantics.

No public capabilities, feature token, or generation parameter is needed. Under its mutation lock, egress validates mode/profile, `I`, static configuration, candidate, and runtime state. Optional future read-only GET diagnostics are neither authority nor a release dependency. Internal state cannot switch unhandled between validation/publication, but process generation need not be exposed to SDKs.

### Errors and observability

Retain egress `http.Error` plaintext/status and existing server/SDK mapping; no `HTTPS_PORT_*` public symbols or new error envelope.

| Failure | Result |
|---|---|
| Invalid/empty, HTTP-only, old/new conflict, missing replacement ports | Existing 400 with field and bounded reason |
| Unintercepted port, unsupported profile, static conflict | Existing 400 binding validation; no automatic environment change/downgrade retry |
| Stale `expectedRevision` | Existing conflict semantics |
| Unknown live receiver/uncertain ACK | Existing sanitized 503/recovery, not proof of isolation |
| New-path identity failure | Reject H1/close as necessary; validated fail-closed H2 behavior |
| Certificate failure | No injection or credential transmission |

Errors/logs include necessary fields/ports/bounded reasons, never credentials, snapshots, headers, proxy passwords, or tokens. Avoid unbounded host/arbitrary-port metric labels.

## Test Plan

These are **future implementation acceptance requirements**, not results from this documentation PR. Use controlled TLS origins and independent certificates, recording actual origin credential receipt. HTTP 200/proxy logs alone are insufficient. Later live/Fast Sandbox/chaining tests gate their own stages, not initial `all`.

| Area | Cases | Required evidence |
|---|---|---|
| Compatibility | Omission, legacy 80/443, explicit 443, 8443, 443+8443 | Historical input only 443; explicit 8443 never expands to 443 |
| Schema/presence | Empty, create-null, replacement-null, float/bool/string, zero/65536, duplicates/bounds/conflicts | Consistent validation, no publication on failure, empty never omitted |
| Transport | 443/8443/9443, TLS-on-80 rejection, plaintext 8443, unintercepted 8443 | Only authorized/intercepted actual HTTPS gets credentials |
| Identity | H1/H2 authority/Host, absolute targets, omitted/explicit ports, duplicate/missing/invalid headers, IP/userinfo, spoofed SNI, bad cert | No wrong-origin credential, omission=443, no legacy fallback |
| Routes/addons | Initial transparent targets; later DNAT/proxy3128-to-origin8443; target-changing hooks, coalescing | Frozen origin/final route/TLS agree; no post-check rewrite or cross-host/port injection |
| Matching | Exact/wildcard, path/method, encoded slash/dot, overlap/disjoint ports | Matcher/path protection share port semantics; no false disjoint-port conflicts |
| Static | Intersecting/nonintersecting exact/wildcard at 443/8443 | Host-wide binding rejection without wider live bypass |
| Network | Domain allowed, 8443 denied; redirect/proxy-origin paths | Real packets prove original-port enforcement and no credential receipt |
| Operator configuration | Docker/Kubernetes caller EXTRA_PORTS addition/replacement/equal-value override; trusted-source propagation; legacy profile | Migrated profile rejects before side effects; installed `I` derives from operator config; legacy profile cannot activate the new feature |
| CLI and C# | CLI JSON/YAML create/PATCH wire capture, SDK minimum/pre-HTTP guard, C# presence/serialization/read-back | No explicit/empty field loss; incompatible CLI SDK fails before HTTP; all five SDKs meet the same contract |
| Startup/restore | Empty vs missing state, reduced `I`, mixed versions, install failure, restart mismatch | Egress withheld until coherence; actual isolation/teardown demonstrated |
| Replacement | Whole vault, bindings.replace, full PATCH, true partial, same-mutation delete/add, delete | Frozen-state guards preserve presence and allow revocation |
| Versions | Old SDK/new stack, new SDK/old server or egress, lossy replace, disk v1/v2, later live v1 receiver | Explicit limits, no silent 443 conversion, unknown durable version preserved |
| Live selection (later) | Same host/separate ports, HTTP-only, ECH/no-SNI, missing snapshot, full registry | Tuple-only decrypt and preserved opaque/deny boundaries |
| Live mutation/failure (later) | Create/PATCH/delete/replay, concurrent policy, port-only removal, prepare rejection, lost ACK, uncertain readback, termination | Atomic candidate/ACK, tuple fence, failure-stage-correct recovery |
| Connections | Keepalive, H2 streams, opaque add, remove/re-add, coalescing | No cross-port/subject reuse; reconnect old opaque; live drain only where promised |
| Profiles | Docker/iptables, Kubernetes, later Fast Sandbox/nft subject isolation | Installed-rule and origin evidence per advertised profile; unsupported rejects |
| Hygiene | Errors/logs/metrics, redaction, snapshot entry/byte bounds | No secrets or unbounded labels/sets |

Mandatory regression: two services share a name/certificate on 443 and 8443 with different credential expectations. Authorize one at a time; the other never receives its credential even if intercepted. Later port-only live mutation must revoke the correct tuple despite unchanged SNI; this does not promise live drain in `all`.

Acceptance requires baseline regressions, unit tests, shared Go/Python goldens, real TLS/H1/H2 integration, and network interception E2E for every advertised mode/profile. Pre-commit failure has no state effects; post-commit uncertainty is not success/rollback. Operator docs distinguish network permission, interception, authorization, CA, and authority port, including successful 8443 and unintercepted failure examples. OSEP status reflects only completed increments, not graduation of all experiments.

## Drawbacks

This is more than removing `port != 443`: presence-sensitive APIs, durable versioning, replacement guards, raw identity/routing integration, and real enforcement/isolation proofs remain substantial. Static conflicts and interception recreation reduce flexibility. This proposal neither fixes every legacy omitted-field 443 limitation nor reconstructs old-client lost intent. Splitting live work does not establish a fixed effort estimate or schedule.

## Alternatives

- Reuse `ports`: fewer fields, but incompatible historical authorization, so rejected.
- Scheme-to-ports map: general, but prematurely designs nonstandard HTTP.
- Full origins array: expressive, but expands host/path/binding migration beyond this increment.
- Matcher-only widening: omits identity, path protection, interception, persistence, SDK, and revocation; unsafe.
- H1-only initial release: possible explicit maintainer decision with updated matrix/H2 rejection, not this proposal's target.
- Public capabilities: do not recover client-side field loss and add a protocol; use locked internal validation instead.

## Infrastructure Needed

Reuse redirect/parser infrastructure, CRUD, later-live revision machinery, and coherent images containing Go plus fixed system Python addons. No new public service or mandatory credential cache. Acceptance needs controlled TLS origins/certificates, Docker/Kubernetes sidecars, real iptables/nft enforcement and failure-isolation fixtures, and Go/Python golden vectors. Later add per-subject Fast Sandbox and CONNECT fixtures. Pin actual mitmproxy metadata/hook semantics rather than assume parity across modes.

## Upgrade & Migration Strategy

Disk **vault v2** is independent of decision snapshot v2 and IPC envelope versions. Read v1 as omitted/default-443 state; after writing any new field persist v2 with presence. Preserve existing rejection/preservation of unknown disk versions, not corruption handling or silent field loss. [S11] Initial `all` requires disk v2/active reader, not live snapshot v2.

Only paired Go/system-addon images are supported. System scripts stay fixed-loaded and operator scripts retain trusted-deployment handling. Startup/deployment mismatches must not release workloads; no public negotiation for manually mixed binaries/scripts. Publish minimum CLI/SDK/server/egress versions and verify field-retaining active reads and old-end rejection. Update both specs, forwarding, all five SDKs, CLI dependency/compatibility checks, docs, and examples together. Operators activating the feature must complete the scoped trusted-configuration migration above; legacy request-env profiles remain ineligible until migrated.

Before downgrade, revoke new-port configuration through APIs. Live requires ACK and drain; `all` requires controlled teardown to eliminate retained connections. Removing non-443 authorization does **not** make v2 readable by old software; explicit `[443]` remains new-format state. Initial downgrade supports controlled runtime/vault recreation from reviewed legacy-only configuration, not automatic v2-to-v1 conversion. Old software rejects/preserves v2; no hand-editing secret files or field-dropping conversion. Credential-source resolution/caching protocols stay unchanged.

`EXTRA_PORTS` is experimental and may change/disappear. [S12] Field/SDK/examples retain that boundary. Stability requires a separate decision on environment configuration, server propagation, Helm/deployment contracts, and support commitments; a public field does not implicitly graduate its dependency.

## Implementation Stages

This PR adds design and indexes only. Proposed later increments:

1. **Design/experimental contract:** agree on decisions, legacy boundary, replacement guards, and shared input vectors; no upstream schedule assumed.
2. **`all` foundations:** presence/normalization, complete candidate/replacement guards, disk v2, active reader, immutable origin, raw H1/H2/final-route validation, canonical operator-sourced `I` with scoped request-env migration and provider rejection tests, real policy/isolation tests. Foundations can split, but field activation waits for the full chain. No live snapshot/token/registry dependency.
3. **Experimental `all` release:** synchronize API, all five SDKs, CLI SDK minimum/pre-HTTP guard and wire-retention tests, docs, and accepted Docker/Kubernetes direct H1/H2 profiles; reject unsupported mutations and document coherent versions without public negotiation.
4. **Experimental live increment:** snapshot v2, tuple selectors, origin-port tokens, per-request admission, pools/revocation/drain, ACK fault tests under existing gates.
5. **Extensions/graduation:** separately validate Fast Sandbox/chaining, then decide `EXTRA_PORTS`/`httpsPorts` stability with deployment/support contracts.

Tests/errors/docs accompany each increment. Relevant surfaces are both OpenAPI files; Go credentialvault models/candidates/snapshot/vaultfile and interception/launch code; Python `system.py`, active reader and live decision/registry/publication modules; server schema/forwarders; all five SDK handwritten/generated models/codecs (including C# serialization); CLI dependency, create/PATCH commands and tests; operator-only extra-port configuration/provider plumbing; credential-vault guide and OSEP-0023 acceptance records. Implementation must inventory remaining derived checks/generated paths; this proposal changes no runtime files.

## References

These links pin source observations; proposed requirements are not claims of current support.

- [S1: Legacy normalization](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L590-L644).
- [S2: Matcher/path ambiguity](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L1020-L1096).
- [S3: Interception parser](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/constants/mitmproxy_ports.go#L23-L95).
- [S4: Snapshot v1](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/decision_snapshot.go#L29-L105).
- [S5: H1/H2 authority](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L1394-L1525).
- [S6: OSEP-0023](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/oseps/0023-credential-bound-tls-interception.md).
- [S7: Legacy ports contract](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/specs/egress-api.yaml#L620-L653).
- [S8: Server strict model](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/server/opensandbox_server/api/schema.py#L344-L365) and [Go strict decoding](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L1340-L1350).
- [S9: Python model](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/sdks/sandbox/python/src/opensandbox/models/sandboxes.py#L484-L500), [Kotlin codec](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/sdks/sandbox/kotlin/sandbox/src/main/kotlin/com/alibaba/opensandbox/sandbox/infrastructure/adapters/service/CredentialVaultJsonCodec.kt#L109-L115), and [active reader](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L649-L812).
- [S10: Live selector](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/tls_decision.py#L146-L157) and [registry](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/tls_registry.py).
- [S11: Disk version loading](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vaultfile.go#L98-L144) and [binding conflicts](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L1245-L1252).
- [S12: EXTRA_PORTS experimental status](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/constants/configuration.go#L117-L120) and [addon loading](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/mitmproxy/launch.go#L82-L105).
- [S13: all active-vault path](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L1635-L1663), [live-only raw H2](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L397-L410), and [all SNI ignore](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/mitmscripts/system.py#L377-L392).
- [S14: Whole-vault replace](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L298-L323), [bindings.replace](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L1016-L1028), and [host-only policy coverage](https://github.com/opensandbox-group/OpenSandbox/blob/0b6cdb286d38b145f3a69fecaa57b30046b03303/components/egress/pkg/credentialvault/vault.go#L1112-L1124).
