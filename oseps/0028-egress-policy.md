---
title: Egress Policy
authors:
  - "TBD"
creation-date: 2026-09-18
last-updated: 2026-09-21
status: draft
---

# OSEP-0028: Egress Policy

## Goal

Define an egress policy model in which sandbox/pool policy and platform controls
govern DNS, while tenant and sandbox/pool policies jointly govern outbound
HTTP/HTTPS requests and their authentication. Provide the same security guarantees
for directly created and pool-allocated sandboxes.

**Single-replica scope:** Network policy is supported only for `BatchSandbox`
resources with `spec.replicas: 1`, for both direct creation and pool allocation.
Network-policy-enabled creation or update requests with multiple replicas are
rejected. Multi-replica network-policy support is out of scope, not merely live
policy updates.

Rate and concurrency limits, AI-specific traffic controls, and usage quotas are
outside the scope of this draft.

1. **Establish deny-by-default tenant request guardrails.** Let administrators
   approve HTTP/HTTPS destinations and constrain protocols, ports, methods, and
   paths. Application requests not explicitly permitted are denied.
2. **Allow sandbox policies to narrow, never broaden, tenant access.** Compute
   effective HTTP/HTTPS access from the intersection of tenant and sandbox/pool
   policies. An omitted sandbox network policy or an empty egress rule list defaults
   to denying workload DNS lookups and application requests rather than inheriting
   tenant permissions. Tenant request-policy changes govern both new and existing
   sandboxes.
3. **Authorize authentication per route.** Let tenant policy specify permitted
   authentication sources and provider scopes. Let a sandbox select an approved
   identity binding or credential-vault binding for each route, without allowing
   that selection to expand the route's permitted destinations or operations.
4. **Keep platform-managed credentials outside workloads.** Use trusted egress
   components to acquire or inject credentials only for an authorized destination
   and request. Keep credential values out of policy resources, sandbox processes,
   API responses, logs, and traces. Keep public policy intent provider-neutral.
5. **Support policy-governed pool allocation.** Let pool templates declare egress
   restrictions within tenant request guardrails. Let allocation select approved
   authentication bindings without replacing the pool's network policy. Prevent
   credentials, bindings, and authorized connections from leaking across sandbox
   assignments.
6. **Enforce consistently and fail closed.** Validate policy before admitting a
   workload and enforce the applicable DNS and application controls outside the
   untrusted workload boundary. Prevent bypass of network and authentication controls;
   reject unsupported or ambiguous constraints rather than silently weakening
   them. Deny affected traffic when required policy or authentication state cannot
   be established.
7. **Make policy decisions and rollout observable.** Expose effective policy,
   revision and enforcement status, and actionable denial reasons. Audit policy
   changes and access decisions with tenant and sandbox attribution without
   exposing credentials.

## User Experience

All management requests
use `OPEN-SANDBOX-API-KEY` and apply to the caller's tenant; users do not supply a
tenant field in the request body.

- **Infrastructure provider:** makes approved identity-provider configurations,
  such as `azure`, available to users.
- **Tenant/platform administrator:** registers approved identities and configures
  the tenant's HTTP/HTTPS guardrails.
- **Sandbox user:** creates a sandbox with narrower rules, or selects a pool and
  approved bindings, without handling platform-issued access tokens.

These examples create one sandbox per request. A pool can serve multiple
independent sandbox allocations, but network policy is not supported for
multi-replica batch requests.

### Register an Approved Identity

An administrator registers an existing identity with
`POST /v1/identity-bindings`:

```json
{
  "name": "reports-reader",
  "provider": "azure",
  "identity": {
    "type": "systemIdentity",
    "resourceId": "/applications/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
  }
}
```

`provider` selects an approved identity-provider configuration by its exact name,
here lowercase `azure`. Use the same name in the tenant policy's identity source.
In this Azure example, `systemIdentity` refers to an existing Entra application.

The administrator uses `GET /v1/identity-bindings/reports-reader` to observe
validation and provider readiness; `GET /v1/identity-bindings` lists the tenant's
bindings. Registration may remain pending or fail with an actionable reason. The
binding must be ready before requests can use it. The sandbox user selects the
binding by name, without receiving its credentials.

### Configure the Tenant Guardrail

The administrator replaces the tenant's policy with `PUT /v1/egress-policy`:

```json
{
  "egress": [
    {
      "name": "reports-read",
      "target": "reports.example.com",
      "protocol": "https",
      "ports": [443],
      "methods": ["GET"],
      "paths": ["/v1/*"],
      "auth": {
        "sources": [
          {
            "type": "identity",
            "provider": {
              "type": "azure",
              "scope": "api://reports/.default"
            }
          },
          { "type": "credentialVault" }
        ]
      }
    },
    {
      "name": "events-write",
      "target": "events.example.com",
      "protocol": "https",
      "ports": [443],
      "methods": ["POST"],
      "paths": ["/v1/events"]
    }
  ]
}
```

Unmatched requests are denied; only allow rules are supported for now.
`auth.sources` is always a list, including when only one source is permitted.
The reports rule permits an identity using `azure` or a credential-vault
binding; it does not select a particular binding. The events rule has no `auth`
and does not request platform-managed authentication. Neither rule alone grants
sandbox access: the sandbox/pool must also allow the request.

`GET /v1/egress-policy` returns the configured policy and its status. Tenant changes
apply to existing and future sandboxes without users recreating them or resubmitting
their individual policies.

### Create or Update a Direct Sandbox

The user supplies a narrower policy when calling `POST /v1/sandboxes`:

```json
{
  "image": { "uri": "python:3.11" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "500m", "memory": "512Mi" },
  "timeout": 3600,
  "networkPolicy": {
    "defaultAction": "deny",
    "egress": [
      {
        "name": "reports-read",
        "action": "allow",
        "target": "reports.example.com",
        "protocol": "https",
        "ports": [443],
        "methods": ["GET"],
        "paths": ["/v1/reports/*"],
        "auth": {
          "source": { "type": "identity", "name": "reports-reader" }
        }
      }
    ]
  }
}
```

`auth.source.name` selects a binding name in the caller's tenant. The user
cannot override the tenant-approved token scope or supply a secret in the policy.

For a standalone sandbox, including one detached from a pool during pause, the
proposed update operation is
`PUT /v1/sandboxes/{sandboxId}/networkpolicy`, with a body containing the complete
replacement `networkPolicy` object. Omitted rules are removed rather than merged.
`GET /v1/sandboxes/{sandboxId}/networkpolicy` returns the configured policy and its
status. Creation without `networkPolicy`, or an explicit empty `egress` list,
denies workload DNS lookups and outbound requests rather than inheriting tenant
permissions.

For vault authentication, the source can instead be
`{"type":"credentialVault","name":"reports-token"}` when the tenant permits it.
The user may select this name before the vault binding is provisioned through
credential-vault management. The policy is accepted but remains pending, with
affected egress blocked, until the selected vault binding exists, is authorized,
and is ready; provisioning it later does not require resubmitting the policy.
Identity selections instead require an existing, valid, administrator-approved
binding; unknown, unauthorized, or incompatible identity selections are rejected.
No credentialed request is allowed before its selected binding is ready. Vault
secret values stay outside these policy requests and responses.

### Create and Use a Pool

An authorized pool creator supplies the shared rules once with
`POST /v1/pools`:

```json
{
  "name": "reports-pool",
  "template": {
    "spec": {
      "containers": [
        {
          "name": "sandbox",
          "image": "python:3.11",
          "command": ["tail", "-f", "/dev/null"]
        }
      ]
    }
  },
  "capacitySpec": {
    "bufferMax": 3,
    "bufferMin": 1,
    "poolMax": 10,
    "poolMin": 0
  },
  "networkPolicy": {
    "egress": [
      {
        "name": "reports-read",
        "target": "reports.example.com",
        "protocol": "https",
        "ports": [443],
        "methods": ["GET"],
        "paths": ["/v1/reports/*"],
        "auth": { "source": { "type": "identity" } }
      }
    ]
  }
}
```

The pool rule declares the required authentication type, but not a binding name.
Every sandbox allocated from the pool uses these rules. Omitting the pool's
`networkPolicy` also denies workload DNS lookups and outbound requests.

The sandbox user then calls `POST /v1/sandboxes`:

```json
{
  "timeout": 3600,
  "extensions": { "poolRef": "reports-pool" },
  "egress": {
    "authBindings": [
      { "bindingName": "reports-reader", "ruleName": "reports-read" }
    ]
  }
}
```

`networkPolicy` is rejected in a pool allocation request. Each binding selection
has exactly `bindingName` and `ruleName`; select a binding compatible with the
named rule's authentication type. Different allocations can select different
bindings while sharing the same pool rules. For a credential-vault rule, the user
may select a name before that vault binding exists; the allocation's egress stays
pending until the binding is authorized and ready, without blocking other
allocations using ready bindings.

The pool's rules remain immutable. Each allocation's `egress.authBindings` is also
immutable while the sandbox remains pool-backed.
Changing rules requires a new pool; changing selections requires a new sandbox
allocation, which may use the same pool. An initially omitted or empty list cannot be
populated later. Creating a vault binding later under its already-selected name
does not change the selection. If a selected binding is no longer ready or
approved, requests requiring it fail.

### Observe Enforcement and Manage Changes

Once the policy is active and the selected binding is ready, applications make
ordinary HTTP/HTTPS requests without acquiring provider tokens themselves.
For example, from either sandbox
created above:

```bash
curl https://reports.example.com/v1/reports/daily
```

Applications must use the platform-provided HTTPS trust configuration. The example
request uses the selected reports identity without exposing its access token to
the application. In these examples, `POST` to the reports API, `GET /v1/admin`, and
requests to `events.example.com` are denied because the sandbox/pool rules do not
allow them, even where tenant policy permits them.

