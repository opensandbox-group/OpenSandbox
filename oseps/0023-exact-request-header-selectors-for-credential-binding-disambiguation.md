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
| R1 | `CredentialMatch` may contain at most four AND-combined exact request-header predicates. | Must Have |
| R2 | Header names match case-insensitively. Values match case-sensitively after trimming only outer HTTP optional whitespace (SP and HTAB). | Must Have |
| R3 | Each selected request header must occur exactly once; missing or repeated headers do not satisfy a predicate. | Must Have |
| R4 | Selectors permit ordinary end-to-end headers, including `Authorization`, but reject HTTP/2 pseudo-headers and these case-insensitive names: `Host`, `Content-Length`, `Content-Type`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`, `Trailer`, `Proxy-Authorization`, `Proxy-Authenticate`, `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto`. | Must Have |
| R5 | Overlapping destination scopes are valid only when selectors prove the bindings cannot both match one request. | Must Have |
| R6 | Public reads return selector header names and `valueConfigured: true`, never selector values. | Must Have |
| R7 | Unsupported selector fields are rejected, never silently removed or broadened. | Must Have |
| R8 | Selectors are non-secret routing hints, not an authorization boundary. | Must Have |

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

Existing scheme, host, method, and path checks run first. A binding is eligible only when every `requestHeaders` predicate matches the original intercepted request. Credential Proxy selects exactly one eligible binding, then applies its existing authentication injection behavior.

No eligible binding preserves current behavior: inject no credential and let ordinary egress policy decide whether traffic may continue. Multiple eligible bindings retain the current ambiguous-binding denial.

### Notes/Constraints/Caveats

- A selector is a routing hint, not authentication. Any sandbox process can construct one, so it cannot restrict a process to a subset of credentials.
- Matching occurs before injection or replacement. A matching placeholder `Authorization` header is subsequently replaced when the selected binding injects `Authorization`.
- Callers retain desired selector values; public read APIs intentionally cannot reconstruct selector-bound bindings.
- This proposal does not widen transparent interception ports or alter egress policy coverage requirements.

### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| A selector value leaks. | Treat it as non-secret, write-only input and exclude configured values from public reads, logs, metrics, diagnostics, and errors. Documentation may use clearly fictional placeholders only. |
| Proxy and upstream interpret repeated headers differently. | Require exactly one occurrence and reject routing/framing/proxy-control names. |
| Two bindings select different credentials for one request. | Preserve fail-closed ambiguous matching and allow overlap only when static validation proves predicates disjoint. |
| A mixed egress/SDK deployment broadens a binding. | Add the field additively and require older runtimes to reject it rather than ignore it. |
| Strict validation rejects a useful future policy. | Keep exact conjunctions narrowly scoped; predicate kinds or precedence require a separate proposal. |

## Design Details

### Data model and API

Add `requestHeaders` to the public `CredentialMatch` request schema:

```yaml
requestHeaders:
  type: array
  maxItems: 4
  items:
    type: object
    required: [name, value]
    properties:
      name: {type: string}
      value: {type: string, maxLength: 4096, writeOnly: true}
    additionalProperties: false
```

`name` must be a non-empty HTTP field-name token as defined by RFC 9110; it is not whitespace-trimmed. After trimming only outer HTTP optional whitespace, `value` must be non-empty. A binding must not repeat a header name after case-insensitive normalization. This prevents unsatisfiable predicates.

Public binding reads return a sanitized match shape:

```yaml
requestHeaders:
  - name: Authorization
    valueConfigured: true
```

The egress sidecar's private active snapshot retains exact values for matching; public binding metadata never does.

### Matching and selection

For each base-matching binding, Credential Proxy evaluates all normalized `requestHeaders` predicates against original request headers:

1. Header names compare case-insensitively.
2. The selected header must have exactly one occurrence.
3. After trimming only outer SP and HTAB, its value must exactly equal the configured value.
4. Values otherwise receive no case folding, decoding, internal-whitespace normalization, prefix matching, or expression evaluation.

Existing host precedence is unchanged. At the selected host precedence, zero eligible bindings yields no injection and two or more yields the current ambiguous-binding denial. Selector count is not a precedence dimension.

### Candidate validation

Vault creation and mutation validate the complete post-mutation binding set. For bindings with overlapping base scopes, both bindings must declare selectors. They may coexist only if they share a normalized header name whose configured exact values differ, proving no request can satisfy both conjunctions. Otherwise the candidate is rejected. In particular, a selector-bound binding cannot overlap a generic binding, and predicates on different header names do not prove disjointness because a request can carry both headers.

The rule is deliberately conservative. Failed validation or proxy acknowledgement leaves the previous acknowledged revision active.

### Privacy and observability

Do not include configured selector values in serialized metadata, API errors, structured logs, metrics labels, tracing attributes, diagnostics, or test failure output. Documentation and tests may use clearly fictional placeholder values, never values copied from a real binding. Secret-free audit output may include binding name, decision (`matched`, `no_match`, or `ambiguous`), and selected header names.

### SDKs, CLI, and documentation

The egress OpenAPI contract is the source of truth. Supported SDKs must preserve `requestHeaders` on create and patch and expose the sanitized read shape. They must not fabricate selector values from a read response. CLI and documentation must identify selector values as write-only non-secret inputs and must not render them in normal inspect/list output.

## Test Plan

### Unit and schema tests

- Accept valid exact conjunctions; reject more than four entries, blank values, duplicate normalized names, invalid names, and forbidden names.
- Verify case-insensitive names, case-sensitive values, outer-OWS trimming, and no normalization of internal whitespace or value casing.
- Verify missing or duplicate selected headers do not match.
- Verify distinct values for the same header coexist; generic/selector overlap, identical selectors, and different-header selectors are rejected when base scopes overlap.
- Verify reads expose names and `valueConfigured: true`, never values, and that values cannot appear in errors, logs, metrics, or diagnostics.

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

## Infrastructure Needed

No new service, storage system, or third-party dependency is required. This uses the existing Credential Vault sidecar API, active snapshot, Credential Proxy path, and SDK generation/release process.

## Upgrade & Migration Strategy

The change is additive. Existing bindings omit `requestHeaders` and preserve current behavior; no Vault state migration is required.

Release egress API/runtime support before SDK helpers that create selector-bound bindings. A newer SDK targeting an older sidecar must receive a validation error, never silently omit selectors and create a broader binding.

Operators adopt selectors by replacing a complete binding through the existing revisioned Vault mutation API. Failed validation or acknowledgement leaves the previous acknowledged revision active.
