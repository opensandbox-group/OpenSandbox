---
title: Add Envoy sidecar option
authors:
  - "@ryanzhang-oss"
creation-date: 2026-07-06
last-updated: 2026-07-15
status: provisional
---

# OSEP-0016: Add Envoy sidecar option

<!-- toc -->
- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Traffic Model](#traffic-model)
  - [SDK Extension Model](#sdk-extension-model)
  - [Sandbox API Shape](#sandbox-api-shape)
  - [Policy Semantics](#policy-semantics)
  - [Future Centralized Policy Integration](#future-centralized-policy-integration)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Current Egress Sidecar Behavior](#current-egress-sidecar-behavior)
  - [L7 Backend Selection](#l7-backend-selection)
  - [Envoy Interception](#envoy-interception)
  - [xDS Generation](#xds-generation)
  - [HTTPS Handling](#https-handling)
  - [Runtime Policy API](#runtime-policy-api)
  - [POC Scope, Status, and Acceptance Criteria](#poc-scope-status-and-acceptance-criteria)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Add an optional [Envoy](https://www.envoyproxy.io/)-based L7 egress backend to the [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) egress sidecar as an alternative to the current experimental transparent [mitmproxy](https://mitmproxy.org/) path for outbound HTTP and HTTPS capture. Envoy is opt-in and mutually exclusive with mitmproxy within a single sandbox; the existing mitmproxy path remains a supported option and is not removed or deprecated, and the DNS proxy and nftables mechanisms remain the L3/L4 and FQDN safety boundary.

OpenSandbox uses upstream [Gateway API](https://gateway-api.sigs.k8s.io/) resources directly for the Kubernetes-facing egress API as an overall design principle, `GatewayClass`, `Gateway`, and `HTTPRoute` keep their `gateway.networking.k8s.io` GVKs and field semantics; OpenSandbox does not define copies of those APIs. Policy and backend behavior that Gateway API does not define, such as CEL authorization, external authorization, and dynamic forward proxy behavior, should use the real [agentgateway](https://github.com/agentgateway/agentgateway) CRDs ([`AgentgatewayPolicy`](https://agentgateway.dev/docs/kubernetes/latest/reference/api-kubespec/policies/) and [`AgentgatewayBackend`](https://agentgateway.dev/docs/kubernetes/latest/reference/api-kubespec/backends/); full [agentgateway API reference](https://agentgateway.dev/docs/kubernetes/latest/reference/api/)) or the same schemas when embedded in a non-Kubernetes sandbox API. HTTP traffic can use request-aware policy such as host, path, method, headers, external authorization, and [CEL](https://cel.dev/) authorization rules. HTTPS traffic is supported without TLS interception in the first draft, so policy is limited to non-decrypting attributes such as destination, port, and SNI, backed by DNS+nft enforcement.

SDKs carry these optional policy documents through the versioned extension resource model defined in this proposal. The public discriminator is the resource's `apiVersion` and `kind`, not `envoy` or `mitmproxy`. The existing `networkPolicy` model and sidecar `/policy` API remain the portable baseline, while capability discovery determines whether a sandbox can accept additional policy resources. The envelope and SDK conventions are intentionally reusable by other OpenSandbox API domains, but this proposal proves them in egress before promoting them to a repository-wide contract.

## Motivation

OpenSandbox currently provides FQDN/IP/CIDR egress control through DNS interception and optional nftables enforcement. This is a strong network boundary, but it cannot naturally express richer L7 policy such as HTTP method, path, header, route, external authorization, and future MCP tool-level authorization.

The current transparent mitmproxy mode proves that an L7 interceptor can coexist with DNS+nft, but mitmproxy is experimental in OpenSandbox and is less widely adopted as a production data-plane component. Envoy is a better long-term L7 enforcement backend because it has standard [xDS](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol) APIs, mature HTTP filters, [RBAC](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/rbac_filter), [ext_authz](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/ext_authz_filter), original-destination support, and broad operator familiarity.

This design is also intended to keep OpenSandbox aligned with the broader agent networking ecosystem. [kube-agentic-networking](https://github.com/kubernetes-sigs/kube-agentic-networking) demonstrates how Gateway API, backend references, access policies, RBAC, ext_authz, and [MCP](https://modelcontextprotocol.io/)-aware metadata can be translated into Envoy xDS. [agentgateway](https://github.com/agentgateway/agentgateway) demonstrates a gateway-shaped policy model with listener, route, backend, policy attachment points, fixed policy phases, CEL authorization, and agentic protocol concepts. OpenSandbox should keep enforcement scoped to each sandbox for the first implementation, while using upstream Kubernetes and agentgateway APIs so the same resources can later participate in centralized enforcement.

### Goals

- Keep everything users rely on today: existing DNS, nftables, and FQDN/IP/CIDR egress controls keep working unchanged, with the same default-deny safety guarantees.
- Give users richer L7 egress policy on top of what they already have: control outbound HTTP by host, path, method, and headers instead of only by domain or IP.
- Let users express fine-grained authorization with CEL rules (`allow`, `deny`, `require`) so they can encode intent, not just allow-lists.
- Require no application changes: sandbox workloads keep sending normal HTTP/HTTPS traffic and get the new policy transparently.
- Let users opt in gradually: L7 policy is optional, additive, and does not change behavior for sandboxes that do not use it.
- Keep the same reliability expectations users have today: fail-closed startup and health behavior comparable to the current egress sidecar.
- Give users a policy model that grows with them: room for external authorization, MCP-aware policy, and future centralized policy managed across many sandboxes.
- Give SDK users one versioned extension mechanism that can carry Gateway API policy without coupling the public API to an egress implementation.

### Non-Goals

- Removing, deprecating, or replacing the existing transparent mitmproxy L7 path; Envoy is an additional opt-in backend that users choose per sandbox.
- Running Envoy and mitmproxy as simultaneous transparent interceptors for the same outbound ports.
- TLS interception, generated CA trust, or decrypted HTTPS request inspection in the first implementation.
- Replacing [Kubernetes NetworkPolicy](https://kubernetes.io/docs/concepts/services-networking/network-policies/) or CNI-level controls.
- Requiring applications to configure HTTP proxy environment variables.
- Making Envoy the only enforcement boundary for non-HTTP protocols.
- Introducing a multi-sandbox central policy controller in the first implementation.
- Replacing the existing string-only `CreateSandboxRequest.extensions` provider parameter map with structured policy.

## Requirements

- Envoy mode must coexist with DNS proxying and nftables in the shared sandbox network namespace.
- Envoy-owned upstream traffic must bypass Envoy's own transparent redirect to avoid capture loops.
- DNS proxy upstream traffic must continue using the existing mark or destination exemption path.
- nftables must continue to gate final outbound connections in dns+nft mode.
- The sidecar must fail closed if transparent capture is requested but Envoy cannot start or redirect rules cannot be installed.
- The configuration must make mitmproxy and Envoy mutually exclusive L7 backends.
- Gateway API resources used by this proposal must keep upstream GVKs and field semantics; OpenSandbox must not clone `gateway.networking.k8s.io` APIs under an OpenSandbox API group.
- The per-sandbox Gateway API and agentgateway resources must be scoped to a single sandbox in the first implementation by namespace, owner reference, label, or explicit sandbox reference.
- This binding must work for both OpenSandbox `BatchSandbox` and the upstream [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) `Sandbox` workloads; only the owning workload object's GVK differs.
- OpenSandbox-specific transparent egress behavior, such as Envoy original-destination forwarding, must be controller behavior behind these resources rather than new fields on upstream Gateway API objects.
- CEL policy evaluation must use an explicit, documented context and fail-closed semantics for required conditions.
- Existing `networkPolicy`, `/policy`, and Credential Vault contracts must remain backward compatible.
- Public policy documents must select a versioned contract rather than a concrete L7 backend; implementation selection remains server or operator configuration.
- The egress SDK must expose capability discovery and reject unsupported or conflicting extension resources before reporting a sandbox ready.

## Proposal

Introduce an Envoy L7 backend for the egress sidecar as an optional, opt-in mode alongside the existing mitmproxy path. In Envoy mode, the sidecar starts Envoy locally, waits for readiness, installs transparent `OUTPUT` redirect rules for outbound TCP `80/443`, and configures Envoy through generated xDS resources. DNS interception and nftables policy continue to run exactly as they do today, and deployments that do not enable Envoy keep using DNS/nft alone or the existing mitmproxy path.

Expose this through upstream Gateway API resources plus agentgateway policy/backend resources rather than a new OpenSandbox route API. `networkPolicy` remains the existing network boundary for domain, IP, CIDR, and default action. Gateway API resources describe listeners and routes for the traffic Envoy can observe. Agentgateway resources describe policy and backend behavior that Gateway API intentionally leaves to implementations. SDKs carry those documents as versioned extension resources, and the egress endpoint validates their exact `apiVersion` and `kind` against its capabilities. A future centralized layer can then reuse the same resources instead of translating from an OpenSandbox-specific route model.

### Traffic Model

The intended traffic flow is:

```text
sandbox process
  -> DNS query: OUTPUT dport 53 -> OpenSandbox DNS proxy -> optional dynamic nftables allow entry
  -> HTTP/HTTPS: OUTPUT dport 80/443 -> Envoy listener
  -> Envoy filters: HTTP L7 policy or HTTPS SNI/original-destination policy
  -> Envoy upstream connection bypasses Envoy redirect by uid or mark
  -> nft output policy gates final egress in dns+nft mode
```

This makes Envoy an additive L7 enforcement point behind the existing network boundary. The current DNS/nft behavior remains authoritative for FQDN/IP/CIDR allow and deny decisions; Envoy adds richer policy for protocols it can understand without weakening fail-closed network enforcement.

### SDK Extension Model

`networkPolicy` is a stable core field supported by every egress mode. Optional L7 policy uses extension resources whose entries contain `apiVersion`, `kind`, `metadata`, and `spec`. Gateway API and agentgateway objects therefore retain their upstream wire shape.

```yaml
expectedRevision: 7
resources:
  - apiVersion: gateway.networking.k8s.io/v1
    kind: HTTPRoute
    metadata:
      name: api-example
    spec:
      hostnames:
        - api.example.com
      rules:
        - matches:
            - method: GET
          backendRefs:
            - group: agentgateway.dev
              kind: AgentgatewayBackend
              name: original-destination
```

The generic resource identity is `(apiVersion, kind, metadata.namespace, metadata.name)`. `apiVersion`, `kind`, `metadata.name`, and `spec` are required. Fields inside `metadata` and `spec` remain open in generated SDK wire types so newer resource fields survive read-modify-write, while the egress implementation validates them against the exact registered schema before accepting an update. Unknown kinds return `UNSUPPORTED_EXTENSION`; known kinds unavailable for the active implementation return `CAPABILITY_UNAVAILABLE`.

The egress API exposes `GET /capabilities`, `GET /extensions`, and atomic `PUT /extensions`. Capability entries report exact resource versions, availability, supported operations, optional qualified features, and an unavailable reason. Extension state includes a monotonically increasing revision, the complete resource set, and `Accepted`/`Programmed` conditions. Replacement supports an optional `expectedRevision`; a stale revision returns `REVISION_CONFLICT`, and an empty resource list clears extensions without changing `/policy`.

The same envelope can be adopted by another API domain by defining a domain-scoped capability endpoint and resource collection. It must not reuse the existing string-only `CreateSandboxRequest.extensions` map, silently ignore unsupported fields, or expose backend names as resource types. Once a second domain proves the pattern, the common schemas can move into a dedicated cross-domain proposal without changing the egress wire format.

The server, not the policy document, maps the requested resource kinds to Envoy. A sandbox configured with mitmproxy can continue to use `networkPolicy` and Credential Vault, but its capability response marks Gateway API policy kinds unavailable. Submitting the example to that sandbox fails with `CAPABILITY_UNAVAILABLE`; the server does not reinterpret or silently ignore it. A request containing extension kinds that require mutually exclusive implementations fails with `EXTENSION_CONFLICT`.

The public SDK facade adds `getCapabilities()`, `getExtensions()`, and `replaceExtensions(resources, expectedRevision)`. SDKs preserve unknown extension documents as JSON-compatible values so a newer server does not force an immediate SDK release. Typed helpers may be added for OpenSandbox-owned extensions, but the SDK must not publish partial clones of Gateway API models.

### Sandbox API Shape

The Kubernetes-facing API must use upstream Gateway API directly wherever Gateway API already owns the concept. This is the long-term API contract, not a temporary v1alpha1 shape. The implementation should reconcile the following resource set for each sandbox that opts into Envoy egress:

1. `GatewayClass` (`gateway.networking.k8s.io/v1`, kind `GatewayClass`) identifies the OpenSandbox Envoy egress controller. This is usually cluster-wide rather than per-sandbox.
2. `Gateway` (`gateway.networking.k8s.io/v1`, kind `Gateway`) is the per-sandbox capture surface. It declares the HTTP and TLS listeners that the sidecar transparently captures. Unlike a normal Gateway API implementation, this `Gateway` does not provision a new proxy Deployment; its data plane is the Envoy that already runs as a sidecar inside that sandbox's Pod, so the `Gateway` is only the configuration surface for that one sidecar.
3. `HTTPRoute` (`gateway.networking.k8s.io/v1`, kind `HTTPRoute`) expresses plaintext HTTP host/path/method/header matching and attaches to the per-sandbox `Gateway` with `parentRefs`.
4. `TLSRoute` (`gateway.networking.k8s.io/v1alpha2`, kind `TLSRoute`) should be used for non-decrypting HTTPS/SNI routing where it is installed. `TLSRoute` currently ships only in the Gateway API experimental channel and has not graduated to `v1`, so clusters that install only the standard channel will reject it; on those clusters HTTPS policy should remain listener/SNI-level until `TLSRoute` is available rather than inventing an OpenSandbox TLS route.
5. [`AgentgatewayBackend`](https://agentgateway.dev/docs/kubernetes/latest/reference/api-kubespec/backends/) and [`AgentgatewayPolicy`](https://agentgateway.dev/docs/kubernetes/latest/reference/api-kubespec/policies/) (`agentgateway.dev/v1alpha1`) provide agentgateway-compatible backend and policy attachment for behavior Gateway API does not define, such as [dynamic forward proxy](https://agentgateway.dev/docs/kubernetes/latest/traffic-management/dfp/), CEL authorization, and ext_authz.

#### How a Gateway pairs with a sandbox

An individual sandbox is a Pod created and owned by a sandbox workload object. OpenSandbox supports two such workloads, selected by the configured workload provider: its own `BatchSandbox` (`sandbox.opensandbox.io/v1alpha1`) and the upstream [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) `Sandbox` (`agents.x-k8s.io/v1alpha1`). The pairing below works the same way for either workload; only the owning object's GVK differs. For the first implementation, Envoy L7 egress is scoped to single-Pod sandboxes: a `BatchSandbox` with `spec.replicas > 1` has no single Pod for `Gateway` → Pod resolution, so the controller must either fan the compiled xDS out to every owned Pod's sidecar or reject Envoy mode for multi-replica sandboxes. The examples below assume `replicas: 1`. A per-sandbox `Gateway` is paired to one specific sandbox as follows:

1. **Controller selection**: `GatewayClass.spec.controllerName` (`opensandbox.ai/egress-envoy`) tells the OpenSandbox egress controller which `Gateway` objects are its responsibility. Gateways of any other class are ignored, so this does not interfere with other Gateway API implementations in the cluster.
2. **Gateway → sandbox binding via owner reference**: the controller creates one `Gateway` per sandbox and stamps it with an `ownerReferences` entry pointing at that sandbox's workload object (a `BatchSandbox`, or the agent-sandbox `Sandbox`), plus a label such as `sandbox.opensandbox.io/sandbox: sandbox-1`. The owner reference is the actual pairing: it ties the `Gateway` lifecycle to `sandbox-1`, so when the sandbox is deleted, Kubernetes garbage-collects the `Gateway`. The label lets the controller resolve `Gateway` → the sandbox's Pod and its sidecar Envoy.
3. **Route and policy attachment**: `HTTPRoute`/`TLSRoute` use `parentRefs` to attach to that sandbox's `Gateway`, and `AgentgatewayPolicy` uses `targetRefs` to attach to those routes. Two things do not happen automatically and must be enforced by the controller. First, attachment references do not create owner references or trigger garbage collection, so the controller must explicitly stamp the same sandbox `ownerReferences` and label onto every route, backend, and policy it accepts for `sandbox-1`; that is what lets Kubernetes garbage-collect them with the sandbox and prevents a stale route from a deleted sandbox being reused by a later sandbox with the same `Gateway` name. Second, `allowedRoutes.namespaces.from: Same` only restricts attachment to the same namespace, not to a single sandbox, so in a namespace hosting more than one sandbox the controller must reject any route or policy whose sandbox owner reference or label does not match the target `Gateway` before compiling it into that sandbox's xDS.
4. **Config delivery**: the controller reads the `Gateway`, routes, backends, and policies bound to `sandbox-1`, compiles them into Envoy xDS, and pushes that xDS only to `sandbox-1`'s sidecar Envoy. Another sandbox's resources compile independently to that sandbox's own Envoy.

Typically the OpenSandbox lifecycle controller creates the `Gateway` and a default route when it creates the sandbox; users or operators can then add `HTTPRoute` and `AgentgatewayPolicy` objects that reference that `Gateway` to shape L7 policy. Runtime SDK clients use the egress extension API for implementations that advertise `replace`. Cluster-scoped resources such as `GatewayClass` remain operator-owned and are not accepted from an unprivileged sandbox endpoint. For non-Kubernetes runtimes, the egress sidecar stores the same extension resource set and runs the shared translator directly, but the field shapes remain the upstream Gateway API and agentgateway schemas.

The examples below use a sandbox named `sandbox-1` in namespace `sandboxes`.

**1. BatchSandbox, GatewayClass, and Gateway** (the sandbox and its capture surface):

```yaml
# The individual sandbox. Its Pod runs the OpenSandbox egress sidecar (Envoy).
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: sandbox-1
  namespace: sandboxes
spec:
  replicas: 1
  # ... sandbox pod template ...
---
# Cluster-wide: identifies the OpenSandbox Envoy egress controller.
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: opensandbox-egress-envoy
spec:
  controllerName: opensandbox.ai/egress-envoy
---
# Per-sandbox capture surface, owned by sandbox-1 so it is deleted with the sandbox.
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: sandbox-1-egress
  namespace: sandboxes
  labels:
    sandbox.opensandbox.io/sandbox: sandbox-1
  ownerReferences:
    - apiVersion: sandbox.opensandbox.io/v1alpha1
      kind: BatchSandbox
      name: sandbox-1
      uid: <sandbox-1-uid>       # set by the controller from the live BatchSandbox
      controller: true
      blockOwnerDeletion: true
spec:
  gatewayClassName: opensandbox-egress-envoy
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Same
    - name: tls
      protocol: TLS
      port: 443
      tls:
        mode: Passthrough
      allowedRoutes:
        namespaces:
          from: Same
```

The example uses a `BatchSandbox`. In agent-sandbox mode the only change is the owning object: the `ownerReferences` entry (and the resolving label) point at a `Sandbox` (`agents.x-k8s.io/v1alpha1`) instead of a `BatchSandbox`. The `GatewayClass`, `Gateway`, `HTTPRoute`, `TLSRoute`, `AgentgatewayBackend`, and `AgentgatewayPolicy` objects are identical:

```yaml
# agent-sandbox variant: same Gateway, different owner reference.
  ownerReferences:
    - apiVersion: agents.x-k8s.io/v1alpha1
      kind: Sandbox
      name: sandbox-1
      uid: <sandbox-1-uid>       # set by the controller from the live Sandbox
      controller: true
      blockOwnerDeletion: true
```

**2. AgentgatewayBackend** (dynamic egress backend):

```yaml
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayBackend
metadata:
  name: original-destination
  namespace: sandboxes
spec:
  dynamicForwardProxy: {}
```

**3. HTTPRoute** (HTTP matching and forwarding):

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: sandbox-1-api-example
  namespace: sandboxes
spec:
  parentRefs:
    - name: sandbox-1-egress
      sectionName: http
  hostnames:
    - api.example.com
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - group: agentgateway.dev
          kind: AgentgatewayBackend
          name: original-destination
```

**4. AgentgatewayPolicy** (authorization and other policy):

```yaml
apiVersion: agentgateway.dev/v1alpha1
kind: AgentgatewayPolicy
metadata:
  name: sandbox-1-block-admin-writes
  namespace: sandboxes
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: sandbox-1-api-example
  traffic:
    authorization:
      action: Deny
      policy:
        matchExpressions:
          - 'request.method == "PUT" && request.path.startsWith("/admin")'
    extAuth:
      backendRef:
        group: ""
        kind: Service
        name: policy-service
        port: 9000
```

In this example, Gateway API owns the listener and route shape. The `HTTPRoute` selects traffic for `api.example.com` and forwards it through an agentgateway dynamic backend that OpenSandbox compiles to Envoy dynamic-forward-proxy or original-destination behavior. The `AgentgatewayPolicy` attaches to that `HTTPRoute`, blocks any `PUT` to `/admin`, and also configures an external authorization service. Because the local authorization policy uses `action: Deny`, non-matching traffic is not denied by that CEL rule, but it is still subject to ext_authz and any other attached policies; see [Policy Semantics](#policy-semantics) for precedence.

For non-decrypting HTTPS, OpenSandbox should use the upstream Gateway API `TLSRoute` shape when it chooses to support SNI routes:

```yaml
# TLSRoute ships only in the Gateway API experimental channel (not yet graduated to v1).
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TLSRoute
metadata:
  name: sandbox-1-api-example-tls
  namespace: sandboxes
spec:
  parentRefs:
    - name: sandbox-1-egress
      sectionName: tls
  hostnames:
    - api.example.com
  rules:
    - backendRefs:
        - group: agentgateway.dev
          kind: AgentgatewayBackend
          name: original-destination
```

The important design rule is that any object under `gateway.networking.k8s.io` must remain a valid upstream Gateway API object. OpenSandbox can constrain which fields it supports and report unsupported fields through status, but it should not add OpenSandbox-only fields to upstream objects or create OpenSandbox-branded copies of those objects.

### Policy Semantics

`AgentgatewayPolicy` should follow the agentgateway [fixed phase model](https://agentgateway.dev/docs/kubernetes/latest/about/policies/filter-order/) (the `frontend`, `traffic`, and `backend` [policy sections](https://agentgateway.dev/docs/kubernetes/latest/about/policies/overview/)):

1. **Frontend/capture**: transparent listener setup, TLS/SNI inspection, network authorization.
2. **Traffic/pre-routing**: request authentication, external authorization, and transformations that must run before route selection.
3. **Traffic/post-routing**: route-level authorization, rate limiting, header mutations, and direct responses.
4. **Backend**: upstream connection policy such as backend TLS, credential injection, health, and future MCP-specific controls.

For the first implementation, only a subset must be implemented: `traffic.authorization`, `traffic.extAuth`, and backend forwarding through `AgentgatewayBackend`. The phase model should still be documented now so later policy additions have a stable ordering.

Kubernetes [`AgentgatewayPolicy`](https://agentgateway.dev/docs/kubernetes/latest/reference/api-kubespec/policies/) authorization uses `traffic.authorization.action` plus `traffic.authorization.policy.matchExpressions` (see agentgateway [CEL-based RBAC](https://agentgateway.dev/docs/kubernetes/latest/llm/rbac/)). Each expression in `matchExpressions` must evaluate to true for that policy to match. The action gives the matching policy one of three effects:

| Action | Behavior |
|--------|----------|
| `Deny` | If any attached `Deny` policy matches, the request or connection is denied. |
| `Require` | Every attached `Require` policy must match. Missing data or evaluation errors fail closed. |
| `Allow` | If any `Allow` policies are attached, at least one must match. If no `Allow` policies are attached, unmatched traffic is allowed unless denied or failed by `Require`. |

The initial CEL context should use names that map to agentgateway's [CEL variables](https://agentgateway.dev/docs/kubernetes/latest/reference/cel/variables/) where practical:

- `request.method`, `request.host`, `request.path`, `request.pathAndQuery`, `request.headers`
- `source.address`, `source.port`
- `destination.address`, `destination.port`, `destination.sni`
- `backend.name`, `backend.type`, `backend.protocol`

Future MCP-aware policy can add `mcp.methodName`, `mcp.tool.name`, `mcp.tool.target`, `mcp.prompt.name`, and `mcp.resource.name`.

### Future Centralized Policy Integration

The first implementation is per-sandbox. A future OSEP can add a centralized policy attachment model that integrates with agentgateway or a similar control plane.

In that centralized model, the Kubernetes Gateway API and agentgateway resources represent policy on a shared central gateway, not one copy of the API per sandbox. Operators should be able to define central `Gateway`, route, backend, and policy resources that target sandboxes by tenant, project, labels, runtime profile, or sandbox pool. The local Envoy sidecar in each sandbox would mostly become a transparent forwarding point: it captures outbound traffic, preserves enough source/sandbox identity for policy, and forwards the request or connection to the central gateway. The central gateway then evaluates the central policies and decides whether to allow or deny the traffic.

Per-sandbox policy may still exist later, but only as an optional local block before traffic is forwarded to the central gateway. A sandbox-local policy must not make egress more permissive than the central gateway policy. For example, if the central gateway denies `github.com`, a sandbox-level policy cannot allow `github.com`. If the central gateway allows `github.com`, a sandbox-level policy can still block `github.com` for that sandbox before forwarding. In other words, central policy is the authority for allowed egress, and local policy can only narrow the effective result. This local blocking layer should be treated as a future extension of the centralized model, not a requirement for the first centralized design.

The future model should:

- use upstream Gateway API and agentgateway resources for centralized policy instead of defining OpenSandbox-specific Gateway API clones
- allow central policies to target sandboxes by tenant, project, labels, runtime profile, or sandbox pool
- make the local Envoy sidecar forward captured traffic to the central gateway for policy enforcement rather than requiring every central policy to be compiled into each sandbox
- reuse agentgateway `frontend`, `traffic`, and `backend` policy sections instead of defining OpenSandbox-specific policy sections
- define deterministic conflict rules where central gateway `Deny` always wins, central gateway `Require` must still be satisfied, and any future sandbox-local policy can only add narrower block constraints
- preserve the CEL context naming above so policies can be shared between OpenSandbox-local Envoy enforcement and agentgateway centralized enforcement
- support delegating enforcement to a centralized agentgateway data plane without translating from an OpenSandbox-specific Gateway API clone

### Notes/Constraints/Caveats

- Envoy should occupy the same transparent interception role that mitmproxy occupies today. Running both for `80/443` in the same network namespace would create ambiguous redirect ordering and should be rejected by validation.
- Envoy transparent interception should use the original-destination listener filter so Envoy can recover the pre-redirect destination from `SO_ORIGINAL_DST`.
- For TLS, Envoy can enforce SNI and connection metadata without decrypting. HTTP request-level policy for HTTPS is out of scope for the first draft because it requires TLS interception or explicit proxying.
- The first implementation should focus on outbound HTTP and non-decrypting HTTPS. Other TCP protocols can continue to rely on DNS+nft enforcement.
- CEL rules are powerful and should start with a small documented context rather than exposing arbitrary process or sandbox state.

### Risks and Mitigations

- Redirect loops: exclude Envoy-owned upstream traffic by running Envoy under a dedicated UID or by setting a socket mark for upstream connections.
- Policy bypass: keep nftables as the final egress gate in dns+nft mode and preserve default-deny behavior.
- TLS expectations: document that HTTPS v1 policy is limited to SNI, destination, and connection metadata; do not imply path/method/header enforcement for HTTPS without a later TLS interception design.
- Control-plane reachability: any external authorization or policy backend must itself be permitted by the sandbox `networkPolicy` so dns+nft does not block Envoy's control callouts.
- Startup race: install redirect rules only after Envoy listeners and xDS config are ready; report unhealthy until the full path is active.
- Operational complexity: keep Envoy mode optional and mutually exclusive with mitmproxy mode.
- Policy ambiguity: keep fixed phase ordering and explicit rule semantics so policy outcomes are predictable.

## Design Details

### Current Egress Sidecar Behavior

Current egress behavior already separates enforcement into three layers:

- DNS redirect: `OUTPUT` UDP/TCP port `53` is redirected to the local DNS proxy.
- nftables filter: dns+nft mode applies static IP/CIDR sets and dynamic DNS-resolved allow sets in an `output` hook.
- experimental L7 path: transparent mitmproxy redirects non-mitmproxy-owned TCP `80/443` traffic to a local listener.

Envoy should be offered as an alternative to this third layer, selectable instead of mitmproxy rather than replacing it. When Envoy mode is enabled, the sidecar starts Envoy as a dedicated user, installs transparent `80/443` redirect rules that skip that user, and removes those rules on shutdown, mirroring the existing mitmproxy self-bypass model. When Envoy mode is not enabled, the mitmproxy path (and DNS-only or dns+nft mode) continues to work unchanged.

### L7 Backend Selection

The egress sidecar supports at most one transparent L7 interceptor at a time, chosen at sandbox startup. The `dns` and `dns+nft` modes are unchanged and run with or without an L7 backend.

- The existing transparent mitmproxy path is enabled by `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT=true` (with `OPENSANDBOX_EGRESS_MITMPROXY_PORT`, default `18081`). It starts `mitmdump` as the dedicated `mitmproxy` user and redirects outbound `80/443` to it, skipping that user's own UID.
- Envoy mode is enabled by an equivalent, mutually exclusive setting (for example a single `OPENSANDBOX_EGRESS_L7_BACKEND=none|mitmproxy|envoy` selector, with the current `OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT` kept as a backward-compatible alias for `mitmproxy`). It starts Envoy as a dedicated `envoy` user and redirects outbound `80/443` to Envoy's listener, skipping the `envoy` UID.

The sidecar backend setting is implementation configuration, not a public policy discriminator. The server or operator selects the backend before sandbox startup and maps it to sidecar settings such as `OPENSANDBOX_EGRESS_L7_BACKEND`. The runtime endpoint then advertises only the resource contracts that backend can enforce: Gateway API or agentgateway policy resources require Envoy, `credentialProxy.enabled` requires mitmproxy, and core `networkPolicy` requires neither L7 backend. A future create-time extension field may derive a backend requirement before workload creation, but that is outside the current egress API POC.

This keeps implementation names visible where operators need them without embedding them in SDK policy documents. A capability error may include the active and required implementation in diagnostic text, but clients branch on the stable error code and resource identity. Enabling both mitmproxy and Envoy for the same sandbox must be rejected before workload creation, because both would compete for the transparent `80/443` redirect. Incompatible resources sent to the active backend return `CAPABILITY_UNAVAILABLE` without changing the current extension revision. In the sidecar, Envoy startup mirrors the existing `startMitmproxyTransparentIfEnabled()` gate with a parallel `startEnvoyTransparentIfEnabled()`; only one runs, and the shared health gate stays unhealthy until the selected backend is ready, preserving the current fail-closed startup behavior.

### Envoy Interception

The sidecar should launch Envoy only after the DNS proxy and nftables setup are ready. Envoy startup should include:

1. Render bootstrap configuration pointing Envoy at local xDS or a filesystem snapshot.
2. Start Envoy as a dedicated `envoy` user.
3. Wait for Envoy listener/admin readiness.
4. Install IPv4 and, where supported, IPv6 transparent redirect rules for outbound TCP `80/443`, excluding loopback and Envoy-owned traffic.
5. Mark the egress sidecar healthy only after DNS, nftables, Envoy, and redirect setup all succeed.

For transparent redirects, Envoy should use the original-destination listener filter. The default forwarding path should use an original-destination cluster so Envoy connects to the pre-redirect upstream address after policy passes.

The sidecar owns Envoy's lifecycle and its configuration channel; Envoy does not read the Kubernetes API server. Envoy's bootstrap points its aggregated xDS (ADS) at a local endpoint served by the egress sidecar on loopback (for example `127.0.0.1:15355`). The sidecar runs a small in-process xDS snapshot cache: when it receives a new compiled configuration from the control plane, it swaps the snapshot atomically and Envoy hot-reloads without a restart. A filesystem xDS snapshot that Envoy watches is an acceptable fallback where a local gRPC xDS server is undesirable. Because the sidecar only applies the xDS handed to it and never watches Gateway API objects, it stays a thin data plane with no cluster RBAC.

### xDS Generation

Gateway API translation runs at the authoring boundary. For controller-managed Kubernetes resources, the OpenSandbox egress controller that owns the `opensandbox.ai/egress-envoy` `GatewayClass` translates and delivers xDS. For resources submitted directly to the egress API, the sidecar invokes the same translation library locally. The sidecar never watches the Kubernetes API, and a given resource set compiles to the same xDS regardless of which authoring path supplied it.

The xDS generation model should borrow concepts from kube-agentic-networking:

- listeners for transparent inbound-to-Envoy traffic, using original destination metadata and TLS inspector where needed
- route configurations for HTTP host/path/method/header matching
- clusters for original-destination forwarding and, where needed later, named upstream services
- HTTP filters ordered predictably: metadata extraction, external authorization, RBAC/CEL authorization, optional mutation, router

Gateway API route matches should lower directly into Envoy route match fields. Agentgateway `AgentgatewayPolicy` authorization should lower to Envoy RBAC policy conditions where possible. If Envoy-native CEL support is insufficient for a rule, the implementation should reject the policy at validation time or require an external authorization backend rather than silently weakening policy.

Every generated snapshot has a monotonically increasing extension revision and a content digest. The control plane sends one complete snapshot over the authenticated delivery channel; the sidecar stages it and keeps the previous snapshot active until Envoy ACKs the new xDS revision. All required xDS resource types must ACK that revision before it is committed. A partial ACK remains staged, and any required resource-type NACK or timeout fails the whole revision, so the sidecar never reports or publishes a mixed-revision configuration. On failure, the sidecar discards the staged snapshot, reports the rejected revision and reason, and leaves the previous revision active. Only complete acknowledgment allows the control plane to publish `Programmed=True` and the new effective extension revision.

Delivery is idempotent by revision and digest. If the control plane loses the response, it retries the same pair or queries the sidecar's applied revision instead of allocating a new revision. A sidecar that restarts remains unhealthy and fail-closed until it restores or receives the last acknowledged snapshot. The filesystem-snapshot fallback must provide the same stage, acknowledge, and atomic-rename behavior; merely writing several xDS files in place is not conformant.

Cross-resource validation runs before snapshot generation. Unsupported Gateway API or agentgateway fields are rejected with `INVALID_EXTENSION` and a field path. An `extAuth` backend must resolve to an authorized in-sandbox or control-plane service and must be reachable under the core `networkPolicy`; otherwise the request fails with `CAPABILITY_UNAVAILABLE` rather than programming a filter whose authorization callouts will be blocked by nftables.

### HTTPS Handling

The first implementation must not perform TLS interception. HTTPS handling is therefore limited to:

- transparent TCP capture on port `443`
- TLS inspector / SNI-based matching
- destination IP and port matching
- original-destination forwarding after policy passes
- DNS+nft enforcement as the authoritative domain/IP boundary

HTTP method, path, query, and header policy applies to plaintext HTTP traffic and to any future explicitly proxied or decrypted HTTPS mode, but not to transparent HTTPS in this first draft.

The Kubernetes controller detects `TLSRoute` support through API discovery and reports the exact `gateway.networking.k8s.io/v1alpha2`, `TLSRoute` capability as unavailable when the experimental CRD is absent. Submitting a `TLSRoute` in that state is rejected; it is not silently downgraded. Gateway listener and SNI behavior that remains available without `TLSRoute` is reported as separate qualified capability features.

### Runtime Policy API

OpenSandbox supports two authoring paths with the same resource shapes: the direct egress API consumed by SDKs and the Kubernetes API consumed by the controller. The sidecar never reads the Kubernetes API server directly.

**DNS/nft policy.** The existing `POST /policy` endpoint on the sidecar (authenticated with the egress token, listening on the sidecar's local address) continues to own the DNS/nft `networkPolicy` for every mode. This proposal does not change it.

**SDK facade and API ownership.** The SDK presents core and extended policy through one egress service and one resolved egress endpoint. The `egress-api.yaml` contract now includes:

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/capabilities` | Report extension kinds available for this egress endpoint. |
| `GET` | `/extensions` | Return the effective extension resource set, revision, and conditions. |
| `PUT` | `/extensions` | Atomically replace the set using an optional expected revision. |

There is no generic `PATCH` in the first version because references can span several heterogeneous resources; an empty `resources` list clears only L7 extensions and preserves `/policy`. The egress sidecar validates the whole candidate set and publishes a new revision only after the active implementation acknowledges it.

**Envoy L7 policy on the Kubernetes runtime.** Gateway API and agentgateway resources may be authored in the Kubernetes API server. End to end:

1. The lifecycle controller, an authorized user, or the sandbox-scoped extension API creates the per-sandbox `Gateway`, `HTTPRoute`/`TLSRoute`, `AgentgatewayBackend`, and `AgentgatewayPolicy` objects bound to `sandbox-1`.
2. The OpenSandbox egress controller, which owns the `opensandbox.ai/egress-envoy` `GatewayClass`, reconciles those objects: it resolves `parentRefs`/`targetRefs`/`backendRefs`, validates them, and runs the shared translator to produce an Envoy xDS snapshot for `sandbox-1`.
3. The controller pushes that snapshot to `sandbox-1`'s sidecar over the authenticated local delivery channel, and sets Gateway API/agentgateway status (`Accepted`, `Programmed`, or a rejection reason) from the sidecar's acknowledgment.
4. The sidecar swaps its local xDS snapshot and Envoy applies it. If Envoy rejects the configuration, the sidecar keeps the previous working snapshot and reports the failure through status, so a bad policy never silently opens or closes the sandbox.

When Kubernetes resources are the authoritative source, the direct egress API reports their effective extension state but does not permit a competing writer: its capability operations omit `replace`, and `PUT /extensions` returns `CAPABILITY_UNAVAILABLE`. This prevents controller and SDK updates from racing.

**Envoy L7 policy on non-Kubernetes runtimes.** There is no API server, so SDKs author resources through the direct egress endpoint. The endpoint is the sandbox ownership boundary; `metadata.namespace` may be defaulted for reference resolution, and Kubernetes owner references are not required. The sidecar stores the revisioned resource set, runs the shared validator and translator, and applies the generated xDS snapshot.

In both authoring paths the commit is fail-closed: validate the complete resource set, resolve `parentRefs`/`targetRefs`/`backendRefs`, generate xDS, apply it to Envoy, wait for acknowledgment, then publish the new revision. On any failure the previous working configuration remains active and the API returns a structured extension error with a stable string code, message, optional resource identity, and optional field path.

### POC Scope, Status, and Acceptance Criteria

The current branch contains an API, SDK, and runtime-transaction POC, not an Envoy enforcement implementation.

See the [Egress Extension API POC walkthrough](../docs/examples/egress-extension-poc.md) for detailed build, Kubernetes deployment, verification, failure-case, and cleanup steps validated on `testmember-3-admin`.

**Implemented in the POC:**

- [x] `egress-api.yaml` defines `GET /capabilities`, `GET /extensions`, and atomic `PUT /extensions`, plus resource, capability, revision, condition, and structured error schemas.
- [x] The JavaScript generated egress client contains the new paths and open `metadata`/`spec` wire types.
- [x] The stable JavaScript SDK exports reusable extension models, adds `getCapabilities()`, `getExtensions()`, and `replaceExtensions()` to `Egress`, and exposes them through `Sandbox.getEgressCapabilities()`, `Sandbox.getEgressExtensions()`, and `Sandbox.replaceEgressExtensions()`.
- [x] A focused SDK test proves exact capability parsing, expected-revision replacement, and lossless round-tripping of unknown `metadata` and `spec` fields.
- [x] The sidecar serves authenticated capability and extension routes backed by an in-memory revisioned store with strict envelopes, exact capability checks, optimistic concurrency, and rollback on apply failure.
- [x] An explicit storage-only POC applier (`OPENSANDBOX_EGRESS_EXTENSIONS_POC=true`) makes the flow runnable without claiming Envoy enforcement; it reports `Programmed=Unknown` and `PocStored`.

**Not implemented in the POC:**

- [ ] Durable persistence and full Gateway API/agentgateway schema and reference validation.
- [ ] Translation of accepted resources to Envoy xDS.
- [ ] Envoy process lifecycle, transparent redirect, and xDS ACK/NACK delivery.
- [ ] Python and other language SDK facades.
- [ ] Lifecycle create-time extension resources or automatic backend selection from requested resources.

The next implementation scope is:

1. Replace the storage-only applier with full Gateway API/agentgateway schema, reference, scope, capability, and conflict validation.
2. Translate `Gateway`, `HTTPRoute`, and `AgentgatewayBackend` into the first single-replica Envoy path. Continue to report `TLSRoute` and `AgentgatewayPolicy` as unavailable until their translation is implemented; never accept and ignore them.
3. Apply one complete xDS snapshot with revision/digest idempotency and Envoy ACK/NACK handling. Keep the last acknowledged snapshot active on every failed update.
4. Add durable state recovery and equivalent JSON-preserving facades to the other SDK languages before the API graduates beyond experimental status.

The API/runtime transaction POC is successful because it demonstrates:

- Existing direct sidecar egress SDK tests pass unchanged when extension methods are not used.
- Unknown JSON fields survive an SDK and sidecar read-modify-write round trip.
- A stale expected revision returns `REVISION_CONFLICT`; unknown and unavailable resource kinds return distinct errors.
- A failed injected applier leaves the prior revision active, proving the transaction boundary needed for future Envoy NACK handling.
- Capability discovery reports exact resource versions and marks known but unavailable kinds with reasons.

The Envoy enforcement POC remains successful only when an Envoy-capable sandbox accepts a core deny-default policy plus a valid Gateway resource set, enforces the translated HTTP route, and retains its previous snapshot after an Envoy NACK or timeout.

## Test Plan

- Unit test Envoy redirect rule generation, including loopback and Envoy UID bypass.
- Unit test xDS translation from representative `Gateway`, `HTTPRoute`, `TLSRoute`, `AgentgatewayBackend`, and `AgentgatewayPolicy` inputs.
- Unit test CEL `Allow`, `Deny`, and `Require` semantics, especially missing-field behavior.
- Unit test extension capability discovery, unsupported kinds, conflicting backend requirements, and stale revision handling.
- SDK contract test that unknown extension resources round-trip without losing `metadata` or `spec` fields.
- Integration test `dns+nft+envoy` default deny with allowed FQDN and denied FQDN.
- Integration test HTTP method/path/header policy on outbound port `80`.
- Integration test HTTPS SNI/destination policy on outbound port `443` without TLS interception.
- Integration test that Envoy upstream traffic does not loop back into Envoy.
- Integration test health behavior when Envoy fails to start or xDS config is invalid.
- Integration test idempotent snapshot retry after a lost acknowledgment and previous-revision retention after Envoy NACK or timeout.
- Benchmark overhead against `dns+nft` and the current `dns+nft+mitmproxy` benchmark.

## Drawbacks

- Envoy increases image size, startup complexity, and runtime memory use.
- L7 policy semantics for HTTPS are limited unless TLS interception or explicit proxying is introduced later.
- xDS translation adds a new control-plane surface inside the egress component.
- CEL support increases policy expressiveness but also raises validation, observability, and debugging requirements.

## Alternatives

- Keep mitmproxy as the only L7 backend. This is simpler, but Envoy is more widely accepted for production traffic policy and has standard xDS/RBAC/ext_authz primitives.
- Run Envoy alongside mitmproxy. This is rejected for the initial design because both would compete for transparent `80/443` interception in the same network namespace.
- Replace DNS/nft with Envoy. This is rejected because DNS/nft already provides protocol-independent fail-closed enforcement and should remain the default boundary.
- Add a small `networkPolicy.l7` extension instead of Gateway API resources. This is rejected because it would blur L3/L4 and L7 concerns and would require translating from an OpenSandbox-specific shape into the upstream Gateway API later.
- Add `type: envoy` or `type: mitmproxy` to `NetworkPolicy`. This is rejected because the backend is an implementation choice, while core DNS/nft policy and optional L7 policy compose and have independent schemas.
- Define OpenSandbox-specific `EgressGateway` or `EgressRoute` resources. This is rejected because Gateway API is the upstream common API for gateways and routes, and `gateway.networking.k8s.io` objects should not be cloned under an OpenSandbox API group.
- Start with structured matches only and defer CEL. This is rejected because CEL is central to the agentgateway-style policy model and avoids inventing a long list of one-off match fields.

## Infrastructure Needed

- Envoy binary or image layer in the egress component.
- Generated Envoy bootstrap and xDS snapshot support.
- A dedicated `envoy` runtime user for redirect bypass.
- Policy examples for allowing any external authorization backends through the existing `networkPolicy` when dns+nft mode is enabled.
- Optional test fixtures for validating generated xDS resources.
- Documentation for policy phases, CEL context, and HTTPS limitations.
- Shared extension resource, capability, revision, condition, and error models in the egress spec and handwritten SDK facades.

## Upgrade & Migration Strategy

- Existing `dns` and `dns+nft` deployments continue unchanged.
- mitmproxy transparent mode remains a fully supported option and is not deprecated; Envoy is an additional opt-in L7 backend that is mutually exclusive with mitmproxy within a single sandbox.
- Opting a Kubernetes sandbox into Envoy requires operator/server configuration to start the Envoy backend plus a per-sandbox `Gateway` referencing the OpenSandbox Envoy egress `GatewayClass`, with `HTTPRoute`/`TLSRoute` and `AgentgatewayPolicy` resources as needed. Creating Gateway API objects alone for a sandbox whose backend does not satisfy them returns unavailable status and does not produce an accepted-looking configuration with no data plane.
- Switching an existing sandbox between mitmproxy and Envoy in place is not supported. Recreate the sandbox with a compatible extension set; this proposal defines no in-flight request semantics for a backend switch.
- Existing `networkPolicy` behavior remains the compatibility baseline and is not migrated into Gateway API resources automatically.
- Existing SDK egress methods continue to use the direct sidecar API. New extension methods are additive and use the same resolved egress endpoint.
- A later implementation can provide migration guidance for credential vault behavior that currently depends on mitmproxy scripts.