A successful create or update response does not necessarily mean the requested
policy is active. Users can check status to distinguish pending, failed, and
active enforcement, with actionable reasons that do not expose secrets. A policy
status of `status.rollout.state: enforcing` means the requested policy is active.
Affected requests remain blocked while a required policy or binding is not ready;
there is no temporary unrestricted access.

| User action | Expected experience |
| --- | --- |
| Update tenant policy | Existing sandboxes use the latest tenant guardrail without users recreating them or resubmitting individual policies. |
| Update a standalone sandbox policy | Supported for a directly created sandbox or one detached from a pool during pause; affected requests remain blocked until the updated policy is active. |
| Change pool rules or pooled binding selections | Rejected; create a new pool or sandbox allocation, respectively. |
| Select an unknown, unauthorized, or incompatible identity binding | Rejected; identity selections require an existing, valid, approved binding. |
| Select a credential-vault binding that does not exist or is not ready yet | Accepted, but the policy or affected pooled allocation's egress remains pending until that binding is authorized and ready. The vault binding may be provisioned later without changing the selection. |
| Select an unknown rule, disallowed authentication source, or an existing unauthorized/incompatible vault binding | Rejected; deferred vault provisioning does not bypass rule, type, or permission checks. |
| Pause a sandbox | Outbound access stops; rules and binding selections are retained. A pool-created sandbox becomes standalone with its own equivalent policy for resume. Existing connections may be interrupted. |
| Resume a sandbox | Egress stays blocked until current policies and required bindings are ready. Applications reconnect; requests allowed before pause may now be denied. |
| Delete a binding with `DELETE /v1/identity-bindings/reports-reader` | Blocked while referenced or in use; remove policy references or delete consuming sandboxes first, including immutable pooled selections. |
| Delete a sandbox | Its outbound access stops; other sandbox allocations from the same pool are unaffected. |

## High-Level Architecture

The proposed architecture separates policy management from traffic enforcement.
The lifecycle server accepts and validates policy intent; Kubernetes resources
hold the desired enforcement state. Two egress policy controllers independently
compile tenant and sandbox policy into separate data-plane configurations.
Per-sandbox Envoy terminates workload-side TLS and enforces sandbox request policy.
Agentgateway enforces tenant request policy and injects authorized credentials.
Passing both checks enforces their intersection for HTTP/HTTPS requests without
routing requests through the lifecycle server.

The sandbox layer uses a `SandboxEgressPolicy` for a standalone batch, or one
shared `PoolEgressPolicy` while batches remain pool-backed. Pool allocation does
not create individual policy CRs. During pause, a pooled batch receives its own
`SandboxEgressPolicy` before detaching from the pool; resume uses that policy.
These are alternative sources for the same enforcement layer, not additional
layers to merge.

Approved external identities are registered as `SandboxIdentityBinding` objects.
Each binding names an `IdentityProviderClass`, whose `controllerName` selects the
provider controller responsible for validating the identity, reconciling
provider-side trust, and publishing resolved identity metadata and readiness.
Standalone batches select bindings by name in `SandboxEgressPolicy`; pooled
batches select them in `BatchSandbox.spec.networkPolicy.egress.authBindings`
alongside the shared `PoolEgressPolicy`. `TenantEgressPolicy` constrains permitted
provider classes and scopes. Credentialed requests require the latest binding
generation to be ready and authorized. The workload authorization service generates
tokens: it fetches credential material from the vault for `credentialVault` bindings
or invokes the binary named in `IdentityProviderClass` for `identity` bindings.
Agentgateway only injects the authorized token; the provider controller manages
identity setup rather than request-time token issuance.

This section describes the target Kubernetes deployment, not the current runtime
implementation. The detailed design below proposes Kubernetes resource storage;
final API schemas and configuration-distribution wire formats remain separate
design decisions.

### Controller Responsibilities

Use two logical egress policy controllers, which may run in the same
controller-manager process and share policy validation, normalization, and
compilation helpers. Separate controllers do not require separate services or
different policy semantics. Provider controllers reconcile identity bindings
independently; they do not compile either egress policy layer.

| Controller | Watches | Owns and generates |
| --- | --- | --- |
| Tenant egress policy controller | Tenant egress policy and registration of its data-plane consumers. | Tenant request policy for Agentgateway and authentication-source and scope constraints for the authorization service. Owns tenant-layer rollout status; does not configure the director or sandbox DNS rules. |
| Sandbox egress policy controller | `SandboxEgressPolicy`, `PoolEgressPolicy`, referenced bindings, and batch/pool consumer lifecycle. | Batch or shared pool request policy with per-route binding context for Envoy, and DNS constraints for the director. Creates the standalone policy during pooled pause handoff. Tracks required and acknowledged policy UID/generation in trusted sandbox registration/readiness state for gateway checks, rather than generating a separate binding or destination-metadata payload. Owns sandbox-layer rollout status and cleanup; no separate pool egress controller is needed. |
| Identity provider controller | `SandboxIdentityBinding` objects whose `IdentityProviderClass` selects this controller, and their referenced classes. | Validates identity registration and reconciles provider-side trust, such as Azure federation for the workload authorization service's ServiceAccount. Publishes non-secret resolved identity and readiness in binding status for egress components to consume. Owns provider-side cleanup once bindings are no longer referenced or in use; does not grant network access or issue request-time tokens. |

The DNS proxy in the existing egress sidecar provides the director's DNS filtering,
using only sandbox/pool rules and platform DNS controls. Retain this proxy even
when all application traffic must pass through Envoy: DNS queries can themselves
carry data, independently of subsequent HTTP/HTTPS requests. It forwards only
permitted queries to platform-configured upstream resolvers; trusted runtime
controls prevent direct DNS access that bypasses the proxy. DNS filtering reduces
this exposure but does not prevent data leakage through permitted,
attacker-controlled names or subdomains.

Tenant policy governs gateway-mediated HTTP/HTTPS traffic, not DNS queries. A name
allowed by the sandbox/pool may still resolve after tenant permission is removed,
while Agentgateway rejects application requests. This does not provide tenant-level
DNS leakage prevention. Agentgateway still checks the actual upstream hostname
and address; DNS permission alone does not authorize an application connection.

Each egress policy controller reconciles creation, permitted changes, and deletion,
and retries after delivery failures or restarts. It compiles only its
own layer, publishes versioned configuration, and records acknowledgements from
the relevant consumers. Each consumer receives only the configuration required for
its role. The complete path must have both enforcement stages ready before
workload egress is enabled. Unchanged configuration can be reused.

The separation has four rules:

1. **Separate ownership.** Each egress policy controller writes only its own
   generated policy configuration and status. They must not both replace the same
   gateway route, listener, or combined policy object. Shared proxy bootstrap
   configuration is separate from these layer-owned outputs.
2. **Sequential enforcement.** Envoy checks sandbox policy before forwarding the
   request to Agentgateway, which checks tenant policy. Every HTTP/HTTPS request
   must pass both stages with the same normalized destination, method, and path. The
   authorization service is called only for routes requiring managed credentials,
   to check tenant authentication constraints against the sandbox's approved binding
   selection. Policy intersection does not depend on merging both HTTP rule sets
   inside one proxy.
3. **Independent updates.** A tenant change recompiles and distributes only the
   tenant layer to Agentgateway and the authorization service. It does not rewrite
   sandbox/pool policy, update Envoy's sandbox HTTP rules, or redistribute DNS
   configuration to sandboxes. Sandbox-policy updates are supported only for a
   standalone `BatchSandbox` with `spec.replicas: 1`, updating its Envoy
   policy and director DNS rules, with installation readiness tracked separately.
   Network policy is not supported for multi-replica batches, including at creation.
   Pool egress rules are immutable for the pool's lifetime; allocation only adds
   consumers and their binding selections, without rewriting the shared policy.
   For now, a pooled batch's `spec.networkPolicy.egress.authBindings` is immutable
   while pool-backed; selecting different bindings requires a new `BatchSandbox`.
   Pause detachment transfers the existing selections into a standalone policy;
   it is not a live pooled-selection update.
4. **Fail-closed activation.** Consumers acknowledge the revisions they actually
   enforce. Missing, invalid, or stale required layers deny application requests.
   Missing, invalid, or stale sandbox/pool DNS configuration denies workload
   lookups. An omitted sandbox network policy is normalized to `defaultAction: deny`
   with `egress: []`; no allow rule is synthesized. An installed deny-all policy is
   valid enforcement state, unlike missing configuration. Removing tenant policy
   does not leave sandbox policy as a standalone grant of HTTP/HTTPS access.

Neither egress policy controller generates a merged effective-policy resource.
Effective HTTP/HTTPS access is evaluated from the active tenant layer and the
admitted sandbox layer. Each layer's policy resource UID and generation identify
the revision used for a decision.

### Lifecycle Server and Data Plane

```text
Administrator / Sandbox creator
              |
              v
      Lifecycle server
      tenant resolution, validation, admission metadata
              |
              v
      Kubernetes desired state
              |
              +----> Identity provider controller (selected by provider class)
              |             -> provider: identity validation and trust setup
              |             -> binding status: resolved identity and readiness
              |
              +----> Tenant egress policy controller
              |             -> Agentgateway: tenant request policy
              |             -> authorization service: tenant auth constraints
              |
              +----> Authorization service (read-only CR watches)
              |             -> binding validation and latest ready state
              |
              +----> Sandbox egress policy controller
                            -> Envoy: sandbox request policy
                            -> director: sandbox/pool DNS constraints
                            -> registration/readiness: required and installed revisions

      Data-plane acknowledgements and failures
              -> owning controller -> layer status -> lifecycle server -> caller
```

1. **Resolve and validate policy intent.** The lifecycle server derives the tenant
   and namespace from the caller's API key. For tenant policy changes, it validates
   destinations, request constraints, permitted authentication sources, and runtime
   capabilities. Desired policy and binding references are recorded in
   tenant-scoped resources without embedding credentials.
