---
title: Credential Vault and Credential Proxy
authors:
  - "@jwx0925"
creation-date: 2026-05-28
last-updated: 2026-06-23
status: implemented
---

# OSEP-0012: Credential Vault and Credential Proxy

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
  - [Architecture Overview](#architecture-overview)
  - [Request Flow](#request-flow)
  - [API Schema](#api-schema)
  - [Credential Sources](#credential-sources)
  - [Runtime Credential Mutations](#runtime-credential-mutations)
  - [SDK API](#sdk-api)
  - [Kubernetes Runtime Delivery](#kubernetes-runtime-delivery)
  - [Credential Injection](#credential-injection)
  - [Response Redaction and Echo Handling](#response-redaction-and-echo-handling)
  - [Runtime Path](#runtime-path)
  - [Policy and Egress Integration](#policy-and-egress-integration)
  - [Observability](#observability)
  - [Component Changes](#component-changes)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal introduces **Credential Vault**, a brokered credential layer for OpenSandbox, and **Credential Proxy**, the runtime component that injects scoped credentials into approved outbound requests without exposing plaintext credentials inside the sandbox.

Instead of passing secrets through environment variables, files, or command arguments, users create a sandbox-local Credential Vault after sandbox creation and bind credentials to approved outbound request scopes. Credential Proxy evaluates the sandbox identity, destination, method, path, and authentication policy before adding credentials to outbound HTTP requests.

## Motivation

AI agents frequently need credentials to call external systems such as GitHub, model APIs, cloud storage, package registries, databases, and internal services. Today the common approach is to place secrets inside the sandbox as environment variables or files. That makes the secret available to every process in the sandbox, and an untrusted or compromised agent can print, persist, exfiltrate, or transform the secret.

OpenSandbox already provides isolated execution and per-sandbox egress control. The next security requirement is brokered credential use: a sandboxed agent should be able to use an approved credential for an approved destination, but it should not be able to read the underlying plaintext credential.

Credential Vault extends OpenSandbox's sandbox security model from:

- where code can run,
- what network destinations it can reach,

to:

- what credentials it can use for those destinations.

### Goals

1. **Brokered credentials**: Let sandboxed workloads use credentials without receiving plaintext secret values through OpenSandbox-managed environment variables, files, Credential Vault API responses, diagnostics, or logs.
2. **Declarative binding**: Add a credential binding model that separates write-only credential sources from destination matching and typed authentication behavior.
3. **Policy-aware runtime injection**: Inject credentials only when sandbox identity, destination FQDN, HTTP method, and path all match the binding.
4. **Egress alignment**: Integrate with `networkPolicy.egress` so credential scope and network reachability are consistent.
5. **Runtime agnostic**: Support both Docker and Kubernetes through the existing egress sidecar pattern that shares the sandbox network namespace.
6. **Transparent by default**: Use the existing egress transparent mitmproxy path as Credential Proxy so applications do not need proxy or base URL changes.
7. **Auditable and redacted**: Emit useful audit events and metrics while redacting credential material from logs, diagnostics, and responses.
8. **Backward compatible**: Keep credential values and Credential Vault revisions out of the sandbox creation request. Sandbox creation may carry an explicit `credentialProxy.enabled` startup opt-in so the runtime can prepare transparent MITM before user code starts; the Credential Vault itself is still created after sandbox creation through the egress sidecar API.
9. **Runtime mutation**: Let callers with egress endpoint access create a sandbox Credential Vault and add, replace, and delete sandbox credential bindings while the sandbox is running without exposing plaintext credentials to sandbox processes.

### Non-Goals

1. **General-purpose secret manager**: Credential Vault is not intended to replace HashiCorp Vault, Infisical, cloud secret managers, or Kubernetes Secret. It brokers credentials from configured sources into sandbox traffic.
2. **Secret lifecycle management**: Rotation, versioning, approval workflows, and cross-environment secret synchronization are out of scope for the initial design.
3. **Plaintext exposure inside sandbox**: This proposal does not add an API for sandbox processes to retrieve raw credential values.
4. **Generic body rewriting as MVP**: Request/response body mutation is out of scope for the MVP; header injection is sufficient for the first set of credential use cases.
5. **Per-process policy**: Credential policies apply to a sandbox, not to individual processes inside that sandbox.
6. **Non-HTTP protocols as MVP**: SSH, database wire protocols, Git smart protocol credential helpers, and arbitrary TCP credential injection are future work.
7. **Replacing egress policy**: Credential Vault complements egress control but does not replace network allow/deny enforcement.

## Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| R1 | Users can create a sandbox-local Credential Vault after the sandbox is created | Must Have |
| R2 | Plaintext credentials are not exposed through OpenSandbox-managed sandbox env vars, files, Credential Vault API responses, diagnostics, or logs | Must Have |
| R3 | Credential Proxy injects credentials only for matching scheme, port, FQDN, HTTP method, and path scope | Must Have |
| R4 | Initial injection supports HTTP request headers | Must Have |
| R5 | Inline ephemeral values can be used as the public credential input | Must Have |
| R6 | Credential Vault creation requires explicit `networkPolicy.egress` coverage for every binding target | Must Have |
| R7 | Audit logs and metrics identify binding usage without logging credential values | Must Have |
| R8 | Docker and Kubernetes runtimes use the same user-facing API semantics | Must Have |
| R9 | Credential Proxy is default-deny for missing, invalid, or non-matching bindings | Must Have |
| R10 | The runtime uses egress transparent mitmproxy as the Credential Proxy implementation | Must Have |
| R11 | Credential Vault creation fails closed when transparent redirect, mitm readiness, CA bootstrap, or egress API auth cannot be configured | Must Have |
| R12 | Callers with egress API access can add, replace, and delete credential bindings for a running sandbox | Must Have |
| R13 | Runtime credential mutations are applied atomically by Credential Proxy with revision acknowledgement or fail without partial metadata/runtime drift | Must Have |
| R14 | Future secret managers can be added through a provider interface | Should Have |

## Proposal

Add Credential Vault as an egress sidecar runtime API. Add Credential Proxy as the credential-aware runtime behavior of the existing egress transparent mitmproxy path.

The first implementation supports **transparent proxy mode**:

1. The user creates a sandbox normally, without `credentialVault` or plaintext credential material in `CreateSandboxRequest`. When Credential Vault will be used, the request sets `credentialProxy.enabled: true` so the runtime starts the egress sidecar with transparent MITM support before user code starts.
2. The user resolves the sandbox egress endpoint, for example by calling `GET /v1/sandboxes/{sandboxId}/endpoints/18080`.
3. The user calls the egress sidecar API `POST /credential-vault` with initial `credentials` and `bindings`.
4. The sandbox application container sends normal outbound HTTP/HTTPS traffic, for example to `https://api.github.com/repos/alibaba/OpenSandbox`.
5. Egress transparent mitmproxy intercepts outbound `TCP 80/443` traffic in the sandbox network namespace.
6. Credential Proxy evaluates the intercepted request against the sandbox credential bindings, including scheme and port.
7. If exactly one binding matches and policy allows the request, Credential Proxy fetches or receives the credential material from a trusted source path and injects it into the request.
8. The external service receives the credential-bearing request; the sandbox process only sees the service response.

The MVP also supports runtime credential mutation through the egress sidecar API. After a Credential Vault exists, a caller with egress API access may add, replace, or delete sandbox-local credentials and bindings. The egress sidecar validates the mutation against the same match, effective egress policy, intercept-port, ambiguity, and redaction rules used at vault creation time, then asks Credential Proxy to atomically load a new binding revision. The create or mutation request succeeds only after Credential Proxy acknowledges the revision; otherwise no inactive binding revision is reported as active.

At a high level:

```
┌──────────────────────┐       lifecycle        ┌──────────────────────────────────┐
│ OpenSandbox Client    │───────────────────────▶│ OpenSandbox Server               │
└──────────┬───────────┘                        │                                  │
           │                                    │ Lifecycle API                    │
           │                                    │ - create sandbox                 │
           │                                    │ - resolve egress endpoint/auth   │
           │                                    └───────────────┬──────────────────┘
           │                                                    │ create/start sandbox runtime
           │                                                    ▼
           │                         ┌──────────────────────────────────────────────┐
           │                         │ Sandbox Pod / Network Namespace              │
           │                         │                                              │
           │                         │ ┌──────────────────────┐                     │
           │                         │ │ Application Container │                     │
           │                         │ │ - no plaintext secret │                     │
           │                         │ │ - no proxy config     │                     │
           │                         │ └──────────┬───────────┘                     │
           │                         │            │ HTTP(S)                         │
           │                         │            ▼                                 │
           │ Egress API              │ ┌───────────────────────────────┐            │
           │ with endpoint + auth:   │ │ Egress Sidecar                 │            │
           │ - POST vault            │ │ - Egress API                  │            │
           │ - PATCH vault           │ │ - Credential Proxy            │            │
           │ - GET credentials       │ │   (transparent mitmproxy)      │            │
           │ - GET bindings          │ │ - vault revision API           │            │
           └────────────────────────▶│ │ - policy, injection, audit     │            │
                                     │ └───────────────┬───────────────┘            │
                                     └─────────────────┼────────────────────────────┘
                                                       │ authenticated request
                                                       ▼
                                               External Service
```

### Notes/Constraints/Caveats

1. **Credential Proxy is the egress transparent mitmproxy path**: This proposal does not introduce an explicit proxy or local gateway mode. Applications keep using their normal target URLs.
2. **HTTPS interception requires trusted CA setup**: Transparent HTTPS injection depends on the sandbox trusting the mitmproxy root CA. Images or runtime startup must install the OpenSandbox MITM CA, otherwise HTTPS handshakes fail.
3. **Credential source access is sidecar-local in the MVP**: Runtime sidecars should not be granted broad cluster secret access. The MVP accepts only inline values sent to the egress sidecar API and converts them into sandbox-scoped in-memory material.
4. **Credential material must be short-lived in memory**: Credential Proxy should not persist plaintext credentials on disk. If temporary files are unavoidable for runtime delivery, they must be scoped to one sandbox and cleaned up with sandbox deletion.
5. **Binding scope must be covered by egress scope**: A sandbox must include `networkPolicy.egress` before a Credential Vault can be created, and every credential binding target must be covered by an explicit allow rule. Missing or inconsistent egress policy fails vault creation or mutation.
6. **Multiple matching bindings are ambiguous**: If more than one binding matches a request and no deterministic precedence is declared, Credential Proxy must fail closed.
7. **Upstream echo is outside the absolute secrecy guarantee**: OpenSandbox-managed runtime, API, diagnostic, and log surfaces must not expose credentials, but an upstream service can still echo request headers in response bodies or headers. The MVP redacts known credential values from response headers and does not rewrite response bodies. Users should avoid binding credentials to services that echo sensitive request headers in response bodies.
8. **Runtime mutations use egress API auth**: Callers that can resolve the egress endpoint and provide the required egress auth header can create or mutate the Credential Vault.

### Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Sandbox bypasses Credential Proxy | Credential not injected, or traffic reaches destination without policy mediation | Use egress transparent redirect for TCP 80/443 and recommend `networkPolicy.defaultAction=deny` with `dns+nft` |
| Credential leakage through logs | Secret exposure | Central redaction helpers; never log injected headers or rendered values; regression tests for logs |
| Upstream echoes injected credential | Credential appears in sandbox-visible response content | Redact known credential values from response headers; do not promise body rewriting in the MVP; document that services which echo sensitive request headers in response bodies are unsupported |
| Inline credential value leaked by API logging | Secret exposure | Treat inline credential `value` as write-only input; redact request bodies, validation errors, SDK debug logs, and sidecar logs |
| Credential source over-permissioned to sidecars | Cluster-wide secret access risk | MVP supports only inline values sent to the egress sidecar API; sidecar has no Kubernetes API permission by default |
| Binding and egress policy drift | Credential may be configured for unreachable or unintended destinations | Require `networkPolicy.egress` and fail vault creation or mutation when any binding target is not covered |
| Header injection into wrong host due to redirects | Credential sent to unintended destination | Re-evaluate policy after each redirected request; strip injected credentials on cross-host redirect unless target scope matches |
| Credential injected over cleartext HTTP | Credential exposed on the network | Default binding scope to `schemes: [https]` and `ports: [443]`; require explicit opt-in for any HTTP injection |
| HTTPS CA not trusted by sandbox image | Authenticated HTTPS requests fail | Install/export the OpenSandbox mitmproxy CA during sandbox startup or document image requirements |
| Transparent redirect unavailable | Credential traffic bypasses proxy policy | Fail vault creation when credential bindings are present and mitmproxy, iptables redirect, CA bootstrap, or egress auth cannot be configured |
| Untrusted mitm addon observes injected headers | Future credential exposure if sandbox users can control addon loading | Current egress mitm addons are operator/platform-controlled and are not a sandbox-user API. Preserve that boundary: reject any future sandbox-supplied mitm addons for credential-enabled sandboxes, and allow only operator-configured trusted addons. |
| Egress policy API unauthenticated | Sandbox process rewrites sidecar policy | Always provision `OPENSANDBOX_EGRESS_TOKEN` for credential-enabled sidecars, even when network policy would otherwise be omitted |
| IPv6 bypasses transparent MITM | HTTP(S) traffic avoids inspection, injection, and audit | Short term: fail closed unless IPv6 is disabled or equivalently redirected; long term: support IPv6 through ip6tables/nftables redirect and test coverage |
| Upstream TLS verification disabled | Credential injected into a spoofed upstream peer | Reject `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true` for credential-enabled sandboxes |
| Multiple bindings match one request | Wrong credential injection | Fail closed unless a single highest-priority binding is configured |
| Long-lived credentials remain in proxy memory | Expanded exposure window | Cache with TTL, zero buffers where practical, prefer short-lived tokens from providers |
| Runtime mutation accepted but not loaded by proxy | API response and active proxy state disagree about which credentials are active | Treat mutations as revisioned transactions; do not report success until Credential Proxy acknowledges the new revision; keep the previous revision active on failure |
| Sandbox process calls mutation API | Sandbox can bind credentials to attacker-controlled targets | Do not expose the egress API auth token to application containers; require the same endpoint auth used by other egress APIs |
| Users expect full secret management | Product confusion | Document Credential Vault as a broker layer, not a standalone secret manager |

## Design Details

### Terminology

- **Credential Vault**: Egress sidecar runtime capability for declaring, validating, and managing credential bindings on a sandbox.
- **Credential Proxy**: Credential-aware runtime behavior in the egress sidecar's transparent mitmproxy path. It evaluates outbound HTTP/HTTPS requests and injects credentials when policy matches.
- **Credential**: A per-sandbox named credential with a write-only source. Bindings reference credentials by sandbox-local name instead of embedding secret values in injection configuration.
- **Credential Binding**: A per-sandbox declaration that connects an outbound request match to a typed authentication rule. Bindings reference a named sandbox-local credential.
- **Credential Source**: Credential material supplied to Credential Proxy. In the MVP egress API this is an inline ephemeral value accepted only through `POST /credential-vault` or runtime credential mutation APIs.
- **Credential Injection**: The act of adding credential material to an outbound request, for example as an `Authorization` header.

### Architecture Overview

Credential Vault should be modeled as a runtime API on the egress sidecar. Credential Proxy should be modeled as a credential-aware extension of the existing egress sidecar transparent mitmproxy path:

- Egress sidecar controls which network destinations are reachable.
- Credential Proxy controls which credentials are attached to allowed outbound HTTP/HTTPS requests.

For Kubernetes, this means the existing egress sidecar in the sandbox Pod starts mitmproxy transparent mode and loads OpenSandbox's credential addon. For Docker, this means the existing egress sidecar shares the sandbox network namespace, redirects outbound configured HTTP/HTTPS ports to mitmproxy, and runs the same credential addon. The MVP built-in intercept ports are `80` and `443`.

The egress sidecar already has the transparent MITM primitives required for Credential Proxy:

- starts `mitmdump --mode transparent`,
- redirects outbound configured HTTP/HTTPS destination ports, initially `TCP 80/443`, to the mitmproxy listener using `iptables`,
- loads system and operator-configured mitm addons,
- exports the mitmproxy root CA,
- exposes health readiness so sandboxes do not start before interception is ready.

Credential Vault adds a first-party credential addon and runtime vault revision API to that path.

### Request Flow

For a GitHub read-only token binding:

1. Sandbox process calls `https://api.github.com/repos/alibaba/OpenSandbox` normally.
2. Egress transparent mitmproxy intercepts the request and exposes host `api.github.com`, method `GET`, and path `/repos/alibaba/OpenSandbox` to the Credential Proxy addon.
3. Credential Proxy loads matching bindings for the sandbox.
4. It finds `github-readonly` where:
   - `hosts` contains `api.github.com`,
   - `methods` contains `GET`,
   - `paths` contains `/repos/*`,
   - `auth.type` is `bearer`.
5. It retrieves material for the binding's referenced sandbox-local credential.
6. It sends the upstream request with:

```http
Authorization: Bearer <redacted>
```

7. It records an audit event with sandbox ID, binding name, target, method, path pattern, decision, and response status. The credential value is not logged.

### API Schema

Extension to `specs/egress-api.yaml`:

```yaml
paths:
  /credential-vault:
    post:
      summary: Create a sandbox-local Credential Vault and activate its initial revision.
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CredentialVaultCreateRequest'
    get:
      summary: List sanitized Credential Vault metadata for a sandbox.
    patch:
      summary: Atomically mutate sandbox-local credentials and bindings.
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CredentialVaultMutationRequest'
    delete:
      summary: Delete the sandbox-local Credential Vault and remove all active runtime material.

  /credential-vault/credentials:
    get:
      summary: List sanitized credential metadata for a sandbox.

  /credential-vault/credentials/{credential_name}:
    get:
      summary: Get sanitized credential metadata.

  /credential-vault/bindings:
    get:
      summary: List credential binding metadata for a sandbox.

  /credential-vault/bindings/{binding_name}:
    get:
      summary: Get sanitized credential binding metadata.

components:
  schemas:
    CredentialVaultCreateRequest:
      type: object
      required: [credentials, bindings]
      description: Initial sandbox-local Credential Vault revision accepted directly by the egress sidecar.
      properties:
        credentials:
          type: array
          items:
            $ref: '#/components/schemas/Credential'
        bindings:
          type: array
          items:
            $ref: '#/components/schemas/CredentialBinding'
      additionalProperties: false

    CredentialVaultMutationResult:
      type: object
      required: [revision, credentials, bindings]
      properties:
        revision:
          type: integer
          description: Monotonic sandbox-local credential binding revision acknowledged by Credential Proxy.
        credentials:
          type: array
          items:
            $ref: '#/components/schemas/CredentialMetadata'
        bindings:
          type: array
          items:
            $ref: '#/components/schemas/CredentialBindingMetadata'
      additionalProperties: false

    CredentialMetadata:
      type: object
      required: [name, sourceType, revision]
      properties:
        name:
          type: string
        sourceType:
          type: string
        revision:
          type: integer
      additionalProperties: false

    CredentialVaultMutationRequest:
      type: object
      properties:
        expectedRevision:
          type: integer
          description: Optional optimistic concurrency guard. If set, the egress sidecar rejects the mutation when the current acknowledged revision differs.
        credentials:
          $ref: '#/components/schemas/CredentialMutationSet'
        bindings:
          $ref: '#/components/schemas/CredentialBindingMutationSet'
      additionalProperties: false

    CredentialMutationSet:
      type: object
      properties:
        add:
          type: array
          items:
            $ref: '#/components/schemas/Credential'
        replace:
          type: array
          items:
            $ref: '#/components/schemas/Credential'
        delete:
          type: array
          items:
            type: string
      additionalProperties: false

    CredentialBindingMutationSet:
      type: object
      properties:
        add:
          type: array
          items:
            $ref: '#/components/schemas/CredentialBinding'
        replace:
          type: array
          items:
            $ref: '#/components/schemas/CredentialBinding'
        delete:
          type: array
          items:
            type: string
      additionalProperties: false

    CredentialBindingMetadata:
      type: object
      required: [name, revision]
      properties:
        name:
          type: string
        revision:
          type: integer
        match:
          $ref: '#/components/schemas/CredentialMatch'
        auth:
          $ref: '#/components/schemas/CredentialAuthMetadata'
      additionalProperties: false

    Credential:
      type: object
      required: [name, source]
      properties:
        name:
          type: string
          description: Sandbox-local credential name referenced by one or more bindings.
        source:
          $ref: '#/components/schemas/CredentialSource'
      additionalProperties: false

    CredentialSource:
      oneOf:
        - $ref: '#/components/schemas/InlineCredentialSource'
      discriminator:
        propertyName: type
        mapping:
          inline: '#/components/schemas/InlineCredentialSource'

    InlineCredentialSource:
      type: object
      required: [type, value]
      properties:
        type:
          type: string
          enum: [inline]
        value:
          type: string
          writeOnly: true
          description: Inline ephemeral credential value accepted through Credential Vault creation or runtime credential mutation APIs. Never returned, logged, or persisted as plaintext.
      additionalProperties: false

    CredentialBinding:
      type: object
      required: [name, match, auth]
      properties:
        name:
          type: string
          description: Sandbox-local credential binding name.
        match:
          $ref: '#/components/schemas/CredentialMatch'
        auth:
          $ref: '#/components/schemas/CredentialAuth'
      additionalProperties: false

    CredentialMatch:
      type: object
      required: [hosts]
      properties:
        schemes:
          type: array
          items:
            type: string
            enum: [https, http]
          default: [https]
          description: URL schemes eligible for credential injection. Defaults to HTTPS only.
        ports:
          type: array
          items:
            type: integer
          default: [443]
          description: Destination ports eligible for credential injection. Defaults to 443.
        hosts:
          type: array
          items:
            type: string
          description: Exact FQDN hosts or leftmost-label wildcard hosts, for example api.github.com or *.example.com.
        methods:
          type: array
          items:
            type: string
          default: [GET, POST, PUT, PATCH, DELETE]
        paths:
          type: array
          items:
            type: string
          default: ["/*"]
      additionalProperties: false

    CredentialAuth:
      oneOf:
        - $ref: '#/components/schemas/BearerCredentialAuth'
        - $ref: '#/components/schemas/BasicCredentialAuth'
        - $ref: '#/components/schemas/ApiKeyCredentialAuth'
        - $ref: '#/components/schemas/CustomHeadersCredentialAuth'
      discriminator:
        propertyName: type
        mapping:
          bearer: '#/components/schemas/BearerCredentialAuth'
          basic: '#/components/schemas/BasicCredentialAuth'
          apiKey: '#/components/schemas/ApiKeyCredentialAuth'
          customHeaders: '#/components/schemas/CustomHeadersCredentialAuth'

    BearerCredentialAuth:
      type: object
      required: [type, credential]
      properties:
        type:
          type: string
          enum: [bearer]
        credential:
          type: string
          description: Sandbox-local credential name.
      additionalProperties: false

    BasicCredentialAuth:
      type: object
      required: [type, credential]
      properties:
        type:
          type: string
          enum: [basic]
        credential:
          type: string
          description: Sandbox-local credential name containing a pre-encoded base64(username:password) value.
      additionalProperties: false

    ApiKeyCredentialAuth:
      type: object
      required: [type, name, credential]
      properties:
        type:
          type: string
          enum: [apiKey]
        name:
          type: string
          pattern: '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+$'
          description: Header name used for the API key.
        credential:
          type: string
          description: Sandbox-local credential name.
      additionalProperties: false

    CustomHeadersCredentialAuth:
      type: object
      required: [type, headers]
      properties:
        type:
          type: string
          enum: [customHeaders]
        headers:
          type: array
          minItems: 1
          items:
            $ref: '#/components/schemas/CustomHeaderEntry'
          description: Headers to inject. Each header references its own sandbox-local credential.
      additionalProperties: false

    CustomHeaderEntry:
      type: object
      required: [name, credential]
      properties:
        name:
          type: string
          pattern: '^[A-Za-z0-9!#$%&''*+/=?^_`{|}~-]+$'
          description: Header name to inject. The header value is the referenced credential value.
        credential:
          type: string
          description: Sandbox-local credential name used for this header.
      additionalProperties: false

    CredentialAuthMetadata:
      type: object
      required: [type]
      properties:
        type:
          type: string
        name:
          type: string
          description: Header name when applicable.
      additionalProperties: false
```

Validation rules:

- A `CredentialBinding` must include `name`, `match`, and typed `auth`.
- `CredentialVaultCreateRequest.credentials[].name` values and active credential names after any mutation must be unique within a sandbox.
- `CredentialVaultCreateRequest.bindings[].name` values and active binding names after any mutation must be unique within a sandbox.
- Every credential reference in `auth.credential` and `auth.headers[].credential` must reference a credential declared in the vault creation request or active post-mutation credential set.
- Header names in `apiKey` and `customHeaders.headers[]` must be valid HTTP field names and must not be routing, framing, hop-by-hop, or proxy-control headers. The egress sidecar must reject at least `Host`, `Content-Length`, `Content-Type`, `Transfer-Encoding`, `Connection`, `Upgrade`, `TE`, `Trailer`, `Proxy-Authorization`, `Proxy-Authenticate`, `Forwarded`, `X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto` unless a future design explicitly allows them.
- Inline ephemeral is the only public MVP credential source. Future provider-backed sources must be added as new `CredentialSource` discriminator variants with source-specific authorization and destination constraints.
- `match.hosts[]` supports only exact hosts such as `api.github.com` and leftmost-label subdomain wildcards such as `*.github.com`. The wildcard form matches `api.github.com` and `a.b.github.com`, but does not match the apex host `github.com`.
- Host matching must normalize hosts before evaluation, including lowercase normalization and IDNA handling. Other wildcard forms such as `api.*.com`, `*github.com`, and `*` are invalid.
- Exact host matches have higher precedence than wildcard host matches. If more than one binding still matches at the same precedence after scheme, port, host, method, and path evaluation, the request must fail closed as an ambiguous match.
- Runtime mutation APIs require the egress API auth header. Sandbox application containers must not receive credentials that allow them to call these APIs.
- Runtime mutation APIs return only sanitized metadata and the acknowledged `revision`; they never return credential source values.

Example sandbox creation request:

```json
{
  "image": "python:3.12",
  "networkPolicy": {
    "defaultAction": "deny",
    "egress": [
      { "action": "allow", "target": "api.github.com" }
    ]
  },
  "credentialProxy": {
    "enabled": true
  }
}
```

Example Credential Vault creation request:

```json
{
  "credentials": [
    {
      "name": "github-token",
      "source": {
        "type": "inline",
        "value": "ghp_xxx"
      }
    }
  ],
  "bindings": [
    {
      "name": "github-readonly",
      "match": {
        "schemes": ["https"],
        "ports": [443],
        "hosts": ["api.github.com"],
        "methods": ["GET"],
        "paths": ["/repos/*", "/search/*"]
      },
      "auth": {
        "type": "bearer",
        "credential": "github-token"
      }
    }
  ]
}
```

### Credential Sources

The MVP exposes one public credential source type: `inline`.

- Available for cases where a caller creates a sandbox-scoped credential after sandbox creation or adds one to an already-running sandbox through the egress runtime API.
- The inline value is accepted only in `CredentialVaultCreateRequest.credentials[]` and egress runtime credential mutation requests.
- `source.type: inline` is required so future provider-backed sources can be added as declared discriminator variants.
- The egress sidecar must treat the value as write-only: do not return it in API responses, do not persist it as plaintext, and redact it from logs and validation errors.
- The egress sidecar converts the value into sandbox-scoped in-memory runtime credential material for Credential Proxy.
- For the MVP, Credential Vault state is runtime-only sidecar state. Pod or sidecar rebuild persistence is out of scope.

Example:

```json
{
  "name": "github-token",
  "source": {
    "type": "inline",
    "value": "ghp_xxx"
  }
}
```

Kubernetes Secret is not a user-facing credential source in the MVP. Sandbox creators cannot reference arbitrary pre-existing Kubernetes Secret names. Kubernetes runtime does not require generated Secrets to create or update a Credential Vault; callers deliver complete revisions directly through the egress sidecar Credential Vault API.

Future providers may include HashiCorp Vault, Infisical, AWS Secrets Manager, GCP Secret Manager, Azure Key Vault, internal credential brokers, and a Kubernetes Secret provider. Adding any user-selectable source provider requires a separate authorization model, namespace/scope policy, allowlists/selectors, requester authorization, and source-to-destination constraints.

### Runtime Credential Mutations

The MVP supports explicit creation of a sandbox-local Credential Vault after sandbox creation, read APIs for sandbox-local credentials and bindings, and one vault-level atomic mutation endpoint for all credential and binding additions, replacements, and deletions. These APIs are egress sidecar runtime APIs. Callers reach them by resolving the sandbox egress endpoint, for example port `18080`, and sending the endpoint's required egress auth headers.

Supported operations:

- `POST /credential-vault`: creates the sandbox-local Credential Vault, activates the initial complete revision, and returns sanitized metadata for the acknowledged revision.
- `GET /credential-vault`: returns sanitized credential metadata, sanitized binding metadata, and the current acknowledged revision.
- `PATCH /credential-vault`: atomically applies credential and binding additions, replacements, and deletions in one revisioned transaction.
- `DELETE /credential-vault`: deletes the sandbox-local Credential Vault and removes all active runtime material.
- `GET /credential-vault/credentials`: lists sanitized credential metadata and the current acknowledged revision.
- `GET /credential-vault/credentials/{credential_name}`: returns sanitized metadata for one credential.
- `GET /credential-vault/bindings`: lists sanitized binding metadata and the current acknowledged revision.
- `GET /credential-vault/bindings/{binding_name}`: returns sanitized metadata for one binding.

`POST /credential-vault` is the only API that creates a previously absent vault. `PATCH /credential-vault` is the only API that changes an existing vault. This keeps all writes atomic at the complete vault revision level, including single-resource changes such as adding one credential or deleting one binding. The credential and binding subresources are read-only views over the current acknowledged revision.

Example runtime add of a GitHub credential and matching binding after the vault exists:

```json
{
  "expectedRevision": 3,
  "credentials": {
    "add": [
      {
        "name": "github-token",
        "source": {
          "type": "inline",
          "value": "ghp_xxx"
        }
      }
    ]
  },
  "bindings": {
    "add": [
      {
        "name": "github-readonly",
        "match": {
          "schemes": ["https"],
          "ports": [443],
          "hosts": ["api.github.com"],
          "methods": ["GET"],
          "paths": ["/repos/*", "/search/*"]
        },
        "auth": {
          "type": "bearer",
          "credential": "github-token"
        }
      }
    ]
  }
}
```

Mutation rules:

- The egress sidecar must be running and ready to accept credential proxy updates. Stopped, deleted, or unreachable sandboxes reject runtime mutation requests by construction because their egress endpoint is unavailable.
- `POST /credential-vault` fails if a vault already exists for the sandbox. `PATCH /credential-vault` fails if the vault does not exist.
- Vault creation and runtime mutation requests require the sandbox runtime to expose the egress Credential Vault API and have Credential Proxy prerequisites ready, including transparent redirect, CA bootstrap, credential addon loading, startup gate, and egress API auth.
- Add and replace entries use the same `CredentialBinding` or `Credential` shape as vault creation.
- The egress sidecar applies all requested deletes, replacements, and additions to a candidate credential/binding set, then validates that complete post-mutation set before activating it in Credential Proxy.
- Add entries fail if the credential or binding name already exists in the pre-mutation set or appears more than once in the mutation request. Replace entries fail if the credential or binding name does not exist, unless a future upsert mode is explicitly added.
- The credential and binding subresource APIs are read-only. Add, replace, and delete operations for credentials and bindings must use `PATCH /credential-vault`; names are carried only in the mutation entries.
- Delete entries fail if the credential or binding name does not exist. Credential delete entries also fail when the post-mutation binding set still references that credential.
- A mutation request must not mention the same credential or binding name in multiple operations, for example both `replace` and `delete`.
- If `expectedRevision` is set and does not match the current acknowledged revision, the egress sidecar rejects the mutation before preparing runtime material.
- The egress sidecar validates binding hosts against its effective runtime egress policy. Runtime mutation does not widen network reachability; callers must update egress policy through the egress policy API first if needed.
- The egress sidecar runs ambiguity checks against the complete post-mutation binding set. If the new set can produce multiple matching bindings without deterministic precedence, the mutation fails.
- The egress sidecar rejects matches that require transparent MITM intercept ports not configured for the sandbox runtime.
- The egress sidecar rejects mutations for hosts that match `ignore_hosts`.

Revision and acknowledgement:

- Each accepted vault creation or mutation candidate receives a monotonically increasing sandbox-local `revision`.
- The egress sidecar prepares sandbox-scoped runtime material and asks Credential Proxy to load the complete candidate revision.
- Credential Proxy validates and loads the revision into an immutable in-memory snapshot, then atomically swaps the active snapshot.
- The API returns success only after Credential Proxy acknowledges the revision. The response includes the acknowledged revision and sanitized binding metadata.
- If Credential Proxy is unavailable, rejects the update, or times out, the egress sidecar must not report success. The previous acknowledged revision remains active.

Request handling during mutation:

- Requests already being processed may complete using the snapshot that was active when evaluation started.
- New requests after the proxy acknowledges a revision must use the new snapshot.
- Deleting a binding stops future injection after acknowledgement. It does not attempt to revoke credentials already sent to an upstream service in prior requests.

Runtime material cleanup:

- On replace, old runtime credential material must be removed after the new revision is acknowledged.
- On delete, runtime credential material for that binding must be removed after the delete revision is acknowledged.
- If the egress sidecar prepared candidate runtime material for vault creation, add, or replace and Credential Proxy rejects or times out before acknowledgement, the sidecar must delete or revoke the candidate material as part of failure handling. A failed request must not leave files or handles for an inactive revision.
- On sandbox deletion, sidecar exit removes in-memory runtime credential material and pending mutation state.

### SDK API

SDKs should expose Credential Vault through a sandbox-scoped facade for vault content and mutations. Sandbox creation and vault creation remain separate operations: `Sandbox.create()` may expose `credentialProxy.enabled` only as a runtime startup opt-in, while credentials and bindings are created later through the facade.

1. Create or connect to a sandbox.
2. Resolve the egress endpoint and endpoint auth headers through the existing lifecycle endpoint API.
3. Call the egress sidecar Credential Vault API directly.

The SDK should hide endpoint resolution and egress auth header propagation from callers, following the existing runtime egress policy helper pattern.

#### TypeScript / JavaScript

Expose a `credentialVault` facade on `Sandbox`:

```ts
const sandbox = await Sandbox.create({
  image: "ubuntu:22.04",
  networkPolicy: {
    defaultAction: "deny",
    egress: [{ action: "allow", target: "api.github.com" }],
  },
  credentialProxy: { enabled: true },
});

await sandbox.credentialVault.create({
  credentials: [
    {
      name: "github-token",
      source: { value: "ghp_xxx" },
    },
  ],
  bindings: [
    {
      name: "github-api",
      match: {
        schemes: ["https"],
        ports: [443],
        hosts: ["api.github.com"],
        methods: ["GET"],
        paths: ["/repos/*"],
      },
      auth: {
        type: "bearer",
        credential: "github-token",
      },
    },
  ],
});
```

Recommended TypeScript interface:

```ts
interface CredentialVault {
  create(req: CredentialVaultCreateRequest): Promise<CredentialVaultRevision>;
  get(): Promise<CredentialVaultState>;
  patch(req: CredentialVaultPatchRequest): Promise<CredentialVaultRevision>;
  delete(): Promise<void>;

  listCredentials(): Promise<CredentialMetadata[]>;
  getCredential(name: string): Promise<CredentialMetadata>;

  listBindings(): Promise<CredentialBindingMetadata[]>;
  getBinding(name: string): Promise<CredentialBindingMetadata>;
}
```

All credential and binding writes use `patch()`:

```ts
await sandbox.credentialVault.patch({
  expectedRevision: 3,
  credentials: {
    add: [
      {
        name: "github-token",
        source: { value: "ghp_xxx" },
      },
    ],
    replace: [],
    delete: ["old-token"],
  },
  bindings: {
    add: [],
    replace: [],
    delete: ["old-binding"],
  },
});
```

The SDK should not expose convenience methods such as `addCredential()`, `replaceCredential()`, `deleteCredential()`, `addBinding()`, `replaceBinding()`, or `deleteBinding()` in the MVP. Even if such helpers internally call `PATCH /credential-vault`, they would reintroduce a subresource write model that the API intentionally avoids.

Recommended public model names:

```ts
type Credential = {
  name: string;
  source: InlineCredentialSource;
};

type InlineCredentialSource = {
  value: string;
};

type CredentialBinding = {
  name: string;
  match: CredentialMatch;
  auth: CredentialAuth;
};

type CredentialAuth =
  | { type: "bearer"; credential: string }
  | { type: "basic"; credential: string }
  | { type: "apiKey"; name: string; credential: string }
  | { type: "customHeaders"; headers: Array<{ name: string; credential: string }> };
```

For the MVP, SDK public inputs treat inline as the default credential source type, so callers do not need to specify `source.type`. SDK adapters should normalize SDK input to the wire format expected by the egress API. SDK `CredentialAuth` shapes should match the egress API wire schema: `basic.credential` references a credential containing the pre-encoded `base64(username:password)` value, and `apiKey` injects into a request header named by `name`. The `InlineCredentialSource.value` field is write-only from the API perspective. SDKs must not log it, echo it in debug output, or include it in sanitized read models.

#### Python

Python should expose the same operation set with snake_case naming:

```py
sandbox = await Sandbox.create(
    image="ubuntu:22.04",
    network_policy={
        "defaultAction": "deny",
        "egress": [{"action": "allow", "target": "api.github.com"}],
    },
    credential_proxy={"enabled": True},
)

await sandbox.credential_vault.create(
    credentials=[
        {
            "name": "github-token",
            "source": {"value": "ghp_xxx"},
        }
    ],
    bindings=[
        {
            "name": "github-api",
            "match": {
                "schemes": ["https"],
                "ports": [443],
                "hosts": ["api.github.com"],
                "methods": ["GET"],
                "paths": ["/repos/*"],
            },
            "auth": {"type": "bearer", "credential": "github-token"},
        }
    ],
)

state = await sandbox.credential_vault.get()

await sandbox.credential_vault.patch(
    expected_revision=state.revision,
    credentials={
        "add": [],
        "replace": [],
        "delete": ["old-token"],
    },
    bindings={
        "add": [],
        "replace": [],
        "delete": ["old-binding"],
    },
)

await sandbox.credential_vault.delete()

credentials = await sandbox.credential_vault.list_credentials()
credential = await sandbox.credential_vault.get_credential("github-token")
bindings = await sandbox.credential_vault.list_bindings()
binding = await sandbox.credential_vault.get_binding("github-api")
```

The cross-language SDK contract is:

- `create` initializes a previously absent vault.
- `get` and `delete` operate on the complete vault.
- `patch` is the only update API for credentials and bindings.
- Credential and binding subresources are read-only list/get helpers.
- SDKs resolve the egress endpoint, merge endpoint headers, and call the sidecar directly.
- SDKs may expose `credential_proxy` / `credentialProxy` on sandbox creation only to opt into credential-capable transparent MITM startup. SDKs must not put Credential Vault credentials, bindings, or plaintext credential material into sandbox creation requests.

### Kubernetes Runtime Delivery

In Kubernetes deployments, Credential Vault is runtime-only state owned by the egress sidecar:

- Plaintext material needed by Credential Proxy is delivered through the egress sidecar Credential Vault API.
- The default Kubernetes path does not require creating, mounting, or refreshing a Kubernetes Secret.
- Kubernetes Secrets are not a user-facing credential source in the MVP, and generated Secrets are not part of the MVP persistence model.
- Pod rebuild, sidecar restart recovery, and durable encrypted Vault snapshots are out of scope for the MVP. If the egress sidecar process loses its in-memory state, callers must recreate the Credential Vault through the egress API.

Runtime delivery flow:

1. The caller resolves the sandbox egress endpoint, normally port `18080`, through the lifecycle endpoint API.
2. The caller sends `POST /credential-vault` or a mutation request directly to the egress sidecar with the required egress auth header.
3. The egress sidecar validates the candidate revision against its effective egress policy, transparent intercept ports, `ignore_hosts`, and binding ambiguity rules.
4. Credential Proxy loads the candidate into an immutable in-memory snapshot, atomically swaps the active revision, and acknowledges the revision.
5. If validation or loading fails, the previous acknowledged revision remains active. For initial vault creation, no vault becomes active.

This model avoids relying on kubelet Secret volume refresh timing for correctness. The active runtime revision is the in-memory snapshot acknowledged by Credential Proxy through the egress Credential Vault API.

### Credential Injection

The MVP supports request header injection only. User-facing bindings express injection as typed `auth` rules instead of embedding plaintext credentials or arbitrary rendered header values in configuration. Credential Proxy expands typed auth into concrete request headers after the transparent mitmproxy path has decoded request metadata.

Examples:

```yaml
auth:
  type: bearer
  credential: github-token
```

injects:

```http
Authorization: Bearer <github-token>
```

```yaml
auth:
  type: apiKey
  name: PRIVATE-TOKEN
  credential: gitlab-token
```

injects:

```http
PRIVATE-TOKEN: <gitlab-token>
```

```yaml
auth:
  type: customHeaders
  headers:
    - name: X-Access-Token
      credential: access-token
    - name: X-Client-Secret
      credential: client-secret
```

injects:

```http
X-Access-Token: <access-token>
X-Client-Secret: <client-secret>
```

Rules:

- Built-in auth types are preferred over `customHeaders` because they keep credential material separate from injection shape.
- `apiKey` and each `customHeaders.headers[]` entry inject the referenced credential value as the complete header value. They do not support custom value rendering, prefixes, suffixes, filters, functions, string concatenation, arbitrary expressions, or user-defined transforms.
- Callers that need `Authorization: Bearer <token>` must use `auth.type: bearer`; callers that need `Authorization: Basic <value>` must use `auth.type: basic`.
- Credential Proxy must reject duplicate header names within a single `customHeaders` auth rule unless a future merge strategy is explicitly added.
- Credential Proxy must inject only for HTTPS on port 443 by default.
- The transparent path supports credential injection only for destination ports that the egress sidecar redirects into mitmproxy. The MVP supports `443` by default and `80` when explicit operator policy enables cleartext HTTP injection.
- Any binding match that uses ports outside `80/443` must be rejected unless the operator explicitly configures those ports as transparent MITM intercept ports and the runtime can install matching redirect rules.
- Credential Proxy must reject attempts to inject hop-by-hop proxy headers unless explicitly allowed by implementation.
- Credential Proxy must remove any existing request header with the same name before injecting a credential, unless a future merge strategy is added.
- On redirect, Credential Proxy must re-evaluate the binding match before preserving injected headers.

### Response Redaction and Echo Handling

Credential Proxy redacts known credential values from upstream response headers. The MVP intentionally does not rewrite response bodies, including text-like bodies. This keeps the transparent proxy path simple and avoids buffering or transforming application payloads.

Redaction must include both source and rendered credential values, including:

- raw `credential` source material,
- rendered injected header values such as `Bearer <token>`,
- rendered Basic header values such as `Basic <base64(username:token)>`.

This redaction is best effort, not an absolute secrecy guarantee:

- Response headers that contain known credential values should be stripped or redacted.
- Response bodies, including JSON/text debug payloads, binary payloads, compressed payloads, streaming responses, and very large responses, are passed through without credential redaction in the MVP.
- Operators should not bind credentials to upstream services that intentionally echo sensitive request headers in response bodies.

The formal guarantee is that OpenSandbox-managed runtime, API, diagnostic, and log surfaces do not expose plaintext credentials. It cannot guarantee that arbitrary upstream services will never include an injected credential in application-level response content.

### Runtime Path

The MVP runtime path is **transparent proxy**. Credential Vault content remains a runtime sidecar API, but the lifecycle create request includes `credentialProxy.enabled` as an explicit startup opt-in so the platform can prepare the transparent proxy path before application code starts.

#### Transparent Proxy Path

When `credentialProxy.enabled` is set, the runtime prepares the existing egress transparent mitmproxy path before a Credential Vault is created. The application container keeps using normal outbound URLs. Credential Proxy runs as an OpenSandbox-managed mitm addon loaded by the egress sidecar. Because Kubernetes cannot add a sidecar to an already-created Pod, the runtime must expose a credential-capable egress sidecar before `POST /credential-vault` can succeed; the vault itself is created by calling the egress sidecar API after sandbox creation.

Advantages:

- No application proxy or base URL changes.
- Reuses existing egress sidecar network namespace, `iptables` redirect, health gate, and mitmproxy integration.
- Works with existing HTTP clients, SDKs, CLIs, and agent-generated code as long as they use intercepted HTTP/HTTPS ports and trust the sandbox CA. The MVP supported ports are `80/443`.
- Keeps credential policy enforcement at the egress boundary, where network policy is already enforced.

Limitations:

- Requires Linux network namespace support and `CAP_NET_ADMIN` for the egress sidecar.
- Requires the sandbox to trust the mitmproxy CA for HTTPS interception.
- Applies to HTTP/HTTPS traffic on configured transparent MITM intercept ports. The MVP supports `80/443`; additional ports require operator configuration and runtime redirect support. Non-HTTP protocols need future designs.
- Credential-bound targets must not match `ignore_hosts`. Because `ignore_hosts` is pass-through TLS, Credential Proxy cannot inspect, inject, redact, or audit those flows.
- Short term: Credential Vault creation must fail closed unless IPv6 HTTP(S) egress is disabled or covered by equivalent transparent redirect. Long term: IPv6 support should use ip6tables or nftables redirect with the same tests as IPv4.
- Credential Vault creation must reject upstream TLS-insecure mode such as `OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE=true`.
- `POST /credential-vault` must fail closed if mitmproxy, `iptables` redirect, CA bootstrap, credential addon loading, or egress API authentication cannot be configured.
- Credential-capable runtimes must ensure the egress sidecar is actually ready before accepting Credential Vault creation. Readiness alone is not sufficient if sandbox code can bypass redirect rules, mitmproxy, CA bootstrap, or credential addon loading.
- MITM addon trust boundary: current egress mitm addons are configured by the operator/platform through egress sidecar configuration, not by sandbox users. This is not a credential leak by itself because operator-configured addons are part of the platform trust boundary. Credential-enabled sandboxes must preserve this boundary by rejecting any future sandbox-supplied addon mechanism and by ensuring addon script paths do not come from app-container-writable locations. Operator-configured trusted addons may be loaded and must follow the same credential redaction and audit rules.

### Policy and Egress Integration

Sandboxes must include `networkPolicy.egress` before a Credential Vault can be created. The egress sidecar's Credential Vault API is the enforcement point because it owns the effective runtime egress policy.

Required validation:

- Every credential binding host must be covered by an explicit `networkPolicy.egress` allow rule. `networkPolicy.defaultAction=allow` does not count as credential binding coverage.
- The egress sidecar must also reject any Credential Vault revision whose binding match conflicts with its effective runtime egress policy, including ordered deny rules, first-match behavior, `ignore_hosts`, and non-intercepted ports.
- `networkPolicy.defaultAction` should be `deny`; if `allow` is accepted for compatibility, credential-bearing destinations still require explicit allow rules.
- If `networkPolicy` is omitted, if `egress` is empty, if a binding host lacks explicit allow coverage, or if a binding host is not reachable under the effective egress policy, vault creation or mutation must fail with HTTP 400.
- Runtime egress policy PATCH/DELETE requests for credential-enabled sandboxes must revalidate the complete active binding set. Updates that would make any active binding host non-allowed must fail, or the egress API must reject policy mutation while credentials are bound.

Suggested egress sidecar configuration:

```toml
[egress.credential_vault]
enabled = true
egress_validation = "strict"
transparent_intercept_ports = [80, 443]
```

For sandboxes with an active Credential Vault, strict validation is required. Non-strict egress validation would allow credential-bearing traffic to run without an explicit network policy boundary.

`transparent_intercept_ports` defines the destination ports that the egress sidecar must redirect into transparent mitmproxy for credential injection. The default MVP value is `[80, 443]`. Operators may add ports only when the runtime supports redirecting those ports and the deployment has test coverage for them. Vault creation or mutation must fail if any credential binding `match.ports` value is not included in `transparent_intercept_ports`.

Credential-capable sidecars must always require `OPENSANDBOX_EGRESS_TOKEN` for both the egress policy API and the direct egress Credential Vault API. The application container must not receive this token.

### Observability

Credential Proxy should emit structured logs and metrics without credential values.

Suggested audit log fields:

- `sandbox_id`
- `credential_binding`
- `credential_vault_revision`
- `operation` (`create`, `runtime_add`, `runtime_replace`, `runtime_delete`, `inject`)
- `actor_id` for egress API callers when available
- `credential_source_type`
- `target_host`
- `method`
- `path_pattern`
- `decision` (`injected`, `denied`, `no_match`, `ambiguous_match`, `source_error`)
- `status_code`
- `duration_ms`
- `request_id`

Suggested metrics:

- `opensandbox_credential_proxy_requests_total`
- `opensandbox_credential_proxy_injections_total`
- `opensandbox_credential_proxy_denials_total`
- `opensandbox_credential_proxy_source_errors_total`
- `opensandbox_credential_vault_mutations_total`
- `opensandbox_credential_vault_mutation_failures_total`
- `opensandbox_credential_proxy_request_duration_seconds`

All diagnostics APIs that surface runtime logs must preserve redaction behavior.

### Component Changes

#### Specs

- Add Credential Vault create, read, delete, and mutation schemas to `specs/egress-api.yaml`.
- Add examples for normal sandbox creation with `credentialProxy.enabled`, egress endpoint resolution, and direct `POST /credential-vault`.
- Add Credential Vault metadata reads and atomic mutation APIs for adding, replacing, and deleting sandbox-local credentials and bindings.
- Add examples for vault create, runtime add, replace, and delete flows, including acknowledged revision responses and failed proxy acknowledgement errors.

#### Server

- Ensure Docker and Kubernetes runtimes can expose the egress sidecar endpoint and egress auth header through the existing endpoint API.
- Ensure Docker and Kubernetes runtimes expose egress transparent mitmproxy, egress API auth, and credential addon support before callers can successfully create a Credential Vault.
- Do not add lifecycle Credential Vault handlers in the MVP; Credential Vault state is owned by the egress sidecar.

#### Components / Egress

- Extend `components/egress` transparent mitmproxy support with a first-party credential addon.
- Add egress sidecar config for `[egress.credential_vault]`, including enablement, strict egress validation, and transparent intercept ports.
- Keep transparent MITM redirect ports configurable. The default credential-enabled intercept set is `80/443`; additional ports require explicit operator configuration and matching redirect rules.
- Expose an egress Credential Vault API protected by egress API auth.
- Apply runtime binding revisions through immutable snapshots and atomic swap; keep the previous acknowledged revision active on reload failure.
- Implement binding evaluation, header injection, redaction, and audit events in the mitm addon path.
- Keep the existing system addon behavior for streaming.
- Reject sandbox-supplied mitm addon loading for credential-enabled sandboxes. Operator-configured trusted addons may run as part of the platform trust boundary.
- Reject Credential Vault creation when transparent redirect, credential addon loading, CA bootstrap, upstream TLS verification, IPv6 coverage/disablement, or egress API auth is unavailable.
- Expose a readiness signal that means redirect rules, mitmproxy, credential addon loading, CA material, upstream TLS verification, IPv6 handling, and egress API auth are all ready before application startup is released.

#### Kubernetes

- Ensure the egress sidecar can run transparent mitmproxy and accept the egress Credential Vault API before `POST /credential-vault` succeeds.
- Deliver initial vault creation and runtime add, replace, and delete operations through the egress Credential Vault API as complete candidate snapshots.
- Do not require Kubernetes Secrets for Credential Vault creation or mutation delivery.
- Ensure Credential Proxy has no broad Kubernetes API permissions by default.
- Ensure the mitmproxy CA is trusted by the sandbox application container when HTTPS interception is enabled.
- Ensure generated egress API auth material is returned only through the endpoint resolution flow and is not injected into the application container.
- Add an init/startup gate so application containers do not run user code until the egress sidecar readiness signal is ready for credential-enabled sandboxes.

#### Docker

- Enable the egress sidecar with transparent mitmproxy sharing the sandbox network namespace.
- Ensure the mitmproxy CA is trusted by the sandbox application container when HTTPS interception is enabled.
- Deliver initial vault creation and runtime add, replace, and delete revisions through the direct egress Credential Vault API protected by egress API auth.
- Create, rotate, and clean up sandbox-scoped credential material for vault creation, runtime add, replace, delete, and sandbox deletion.
- Ensure generated egress API auth material is not exposed to the application container.
- Start or release the sandbox application container only after the egress sidecar readiness signal is ready for credential-enabled sandboxes.

#### SDKs and CLI

- Add sandbox-scoped Credential Vault facades such as `sandbox.credentialVault` in TypeScript/JavaScript and `sandbox.credential_vault` in Python.
- Expose sandbox creation `credentialProxy` / `credential_proxy` only as the explicit startup opt-in for credential-capable transparent MITM; do not accept Credential Vault content there.
- Add typed request and response models for credentials, bindings, vault state, vault revisions, and vault patch requests.
- Add client helpers for resolving the egress endpoint, merging endpoint auth headers, and calling Credential Vault APIs directly on that endpoint.
- Expose `create`, `get`, `patch`, `delete`, `listCredentials`, `getCredential`, `listBindings`, and `getBinding` operations.
- Do not expose SDK convenience write helpers for credential or binding subresources in the MVP; all credential and binding writes use the vault-level `patch` operation.
- Add examples for common providers such as GitHub and model APIs.
- CLI may include validation helpers, but it should not print credential values.

## Test Plan

### Unit Tests

- Schema validation accepts valid bindings and rejects bindings missing `name`, `match`, or typed `auth`.
- Schema validation rejects duplicate credential names and duplicate binding names.
- Schema validation rejects binding auth references to unknown credential names, including `customHeaders.headers[].credential`.
- Schema validation accepts `customHeaders` with multiple header entries and rejects duplicate header names within one auth rule.
- Schema validation rejects invalid, routing, framing, hop-by-hop, and proxy-control header names in `apiKey` and `customHeaders`.
- Scheme, port, FQDN, wildcard, method, and path matching work as expected.
- Injection defaults to HTTPS/443 only and rejects HTTP injection unless explicitly configured and permitted.
- Binding matches that use ports outside configured transparent MITM intercept ports are rejected.
- Multiple matching bindings fail closed.
- Existing headers with injected names, including names from `customHeaders`, are replaced or rejected according to the selected implementation rule.
- Redaction removes credential values from logs and errors.
- Response redaction removes known credential values from response headers.
- Inline credential `value` is accepted only as write-only Credential Vault create or runtime mutation input and never appears in serialized sandbox metadata or API responses.
- Egress validation requires `networkPolicy.egress`, catches binding hosts not covered by explicit allow rules, and does not treat `defaultAction=allow` as credential binding coverage.
- The egress Credential Vault API rejects candidate revisions that conflict with the sidecar's effective egress policy.
- Runtime egress policy mutations reject updates that would make active credential binding hosts non-allowed.
- `POST /credential-vault` rejects duplicate credential and binding names, rejects unknown credential references, fails when the vault already exists, and returns success only after the initial revision is acknowledged by Credential Proxy.
- Credential and binding subresources expose read-only list/get APIs; all add, replace, and delete operations use `PATCH /credential-vault`.
- Vault-level atomic PATCH rejects duplicate credential and binding names, replace rejects unknown names, delete rejects unknown names, and one request cannot mention the same name in multiple operations.
- Runtime credential delete rejects credentials that are still referenced by active bindings.
- `PATCH /credential-vault` rejects requests when the vault does not exist.
- Runtime mutations reject post-mutation binding sets that create ambiguous matches.
- Runtime mutation revision handling keeps the previous acknowledged revision active when proxy acknowledgement fails and cleans up candidate runtime material prepared for the failed revision.
- SDK facades resolve the egress endpoint, merge endpoint headers, call the sidecar directly, expose `CredentialAuth` shapes that match the egress API wire schema, do not expose credential or binding subresource write helpers, and only expose `credentialProxy` / `credential_proxy` on sandbox creation as a startup opt-in.

### Integration Tests

- Docker sandbox with inline ephemeral source can call a mock HTTP/HTTPS server and cannot recover the inline credential from environment, filesystem, diagnostics, or Credential Vault read responses.
- Docker sandbox cannot read credential value from environment variables, mounted files, egress API response, or diagnostics.
- Kubernetes sandbox with inline ephemeral source delivers the vault revision through the egress Credential Vault API without requiring generated Kubernetes Secrets, and cleans up sandbox-scoped runtime material on sandbox deletion.
- Kubernetes runtime rejects requests that try to reference arbitrary pre-existing Kubernetes Secret names as credential sources.
- Running Docker and Kubernetes sandboxes can create a Credential Vault through the egress API, receive an acknowledged initial revision, and use it for subsequent matching requests.
- Running Docker and Kubernetes sandboxes can replace a credential binding and observe that subsequent requests use the replacement while prior credential material is cleaned up.
- Running Docker and Kubernetes sandboxes can delete a credential binding and observe that subsequent matching requests no longer receive injected credentials, then delete the now-unused credential material.
- Runtime mutation requests fail without activating partial metadata when Credential Proxy acknowledgement fails or times out.
- Runtime mutation requests that fail after material preparation clean up files or handles for the inactive revision.
- Sandbox application containers cannot call runtime credential mutation APIs or access the egress API token used for proxy reload.
- Credential Proxy denies non-matching hosts, paths, and methods.
- The egress sidecar rejects Credential Vault creation when a binding target is denied by the current effective egress policy.
- Credential Vault creation fails when transparent redirect, mitm readiness, CA bootstrap, credential addon loading, or egress API auth cannot be configured.
- Credential-enabled sandbox code cannot run before the egress sidecar readiness signal is ready.
- Credential Vault creation rejects a binding that targets a non-configured port, and accepts the same port only after the operator configures transparent MITM interception for that port.
- Credential-capable sidecars require egress API auth for the Credential Vault API.
- Sandbox-supplied mitm addons are not loaded for credential-enabled sandboxes; operator-configured trusted addons may run.
- Credential Vault creation fails when IPv6 is enabled without equivalent transparent redirect coverage.
- Credential Vault creation rejects upstream TLS-insecure mode.
- Credential Vault creation rejects bindings that match `ignore_hosts`.
- Cross-host redirect strips or re-evaluates injected credentials.
- Sandbox deletion cleans up Credential Proxy and any sandbox-scoped credential material.

### E2E Tests

- Create a sandbox with `networkPolicy.defaultAction=deny`, allow `api.github.com`, resolve the egress endpoint, call `POST /credential-vault` with a read-only GitHub credential and binding, and verify a normal `https://api.github.com/...` call succeeds through Credential Proxy.
- Attempt a runtime credential or binding add before `POST /credential-vault` and verify it fails because the vault does not exist.
- Create a Credential Vault with empty initial credentials and bindings, add a read-only GitHub credential and binding at runtime, and verify subsequent normal `https://api.github.com/...` calls succeed through Credential Proxy.
- Replace the runtime GitHub credential with an invalid value and verify subsequent matching requests fail authentication without exposing either credential in logs or egress API responses.
- Delete the runtime GitHub credential binding and verify subsequent matching requests are not credentialed.
- Verify direct access to a non-allowed domain fails under egress policy.
- Verify logs and diagnostic APIs never contain the credential string.
- Verify a mock upstream that echoes request headers does not expose known credential values in supported response headers.
- Verify response header redaction covers both raw and rendered injected credential values.

## Drawbacks

- Requires enabling transparent MITM for credential-bearing HTTP/HTTPS traffic.
- Adds a new credential-aware API and path inside egress.
- Runtime credential mutation adds revision, acknowledgement, rollback, and cleanup complexity to the egress sidecar.
- Requires stricter startup behavior than ordinary egress policy; credential-enabled sandboxes fail closed instead of gracefully degrading when transparent interception is unavailable.
- Keeps MITM addons inside the operator/platform trust boundary; sandbox-supplied addons remain unsupported for credential-enabled sandboxes.
- Cannot provide an absolute secrecy guarantee against arbitrary upstream services that echo credentials in unsupported response encodings or protocols.
- Users may confuse Credential Vault with a full secret management system.
- Debugging outbound requests becomes more complex because credentials are injected outside the application process.
- Header injection covers common API use cases but not all credential workflows, such as SSH private keys or database passwords.

## Alternatives

### Inject Secrets as Environment Variables

This is simple and already common, but it exposes plaintext credentials to the sandbox process. It does not satisfy the primary security goal.

### Mount Secrets as Files

This avoids environment variable leakage but still exposes plaintext credentials to sandbox processes. Agents can read, print, copy, or upload the files.

### Rely Only on External Secret Managers

External secret managers are still needed as sources, but sandbox workloads would need secret manager credentials to fetch secrets directly. That moves the same exposure problem into a different API.

### SDK-only Credential Clients

SDK mediation can be safer and more structured, but it requires language-specific client changes and does not cover existing CLIs, package managers, curl, git-over-HTTPS, or arbitrary agent-generated code. Credential Proxy works at the runtime egress boundary.

### Explicit Proxy or Local Gateway First

Explicit proxy and local gateway modes avoid transparent network interception, but they require application configuration and do not match the current OpenSandbox egress direction. The existing egress transparent mitmproxy path already provides the correct runtime interception point for Credential Proxy.

## Infrastructure Needed

- No new Credential Proxy component image for the MVP; Credential Proxy is implemented in the existing egress image through transparent mitmproxy and a first-party credential addon.
- An egress Credential Vault API for initial vault creation and runtime binding revisions, protected by egress API authentication.
- CI tests for Docker and Kubernetes runtime paths.
- Documentation and examples for common credential binding patterns.
- Sandbox image or runtime support for trusting the OpenSandbox mitmproxy CA.

No new required external service is introduced by the MVP.

## Upgrade & Migration Strategy

Credential Vault is opt-in. Existing sandbox creation requests, SDK calls, egress policies, and runtime behavior remain unchanged unless callers invoke the explicit egress Credential Vault APIs.

Recommended rollout:

1. Add egress API schema and sidecar validation behind `[egress.credential_vault].enabled = false` by default.
2. Extend egress transparent mitmproxy with credential addon support and an egress Credential Vault API.
3. Add Credential Vault create and runtime mutation APIs, revision tracking, proxy acknowledgement, and rollback semantics.
4. Implement Kubernetes and Docker runtime delivery through the egress Credential Vault API.
5. Add SDK models and CLI examples.
6. Document production guidance: use `networkPolicy.defaultAction=deny`, keep credential targets narrow, avoid broad methods and paths, and monitor audit events.

No migration is required for existing users. Users currently injecting secrets through environment variables can gradually migrate by creating a sandbox normally, resolving the egress endpoint, and creating a sandbox-scoped Credential Vault through the egress API.
