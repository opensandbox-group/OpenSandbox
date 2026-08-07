---
title: Exact Request-Header Selectors for Credential Binding Disambiguation
authors:
  - "@andreweacott"
creation-date: 2026-08-06
last-updated: 2026-08-07
status: draft
---

# OSEP-0023: Exact Request-Header Selectors for Credential Binding Disambiguation

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
  - [Data model and API](#data-model-and-api)
  - [Matching and selection](#matching-and-selection)
  - [Candidate validation](#candidate-validation)
  - [Privacy and observability](#privacy-and-observability)
  - [SDKs, CLI, and documentation](#sdks-cli-and-documentation)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Add optional exact request-header selectors to Credential Vault bindings. They allow multiple bindings with the same destination scope only when their selectors prove that a request cannot match more than one binding. This is an additive extension to OSEP-0012's fail-closed binding model; it does not introduce a general request-policy language or precedence rule.

## Motivation

OSEP-0012 selects credentials by scheme, host, method, and path. If two bindings match the same request at the same host precedence, Credential Proxy fails closed rather than injecting an arbitrary credential. That remains the correct default.

Some legitimate clients need two credentials for the same destination shape. Two logical environments can share an API host and path while using different API keys, or two package clients can use one private registry endpoint with different authentication forms. Those clients can already send distinct non-secret placeholder values before Credential Proxy injects the real credential. Credential Vault cannot currently use that distinction.

### Goals

- Allow a binding to require exact values for one or more existing request headers before it is eligible for credential injection.
- Preserve OSEP-0012's fail-closed behavior when multiple bindings match.
- Keep selector values out of public read APIs, logs, metrics, diagnostics, and error responses.
- Define one wire contract for egress, supported SDKs, CLI output, documentation, and conformance tests.
- Preserve behavior for every existing binding without request-header selectors.

### Non-Goals

- Prefix, wildcard, regular-expression, or case-insensitive value matching.
- Selectors over query parameters, request bodies, cookies, or process identity.
- Generic-binding override precedence or any other implicit fallback rule.
- Per-process authorization inside a sandbox.
- Automatic migration of existing bindings.

## Requirements

| ID | Requirement | Priority |
| --- | --- | --- |
| R1 | `CredentialMatch` may contain one to four AND-combined exact request-header predicates. | Must Have |
| R2 | Header names match case-insensitively. Configured values and request values match case-sensitively after trimming only outer HTTP optional whitespace (SP and HTAB). | Must Have |
| R3 | Each selected request header must occur exactly once; missing or repeated header fields do not satisfy a predicate. | Must Have |
| R4 | Selectors permit ordinary end-to-end headers, including `Authorization`, but reject HTTP/2 pseudo-headers and these case-insensitive names: `Host`, `Content-Length`, `Content-Type`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`, `Trailer`, `Cookie`, `Proxy-Authorization`, `Proxy-Authenticate`, `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto`. The egress API contract is the maintained source for this fixed denylist; changes are reviewed public-contract changes. | Must Have |
| R5 | Bindings whose destination scopes can match a request at the same host precedence are valid only when selectors prove they cannot both match that request. | Must Have |
| R6 | Public reads return selector header names and `valueConfigured: true`, never selector values. | Must Have |
| R7 | `CredentialMatch` keeps `additionalProperties: false`; runtimes reject unknown properties such as `requestHeaders` rather than silently ignoring them. | Must Have |
| R8 | Selectors are non-secret routing hints, not an authorization boundary; any process in the sandbox can construct a selector that chooses any eligible binding for a destination it can reach. | Must Have |

## Proposal

Extend `CredentialMatch` with `requestHeaders`:

```yaml
match:
  schemes: [https]
  hosts: [registry.example.com]
  methods: [GET]
  paths: ["/packages/*"]
  requestHeaders:
    - name: Authorization
      value: "Bearer placeholder-client-a"
```

Existing scheme, host, method, and path checks determine the highest matching host precedence before selectors are evaluated. A binding at that precedence is eligible only when every `requestHeaders` predicate matches the original intercepted request. Credential Proxy injects a credential only when exactly one binding is eligible.

When no binding is eligible at the highest base host precedence, current behavior is preserved: inject no credential and let ordinary egress policy decide whether traffic may continue. Credential Proxy does not fall back to a lower-precedence binding. Multiple eligible bindings retain the current ambiguous-binding denial.

### Notes/Constraints/Caveats

- A selector is a routing hint, not authentication. Any sandbox process can construct one, so it cannot restrict a process to a subset of credentials.
- Matching occurs before injection or replacement. A matching placeholder `Authorization` header is subsequently replaced when the selected binding injects `Authorization`.
- Callers retain desired selector values; public read APIs intentionally cannot reconstruct selector-bound bindings.
- This proposal does not widen transparent interception ports or alter egress policy coverage requirements.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| A selector value leaks. | Treat it as non-secret, write-only input and exclude configured values from public reads, logs, metrics, diagnostics, and errors. Documentation may use clearly fictional placeholders only. |
| Proxy and upstream interpret repeated headers differently. | Count received header fields before coalescing, require exactly one field, and reject routing, framing, hop-by-hop, proxy-control, forwarding, and explicitly out-of-scope `Cookie` names. |
| Two bindings select different credentials for one request. | Preserve fail-closed ambiguous matching and allow overlap only when static validation proves predicates disjoint. |
| A mixed egress/SDK deployment broadens a binding. | Credential Vault has used strict unknown-field decoding since its initial implementation. Retain that behavior, test it against an older supported runtime, and release runtime support before SDK helpers. |
| Strict validation rejects a useful future policy. | Keep exact conjunctions narrowly scoped; predicate kinds or precedence require a separate proposal. |
| A sandbox process spoofs another selector value. | Treat all selector-bound credentials as shared within the sandbox trust boundary. Documentation and UI must not imply per-process authorization; solving that requires a separate proposal. |

## Design Details

### Data model and API

Extend the existing `CredentialMatch` schema in place. Both `CredentialBinding.match` and `CredentialBindingMetadata.match` continue to use `CredentialMatch`, preserving the existing public property type in supported SDKs. The selector schema models both directional representations:

```yaml
CredentialMatch:
  properties:
    requestHeaders:
      type: array
      minItems: 1
      maxItems: 4
      items: {$ref: "#/components/schemas/RequestHeaderSelector"}

RequestHeaderSelector:
  type: object
  required: [name]
  properties:
    name: {type: string}
    value: {type: string, writeOnly: true}
    valueConfigured: {type: boolean, enum: [true], readOnly: true}
  oneOf:
    - required: [value]
    - required: [valueConfigured]
  additionalProperties: false
```

The `oneOf` rejects representations that contain neither directional field or both directional fields. It does not choose the correct operation direction. Create and patch handlers require `name` and `value` and reject `valueConfigured`; get and list serializers require `name` and `valueConfigured` and must never emit `value`. Operation-level schema and conformance tests enforce those directional rules.

`name` must be a non-empty HTTP field-name token as defined by RFC 9110; it is not whitespace-trimmed. On input, the server trims only outer SP and HTAB from `value`, then rejects an empty result and stores the trimmed value as the canonical value used for matching and ambiguity validation. A binding must not repeat a header name after case-insensitive normalization. These rules prevent unsatisfiable or semantically duplicate predicates.

Credential Vault creation and `bindings.add` submit complete bindings. `bindings.replace` also replaces a complete binding, including its complete `match`; omitting `requestHeaders` from that replacement means the replacement binding has no selectors. Omitting `bindings` from a mutation, or omitting a binding from its `add`, `replace`, and `delete` sets, leaves that existing binding unchanged. There is no field-level binding merge or per-selector add/remove operation.

Public binding reads return a sanitized metadata shape:

```yaml
requestHeaders:
  - name: Authorization
    valueConfigured: true
```

The egress sidecar's private active snapshot retains canonical values for matching; public binding metadata never does. Adding `requestHeaders` must not change the type of `CredentialBindingMetadata.match` in any supported SDK.

### Matching and selection

Credential Proxy uses this selection order:

1. Evaluate scheme, host, method, and path for every binding and record each base match's host precedence.
2. Select the highest host precedence among those base matches and discard lower-precedence bindings.
3. Evaluate every normalized `requestHeaders` predicate only for bindings at that precedence:
   - Header names compare case-insensitively.
   - A selected header must have exactly one received field occurrence before library coalescing. Two field lines do not match; one field line containing a comma remains one occurrence whose entire value is compared.
   - After trimming only outer SP and HTAB, the request value must exactly equal the canonical configured value.
   - Values otherwise receive no case folding, decoding, internal-whitespace normalization, prefix matching, or expression evaluation.
4. Zero eligible bindings yields no injection. Exactly one is selected. Two or more retain the current ambiguous-binding denial.

Selector count is not a precedence dimension, and a selector miss never falls through to a lower host precedence.

### Candidate validation

Vault creation and mutation validate the complete post-mutation binding set. For any pair of bindings whose base scopes can match the same request at the same host precedence, both must declare selectors, and they may coexist only if they share a normalized header name whose canonical configured values differ, proving no request can satisfy both conjunctions. Any other such pair is rejected. Two corollaries follow: a selector-bound binding can never overlap a generic binding at the same host precedence, and predicates on different header names never prove disjointness because a single request can carry both headers. Exact-host and overlapping wildcard-host bindings remain valid without selectors because host precedence already picks the exact-host binding rather than treating the pair as ambiguous.

The rule is deliberately conservative. Failed validation or acknowledgement leaves the previous acknowledged revision active.

### Privacy and observability

Do not include configured selector values in serialized metadata, API errors, structured logs, metrics labels, tracing attributes, diagnostics, or test failure output. Documentation and tests may use clearly fictional placeholder values, never values copied from a real binding. Secret-free audit output may include binding name, decision (`matched`, `no_match`, or `ambiguous`), and selected header names.

### SDKs, CLI, and documentation

The egress OpenAPI contract is the source of truth. Supported SDKs add the optional `requestHeaders` property to the existing `CredentialMatch` model and preserve `CredentialBindingMetadata.match` as `CredentialMatch`. SDK and CLI write paths accept `name` and `value`; get, list, and inspect surfaces expose only `name` and `valueConfigured`. They must not fabricate selector values from a read response. CLI and documentation must identify selector values as write-only non-secret inputs and must not render them in normal inspect/list output.

## Test Plan

### Unit and schema tests

- Accept valid exact conjunctions; reject empty arrays, more than four entries, blank values, duplicate normalized names, invalid names, `Cookie`, and other forbidden names.
- Verify case-insensitive names, case-sensitive values, canonical outer-OWS trimming on configured and request values, and no normalization of internal whitespace or value casing. Verify values that differ only in outer SP/HTAB do not prove two bindings disjoint.
- Verify a selected header with two received field lines does not match, while one comma-containing field is one occurrence whose entire value is compared.
- Verify distinct values for the same header coexist; generic/selector overlap, identical selectors, and different-header selectors are rejected when base scopes can match at the same host precedence.
- Verify an exact-host binding and overlapping wildcard-host binding remain valid without selectors and the exact-host binding is selected.
- Verify an exact-host selector miss yields no injection and does not fall through to an overlapping generic wildcard-host binding.
- Verify selector representations containing neither `value` nor `valueConfigured`, or both, fail schema validation. Verify create/patch rejects `valueConfigured`, while get/list never exposes `value` and requires `valueConfigured: true`.
- Verify configured selector values cannot appear in errors, logs, metrics, diagnostics, or test failure output.
- Across supported SDKs, compile existing selector-free constructors/builders and verify their serialized `CredentialMatch` representation is unchanged. Preserve `CredentialMatch` as the type of `CredentialBindingMetadata.match`.
- Verify `bindings.replace` replaces the complete binding: omitting `requestHeaders` removes the previous selectors. Verify a mutation that does not name an existing binding leaves it unchanged.
- Verify a newer payload containing `requestHeaders` is rejected, not silently accepted without effect, by an older supported Credential Vault runtime.

### Integration and end-to-end tests

- Create two bindings for one destination with distinct fake `Authorization` values and verify each request receives only its selected credential.
- Repeat with multiple predicates and verify every predicate is required.
- Verify unmatched traffic continues without injection under normal egress policy.
- Verify an ambiguous candidate revision is rejected without replacing the previous acknowledged revision.
- Run the same request and sanitized-read assertions through all supported SDKs and CLI surfaces that expose Credential Vault.

## Drawbacks

- Callers must retain selector values because reads intentionally hide them.
- Exact matching can be stricter than some client ecosystems expect.
- Conservative validation rejects configurations whose disjointness cannot be proven from exact header values alone.
- Operators must understand that this is credential routing, not process-level access control.

## Alternatives

### Generic binding overridden by a selector-bound binding

This supports defaults plus exceptions, but turns the generic binding into a fallback: a missing or unexpected selector could receive the default credential. It also needs a precedence system. This proposal retains fail-closed behavior.

### Prefix, wildcard, regular-expression, or case-insensitive value matching

These make mutual exclusivity harder to prove and can hide client mistakes. Exact values meet the known placeholder and authentication-form use cases.

### Query, body, cookie, or process-identity selectors

These add parsing, privacy, or authorization semantics beyond credential disambiguation and should be separately proposed.

### Continue requiring one credential shape per destination

This preserves current behavior but cannot solve issue #1373's legitimate same-destination credential cases.

### Fork `CredentialMatch` into distinct write/read types

Mirroring `CredentialAuth`/`CredentialAuthMetadata` would give SDKs compile-time enforcement of the write/read shape split. It is rejected because `CredentialBindingMetadata.match` has always been typed as `CredentialMatch`, unlike `auth`, which was split from the start. Forking it now would break existing selector-free consumers across supported SDKs and belongs only in an explicit, versioned breaking migration.

## Infrastructure Needed

No new service, storage system, capability protocol, or third-party dependency is required. This uses the existing Credential Vault sidecar API, active snapshot, Credential Proxy path, strict JSON decoding, and SDK generation/release process.

## Upgrade & Migration Strategy

The change is additive. Existing bindings omit `requestHeaders` and preserve current behavior; no Vault state migration is required.

Release egress API/runtime support before SDK helpers that create selector-bound bindings. Credential Vault-capable runtimes have used strict unknown-field decoding since the feature's initial implementation, so an older runtime rejects `requestHeaders` instead of silently creating a broader binding. Preserve that behavior with a compatibility test; no capability-negotiation mechanism is introduced.

Operators adopt selectors by replacing a complete binding through the existing revisioned Vault mutation API. Failed validation or acknowledgement leaves the previous acknowledged revision active.