2. **Resolve the sandbox execution context.** On sandbox creation, the server
   requires a single replica for this network-policy path, validates sandbox rules,
   and checks per-route authentication selections against the tenant policy.
   A directly created batch receives its own `SandboxEgressPolicy`.
   For pool allocation, the server ensures a single `PoolEgressPolicy` for the
   resolved pool and reuses it for subsequent allocations. Rules come from the pool
   template; binding selections are recorded on the existing pooled `BatchSandbox`,
   not in a new sandbox policy CR. Either policy uses `defaultAction: deny` and
   `egress: []` when its source defines no network policy. Trusted runtime context
   identifies the applicable policy by kind, UID, and generation.
   The tenant revision used at admission is recorded in durable audit metadata,
   not in sandbox policy intent. Kubernetes admission repeats security-critical
   checks so direct resource creation cannot bypass validation.
3. **Compile and distribute the two layers.** The tenant controller compiles the
   request guardrail for Agentgateway and authentication constraints for the
   authorization service. The sandbox controller compiles the shared batch or pool
   policy into Envoy request rules and director DNS rules. Envoy routes include the
   selected binding type and name. Pool rule matchers are compiled once and reused,
   with binding context populated separately for each allocated batch. The
   authorization service validates that context and resolves current bindings
   through a read-only CR cache; no separate binding configuration is generated.
   Updates travel over authenticated control channels inaccessible to workloads.
   Unsupported constraints fail validation or compilation.
4. **Gate readiness on enforcement.** Outbound access starts denied. Required
   components must acknowledge their assigned policy and identity configuration.
   Envoy must also have workload interception trust and certificate provisioning
   ready, plus its authenticated TLS connection to Agentgateway. Each controller
   publishes its own observed status; the lifecycle server checks both policy
   stages and TLS readiness. Accepted desired state is distinguished from enforced
   state.
5. **Reconcile live changes.** The tenant controller distributes tenant changes to
   Agentgateway and authorization-service consumers serving new and existing
   sandboxes, without recompiling sandbox/pool rules or updating DNS proxies.
   Each controller tracks acknowledgements for its own changes and retries
   failures. A tightening rollout must have a bounded deadline:
   paths that cannot confirm the required revision are blocked or removed from
   service rather than retaining broader access indefinitely. Affected existing
   connections must be re-evaluated or closed within that bound.
6. **Withdraw context before reuse.** The sandbox controller coordinates with the
   runtime/pool controller to disable the departing member's access, then remove
   its installed configuration, active runtime registration, cached credentials,
   and connections. Shared batch, pool, and tenant policies remain for other members.
   A reused instance receives a fresh runtime identity and passes the same
   readiness gate.

The lifecycle server is not an outbound proxy or a per-request authorization
dependency. Installed policy may continue to serve traffic during a management
API outage only while the data plane can establish that its policy and required
identity/revocation state remain valid and sufficiently fresh.

### Data-Plane Traffic and Policy Enforcement

```text
Untrusted workload -- HTTPS connection to the requested destination
        |
        v
Egress director -- sandbox/pool DNS controls; pass workload TLS to Envoy
        |
        v
Per-sandbox Envoy -- terminate workload TLS; enforce sandbox request policy
        | mTLS carrying the inspected HTTP request and trusted context
        v
Shared Agentgateway -- enforce tenant request policy; inject credentials
        |       |
        |       +----> Workload authorization service -- authorize; generate token
        |                  | only for routes with managed credentials
        |                  +----> Credential vault -- fetch credential material
        |                  +----> Identity-provider binary -- request access token
        |                             +----> Identity provider (e.g., Entra ID)
        | HTTPS with upstream certificate verification
        v
Approved external service
```

1. **Capture outbound traffic.** The egress director intercepts DNS and routes
   workload traffic through the local Envoy. Proxy environment variables may help
   cooperative applications, but are not the security boundary. Trusted
   host/runtime network controls prevent direct external connections, alternate
   DNS paths, and access to proxy administration interfaces. The workload cannot
   change those controls or impersonate a trusted proxy. Required platform-service
   exceptions are explicit and do not provide an alternate external-egress path.
   The director filters DNS using the applicable sandbox/pool rules and platform
   DNS controls, independently of tenant policy. Allowed DNS answers must not
   install network permissions for workloads to connect directly to resolved
   external IPs. Application traffic must still traverse Envoy and Agentgateway;
   unsupported workload protocols are denied rather than forwarded around them.
2. **Terminate TLS and enforce sandbox policy.** Envoy terminates workload-side
   HTTPS, normalizes the request, and checks the destination, protocol, port,
   method, and path against the sandbox policy. A denial stops the request here.
   For an allowed request, Envoy removes workload-supplied platform metadata and
   stamps `sandboxId` and `tenantName` from trusted runtime context, plus
   `bindingType` and `bindingName` from the matched route's generated configuration
   when a binding is selected. Routes without a selection carry no binding headers.
   It forwards the inspected request over mTLS with the existing sandbox-policy
   UID/generation attestation. It neither acquires nor injects platform-managed
   service credentials.
3. **Authenticate the sidecar and enforce tenant policy.** Agentgateway terminates
   the internal mTLS connection, authenticates Envoy and its allocation context,
   and checks tenant attribution and the original destination, protocol, port,
   method, and path against its current tenant policy. Its rollout gate denies
   affected traffic when the required policy revision is missing, stale, or not
   ready, including routes without managed credentials. It does not terminate the
   original workload TLS session or repeat Envoy's full sandbox HTTP rule
   evaluation. Unknown allocations or stale sandbox-policy attestations are denied.
   Sandbox context does not pin the tenant revision selected at creation. The
   gateway verifies the actual upstream hostname and resolved address against
   tenant and platform constraints. Any supported sandbox IP/CIDR constraints must
   also be checked against the final address, using the referenced policy CR rather
   than a compiled destination-metadata payload. Rules requiring an unavailable
   gateway check are rejected, not silently weakened. DNS permission alone does
   not authorize a request.
4. **Authorize and generate the token when required.** Only routes whose tenant
   rule has `auth` call the workload authorization service. Agentgateway sends verified
   `sandboxId`, `tenantName`, `bindingType`, and `bindingName`, alongside the original
   normalized request. The service verifies the active sandbox registration and
   the supplied binding against the sandbox's current approved selections, then
   checks the latest tenant rule's permitted source, provider class, and scope.
   Envoy attests which sandbox route matched; the service does not re-match sandbox
   routes to choose a binding. `ruleName` is a pool configuration lookup key, not
   authorization metadata or a tenant-rule selector. Identity binding names resolve
   within the tenant namespace to their
   latest provider-reconciled state. If the current binding generation is not ready,
   the request is denied rather than using an older registration. The service
   generates the token according to the verified `bindingType`:
   - `credentialVault`: fetch the authorized credential material from the vault
     using the selected binding, then create the token required for this request.
   - `identity`: resolve the binding's `IdentityProviderClass` and invoke its
     `spec.tokenProviderBinary` with the resolved identity, approved class
     parameters, and tenant-approved scope. For Azure, the binary exchanges the
     authorization service's projected ServiceAccount JWT with Microsoft Entra ID
     for an access token.

   The service returns the resulting token only to Agentgateway over the protected
   internal authorization channel. Agentgateway injects it into the approved
   upstream request; it neither fetches vault material nor invokes provider
   binaries. Neither flow exposes credentials to Envoy or the workload. Vault
   retrieval or token-generation failures deny the credentialed request.
   Agentgateway removes or rejects conflicting workload-supplied credential
   headers. A failed binding does not
   fall back to another source unless policy explicitly permits it. Routes without
   `auth` skip this service and credential injection, but still pass both
   request-policy stages and Agentgateway's freshness checks. Authorization-service
   unavailability blocks credentialed routes, not otherwise-ready unauthenticated
   routes.
5. **Forward to the approved upstream.** Credentials are injected only after
   required checks succeed and only over a verified HTTPS upstream connection.
   HTTPS forwarding validates the upstream server certificate for the original
   approved origin. Agentgateway does not rewrite the
   approved destination or path or automatically follow redirects. A redirect is
   returned to the workload; any follow-up request passes through Envoy and
   Agentgateway again. Credentials never carry over to a different origin.
6. **Return results and record decisions.** Responses return through the proxy
   chain. Enforcement components emit correlated, redacted access decisions and
   actionable denials with tenant, sandbox, and policy revision attribution.
   Platform-generated responses, logs, and traces must not expose credentials.
   Telemetry export failure does not disable policy enforcement. The initial
   implementation uses [correlated access logs](#correlated-access-logs), without
   distributed tracing.

Missing, stale, or unverifiable policy, identity, or credential state denies the
affected traffic. There is no direct-egress fallback when an enforcement dependency
is unavailable.

### TLS Termination and Trust

There are three separate TLS connections for an HTTPS request: workload to Envoy,
Envoy to Agentgateway using mutual TLS, and Agentgateway to the external service.
The proxies inspect decrypted HTTP, while each network hop remains encrypted.

- **Workload trust:** the workload trusts the platform's interception CA. Envoy
  presents a leaf certificate valid for the requested destination. Platform
  certificate provisioning must support the approved destinations; generating
  policy configuration alone does not supply this capability. Missing certificates
  or failed trust validation deny the affected request.
- **Certificate ownership:** a trusted platform issuer retains interception-CA
  signing keys outside workloads and sidecars. Only Envoy receives its required
  leaf-certificate material and internal mTLS identity. Its private keys and proxy
  configuration must be inaccessible to the untrusted workload.
- **Internal authentication:** Envoy verifies Agentgateway's server identity, and
  Agentgateway verifies Envoy's identity and its association with the sandbox
  allocation. Direct workload connections and caller-supplied identity headers
  cannot substitute for this authenticated context.
- **Separate trust contexts:** interception trust, internal mTLS trust, and external
  upstream verification are configured separately. Agentgateway must not accept an
  interception certificate as proof of the external service's identity. On the
  application-traffic path, injected service credentials appear only on the
  gateway-to-upstream leg.
- **Request integrity:** both proxies use consistent destination and path
  normalization. Envoy's checked scheme, authority, port, method, and path must
  identify the same request Agentgateway authorizes and forwards. Ambiguous
  requests or configurations that would change these fields after Envoy's check
  are rejected.

HTTPS routes requiring HTTP inspection or managed credentials cannot fall back to
TLS passthrough. Workloads that cannot trust the interception CA, including clients
that require incompatible certificate pinning, cannot use those routes. Other
opaque protocols require an explicitly supported network-only path through both
enforcement stages and receive no gateway-injected credentials; unsupported
combinations are denied.

## Detailed Design

### Kubernetes Resource Model

The policy and identity model uses four tenant-scoped custom resource types and a
cluster-scoped provider-class type under `sandbox.opensandbox.io/v1alpha1`.
Each tenant maps to one namespace, resolved by the lifecycle server from the
caller's API key.
There is no caller-writable `spec.tenantId`, cross-namespace policy reference, or
label-based tenant selection. A binding may reference an approved cluster-scoped
provider class; that reference does not change its tenant ownership. Trusted
runtime identity includes the namespace UID so deleting and recreating a namespace
does not restore an old allocation's authority.

| Resource | Cardinality and lifetime | Spec writer | Status owner |
| --- | --- | --- | --- |
| `TenantEgressPolicy` | One object named `default` per tenant namespace; independent of individual sandboxes. | Lifecycle server, accepting administrator-approved tenant policy. | Tenant egress policy controller. |
| `SandboxEgressPolicy` | One object per standalone single-replica `BatchSandbox`, with the same name and namespace; created at direct creation or during pooled pause handoff. Batch deletion waits for policy deletion. | Lifecycle server for user policy intent; sandbox egress policy controller for the controller-only pause handoff. | Sandbox egress policy controller. |
| `PoolEgressPolicy` | One object per `Pool`, with the same name and namespace, shared across its allocated single-replica batches. Pool deletion waits for consumer withdrawal and policy cleanup; individual batch deletion does not delete it. | Lifecycle server, ensuring the pool's immutable policy before allocation activation. | Sandbox egress policy controller. |
| `SandboxIdentityBinding` | One object per approved binding name; reusable by allocations in the same tenant namespace. | Lifecycle server, accepting administrator-approved identity registration with an explicit provider class name. | Provider controller selected by the referenced `IdentityProviderClass`. |
| `IdentityProviderClass` | One or more named, cluster-scoped configurations created for users; multiple classes may select the same controller. Reusable by authorized tenant namespaces. | Infrastructure provider. | No status required in this draft; binding status reports provider readiness. |

Pooled batches keep allocation-specific binding selections on the existing
`BatchSandbox` resource, as described below. Ordinary pool allocation adds no
per-allocation policy CR; pause detachment creates one standalone policy for that
batch.

The lifecycle API's `provider` field maps to
`SandboxIdentityBinding.spec.providerClassName`. For direct sandbox policies,
`auth.source.name` maps to `SandboxEgressPolicy.spec.egress[].auth.source.bindingRef.name`.
The API's `networkPolicy` groups request fields; sandbox/pool policy CRs store
`defaultAction` and `egress` directly under `spec`, without that wrapper.
For pool allocations, the lifecycle API's `egress.authBindings` maps to
`BatchSandbox.spec.networkPolicy.egress.authBindings`. This internal nesting does
not permit a caller-supplied `networkPolicy` override in a pool allocation request.

The controller selected by each class's `spec.controllerName` reconciles its
identity bindings, including validation, provider setup, and publication. The
workload authorization service consumes the resolved registration; it does not
reconcile the binding. Provider controllers do not
compile either policy layer and are not additional egress-policy controllers.
Kubernetes RBAC separates spec writes from each owner's `/status` writes. Workload
service accounts cannot read or modify these resources, their status, or generated
configuration. Trusted operator writes must pass the same admission checks as
lifecycle-server writes.

The CRs are the source of truth for current desired enforcement state. Durable
management storage retains operation records, normalized revision history, and
audit metadata; it is not a competing active-policy store. Controllers watch CRs,
not database rows. Idempotent management operations reconcile partial database/CR
writes, and the API distinguishes a recorded operation, persisted desired state,
and acknowledged enforcement. Generated proxy configuration is rebuildable output,
not another policy-authoring surface. No merged effective-policy CR is introduced.

The target deny-by-default behavior and controller-owned updates differ from the
current allow-all omission default and direct-sidecar mutation API. Their rollout
requires an explicit lifecycle API compatibility/migration plan; the proposed
experience does not change the existing public contracts.

#### Credential Transport Validation

Every egress rule containing `auth` must explicitly set `protocol: https`.
An HTTP rule or a rule with an omitted protocol cannot request managed credentials,
even if its port is `443`. Rules without `auth` may still permit HTTP or HTTPS.

For `TenantEgressPolicy`, `SandboxEgressPolicy`, and `PoolEgressPolicy`, attach the
following CEL validation to the `spec.egress.items` schema, alongside the declared
`auth` and `protocol` properties. Here `self` is one egress rule; Kubernetes rejects
violations on resource creation and update without a custom webhook for this check:

```yaml
x-kubernetes-validations:
  - rule: "!has(self.auth) || (has(self.protocol) && self.protocol == 'https')"
    message: "Egress rules with auth must explicitly set protocol to https."
```

The lifecycle server mirrors this constraint when accepting tenant-policy and
sandbox/pool `networkPolicy` input, before persisting intent. CRD validation also
protects direct Kubernetes writes. Pool-allocation `authBindings` selects bindings,
not rules; validate the shared pool rules instead. Pool rules remain immutable.

CEL checks policy fields, not upstream TLS or referenced resources. Controllers
must compile authenticated routes with HTTPS upstream transport, and Agentgateway
must verify the upstream certificate before forwarding managed credentials.
Binding authorization and other cross-resource checks remain separate.

#### Binding Admission and Readiness

Validate binding selections according to their source type, both in lifecycle API
requests and Kubernetes admission:

- **Identity:** the named `SandboxIdentityBinding` must already exist in the
  tenant namespace, be valid and approved for the caller, and match the rule's
  source/provider requirements. Missing, invalid, unauthorized, or incompatible
  identity selections are rejected rather than accepted as future references.
- **Credential vault:** a same-namespace binding name may be selected before its
  vault binding exists or becomes ready. Admission still checks that the tenant
  permits this source and rejects an existing unauthorized or incompatible
  binding. Acceptance of an unresolved name grants no credential access; the
  resolved binding must pass authorization and readiness checks before activation.

A `SandboxEgressPolicy` with a missing or unready vault binding remains
`Ready=False` with reason `CredentialVaultNotReady`; affected egress stays blocked.
For pooled sandboxes, this condition gates the affected batch's egress activation,
not the shared `PoolEgressPolicy` or other allocations. The selected vault name is
still immutable; later provisioning does not add or change `authBindings`.

The sandbox egress policy controller tracks these references even while unresolved.
Vault creation and readiness changes trigger reconciliation of the referencing
policies or allocations. It revalidates the binding and activates egress only when
the binding is authorized and ready and all existing enforcement-readiness checks
pass. Later loss of readiness or authorization blocks affected traffic again; no
fallback credential is selected.

The controller-only pause handoff preserves already-admitted selections rather
than granting new ones. It does not bypass current binding authorization or
readiness checks before resumed traffic is activated.

### TenantEgressPolicy

The singleton stores the tenant's live HTTP/HTTPS guardrail, not DNS constraints.
It has no sandbox owner reference and is not copied into each sandbox policy.
Admission permits only the name `default`, avoiding policy selection or precedence
between multiple tenant objects.
A missing or deleting singleton denies tenant application traffic; it does not
mean allow all. DNS remains governed by sandbox/pool policy and platform controls.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: TenantEgressPolicy
metadata:
  name: default
  namespace: tenant-a
spec:
  defaultAction: deny
  egress:
    - name: reports-read
      action: allow
      target: reports.example.com
      protocol: https
      ports: [443]
      methods: [GET]
      paths: [/v1/*]
      auth:
        sources:
          - type: identity
            provider:
              type: azure
              scope: api://reports/.default
          - type: credentialVault
    - name: events-write
      action: allow
      target: events.example.com
      protocol: https
      ports: [443]
      methods: [POST]
      paths: [/v1/events]
```

- **Request rules:** `egress` stores named allow rules over destination, protocol,
  port, method, and path. Rule names are unique within the object. The initial
  model fixes `defaultAction` to `deny` and supports only `action: allow`; an empty
  rule list denies all application requests. The shared validator normalizes
  matches and rejects unsupported constraints and overlapping rules with
  conflicting authentication requirements rather than relying on rule order.
- **Authentication contract:** `auth.sources` lists permitted credential source
  types. Each identity source specifies `provider.type` and provider-specific
  token parameters, such as `provider.scope` in the Azure example. The tenant
  supplies these requirements; a sandbox cannot override the provider or token
  scope/audience. Registered identity bindings remain subject to those route
  constraints. Omitting `auth` does not authorize automatic credential injection.
  The `events-write` rule skips `extAuth`, binding lookup, and token acquisition;
  Agentgateway still enforces tenant identity, request rules, and policy freshness.
  A sandbox cannot request platform-managed credentials for that rule.
  Rate limits and AI-specific fields are not part of this model.
- **Provider dispatch:** for an identity source, `provider.type` names the required
  `IdentityProviderClass`. Here `azure` selects the class named `azure`. The selected
  binding's `spec.providerClassName` must equal that value, and its resolved class
  must still be valid. Admission rejects class-name mismatches, and request-time
  authorization repeats the check. Matching only a controller or token-provider
  implementation is insufficient. The class's `spec.controllerName` selects the
  identity controller, while `spec.tokenProviderBinary` names the approved binary
  invoked by the workload authorization service. Tenant and sandbox policies
  cannot supply an executable path or command. After authorization, the service
  invokes that binary with the binding's resolved identity, approved class
  parameters, and tenant-approved scope, not a service-wide default identity or
  scope. The binary must support these per-binding and per-route inputs. Unknown
  classes, unsupported token parameters, unavailable binaries, or
  token-creation failures deny the authenticated request rather than silently
  switching classes or providers.
- **Live updates:** replace validated spec using Kubernetes optimistic concurrency.
  The tenant controller compiles the new generation into Agentgateway request
  policy and authorization-service authentication constraints. It does not modify
  `SandboxEgressPolicy` or `PoolEgressPolicy` objects, Envoy sandbox HTTP rules, or
  director DNS configuration. A rollback is a new generation, not restoration of
  an old revision identifier.

#### Gateway API Reconciliation

After a `TenantEgressPolicy` is created or updated, its controller validates the
spec and reconciles generated Kubernetes resources. The Agentgateway controller
translates those resources into data-plane configuration; the tenant controller
does not push proxy configuration directly or modify Envoy's sandbox policy.

```text
TenantEgressPolicy -> tenant egress policy controller
                  -> HTTPRoute + AgentgatewayBackend + AgentgatewayPolicy
                  -> Agentgateway controller -> Agentgateway data plane
```

The platform owns the shared `GatewayClass`, `Gateway`, internal mTLS listener,
and gateway ServiceAccount. Its listener's `allowedRoutes` admits only approved
tenant namespaces. The platform also provisions the authorization Service, its
ServiceAccount and protected transport, and approved token-provider binaries,
plus a narrowly scoped `ReferenceGrant` in
`opensandbox-system` allowing `AgentgatewayPolicy` objects from `tenant-a` to
reference that Service. These bootstrap resources are not tenant-policy outputs.

For each of the `reports-read` and `events-write` rules above, the controller
generates three objects in `tenant-a`: an `HTTPRoute` for request matching, a static
`AgentgatewayBackend` for the approved destination, and an `AgentgatewayPolicy` for
upstream TLS. Only rules with `auth`, such as `reports-read`, also configure
fail-closed `extAuth`; the `events-write` policy is TLS-only. The latter two kinds
are Agentgateway extensions, not standard Gateway API kinds. There are six objects,
one set per tenant rule, not per sandbox. Each object receives an owner reference to
`TenantEgressPolicy/default` and source-policy UID/generation metadata, omitted here
for brevity. These revisions track reconciliation and audit, not a tenant-policy
version selected by an authorization request.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tenant-egress-reports-read
  namespace: tenant-a
spec:
  parentRefs:
    - name: sandbox-egress
      namespace: opensandbox-system
      sectionName: internal-mtls
  hostnames: [reports.example.com]
  rules:
    - matches:
        - method: GET
          path:
            type: RegularExpression
            value: '^/v1/.*$'
          headers:
            - name: x-opensandbox-tenant
              type: Exact
              value: tenant-a
      backendRefs:
        - group: agentgateway.dev
          kind: AgentgatewayBackend
          name: tenant-egress-reports-read
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayBackend
metadata:
  name: tenant-egress-reports-read
  namespace: tenant-a
spec:
  static:
    host: reports.example.com
    port: 443
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayPolicy
metadata:
  name: tenant-egress-reports-read
  namespace: tenant-a
spec:
  targetRefs:
    - group: agentgateway.dev
      kind: AgentgatewayBackend
      name: tenant-egress-reports-read
  backend:
    tls:
      sni: reports.example.com
      verifySubjectAltNames: [reports.example.com]
    extAuth:
      backendRef:
        group: ""
        kind: Service
        name: workload-authorization
        namespace: opensandbox-system
        port: 9000
      grpc:
        requestMetadata:
          opensandbox: |
            {
              "sandboxId": request.headers["x-opensandbox-sandbox-id"],
              "tenantName": request.headers["x-opensandbox-tenant"],
              "bindingType": request.headers["x-opensandbox-binding-type"],
              "bindingName": request.headers["x-opensandbox-binding-name"]
            }
      failureMode: FailClosed
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tenant-egress-events-write
  namespace: tenant-a
spec:
  parentRefs:
    - name: sandbox-egress
      namespace: opensandbox-system
      sectionName: internal-mtls
  hostnames: [events.example.com]
  rules:
    - matches:
        - method: POST
          path:
            type: Exact
            value: /v1/events
          headers:
            - name: x-opensandbox-tenant
              type: Exact
              value: tenant-a
      backendRefs:
        - group: agentgateway.dev
          kind: AgentgatewayBackend
          name: tenant-egress-events-write
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayBackend
metadata:
  name: tenant-egress-events-write
  namespace: tenant-a
spec:
  static:
    host: events.example.com
    port: 443
---
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayPolicy
metadata:
  name: tenant-egress-events-write
  namespace: tenant-a
spec:
  targetRefs:
    - group: agentgateway.dev
      kind: AgentgatewayBackend
      name: tenant-egress-events-write
  backend:
    tls:
      sni: events.example.com
      verifySubjectAltNames: [events.example.com]
```

- **Tenant isolation:** the namespace scopes object ownership, not request identity.
  Envoy removes caller-supplied identity headers and sets `x-opensandbox-tenant`
  and `x-opensandbox-sandbox-id` from trusted allocation context. Agentgateway
  verifies that both values match the mTLS-authenticated sidecar's active sandbox
  registration and that the selected policy belongs to the same tenant; headers
  alone grant nothing. The gateway strips internal headers before upstream forwarding.
  Tenant validation is mandatory even on routes that inject no credentials.
- **Exact enforcement:** the reports regex represents paths starting with `/v1/`;
  using `PathPrefix: /v1` would also allow `/v1`. The events rule matches only
  `/v1/events`, not its subpaths. Compilation must preserve the admitted path
  semantics. Agentgateway also checks the original destination is
  HTTPS on port 443; routing to a TLS backend is not proof of that. Generate only
  allow routes, with no catch-all forwarding route. No matching route means denial.
- **Credential-request context:** `reports-read` uses `grpc.requestMetadata` to
  forward four required fields: `sandboxId`, `tenantName`, `bindingType`, and
  `bindingName`, rather than using static `grpc.contextExtensions`. Envoy sets
  sandbox and tenant identity from trusted runtime context, and binding type/name
  from its matched route. `bindingType` is the source kind (`identity` or
  `credentialVault`), not the identity provider class such as `azure`. The gateway
  accepts these headers only from the authenticated, registered sidecar; the
  metadata expression itself does not authenticate them. `ruleName` is not sent. The
  authorization request must also carry the original destination, method, and path,
  not the internal proxy connection's destination. Missing or unverifiable context
  denies the request.
- **Credential authorization:** for `reports-read`, the service uses trusted sandbox
  context to validate the supplied type/name against current approved selections in
  `SandboxEgressPolicy`, or `BatchSandbox.spec.networkPolicy.egress.authBindings`
  and the referenced `PoolEgressPolicy`. It relies on Envoy's trusted route
  selection rather than re-matching sandbox routes. The actual request selects the latest tenant rule;
  the service checks permitted sources/scopes and resolves the selected binding's
  latest ready state through a read-only Kubernetes list/watch cache, not a
  controller-generated route-to-binding map. Revoked bindings, missing objects, or
  cache state that cannot meet the required freshness checks deny credentialed
  requests. Reading a
  newer sandbox policy does not prove Envoy has installed it: the sandbox/pool
  policy UID/generation used for lookup must match the revision permitted by the
  sandbox readiness gate. That gate also applies to routes without `extAuth`.
  No tenant revision or rule name is pinned in gRPC configuration; stale or
  unverifiable state denies credential use. This rule
  permits Azure identity with `api://reports/.default` or a vault credential.
  `events-write` makes no external authorization call and receives no managed
  credentials. For credentialed routes, the authorization service generates and
  returns the token for gateway injection. Its vault access and provider-binary
  configuration remain separate from these generated manifests.
  The direct and pool policies below still permit only reports traffic; adding a
  tenant allow rule does not independently grant a sandbox access to events.
- **Rollout and deletion:** reconcile the backend and policy before the route, but
  do not treat apply order as atomic activation. Agentgateway gates affected traffic,
  independently of `extAuth`, until route
  `Accepted`/`ResolvedRefs`, policy acceptance, and required data-plane acknowledgements
  cover the source UID/generation. Check each resource's status against its own
  generation, not the tenant policy's generation. The activation gate is a platform
  integration requirement; applying these manifests alone does not provide it.
  Tightening or deletion blocks affected gateway paths before changing or removing
  obsolete outputs; withdrawing permission only in the authorization service is
  insufficient for routes without `extAuth`.
  Missing or failed configuration denies traffic, and only the controller may write
  these generated resources.

### SandboxEgressPolicy

This resource stores the policy for a standalone single-replica `BatchSandbox`,
not the intersection with tenant policy. Pool-backed batches use `PoolEgressPolicy`
until pause handoff creates this resource and detaches them. A directly created
batch still gets a deny-all policy when no network policy is defined. Like
`TenantEgressPolicy`, it stores `defaultAction` and `egress` directly under `spec`.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxEgressPolicy
metadata:
  name: sbx-7f3a
  namespace: tenant-a
  ownerReferences:
    - apiVersion: sandbox.opensandbox.io/v1alpha1
      kind: BatchSandbox
      name: sbx-7f3a
      uid: 7f3a91bd-40a2-4c85-929b-33a8a469f08d
spec:
  defaultAction: deny
  egress:
    - name: reports-read
      action: allow
      target: reports.example.com
      protocol: https
      ports: [443]
      methods: [GET]
      paths: [/v1/reports/*]
      auth:
        source:
          type: identity
          bindingRef:
            name: reports-reader
```

The binding reference contains only its tenant-local name. The controller uses
`auth.source.type` and `auth.source.bindingRef.name` as the route's `bindingType`
and `bindingName`. The authorization service resolves the current binding; the
sandbox policy does not pin a binding version or copy its resolved identity.

| Field | Meaning and validation |
| --- | --- |
| `metadata.ownerReferences` | Identifies the single same-namespace `BatchSandbox` owner by kind, name, and UID. Admission requires `spec.replicas: 1` and a standalone owner, except for the verified controller-only pooled pause handoff. Ownership metadata alone does not authorize traffic. |
| `metadata.name` | Equals the owning `BatchSandbox.metadata.name` in the same namespace, including policies created during pause handoff. Batch deletion cannot finish until its policy has been deleted. |
| `spec.defaultAction` | Defaults to `deny`; only `deny` is supported. Requests without a matching allow rule are denied. |
| `spec.egress` | Named allow rules over destination, protocol, port, method, and path for the owning batch's single sandbox. An omitted or empty list denies all requests. An explicitly supplied `target: "*"` matches any destination; omitted protocol, port, method, or path constraints impose no restriction on that dimension within the supported traffic model, except that rules with `auth` must explicitly set `protocol: https`. Every allowed request still passes tenant enforcement. |
| `auth.source` | Selects exactly one source permitted by the matching tenant rule. `type` is `identity` or `credentialVault`; `bindingRef.name` names a same-namespace binding of that type, never its secret value. Identity bindings must already be valid and approved; vault bindings may be provisioned later, subject to the binding admission and readiness rules above. Token scope/audience parameters are not sandbox inputs. |

When a supported single-replica direct creation request defines no network policy,
the lifecycle server stores the following deny-all policy:

```yaml
spec:
  defaultAction: deny
  egress: []
```

#### Policy Update Reconciliation

Normal `SandboxEgressPolicy` creation and spec updates require a standalone
`BatchSandbox` with `spec.replicas: 1`. The controller-only
[pause handoff](#pool-to-standalone-policy-handoff) may create the policy before
`poolRef` is cleared, but cannot activate it while the batch is still pooled.
The lifecycle server and Kubernetes admission reject multi-replica owners.
The sandbox egress controller handles an accepted update as follows:

1. **Read and validate.** Load the latest policy UID and `metadata.generation`,
   verify its owner, and locate the sandbox through trusted runtime registration.
   Validate the rules and resolve the latest binding state under the
   [binding admission and readiness rules](#binding-admission-and-readiness).
   A missing or unready vault binding is a pending dependency, not a reason to
   discard its reference or reject the admitted policy. Unsupported constraints
   fail closed rather than being omitted.
2. **Compile two configurations.** Generate Envoy listeners and HTTP routes, and
   director DNS rules. Each bound Envoy route overwrites the binding-type and
   binding-name headers with its selected values; this is part of the route
   configuration, not another compiled output. Tenant rules are not
   merged into these configurations, and tenant-owned Gateway API resources are
   not modified.
3. **Gate and publish.** Block the sandbox's affected DNS and application paths
   before switching configuration. Serve each sandbox only its own configuration
   over authenticated xDS: Listener Discovery Service (LDS) supplies listener and
   filter-chain settings, and Route Discovery Service (RDS) supplies request rules.
   Send director rules through an authenticated policy-update channel. Replace
   complete rule sets, including removals; the controller must prevent superseded
   generations from being published.
4. **Confirm and activate.** Keep resource names stable and correlate consumer
   acknowledgements with the source policy UID/generation in the existing trusted
   sandbox registration/readiness state, not a third compiled configuration.
   Agentgateway uses that state to reject stale sandbox-policy revisions, including
   on routes without managed credentials. Re-enable access only after required
   consumers confirm active enforcement, required bindings are authorized and
   ready, tenant enforcement is ready, and the owner still has one replica.
   Revalidate or close connections authorized under old rules before reopening
   the gate.
   An xDS ACK alone does not prove whole-path readiness. Record the processed
   generation in `status.observedGeneration`; set `Ready=True` only after activation.
5. **Handle failure and restart.** Report failures, keep affected traffic denied,
   and retry the newest desired state idempotently. Isolate unreachable consumers
   within the bounded rollout deadline rather than retaining old permissions
   indefinitely. Replacement sandboxes pass the same readiness gate before use,
   and old registrations are withdrawn.

**Example: compiling `reports-read` into Envoy rules.** The sandbox policy above
allows only `GET https://reports.example.com:443/v1/reports/*`. The controller
generates a TLS-only filter chain for the recovered original destination port:

```yaml
filter_chain_match:
  destination_port: 443
  transport_protocol: tls
  server_names: [reports.example.com]
```

This is a listener fragment, not a complete Envoy configuration. The capture
listener must use `original_dst` and `tls_inspector` listener filters, and the
selected filter chain must terminate TLS using a platform-provisioned certificate.
Its HTTP connection manager subscribes through RDS to the following Envoy v3
`RouteConfiguration`, using its name as `rds.route_config_name`:

```yaml
name: sandbox-tenant-a-sbx-7f3a-https-443
ignore_port_in_host_matching: false
virtual_hosts:
  - name: reports
    domains: ["reports.example.com", "reports.example.com:443"]
    routes:
      - name: reports-read
        match:
          prefix: /v1/reports/
          headers:
            - name: ":method"
              string_match:
                exact: GET
            - name: ":scheme"
              string_match:
                exact: https
        request_headers_to_add:
          - header:
              key: x-opensandbox-binding-type
              value: identity
            append_action: OVERWRITE_IF_EXISTS_OR_ADD
          - header:
              key: x-opensandbox-binding-name
              value: reports-reader
            append_action: OVERWRITE_IF_EXISTS_OR_ADD
        route:
          cluster: agentgateway
      - name: deny-other-reports-requests
        match:
          prefix: /
        direct_response:
          status: 403
  - name: deny-other-destinations
    domains: ["*"]
    routes:
      - name: deny-all
        match:
          prefix: /
        direct_response:
          status: 403
```

The allow route forwards to the platform-managed `agentgateway` mTLS cluster,
never directly to the external destination. The fallback routes return `403`
instead of forwarding unmatched requests. Plaintext HTTP or a different original
port cannot enter this TLS filter chain. An HTTP allow rule would use a separate plaintext filter chain for
its permitted port and an HTTP route table; this HTTPS-only policy creates no
such allow path. No unmatched filter chain may forward traffic around enforcement.

The connection manager must apply the strict authority/path normalization and
trusted-context handling described above. Scheme headers alone are not evidence
of TLS or destination port. TLS certificates, connection-manager configuration,
identity stamping, and the mTLS cluster are omitted from these fragments, but remain
required. Before routing, Envoy strips all workload-supplied reserved context
headers; the matched route then sets its binding headers. Unbound routes leave
them absent, and Agentgateway strips internal headers before external forwarding.
Envoy receives no provider credentials: it passes `identity` and `reports-reader`
to Agentgateway, which includes them with `sandboxId` and `tenantName` in the
authorization request. The service validates and resolves the binding, then
generates the token; Agentgateway injects the returned token. Binding headers do
not trigger authorization or credential injection on a tenant route without `auth`.

For example, changing `methods` to `[GET, HEAD]` recompiles the method matcher;
removing the rule publishes a replacement route table containing only the
wildcard `403` fallback. An empty `egress` list never creates a catch-all forwarding
route or leaves the previous allow route installed.

### PoolEgressPolicy

This resource stores the pool's immutable egress rules once, shared across the
single-replica batches allocated from that pool. Its name and namespace equal those
of the owning `Pool`; the owner reference supplies the pool UID, so no separate
`poolRef` field is needed.
The lifecycle server ensures it idempotently before enabling the first allocation
and reuses it thereafter. Allocation never creates a `SandboxEgressPolicy`, even
when no pool network policy is defined; that case creates one deny-all pool policy
with `defaultAction: deny` and `egress: []`.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: PoolEgressPolicy
metadata:
  name: reports-pool
  namespace: tenant-a
  ownerReferences:
    - apiVersion: sandbox.opensandbox.io/v1alpha1
      kind: Pool
      name: reports-pool
      uid: ae3ca267-24cd-4569-a423-a52e8e7a21a4
spec:
  defaultAction: deny
  egress:
    - name: reports-read
      action: allow
      target: reports.example.com
      protocol: https
      ports: [443]
      methods: [GET]
      paths: [/v1/reports/*]
      auth:
        source:
          type: identity
```

- **Shared enforcement:** the sandbox egress policy controller compiles these rules
  into reusable matchers and populates per-batch binding context in each allocated
  sandbox's Envoy configuration. One batch's selected binding must never appear in
  another batch's configuration. Allocation admission
  requires `BatchSandbox.spec.replicas: 1`. Admission also verifies
  that the owner UID and rules match the resolved pool's frozen network policy;
  conflicting existing resources are rejected, not overwritten. Runtime registration
  resolves the actual pool and policy UID; a pool name supplied in a request header
  grants nothing. Missing, deleting, or mismatched pool policy denies egress, with
  no fallback to a per-batch policy. Prewarmed members stay denied until their
  allocation identity and required enforcement state are ready.
- **Immutable rules:** allocation cannot override the pool's network rules. A rule
  change requires a new pool and policy; tenant guardrails continue to update
  independently. The pool policy contains no tenant-policy reference or
  allocation-specific binding selections.
- **Allocation-specific bindings:** to preserve different credential choices without
  extra CRs, this proposal adds `spec.networkPolicy.egress.authBindings` to the
  existing pooled `BatchSandbox`. This `networkPolicy.egress` object holds binding
  selections, not policy rules; the policy resources' `spec.egress` rule arrays
  remain unchanged. Each list entry has two required fields: `bindingName`, naming
  a binding in the batch's namespace, and `ruleName`, matching a rule's
  `name` in `PoolEgressPolicy.spec.egress`. The controller uses `ruleName` to find
  that rule's `auth.source.type`, producing `bindingType`; `bindingName` comes
  directly from the selection. The shared rule declares the type but no binding
  name. Selections apply to the batch's single sandbox. Admission checks the rule
  declares an authentication source and the tenant permits it, then applies the
  source-specific binding admission and readiness rules above. Identity bindings
  must already be valid and approved; a missing or unready vault binding keeps
  this allocation's egress pending until it is authorized and ready. Duplicate
  rule names, unknown rules, or incompatible selections are rejected. A
  credentialed request without a usable binding is denied. Scope and audience
  remain tenant-policy inputs.
  Envoy stamps the resolved type/name for the matched route; `ruleName` is used
  only for configuration lookup and is not forwarded to the authorization service.
  The service validates the supplied pair against the current batch selections and
  pool rules in its CR cache, rather than choosing the binding itself.
- **Immutable selections:** for now, `spec.networkPolicy.egress.authBindings` is
  fixed when the pooled `BatchSandbox` is created. The lifecycle server and
  Kubernetes admission reject user additions, removals, or changes while pooled,
  including adding selections when `networkPolicy`, its `egress` object, or `authBindings`
  was initially omitted or the list was empty. The sandbox egress policy controller
  configures Envoy from the admitted selections during allocation or sandbox
  replacement; there is no live binding-selection update flow. While pool-backed,
  different selections require a new `BatchSandbox`, which may reuse the same pool.
  This freezes the selections, not the referenced bindings' resolved state:
  authorization still uses the latest
  binding state and enforces readiness and revocation checks.
  The controller-only pause handoff moves those same selections into
  `SandboxEgressPolicy` and removes the pooled-only field after the new policy is
  durable; it is not permission to edit a live pooled allocation.

For example, a pooled batch selects the reports identity on its existing resource.
`spec.networkPolicy.egress.authBindings` is a proposed field, not part of the
current `BatchSandbox` CRD:

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: pooled-reports
  namespace: tenant-a
spec:
  replicas: 1
  poolRef: reports-pool
  networkPolicy:
    egress:
      authBindings:
        - bindingName: reports-reader
          ruleName: reports-read
```

Deleting this batch withdraws its allocation registrations, cached authorizations,
tokens, and connections; a cached CR alone cannot authorize an inactive sandbox.
The shared pool policy remains for other allocations.
Pool deletion stops new allocations and withdraws active consumers before policy
cleanup. A batch being paused retains its pool dependency until its standalone
policy handoff is durable; after detachment, it no longer blocks pool-policy
cleanup. Pool deletion cannot finish while consumers still depend on its policy
or cleanup fails. Pool-backed batches share one `PoolEgressPolicy`; each batch
detached during pause additionally owns one `SandboxEgressPolicy`.

### IdentityProviderClass

The infrastructure provider creates one or more `IdentityProviderClass` objects
for users. Each class names an identity controller for binding reconciliation and
a token-provider binary for request-time invocation by the workload authorization
service.
When registering an identity binding, the user selects an approved class by its
`metadata.name`. The lifecycle server stores that name as
`SandboxIdentityBinding.spec.providerClassName`. For an identity-authenticated
route, the tenant's `auth.sources[].provider.type` must name the same class.
The lifecycle server does not substitute a controller name, issuer URL, or
automatically selected server default.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: IdentityProviderClass
metadata:
  name: azure
spec:
  controllerName: sandbox.opensandbox.io/azure-identity
  tokenProviderBinary: azure-identity-token-provider
  parameters:
    providerTenantId: "<provider-tenant-id>"
    workloadIdentity:
      issuer: "<cluster-oidc-issuer-url>"
      serviceAccountRef:
        name: workload-authorization
        namespace: opensandbox-system
      audience: api://AzureADTokenExchange
```

- **Class name:** `metadata.name` must match the permitted identity source's
  `provider.type` in `TenantEgressPolicy` and the selected binding's
  `spec.providerClassName`; all three are `azure` in these examples. Multiple
  classes can select the same controller with different approved parameters, but
  tenant policy must explicitly name each permitted class. Permission to use one
  class does not authorize another class handled by the same controller.
- **Controller selection:** `spec.controllerName` is the registered identifier of
  the identity controller responsible for bindings that reference this class.
  Here it selects `sandbox.opensandbox.io/azure-identity`. A controller reconciles
  only bindings whose class selects its exact controller name; other controllers
  leave those bindings untouched. This value is not an executable path or command.
- **Token-provider binary:** `spec.tokenProviderBinary` names a platform-approved
  binary installed in the workload authorization service's trusted runtime, such
  as `azure-identity-token-provider`. After authorization, the service resolves
  this name through its approved binary registry and invokes it with the binding's
  resolved identity and tenant-approved token parameters. The field is a binary
  name, not an arbitrary path, shell command, or downloadable executable. The
  provider controller reconciles the binding; the binary performs request-time
  token acquisition. Neither runs in Agentgateway, Envoy, or the sandbox workload.
- **Provider configuration:** `spec.parameters` contains controller-validated,
  non-secret configuration used by the controller and token-provider binary.
  `providerTenantId` refers to the external provider's
  tenant, not the OpenSandbox tenant. Credentials and signing keys remain in the
  authorization service's protected runtime configuration, not in the class or
  binding CR. Provider-controller setup credentials remain separate.
- **Authorization-service workload identity:** for Azure, `workloadIdentity`
  supplies the cluster's OIDC issuer, the authorization service's ServiceAccount,
  and token-exchange audience. The authorization service deployment must run as
  that ServiceAccount and mount a rotating projected token with the specified
  audience; the reference alone does not provision this setup.
  Microsoft Entra ID must be able to discover the issuer's public signing keys.
  The assertion is available only in the authorization service's trusted runtime
  for its provider binary, never to Agentgateway, sandbox workloads, or Envoy.
- **Selection and isolation:** infrastructure providers create classes for users.
  Admission checks that the selected controller is registered, the binary is an
  approved implementation compatible with that controller, and the resolved
  OpenSandbox tenant is permitted to use the requested class;
  knowing a cluster-scoped class name is not sufficient authorization. An omitted,
  unknown, unauthorized, or deleting class is rejected. There is no default class
  or fallback controller. An unavailable selected controller leaves the binding not
  ready.
- **Stable association:** class spec is immutable. Changing its controller,
  binary name, or identity/trust parameters requires a new class and new bindings;
  rotating the provider's runtime credentials does not. The selected controller records the
  resolved class UID and controller name before provider-side setup and publication.
  A deleted or same-name replaced class cannot silently retarget a resolved
  binding: affected bindings become not ready and their authenticated routes
  fail closed.

### SandboxIdentityBinding

This resource registers an approved external identity. It is not the sandbox's
internal mTLS identity, a token cache, or a network permission. Multiple sandbox
allocations may reference it, but possession of its name alone authorizes nothing.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: SandboxIdentityBinding
metadata:
  name: reports-reader
  namespace: tenant-a
spec:
  providerClassName: azure
  identity:
    type: systemIdentity
    resourceId: /applications/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee
```

- **Azure reconciliation:** `systemIdentity` here identifies an existing Entra
  application and service principal, not an Azure system-assigned managed
  identity. The Azure provider controller validates the caller's authority to
  register that identity and resolves its application/client ID. With separately
  granted identity-management permissions, it idempotently ensures a federated
  identity credential (FIC) on the application identified by `resourceId`. The FIC
  uses the class's issuer, subject
  `system:serviceaccount:opensandbox-system:workload-authorization`, and audience
  `api://AzureADTokenExchange`. The subject is derived from the class's
  ServiceAccount namespace and name; it is the authorization service's identity,
  not the gateway's or sandbox's ServiceAccount.
- **Resolved state:** the controller publishes the non-secret resolved client ID,
  FIC reference, and readiness for the observed binding generation. The application
  object ID in `resourceId` locates the FIC's parent; the application/client ID is
  used for token exchange. The binding remains not ready until identity validation
  and federation setup succeed. Propagation delays or exchange failures deny the
  authenticated request and are retried, not bypassed with another identity.
- **Token exchange:** after route authorization, the workload authorization service
  invokes the class's `spec.tokenProviderBinary`. The binary sends the service's
  projected ServiceAccount JWT as a federated client assertion to
  the configured Entra tenant, with the binding's resolved client ID and the
  tenant-approved scope, such as `api://reports/.default`. Microsoft Entra ID issues
  the access token, which the authorization service returns to Agentgateway for
  injection. The controller does not mint it, and neither the authorization service
  nor the gateway needs FIC management permissions. The assertion audience is for
  federation, not the destination API; API permissions or roles must already be
  granted to the target identity.
- **Trust and cleanup:** a shared authorization-service ServiceAccount can exchange
  tokens for every identity that trusts it, so tenant, sandbox, binding, and route checks remain
  mandatory. Binding deletion is blocked while a `SandboxEgressPolicy` or a pooled
  `BatchSandbox.spec.networkPolicy.egress.authBindings` references it, and until
  affected allocation registrations are withdrawn. Once unused, withdraw its
  authorization-service registration and cached tokens before removing
  controller-owned federation state. Do not delete a
  FIC still used by another binding or delete the user's identity. FIC removal does
  not immediately revoke issued access tokens; request-time authorization must
  still deny withdrawn bindings.

### Pause and Resume

Pause suspends a sandbox's outbound authority, not its policy intent. This flow
applies to the same single-replica `BatchSandbox`; creating a different sandbox
from a snapshot must use normal creation and admission checks, not inherit the
source sandbox's runtime authorization.

Retain an existing `SandboxEgressPolicy` while paused. For a pooled batch, create
one during pause from the pool rules and that batch's fixed binding selections;
the resumed standalone sandbox uses this policy instead of `PoolEgressPolicy`.
Binding references remain protected throughout the handoff. Pausing one consumer
must not delete shared policies, federation state, or credentials used by others.

#### Pause

1. **Withdraw access.** Before checkpointing, the lifecycle/runtime controller
   coordinates with the sandbox egress policy controller to close the sandbox's
   DNS and application-traffic gates. Agentgateway marks its runtime registration
   inactive and denies new requests and credential use for that sandbox. Close
   existing proxy connections and invalidate sandbox-scoped authorization caches;
   retained policy objects alone must not keep the old runtime authorized.
2. **Checkpoint workload state, not authorization.** Keep managed credentials,
   sidecar private keys, active registrations, and proxy connection/cache state out
   of snapshots. Restore trusted sidecars from platform bootstrap configuration,
   not checkpointed proxy state. Snapshot upload and restore use trusted platform
   channels, independently of the sandbox's network permissions.
3. **Materialize a standalone policy when pooled.** After checkpoint success and
   before clearing `poolRef`, complete the policy handoff below. A standalone batch
   keeps its existing policy; repeated pause/resume cycles do not create more CRs.
4. **Confirm suspension.** Report pause complete only after withdrawal is
   acknowledged, any required policy handoff is durable, and the old workload is
   stopped or isolated. `SandboxEgressPolicy.Ready` is false while its sandbox is
   paused; the shared pool policy can remain ready for other consumers. In-flight
   requests may already have reached the target; pausing is not a rollback of
   external side effects.

Tenant policies and bindings may change while the sandbox is paused. After
detachment, the batch follows the normal standalone-policy update path; updates
record desired state without activating absent consumers. Rules and selections
of batches that remain pool-backed stay immutable. Provisioning a missing vault
binding can satisfy that dependency while paused, but does not itself reactivate
the sandbox.

#### Resume

1. **Restore behind a closed gate.** Re-establish kernel interception and DNS
   controls before the restored workload can send traffic. Recreate the DNS proxy
   and Envoy, issue fresh sidecar credentials for the restored runtime, and bind
   its new Pod identity to the same tenant and sandbox. The previous runtime's
   registration and sessions remain unusable.
2. **Resolve current enforcement state.** The egress controllers use the current
   tenant policy, the batch-owned `SandboxEgressPolicy`, and latest binding state.
   This includes batches originally created from a pool; resume no longer depends
   on that pool or its policy. Recompile and install configuration rather than
   trusting snapshot-era proxy configuration, DNS caches, or readiness acknowledgements.
   A missing or unready vault keeps egress pending; an invalid, revoked, or
   incompatible identity prevents activation. No snapshot-era credential or
   fallback identity restores access.
3. **Activate only after acknowledgement.** Require the same policy, TLS, binding,
   and data-plane readiness checks as initial creation before enabling the new
   runtime registration and reporting the sandbox usable. Recheck revisions if
   desired state changes during resume. Existing connections are not preserved;
   applications reconnect, and each new request uses current enforcement.

#### Pool-to-Standalone Policy Handoff

The current Kubernetes pause controller retains the `BatchSandbox` but clears
`spec.poolRef` after saving the source Pod template, then resumes it as a standalone
runtime. Before that transition, the sandbox egress policy controller materializes
the standalone policy while outbound access remains blocked:

1. **Build the policy.** Read the verified source `PoolEgressPolicy` and the batch's
   immutable `spec.networkPolicy.egress.authBindings`. Preserve `defaultAction: deny`
   and the pool's destination, protocol, port, method, and path restrictions. For
   each bound rule, take `auth.source.type` from the pool rule and set
   `auth.source.bindingRef.name` from the matching `ruleName` selection's
   `bindingName`. Copy rules without `auth` unchanged. Rules requiring a binding
   but lacking a selection remain denied: omit them rather than converting them
   into unauthenticated allow rules. A selected vault name is retained even if the
   vault is not yet provisioned or ready. No tenant-policy snapshot or credential
   value is copied.
2. **Persist before detachment.** Create `SandboxEgressPolicy` with the batch's
   name, namespace, and owner UID. Admission permits this pooled-owner exception
   only for the authorized handoff controller during pause, with outbound access
   withdrawn and the materialized spec verified against the source rules and fixed
   selections. This preserves prior intent, not a new credential grant; latest
   binding validity still gates activation. Reuse an equivalent owned policy on
   retries; never overwrite a conflicting object or replace it with deny-all
   merely because a handoff dependency cannot be read.
3. **Complete the runtime transition.** Only after the new policy is durable,
   allow the runtime controller to save the template, clear `spec.poolRef`, and
   remove the migrated `spec.networkPolicy.egress.authBindings` in one batch update.
   This controller-only removal is the exception to pooled-selection immutability.
   Creating new binding references before removing old ones avoids a deletion-protection
   gap. Withdraw the old pool consumer registration and release its allocation.
4. **Use the standalone source.** The new policy is the sole sandbox-policy source
   after detachment. Keep it unready while paused; resume installs and acknowledges
   its own UID/generation, not the old pool-policy revision. The original pool and
   policy can be cleaned up once their remaining consumers are gone. The converted
   batch no longer needs a retained pool-policy reference in status. A retry after
   detachment uses the existing standalone policy without rereading the pool.

If materialization or detachment fails, keep egress blocked and retry the handoff
without reporting pause complete. Do not merge both policy sources or fall back
to one at request time. Policy materialization, protected migration of binding
references, exclusion of trusted sidecar state from snapshots, and activation
gates are required integrations; the current controller does not implement this
handoff unchanged.

Pause/resume retries are idempotent. On failure, keep affected egress denied;
even if the old workload survives a failed pause, revalidate current enforcement
before reopening access. Record correlated lifecycle events with tenant, sandbox,
operation outcome, runtime Pod UID, policy revisions, and waiting/failure reasons.
Resumed HTTP requests receive new request IDs, not IDs replayed from the snapshot.

### Correlated Access Logs

The initial version uses structured access logs to answer which sandbox contacted
which destination, which policy was enforced, and where a request failed. This
requires no workload instrumentation or additional policy CRs. Distributed
tracing and request/response payload capture are outside the initial scope.

#### Correlation and Component Responsibilities

For each parsed HTTP request, Envoy generates a platform-controlled `requestId`
before policy evaluation, replacing any workload-supplied `x-request-id`. It
propagates that ID to Agentgateway over the internal mTLS connection and, when
authorization is required, the gateway passes it as logging metadata on the
authorization RPC. This does not change the four authorization context fields or
make the request ID an authorization input. Each participating component records
the same ID, including when it denies the request locally.

Agentgateway accepts this correlation context only from the authenticated sidecar.
Tenant and sandbox attribution comes from trusted allocation context and gateway
identity verification, not the request ID or arbitrary workload headers. Internal
context is stripped before external forwarding. Envoy sets `x-request-id` on HTTP
responses, overwriting any upstream value, so users can supply it when reporting a
failure. Failures before HTTP parsing have a separate event ID instead.

| Component | Records |
| --- | --- |
| DNS proxy | Sandbox identity, sanitized query name/type, DNS decision and reason, resolver outcome, and sandbox/pool policy revision. |
| Envoy | Original destination, method, sanitized path or matched path pattern, sandbox/pool rule and decision, final response status, duration, and byte counts. |
| Agentgateway | Tenant rule and decision, credential-injection outcome, actual upstream address, response origin/status, duration, and byte counts. |
| Authorization service | Binding type/name and generation used, authorization decision, vault retrieval or provider-binary invocation outcome, and token-generation outcome/reason; never the token or credential material. No record is expected for routes that skip authorization. |
| Network interception | Blocked connection attempts, destination/protocol, and reason, including traffic rejected before HTTP parsing. |

DNS and pre-HTTP connection events use their own event IDs. Correlate them by
trusted sandbox identity and time; do not imply an exact DNS-query-to-HTTP-request
relationship. Requests denied before Agentgateway must remain visible in the
component that rejected them.

#### Common Record Fields

Use a common structured envelope, with fields present only when known:

| Category | Fields |
| --- | --- |
| Correlation | Timestamp, component, event type, `requestId` for HTTP or `eventId` otherwise. |
| Identity | Trusted `tenantName` and `sandboxId`; leave attribution unknown if identity verification fails. |
| Request | Destination host/port, protocol, method, sanitized path or matched path pattern; resolved upstream address when available. |
| Policy | Policy kind, name, UID, generation actually enforced, matched rule name, decision, and reason. Each component records its own evaluated layer, not the latest desired revision. |
| Authentication | Binding type/name and generation when resolved; credential injection as `performed`, `skipped`, or `failed`. Never include credential values. |
| Result | Response origin, status, failure stage/reason, duration, and byte counts, as applicable. |

Keep platform policy decisions separate from upstream responses. For example, if
the tenant permits a request without managed authentication and the target rejects
it, the gateway result includes:

```json
{
  "requestId": "cd427153-4bfa-432e-bdc2-0e9d50d7dd24",
  "component": "agentgateway",
  "policyDecision": "allow",
  "credentialInjection": "skipped",
  "responseOrigin": "upstream",
  "upstreamStatus": 401
}
```

This is not a platform policy denial. A local denial instead identifies the
rejecting component and reason, without inventing an upstream status.

#### Collection and Data Protection

Emit unsampled access-decision records and export them asynchronously through the
platform's log collection pipeline. Collection accepts records only from trusted
platform components; user queries are authorized and scoped by tenant. Retention,
redaction, and export settings are platform-owned, not egress-policy fields.

Never capture tokens, cookies, credential headers, bodies, or raw URL query strings.
Sanitize and bound logged destination names and paths, including DNS names, since
they can contain sensitive workload data. Do not dump arbitrary request headers
or provider responses.

Use bounded buffers and expose export failures and dropped-record counts. A logging
outage must not weaken enforcement or create a direct-egress fallback. These logs
provide operational visibility, not a lossless audit guarantee; do not store
per-request records in Kubernetes resources or their status.
