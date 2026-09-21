---
title: Ingress Policy
authors:
  - "TBD"
creation-date: 2026-09-21
last-updated: 2026-09-21
status: draft
---

# OSEP-0029: Ingress Policy

## Goal

Define a deny-by-default ingress policy model in which tenant policies establish
access guardrails and own traffic limits, while sandbox and pool policies can
only narrow the exposed ports, protocols, paths, and methods. Provide the same
security guarantees for directly created and pool-allocated sandboxes. The
initial scope covers application HTTP requests and WebSocket connections.

**Application-only scope:** These ingress policies govern application exposure.
Platform-service access, including execd and the egress-management service, is
out of scope. This proposal does not define or change their access model, and its
application allowlists and traffic limits do not govern those services.

**Single-replica scope:** Ingress policy is supported only for `BatchSandbox`
resources with `spec.replicas: 1`, for both direct creation and pool allocation.
Requests that enable ingress policy for multi-replica `BatchSandbox` resources
are rejected. Multi-replica ingress-policy support is out of scope.

1. **Establish tenant guardrails that sandbox policies can only narrow.** Let
   administrators approve target ports or port ranges, protocols, paths, and
   methods. Compute effective access from the intersection of tenant and
   sandbox/pool selectors. Deny traffic not permitted by the effective policy;
   a sandbox or pool policy must never expand tenant permissions.
2. **Keep traffic limits under administrator control.** Take limits exclusively
   from tenant policy, not sandbox or pool policy. Use protocol-level profiles
   for HTTP request rate and timeouts, and for WebSocket upgrade rate and stream
   duration. Apply each profile independently to each sandbox, across its exposed
   ports and paths, with no tenant-wide aggregate budget. In v1, rate buckets are
   local to each serving Envoy process. Connection and concurrency limits, global
   accounting, and independently configurable rate-burst capacity are out of scope.
   Sandbox users may narrow selectors but cannot configure or override these limits.
3. **Support policy-governed pool allocation.** Let pool templates declare ingress
   restrictions within tenant guardrails. Allocated sandboxes use the pool's
   network policy; reject allocation requests that attempt to supply a replacement
   network policy. Prevent a previous assignment's routes or authorized connections
   from granting access to a later assignment.
4. **Apply tenant policy changes to new and existing sandboxes.** Keep tenant
   policy a live guardrail for directly created and pool-allocated sandboxes, so
   changing tenant permissions or limits does not require recreating workloads.
   Make rollout status visible rather than treating policy acceptance as proof
   that enforcement has completed.
5. **Enforce outside the workload and fail closed.** Validate policy before
   admitting a workload and enforce it before traffic reaches the OpenSandbox
   ingress proxy. Prevent direct access from bypassing the enforcement path.
   Reject unsupported or ambiguous constraints rather than silently weakening
   them, and deny affected traffic when required policy or authorization state
   cannot be established.
6. **Preserve tenant isolation and endpoint routing.** Keep management operations
   scoped to the API-key-resolved tenant. Authorize callers for the intended
   tenant, sandbox, and declared endpoint before forwarding traffic. Preserve the
   existing ingress proxy's endpoint-resolution role without allowing untrusted
   routing input to cross tenant boundaries or expose undeclared ports.
7. **Make access decisions auditable.** Expose effective policy, effective
   per-sandbox limits and their local rate-limit scope, policy revision, and
   enforcement status. Provide actionable denial reasons and audit policy changes
   and access decisions with tenant and sandbox attribution, without exposing
   credentials or sensitive request contents.

## User Experience

All management requests use
`OPEN-SANDBOX-API-KEY` and apply to the caller's tenant; users do not supply a tenant
field in the request body.

- **Tenant/platform administrator:** configures permitted ingress traffic and the
  limits applied independently to each sandbox.
- **Sandbox user:** creates or updates a standalone sandbox with narrower access
  selectors, or allocates a sandbox from a pool with predefined rules. The user
  cannot configure traffic limits.
- **Application client:** obtains an approved endpoint and supplies its required
  access credentials when making HTTP requests or opening WebSocket connections.

These examples create one sandbox per request. A pool may serve multiple
independent sandbox allocations, each with its own limits. Ingress policy is not
supported for multi-replica batch requests.

### Configure the Tenant Guardrail

The administrator replaces the tenant's policy with `PUT /v1/ingress-policy`:

```json
{
  "ingress": [
    {
      "ports": [{ "from": 8080, "to": 9080 }],
      "protocols": ["http"],
      "methods": ["GET", "POST"],
      "paths": ["/api/*"]
    },
    {
      "ports": [{ "from": 8080, "to": 8080 }],
      "protocols": ["websocket"],
      "methods": ["GET"],
      "paths": ["/ws/*"]
    }
  ],
  "limits": {
    "http": {
      "rateLimit": { "requests": 10, "unit": "Second" },
      "timeout": { "requestTimeout": "60s" }
    },
    "websocket": {
      "rateLimit": { "requests": 10, "unit": "Minute" },
      "timeout": {
        "requestTimeout": "0s",
        "maxStreamDuration": "300s",
        "streamIdleTimeout": "60s"
      }
    }
  }
}
```

Unmatched traffic is denied. These are allow rules; a tenant rule alone does not
expose a sandbox endpoint. The sandbox/pool policy must also permit the request's
port, protocol, path, and method.

Tenant rules contain access selectors only. Limits are configured once under
`limits.http` and `limits.websocket`, not separately on each rule. Each sandbox
gets its own HTTP allowance and WebSocket allowance, shared across that sandbox's
exposed ports, paths, and clients. Adding selectors does not add capacity. Traffic
to sandbox A does not consume sandbox B's allowance, even if both belong to the
same tenant or pool. There is no combined HTTP-plus-WebSocket or tenant-wide budget.

**Rate limiting is local in v1.** Each serving gateway replica has independent
rate accounting. Adding replicas can
increase total accepted traffic; the single-replica `BatchSandbox` restriction
does not restrict gateway replicas or create a global traffic cap.

Within either protocol profile, `rateLimit` uses two optional fields:

| Field | Default | Meaning |
| --- | --- | --- |
| `requests` | `10` | Request allowance replenished per unit; also the maximum accumulated allowance. This is a positive integer, not a concurrency setting. |
| `unit` | `"Second"` | Refill interval: `Second`, `Minute`, `Hour`, `Day`, `Week`, `Month`, or `Year`. These are fixed implementation durations, not calendar-aligned windows. |

`rateLimit: {}` selects both defaults. `rateLimit: {"unit": "Minute"}` gives ten
requests per minute. A bucket starts full and refills up to its capacity; there
is no separate burst-capacity field or strict sliding-window guarantee. Eligible
HTTP requests and WebSocket upgrade attempts each consume one token. WebSocket
messages do not consume request tokens. Omitting the entire `rateLimit` object
configures no tenant rate cap for that protocol; omitting fields inside a supplied
object selects defaults rather than disabling it.

The HTTP example allows an initial burst of ten requests per sandbox per gateway
replica, with a refill rate of ten per second. The WebSocket example allows ten
upgrade attempts per minute per sandbox per replica. Across two replicas, each
example can have twice the allowance. Neither is a limit on simultaneous clients.

Connection and concurrency limits, including active WebSocket session limits,
are out of scope. Unsupported limit fields are rejected in v1.

For ordinary HTTP, `requestTimeout: "60s"` limits waiting for the full
upstream response after the request is received; it is not a client-upload deadline.
For WebSocket, `requestTimeout: "0s"` disables that response-completion timeout;
`maxStreamDuration: "300s"` bounds stream lifetime, and `streamIdleTimeout: "60s"`
bounds inactivity. Timeout fields sit directly under each profile's `timeout`
object because `limits.http` or `limits.websocket` already identifies the protocol.
An `http` rule never implicitly permits an upgrade. Policy reads expose effective
values and defaults alongside their scope.

`GET /v1/ingress-policy` returns the configured policy and its status. Tenant
changes apply to existing and future sandboxes without users recreating them or
resubmitting their individual policies.

### Create or Update a Standalone Sandbox

The user supplies narrower selectors when calling `POST /v1/sandboxes`:

```json
{
  "image": { "uri": "python:3.11" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "500m", "memory": "512Mi" },
  "timeout": 3600,
  "networkPolicy": {
    "ingress": [
      {
        "port": 8080,
        "protocol": "http",
        "methods": ["GET"],
        "paths": ["/api/reports/*"]
      },
      {
        "port": 8080,
        "protocol": "websocket",
        "methods": ["GET"],
        "paths": ["/ws/reports"]
      }
    ]
  }
}
```

The user declares the application port and protocol in each ingress rule, then
narrows the tenant-approved methods and paths. Listening on a port does not by
itself expose that port. Sandbox rules contain no `limits`; supplying them is
rejected rather than silently ignored.

For a standalone sandbox, including one detached from a pool during pause, this
draft supports live policy replacement with
`PUT /v1/sandboxes/{sandboxId}/networkpolicy`. Its body is the
complete replacement `networkPolicy` object, not the sandbox creation request.
Omitted rules are removed rather than merged; callers must retain any desired
egress rules when replacing the combined network policy.
`GET /v1/sandboxes/{sandboxId}/networkpolicy` returns the configured policy,
effective per-sandbox limits, and enforcement status.

Creation without `networkPolicy`, or with an omitted or empty `ingress` list,
does not expose application endpoints. A directly created sandbox can start with
deny-all ingress and receive a policy update later. Creation and updates remain
subject to tenant guardrails and the single-replica restriction.

### Create and Use a Pool

An authorized pool creator supplies the shared selectors once with
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
    "ingress": [
      {
        "port": 8080,
        "protocol": "http",
        "methods": ["GET"],
        "paths": ["/api/reports/*"]
      },
      {
        "port": 8080,
        "protocol": "websocket",
        "methods": ["GET"],
        "paths": ["/ws/reports"]
      }
    ]
  }
}
```

Every sandbox allocated from the pool uses these selectors together with the
latest tenant guardrail and administrator-defined per-sandbox limits. Pool rules
cannot contain `limits`. Omitting the pool's ingress policy leaves application
ingress denied, and unallocated pool members are not publicly exposed.

The sandbox user then calls `POST /v1/sandboxes`:

```json
{
  "timeout": 3600,
  "extensions": { "poolRef": "reports-pool" }
}
```

`networkPolicy` is rejected in a pool allocation request. Pool rules are
immutable, and a sandbox cannot replace them while it remains pool-backed.
Changing pooled selectors requires a new pool; changing tenant-owned limits does
not. Pause detachment gives the sandbox its own equivalent policy, after which
the standalone update operation is supported under the current tenant guardrail.

Two allocations from `reports-pool` have the same selectors but independent
traffic limits. A previous assignment's endpoint credentials, routes, or open
connections must not provide access to a later assignment of the same runtime.

### Access the Sandbox and Observe Enforcement

The sandbox user starts an application listening on port `8080` with the declared
HTTP routes and WebSocket endpoint; the example entrypoint only keeps the
sandbox running. Once policy enforcement is active, the user obtains the endpoint
with the existing lookup operation:

```bash
curl "$OPEN_SANDBOX_URL/v1/sandboxes/$SANDBOX_ID/endpoints/8080" \
  -H "OPEN-SANDBOX-API-KEY: $API_KEY"
```

The client uses the returned `endpoint` and includes every required header from
`headers` when accessing it. Management-plane API-key authentication and
data-plane endpoint authorization remain distinct. Obtaining an endpoint or
possessing its access credentials does not bypass ingress policy, and application
endpoint lookup must not expose undeclared application ports. These lookup and
enforcement rules do not cover platform-service access.

For either sandbox created above, an authorized client observes the following
behavior while the policy is active and limits have not been exceeded:

| Client request | Expected experience |
| --- | --- |
| HTTP `GET /api/reports/daily` on port `8080` | Allowed, subject to that sandbox's HTTP limits. |
| HTTP `POST /api/reports/daily` on port `8080` | Denied: the tenant permits `POST`, but the sandbox/pool does not. |
| HTTP `GET /api/admin` on port `8080` | Denied: the path is outside the sandbox/pool's allowed prefix. |
| WebSocket upgrade to `/ws/reports` on port `8080` | Allowed, subject to that sandbox's WebSocket upgrade-rate and stream-timeout settings. |
| Ordinary HTTP `GET /ws/reports` on port `8080` | Denied: WebSocket permission does not grant ordinary HTTP access to that path. |
| Request to undeclared port `9090` | Denied; starting a listener does not grant exposure. |

A successful create or update response does not necessarily mean the requested
policy is active. Users can distinguish pending, failed, and active enforcement
and inspect the applicable policy revision, effective per-sandbox limits, and
local rate-limit scope, alongside actionable denial reasons. Affected traffic
remains blocked while required policy or authorization state is not ready; there
is no temporary unrestricted access.

### Manage Policy and Lifecycle Changes

| User action | Expected experience |
| --- | --- |
| Update tenant selectors or limits | Existing and future sandboxes use the updated guardrail once enforced; no sandbox recreation or per-sandbox resubmission is required. |
| Update a standalone sandbox policy | Supported for a directly created single-replica sandbox or one detached from a pool during pause; affected traffic remains blocked until the updated policy is active. |
| Supply limits in a sandbox or pool rule | Rejected: only the administrator-owned tenant policy configures limits. |
| Omit some or all fields within `rateLimit` | Accepted; omitted fields use their defaults, and explicitly supplied values override them. |
| Supply `networkPolicy` during pool allocation or update pooled selectors | Rejected; select or create a pool with the required rules. |
| Request ingress policy for a multi-replica batch | Rejected rather than partially enforcing the policy. |
| Exceed a sandbox's traffic limits | The affected request or connection is rejected or terminated according to the applicable limit, with an actionable reason; other sandboxes retain their own allowances. |
| Pause a sandbox | New application traffic is blocked; in-flight HTTP requests and WebSocket sessions stop before pause completes. The sandbox ID and access restrictions are retained; a pool-created sandbox becomes standalone with its own equivalent policy. |
| Resume a sandbox | Access remains blocked until the restored runtime and latest applicable policies are ready. Clients reconnect using the current endpoint and required headers; previous connections are not restored. |
| Delete a sandbox | Its ingress access and connections stop; other allocations from the same pool remain independent. |

Policy changes and access decisions are auditable with tenant and sandbox
attribution, without logging endpoint credentials or sensitive request contents.

## High-Level Architecture

The proposed architecture separates policy management, traffic enforcement, and
endpoint resolution. The lifecycle server accepts tenant-scoped policy intent;
Kubernetes resources hold the desired policy and sandbox ownership state. An
ingress policy controller prepares versioned enforcement configuration. The Envoy
Gateway controller is the control plane: it translates Gateway API and Envoy
extension resources into proxy configuration. The shared Envoy proxies are the
data plane: they enforce ingress policy before forwarding approved traffic to the
existing OpenSandbox ingress deployment. "Shared" means they serve multiple sandboxes,
not that rate counters are shared across gateway replicas.

OpenSandbox ingress runs as a separate shared Deployment behind an internal
Kubernetes Service. Envoy forwards approved requests to ingress endpoints through
a normal Service backend reference; ingress resolves and proxies the sandbox
endpoint. The Envoy gateway and ingress deployments scale and roll out
independently. Rate buckets remain local to each Envoy process,
regardless of which ingress replica handles the forwarded request.

Native routing configuration is shared per tenant and protocol, not generated
per sandbox. Each tenant has at most two `HTTPRoute` and `BackendTrafficPolicy`
pairs: HTTP and WebSocket. A shared authorization service resolves the target
allocation and returns trusted routing headers. Envoy selects the tenant's
protocol profile and uses the trusted `BatchSandbox.metadata.uid` to keep each
sandbox's local rate bucket separate. Route and cluster counts scale with tenants;
authorization state and runtime rate buckets still scale with allocations.

The shared bootstrap route supports WebSocket upgrades before authorization
classifies the request. Its fallback backend is a deny-only HTTP listener on the
authorization service's Deployment, not the ingress Service. This
listener always returns `403`; it never proxies traffic or accepts an upgrade.

The OpenSandbox ingress remains responsible for resolving the authorized sandbox
and port to the current routable IP address and proxying traffic. On this
gateway-protected path, it does not authenticate the caller again, evaluate
ingress policies, verify policy revisions, or enforce rate limits. Authorization
belongs to the authorization service; Envoy enforces that decision and the native
traffic limits. The normal application path runs through the gateway rather than
the lifecycle server. Policy evaluation does not require a Kubernetes API call
per request.

This section describes the target Kubernetes deployment. The resources and
integrations below are proposed additions, not claims about current capabilities.
Public schemas and internal configuration-delivery contracts belong in the
detailed design.

```text
Management and reconciliation

  administrator / sandbox user
      -> lifecycle API (API-key-resolved tenant)
      -> Kubernetes admission and policy resources
      -> ingress policy controller
          -> tenant/protocol HTTPRoutes and BackendTrafficPolicies
              -> Envoy Gateway controller -> shared Envoy gateway pods
          -> authorization policy snapshots and readiness
      <- policy and enforcement status

Application traffic

  client
      -> shared Envoy proxy (gateway pod)
          -> strip untrusted internal headers; select bootstrap route
          <-> authorization service (identity, policy, allocation readiness)
          -> reselect tenant/protocol route using authorized headers
          -> sandbox-UID-keyed local rate bucket and stream timeouts
          -> internal ingress Service backend
              -> OpenSandbox ingress pod (separate Deployment)
                  -> look up current routable endpoint by sandbox UID and port
                  -> declared application endpoint (sandbox pod)
```

The authorization service provides decisions without sitting in the
application-body forwarding path. Envoy remains the traffic enforcement point.
Rate tokens remain local to each Envoy process; v1
does not use a shared rate-limit service, concurrency coordinator, or distributed
limit accounting.

### Policy Sources and Ownership

Use three tenant-scoped policy resources:

| Resource | Owner and scope | Contents and mutability |
| --- | --- | --- |
| `TenantIngressPolicy` | One policy for a tenant, managed by its administrator. | Permitted ports, protocols, methods, and paths, plus administrator-owned HTTP and WebSocket limit profiles shared across each sandbox's selectors. Changes apply to existing and future sandboxes. |
| `SandboxIngressPolicy` | One standalone `BatchSandbox` with `spec.replicas: 1`, created directly or detached from a pool during pause. The policy has the batch's name and namespace, with an owner reference to its UID. | Sandbox access selectors, created at direct creation or during controller-only pooled pause handoff. Live replacement is supported once standalone; it cannot define limits or expand the tenant guardrail. |
| `PoolIngressPolicy` | One pool, shared by its allocated sandboxes. | Immutable pool access selectors. Allocations cannot replace these selectors or define their own limits. |

A pool-backed sandbox normally references its pool's policy without a separate
`SandboxIngressPolicy`. During pause, the controller creates the batch-owned
policy before runtime detachment clears `spec.poolRef`; after detachment, that
policy is the sole sandbox-policy source. Standalone and pooled policies are
alternatives, not layers to merge. Pool capacity may exceed one, but every
consuming `BatchSandbox` must have `spec.replicas: 1`.

The lifecycle server resolves the tenant from the management API key and checks
the request before writing resources. Kubernetes RBAC preserves the administrative
boundary for tenant-policy writes. Admission independently validates tenant
ownership, policy constraints, single-replica scope, and pool immutability so
direct Kubernetes access cannot bypass those checks. Both validation paths use
the same rules and defaults, including defaults for omitted fields within
`rateLimit`.

Batch and pool lifecycle state provide the trusted relationship between a sandbox,
its policy source, and its current runtime allocation. Untrusted request headers,
workload labels, and a pod IP alone are not sufficient evidence of that relationship.
An omitted or empty sandbox/pool ingress rule list creates no application exposure.

### Component Responsibilities

| Component | Responsibility |
| --- | --- |
| Lifecycle server | Exposes policy management, validates and defaults requests, and returns configured policy, effective per-sandbox limits, and rollout status. It does not make request-time application authorization decisions. |
| Ingress policy controller | Watches tenant, standalone-sandbox, and pool policies together with consumer lifecycle state. Creates the batch-owned policy during pooled pause handoff. Owns tenant/protocol routes and limit profiles, publishes allocation-aware authorization state, and tracks activation and cleanup. |
| Envoy Gateway controller | Manages the shared Envoy gateway pods and translates controller-owned Gateway API and supported Envoy policy resources into proxy configuration. It does not manage the separate OpenSandbox ingress Deployment. Unsupported constraints must not be silently dropped. |
| Shared Envoy proxies | Terminate public TLS, strip untrusted internal headers before routing, invoke shared authorization, reselect a tenant/protocol route, apply sandbox-UID-keyed local rate limits and stream timeouts, and forward to the internal ingress Service backend. |
| Authorization service | Authenticates the caller, parses the configured ingress mode, resolves current ownership and runtime binding, and evaluates both selector layers against the normalized request. Checks readiness and returns trusted tenant, protocol, and sandbox UID headers for route reselection. Exposes a separate deny-only HTTP listener for bootstrap fallback. |
| OpenSandbox ingress deployment | Runs independently behind a gateway-only internal Service. Uses the trusted tenant and sandbox UID plus the requested port to look up the current routable IP through the configured runtime provider, then proxies HTTP or WebSocket traffic. Maintains endpoint lifecycle and stream closure without repeating authentication or policy checks. |

One logical ingress policy controller owns the generated ingress configuration.
Tenant and sandbox/pool reconciliation must not independently overwrite the same
route or limit profile. Shared listeners, certificates, gateway settings, and the
ingress Deployment and Service remain infrastructure-owned rather than tenant
policy fields. The platform also owns the common header-sanitization and
authorization policies and the deny-by-default bootstrap route. Tenants cannot
override these policies or edit generated routes.

The controller retains the tenant and sandbox/pool policies as separate sources.
Any effective configuration or authorization snapshot is derived state, not a
new independently editable effective-policy resource. Updating the tenant policy
does not rewrite a sandbox user's selectors or mutate an immutable pool policy.

### Request Authorization and Routing

Every application request must satisfy:

```text
authorized caller and current sandbox allocation
  AND request permitted by the active tenant ingress policy
  AND request permitted by the active sandbox ingress policy (standalone OR pool)
  AND applicable per-sandbox traffic limits
```

The standalone or pool policy is selected from trusted ownership state before
evaluation; a caller cannot choose whichever policy is more permissive.

1. **Sanitize before routing.** Envoy accepts traffic on an infrastructure-owned
   listener and removes client-supplied internal identity and routing headers
   before the first route selection. The request initially selects an
   upgrade-capable forwarding bootstrap route that invokes shared authorization.
   Its only fallback backend is the deny-only HTTP listener.
2. **Authorize the target and operation.** The authorization service validates the
   caller, parses the target using the one admin-selected ingress mode, resolves
   current tenant ownership and allocation, checks policy readiness, and evaluates
   port, protocol, method, and normalized application path against both policy layers.
   A request must match a complete rule at each layer; combining unrelated
   selectors from different rules must not manufacture a broader permission.
   Public hostnames and routing hints are not proof of ownership or permission.
3. **Select the authorized profile.** Successful authorization overwrites internal
   tenant, protocol, and sandbox UID headers. With `extAuth.recomputeRoute: true`,
   Envoy reselects the tenant/protocol route, which requires a nonempty sandbox
   UID. The bootstrap and final routes use the same complete authorization
   check; a failed or incomplete classification never forwards to ingress.
4. **Apply per-sandbox limits.** The selected route's tenant-owned profile uses
   the trusted `BatchSandbox.metadata.uid` as a distinct local rate-limit key.
   Requests across all permitted ports, paths, and callers share this sandbox/protocol
   bucket, not a tenant-wide bucket. Envoy applies per-request/stream timeouts;
   it obtains no rate tokens from another replica or an external service.
5. **Forward a trusted route.** Envoy selects a ready ingress endpoint through the
   internal Service backend reference and forwards approved traffic with the
   trusted tenant, protocol, and sandbox UID headers. Preserve the public target,
   method, and application path authorized in step 2; no later filter may change
   those fields or the trusted identity.
6. **Resolve and proxy.** OpenSandbox ingress uses the trusted tenant and sandbox
   UID to resolve the current routable IP, and the configured mode's shared parser
   to obtain the port and application path from the unchanged request. It proxies
   that request without repeating authentication or selector evaluation. A missing
   routing identity or unavailable endpoint fails closed; ingress never falls
   back to a caller-supplied sandbox identity or IP. It removes platform endpoint
   credentials and internal headers before forwarding to the application.

The Envoy-to-ingress hop crosses pods within the cluster. Expose ingress only
through an internal Service and restrict application traffic to trusted Envoy
pods using enforced network controls. A ClusterIP Service alone is not an access
control boundary. Envoy replaces client-supplied internal metadata, and ingress
trusts that metadata only on this gateway-only path. No signed authorization
payload or per-request policy-revision check is required at ingress. Network
controls also prevent clients from bypassing Envoy through direct ingress-pod or
sandbox-pod access.
Header-, host-, URI-, and server-proxy access paths for policy-protected endpoints
must enter the same enforcement path; none may fall back to an unguarded route.

WebSocket authorization and rate accounting occur at upgrade. Accepted streams
remain subject to configured stream timeouts; v1 exposes no active-session limit.
An HTTP permission does
not implicitly authorize an upgrade, and WebSocket messages are not counted as
new HTTP requests.

### Reconciliation, Activation, and Cleanup

Policy acceptance, runtime allocation, and traffic readiness are separate states.
The ingress policy controller reconciles them as follows:

1. **Resolve desired state.** Validate policy ownership and defaults, identify the
   standalone or pool policy source, and determine the required tenant and sandbox
   revisions for each live allocation.
2. **Prepare enforcement.** Generate or retain tenant/protocol routes and limit
   profiles, then publish corresponding allocation-aware authorization state.
   Allocation and selector changes do not generate per-sandbox native resources.
   Gate affected traffic so partially installed layers cannot grant access.
3. **Confirm activation.** Open exposure only after the required policy, local
   rate-limit configuration, and endpoint routing are ready.
   Serving Envoy proxies must have the required configuration, and the separate
   ingress Service must have ready endpoints. Per-sandbox readiness is checked
   separately. Status reports the revisions actually enforced, not merely the
   last successfully written Kubernetes objects.
4. **Reconcile changes.** Tenant updates affect existing consumers without
   modifying their sandbox/pool selectors. Standalone-sandbox updates replace that
   sandbox's selectors. Pool selectors remain immutable. Removed permissions
   must stop admitting traffic and terminate connections that are no longer
   authorized as part of the enforced update.
5. **Retire exposure.** Sandbox deletion or final allocation release removes
   authorization for that assignment, rejects old endpoint credentials, and closes
   affected connections before the runtime can serve a later assignment. Shared tenant
   routes and clusters remain installed. Old dynamic buckets may age out; their
   keys cannot authorize access. A new pool allocation creates a new BatchSandbox
   with a different UID, even if it reuses the same runtime pod, so it never
   reuses the previous sandbox's bucket key.

Runtime release during pause suspends access without retiring the sandbox UID
or losing its policy intent. A pooled sandbox's policy source transfers to its
own policy before detachment; see [Pause and Resume](#pause-and-resume).

A proxy that cannot install a required security update cannot keep serving the
affected routes indefinitely. Activation requires that every serving path either
enforces the required revisions or is gated out. Distribution acknowledgements,
freshness bounds, connection revocation, and failure recovery must be specified
before this design is considered implementable.

### Failure Behavior and Observability

Missing tenant policy, missing sandbox/pool policy, stale sandbox or runtime binding,
unready required local rate-limit configuration, or unavailable required
authorization state denies the affected application traffic. An explicitly
installed deny-all policy is valid enforcement state, unlike missing or
indeterminate configuration. Failures must not trigger fallback to direct pod
access or a less restrictive policy.

An unavailable Envoy replica stops serving traffic. Ingress replicas have their
own readiness and draining lifecycle; new traffic uses ready ingress endpoints.
If none are available, affected requests fail closed rather than bypassing ingress
or forwarding directly to a sandbox. Gateway and ingress rollouts drain their
own HTTP requests and WebSocket connections. A failed ingress replica can break
existing connections; reconnecting clients must pass authorization and local
limit checks again, rather than assuming live connections migrate to another pod.

Expose pending, failed, and active rollout status with the relevant policy
revisions and effective per-sandbox limits, including defaulted values and the
replica-local rate-limit scope. Correlate authorization decisions, limit
rejections, endpoint-resolution failures, and connection termination with tenant, sandbox,
and gateway replica identity in logs and traces so local rate-limit
decisions can be attributed to the serving replica.
Keep credentials and sensitive request contents out of these records; use bounded
metric dimensions rather than tenant or sandbox identifiers as metric labels.
Observability-export failures must not disable policy enforcement.

## Detailed Design

### TenantIngressPolicy

`TenantIngressPolicy` stores the tenant's live ingress guardrail and the
administrator-owned limits applied independently to each sandbox. It is a
namespaced resource under `sandbox.opensandbox.io/v1alpha1`. Each tenant maps to
one namespace, resolved by the lifecycle server from the management API key;
there is no caller-writable `spec.tenantId` or cross-namespace policy selection.

Admission permits only one object named `default` per tenant namespace. The
singleton has no sandbox owner reference and is not copied into sandbox or pool
policies. A missing or deleting singleton denies tenant application ingress;
it does not leave sandbox/pool rules as independent grants of access. Creating
the singleton alone does not expose any endpoint.

```yaml
apiVersion: sandbox.opensandbox.io/v1alpha1
kind: TenantIngressPolicy
metadata:
  name: default
  namespace: tenant-a
spec:
  ingress:
    - ports:
        - from: 8080
          to: 9080
      protocols: [http]
      methods: [GET, POST]
      paths: [/api/*]
    - ports:
        - from: 8080
          to: 8080
      protocols: [websocket]
      methods: [GET]
      paths: [/ws/*]
  limits:
    http:
      rateLimit:
        requests: 10
        unit: Second
      timeout:
        requestTimeout: 60s
    websocket:
      rateLimit:
        requests: 10
        unit: Minute
      timeout:
        requestTimeout: 0s
        maxStreamDuration: 300s
        streamIdleTimeout: 60s
```

The lifecycle server maps the tenant API's `ingress` and `limits` fields to the
corresponding spec fields and returns the stored policy with controller-reported
enforcement status. Only
administrator-authorized writes may replace this spec; Kubernetes RBAC and
admission enforce the same boundary for direct Kubernetes access.

- **Request rules:** `spec.ingress` contains allow rules over application ports,
  protocols, methods, and paths. Unmatched requests are denied; an omitted or
  empty list denies all application ingress. A request must match a complete
  tenant rule and a complete rule in the applicable sandbox/pool policy. Matching
  unrelated selectors from different rules must not create a new permission.
- **Selector validation:** port ranges are inclusive, with
  `1 <= from <= to <= 65535`. V1 supports `http` and `websocket`, with exact paths
  and the terminal `/*` path patterns shown above. Methods and paths are evaluated
  against the same normalized application request used by sandbox/pool checks.
  WebSocket upgrades require explicit WebSocket permission; ordinary HTTP access
  does not grant it. Reject empty selector sets and unsupported match syntax
  rather than interpreting them as unrestricted access.
- **Limit ownership and scope:** `spec.limits.http` and `spec.limits.websocket`
  are the tenant's only limit profiles. Each is applied independently to every
  live sandbox allocation, across all its permitted ports, methods, and paths.
  Limits are not accepted inside individual rules or in sandbox/pool policy.
  HTTP and WebSocket accounting remain separate; the API does not promise one
  combined quota across protocols or Envoy replicas.
- **Native rate-limit shape:** a supplied `rateLimit` object defaults `requests`
  to `10` and `unit` to `Second`. Both `{}` and partial objects are valid; requests
  must be in `1..4294967295`, and units use the native enumeration. Omitting the
  whole object emits no tenant rate-limit configuration. Capacity and refill
  count are coupled, so the API does not expose independent burst parameters.
- **Timeout shape:** expose only the `timeout` fields described below. Validate
  native duration formats before publishing configuration. For WebSocket, reject
  nonzero `requestTimeout`; default
  that timeout to `0s` and use stream-duration/idle controls instead.
- **Unambiguous enforcement:** overlapping allow rules are evaluated as complete
  alternatives and use the same protocol profile; they cannot select different
  limits by rule order. Normalize duplicate selectors without creating extra
  capacity. Reject unsupported fields instead of passing arbitrary Envoy Gateway
  policy settings through the tenant API. Defaulting and policy reads use the
  same effective values.
- **Live updates:** replace the validated spec using Kubernetes optimistic
  concurrency. A new generation applies to existing and future single-replica
  sandboxes, including pool allocations. It does not rewrite
  `SandboxIngressPolicy` or immutable `PoolIngressPolicy` objects. Restoring older
  rule content is another policy update, not reactivation of an old revision.

#### Gateway API and Envoy Gateway Mapping

The ingress policy controller generates Gateway API and Envoy Gateway policy
resources; Envoy Gateway translates them into xDS. The API examples use Envoy
Gateway **v1.9.1** as a translation reference, not as an approved deployment build.

```text
Platform configuration
    -> one admin-selected ingress mode and public hostname
    -> shared Gateway, early header sanitization, shared authorization
    -> upgrade-capable bootstrap HTTPRoute with deny-only HTTP backend

TenantIngressPolicy
    -> at most one HTTPRoute + BackendTrafficPolicy per tenant/protocol

Tenant + standalone OR pool selectors + current allocation
    -> versioned authorization snapshot and readiness

Request -> bootstrap route -> shared authorization
    -> trusted tenant/protocol/sandbox-UID headers -> route recomputation
    -> tenant/protocol route -> sandbox-UID-keyed local rate limit
    -> internal ingress Service -> authorized sandbox endpoint
```

**Resource ownership and scaling.** For each shared Gateway deployment, the
platform owns one `ClientTrafficPolicy` for early header removal, one fail-closed
Gateway-scoped `SecurityPolicy`, and one bootstrap `HTTPRoute` paired with a
`BackendTrafficPolicy` enabling WebSocket upgrades. The bootstrap forwards only
to the authorization service's deny-only HTTP listener if route recomputation
does not select a tenant route. No additional Deployment is needed for this
listener. The controller maintains at most two
`HTTPRoute`/`BackendTrafficPolicy` pairs per tenant: HTTP and WebSocket. Each
protocol route has one rule and one match. Port, path, method, sandbox, and pool
selectors live in authorization state rather than expanding into native routes.

Generated resources live in the platform namespace, such as
`opensandbox-system`, alongside the Gateway and backend Services. Tenants cannot
edit them or attach overriding native policies. Source tenant UID and policy
revision are tracked by controller metadata and status, not invalid
cross-namespace owner references. Cleanup is explicit. The platform controls
listener route attachment and prevents other application routes from bypassing
shared authorization.

Route, policy, and upstream-cluster counts grow with tenants, not allocations.
Creating or deleting a sandbox changes authorization state without creating or
deleting native routes. This removes per-sandbox Kubernetes/xDS resource churn,
but does not make allocation state free: authorization snapshots and active local
rate buckets still grow with allocations, and each serving Envoy replica keeps
its own buckets.

**One admin-selected ingress mode.** Reuse `ingress.gateway.route.mode` as
platform configuration. It is not part of `TenantIngressPolicy`, and one gateway
deployment serves only its configured mode. Bootstrap and tenant routes share
the same mode-specific hostname:

| Mode | Incoming target | Native route hostname | Authorization and ingress handling |
| --- | --- | --- | --- |
| `wildcard` | `https://<sandbox-id>-<port>.<domain>/path` | `*.<domain>` | Resolve sandbox and port from the original authority; authorize `/path`. |
| `header` | `https://<domain>/path` with `OpenSandbox-Ingress-To: <sandbox-id>-<port>` | `<domain>` | Resolve the public target header; authorize `/path`. |
| `uri` | `https://<domain>/<sandbox-id>/<port>/path` | `<domain>` | Resolve the URI prefix; authorize the application path `/path`. Preserve the original URI for ingress to strip once. |

The authorizer and ingress must use consistent parsing and normalization. Reject
malformed or conflicting target selectors rather than falling back to another
mode. Hostnames and public routing hints identify candidates, not permissions.
The authorizer makes the access decision and supplies the trusted sandbox UID.
Ingress uses that UID for endpoint lookup and the same parser for the requested
port and application path, not to evaluate permission again. Preserve the
authorized request between the two hops; in URI mode, ingress strips the routing
prefix once to produce exactly the application path evaluated by authorization.
No per-sandbox hostname, allocation-specific `HTTPRoute`, or multi-mode
normalization service is generated. The existing ingress binary uses
`--mode header` for the platform's wildcard/header modes and `--mode uri` for URI
mode. Changing mode requires a coordinated platform rollout, not a tenant-policy
update.

**Trusted classification and route recomputation.** Reserve these internal
headers; clients cannot provide authoritative values:

| Header | Value supplied by successful authorization |
| --- | --- |
| `x-osb-tenant` | Trusted tenant namespace UID, not a caller-supplied tenant name. |
| `x-osb-protocol` | `http` or `websocket`, determined from the validated operation. |
| `x-osb-sandbox-uid` | Resolved `BatchSandbox.metadata.uid` for either a directly created or pool-allocated sandbox; stable for the object's lifetime. |

`ClientTrafficPolicy.headers.earlyRequestHeaders` removes these headers before
initial route selection. An ordinary route-level request-header filter is not
sufficient for this boundary. The request therefore starts on the bootstrap route.
The shared gRPC authorizer independently authenticates the caller, resolves the
current allocation, evaluates both complete selector layers, and verifies the
required profile and allocation readiness. It overwrites the reserved headers
only on success; the initial route's metadata is not evidence of tenant identity.

`SecurityPolicy.spec.extAuth.recomputeRoute: true` allows the returned headers to
select the tenant/protocol route. Each final route requires exact tenant and
protocol values and a nonempty sandbox UID. Unknown, missing, or unready
classification is denied by authorization; if recomputation does not select a
final route, the bootstrap forwards only to the deny-only HTTP listener, which
returns `403`. An absent rate-limit key must never fall through to unlimited
ingress access.

Use a forwarding bootstrap route with
`BackendTrafficPolicy.spec.httpUpgrade: [{type: websocket}]`, not an
`HTTPRouteFilter` direct response. Envoy checks upgrade support before executing
external authorization; the initial route must therefore permit upgrade
processing so the authorizer can select the final WebSocket route. This does not
grant WebSocket access: the same complete authorization check remains required,
and the deny-only listener never returns `101` or forwards to a sandbox. An
unavailable authorizer denies the request. If the bootstrap fallback is selected
but its listener is unavailable, the request fails closed rather than falling
back to ingress.

Route recomputation is safe only with the same complete, fail-closed
Gateway-scoped authorization policy on the bootstrap and every final route.
Do not rely on re-running authorization after route selection. Forbid per-route
authorization bypasses or overrides, and filters that change the selected route
or policy-relevant request fields after authorization. The single authorized
reclassification changes internal routing headers, not the public host, target,
application path, or operation. Ingress trusts the gateway-supplied identity for
endpoint lookup rather than authorizing the request again, and removes credentials
and all reserved headers before proxying to the application.

**Native field mapping.**

| Policy input | Generated configuration | Scope and constraints |
| --- | --- | --- |
| Platform bootstrap configuration | `HTTPRoute.spec.rules[].backendRefs` to the deny-only listener and `BackendTrafficPolicy.spec.httpUpgrade` with `type: websocket` | Shared across tenants; enables upgrade processing before authorization without granting access. |
| Tenant and sandbox/pool `ingress` selectors | Authorization snapshot, invoked by the shared `SecurityPolicy.spec.extAuth` | Evaluate both selector layers; deny missing policy, undeclared ports, wrong protocol, stale allocations, or unready revisions. |
| Authorized tenant and protocol | Exact internal-header matches in `HTTPRoute.spec.rules[].matches[]` | One shared route per tenant/protocol; no per-allocation route or per-selector matches. |
| Authorized sandbox UID | Required `x-osb-sandbox-uid` header and a local `clientSelectors[].headers[]` entry with `type: Distinct` | Separate dynamic bucket for each `BatchSandbox.metadata.uid` within the shared tenant/protocol route. |
| `limits.<protocol>.rateLimit` | `BackendTrafficPolicy.spec.rateLimit.type: Local` and one `local.rules[].limit` | Copy defaulted `requests` and `unit`; all ports, paths, and clients share the sandbox bucket. Omission emits no tenant rate cap. |
| `limits.http.timeout.requestTimeout` | `BackendTrafficPolicy.spec.timeout.http.requestTimeout` | Per-request upstream response-completion timeout, not a client-upload deadline. |
| `limits.<protocol>.timeout.maxStreamDuration` / `streamIdleTimeout` | Same fields under `BackendTrafficPolicy.spec.timeout.http` | Bound each stream's lifetime and inactivity; omission keeps native/infrastructure defaults, and `0s` disables that timeout. |
| WebSocket response-completion timeout | `BackendTrafficPolicy.spec.timeout.http.requestTimeout: 0s` | Always disable this timeout for upgraded streams; use stream timeouts instead. |

Profiles accept only this supported subset, not arbitrary `BackendTrafficPolicy`
properties. The public `timeout` object stays flat; the native `timeout.http`
wrapper is a translation detail for both HTTP and WebSocket. Shared timeout
configuration applies independently to each request or stream, not to aggregate
tenant traffic. V1 offers no different limit profiles by port or path.

**Per-sandbox local rate buckets.** The distinct sandbox UID descriptor selects
one bucket within a tenant/protocol route. There is no additional tenant-wide
bucket to consume. HTTP and WebSocket use separate routes and buckets, while all
ports, paths, and callers for one sandbox/protocol share its bucket. The key is
the trusted `BatchSandbox.metadata.uid`, never a client-supplied value, reusable
sandbox name, or pod IP. Each pool allocation creates its own BatchSandbox;
reusing a pool pod for another sandbox therefore uses a different UID and bucket.

A BatchSandbox represents one logical sandbox throughout its lifetime.
Selector-only edits and runtime replacement retain its UID and native limit
profile; they must not deliberately reset its rate allowance. Temporarily
removing and restoring access also keeps the same key.

Current runtime binding is separate from the bucket key. The authorizer checks
allocation readiness; ingress only looks up the current routable endpoint for
the trusted sandbox UID and requested port. Paused, deleted, unready, or released
runtimes have no routable entry. A binding change refreshes authorization
readiness and endpoint routing without generating a new sandbox identity.
Withdrawing stale endpoints and closing streams are lifecycle responsibilities,
not a second policy evaluation in ingress.

Ending a sandbox's logical allocation revokes its authorization, credentials,
and connections without rewriting shared routes. Its old bucket may remain cached
until eviction, but the old UID alone cannot authorize further requests. Tenant limit changes or
proxy restarts may recreate local buckets; v1 does not promise token preservation
across those events.

**Dynamic descriptor capacity and validation.** Each Envoy process keeps a
bounded LRU cache of dynamic sandbox-UID buckets for each wildcard descriptor in
a tenant/protocol route. Exceeding that capacity can evict a bucket; a later
request can recreate it full and restore allowance before the original refill.
This limitation is separate from replica-local rate accounting, and sizing must
consider allocation churn as well as active sandboxes.

The unpatched v1.9.1 translation reference sets `maxDynamicDescriptors` on the
listener-level filter but omits it from the route-level configuration, leaving
the route at Envoy's default capacity of **20**. The fix was merged in Envoy
Gateway [#9974](https://github.com/envoyproxy/gateway/pull/9974) on September 16,
2026; it also sets the intended capacity of **10,000** on each route-level filter
configuration. This remains a finite cache, not unlimited per-sandbox accounting.

A deployment build must include the fix, and the fix still needs validation:
inspect emitted route-level `maxDynamicDescriptors`, then exercise exhausted
buckets across more than 20 sandbox UIDs and under expected allocation churn and
configured-capacity pressure. Confirm refill and eviction behavior before
claiming the supported scale. The reference translation checks alone do not
validate the fix or its runtime behavior; no tenant-facing rate-limit API change
is required.

**Concrete generated resources.** The following examples map the
`TenantIngressPolicy/default` in namespace `tenant-a` shown above to Gateway API
and Envoy Gateway resources. The tenant namespace UID is `tenant-uid-a`; the
admin-selected mode is `header`, with public hostname `sandbox.example.test`.
The tenant's selectors remain in authorization state, while its HTTP and
WebSocket limit profiles produce the two route/policy pairs below.

| Resource scope | Gateway API resources | Envoy Gateway policies and filters |
| --- | --- | --- |
| Shared platform configuration, installed once | `HTTPRoute/ingress-bootstrap` | `ClientTrafficPolicy/strip-client-context`, `SecurityPolicy/shared-authorization`, `BackendTrafficPolicy/ingress-bootstrap-upgrades` |
| Generated for this tenant's HTTP profile | `HTTPRoute/tenant-a-http` | `BackendTrafficPolicy/tenant-a-http-limits` |
| Generated for this tenant's WebSocket profile | `HTTPRoute/tenant-a-websocket` | `BackendTrafficPolicy/tenant-a-websocket-limits` |

The platform already owns `shared-gateway` and its `public` listener, TLS,
`opensandbox-ingress:28888`, protected transport, and network isolation. The
`ingress-authorization` Service exposes gRPC authorization on port `9002` and
the deny-only HTTP listener on port `9003`, both served by the authorization
Deployment. The HTTP listener is a proposed addition and always returns `403`
for every method and path, including upgrade attempts. All example resources are
in `opensandbox-system`; source revision metadata is omitted for brevity. These
are controller-managed Gateway API/Envoy Gateway resources, not raw xDS or a
standalone deployment of the platform Services.

**Shared authorization and bootstrap.** This configuration is shared by all
tenants on the Gateway; it is not generated separately for each tenant policy.

```yaml
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: ClientTrafficPolicy
metadata:
  name: strip-client-context
  namespace: opensandbox-system
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: shared-gateway
  headers:
    earlyRequestHeaders:
      remove:
        - x-osb-tenant
        - x-osb-protocol
        - x-osb-sandbox-uid
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: SecurityPolicy
metadata:
  name: shared-authorization
  namespace: opensandbox-system
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: shared-gateway
  extAuth:
    failOpen: false
    recomputeRoute: true
    grpc:
      backendRefs:
        - name: ingress-authorization
          port: 9002
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: ingress-bootstrap
  namespace: opensandbox-system
spec:
  parentRefs:
    - name: shared-gateway
      sectionName: public
  hostnames: [sandbox.example.test]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: ingress-authorization
          port: 9003
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: ingress-bootstrap-upgrades
  namespace: opensandbox-system
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: ingress-bootstrap
  httpUpgrade:
    - type: websocket
```

End-to-end validation must confirm successful authorized HTTP requests and
WebSocket handshakes in all three ingress modes. Requests denied by authorization
or lacking classification or a final route must never reach ingress. Verify
fail-closed behavior when authorization is unavailable or when a selected
fallback backend is unavailable. Successful native translation does not prove
these runtime behaviors.

**Generated HTTP route and policy.** This pair applies `limits.http` from the
tenant policy: ten requests per second per sandbox and a 60-second request
timeout. It serves every authorized sandbox in `tenant-uid-a`. The header matches
make it more specific than the bootstrap route after successful classification.
They do not replace the complete authorization check.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tenant-a-http
  namespace: opensandbox-system
spec:
  parentRefs:
    - name: shared-gateway
      sectionName: public
  hostnames: [sandbox.example.test]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
          headers:
            - name: x-osb-tenant
              type: Exact
              value: tenant-uid-a
            - name: x-osb-protocol
              type: Exact
              value: http
            - name: x-osb-sandbox-uid
              type: RegularExpression
              value: '.+'
      backendRefs:
        - name: opensandbox-ingress
          port: 28888
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: tenant-a-http-limits
  namespace: opensandbox-system
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: tenant-a-http
  rateLimit:
    type: Local
    local:
      rules:
        - clientSelectors:
            - headers:
                - name: x-osb-sandbox-uid
                  type: Distinct
          limit:
            requests: 10
            unit: Second
  timeout:
    http:
      requestTimeout: 60s
```

The resulting descriptor bucket has capacity ten and replenishes ten tokens per
second, independently for each sandbox UID on each serving Envoy process. The
trusted sandbox UID header is mandatory even when a profile omits `rateLimit`,
because it also identifies the authorized sandbox for ingress endpoint lookup.
The stable UID does not make a paused or released endpoint routable.

**Generated WebSocket route and policy.** This pair applies `limits.websocket`
from the same tenant policy: ten upgrade attempts per minute per sandbox,
no response-completion timeout, a 300-second stream lifetime, and a 60-second
stream idle timeout.

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: tenant-a-websocket
  namespace: opensandbox-system
spec:
  parentRefs:
    - name: shared-gateway
      sectionName: public
  hostnames: [sandbox.example.test]
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
          headers:
            - name: x-osb-tenant
              type: Exact
              value: tenant-uid-a
            - name: x-osb-protocol
              type: Exact
              value: websocket
            - name: x-osb-sandbox-uid
              type: RegularExpression
              value: '.+'
      backendRefs:
        - name: opensandbox-ingress
          port: 28888
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: BackendTrafficPolicy
metadata:
  name: tenant-a-websocket-limits
  namespace: opensandbox-system
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      name: tenant-a-websocket
  rateLimit:
    type: Local
    local:
      rules:
        - clientSelectors:
            - headers:
                - name: x-osb-sandbox-uid
                  type: Distinct
          limit:
            requests: 10
            unit: Minute
  timeout:
    http:
      requestTimeout: 0s
      maxStreamDuration: 300s
      streamIdleTimeout: 60s
```

Both tenant routes inherit the shared Gateway authorization policy. No
per-sandbox route or policy is generated: for example, authorized requests with
`x-osb-sandbox-uid: sandbox-uid-a1` and `x-osb-sandbox-uid: sandbox-uid-a2` select
the same tenant/protocol route but consume different local buckets. These values
are written by the authorizer after resolving actual BatchSandbox UIDs, never
accepted from the client.

Only a genuine HTTP/1.1 WebSocket upgrade may receive the `websocket`
classification; a client-supplied `Upgrade` header alone is insufficient. The
authorizer rejects an unauthorized upgrade rather than falling back to the HTTP
profile.

### Pause and Resume

Pause and resume operate on the same single-replica `BatchSandbox`; its
`metadata.uid` and `x-osb-sandbox-uid` rate-limit key remain unchanged.
A pooled sandbox receives an equivalent batch-owned `SandboxIngressPolicy`
before detachment; resume uses that standalone policy and the current tenant
guardrail. Runtime binding and ingress readiness are re-established. The
following coordination is a required ingress-policy integration with the existing
pause/resume lifecycle, not behavior supplied by Gateway API policies alone.
Only application ingress is gated; authenticated lifecycle and policy management
operations, including resume, remain available.

| Lifecycle state | Application ingress behavior |
| --- | --- |
| Running with active ingress enforcement | Admit requests permitted by the current policy and local limits. |
| Pausing | Deny new requests and upgrades; close existing sandbox HTTP/WebSocket streams before releasing the runtime. |
| Paused | Deny application traffic; retain policy intent and shared tenant route/policy resources. |
| Resuming, or runtime ready but ingress not ready | Deny traffic until the new runtime binding and required policy revisions are active. |

#### Pause Flow

1. **Gate new traffic.** When the controller reconciles accepted pause intent,
   publish a non-admitting state for that sandbox UID to the authorizer and mark
   its ingress endpoint non-routable. This is endpoint lifecycle management,
   not a policy check in ingress. Observe lifecycle intent as well as runtime
   readiness: an old Pod may remain ready while a snapshot is being created.
   A pause request being accepted does not by itself prove the gate is installed.
2. **Close streams and withdraw the old endpoint.** Confirm the gate on every
   serving path and close active HTTP and WebSocket streams for this sandbox,
   without closing unrelated streams on multiplexed client connections.
   Invalidate its old resolved endpoint, including cached lookup results. The
   lifecycle controller must not release or recycle the runtime, or report pause
   complete, until this ingress shutdown is acknowledged. Changing an
   authorization snapshot does not by itself close an already-upgraded stream.
3. **Preserve policy before detachment.** Keep a standalone sandbox's policy;
   for a pooled sandbox, persist its standalone policy as described below before
   the runtime controller clears `spec.poolRef`. Do not delete or rewrite the
   shared `SecurityPolicy`
   or tenant `HTTPRoute`/`BackendTrafficPolicy` resources merely to pause one
   sandbox; other sandboxes sharing those resources remain available. Ordinary
   policy reconciliation continues while the sandbox is paused.

The paused-state gate denies otherwise-authorized requests before local rate
limiting. It does not queue or replay requests, and this design adds no
request-triggered resume behavior. Clients must establish new connections after
resume; neither HTTP requests nor WebSocket sessions migrate to the new runtime.

#### Resume Flow

1. **Restore without admitting traffic.** Let the existing lifecycle controller
   restore the runtime while the sandbox's ingress gate remains closed. Do not
   reopen access merely because resume was requested or a replacement Pod exists.
2. **Resolve current policy and endpoint state.** Load the latest
   `TenantIngressPolicy` and the batch-owned `SandboxIngressPolicy`, including
   the policy created during pooled pause handoff. Resume no longer depends on
   the original pool or its policy.
   Resolve the restored runtime and declared endpoints, and replace the old
   runtime binding in authorization state and ingress endpoint lookup state.
   Ingress receives endpoint lifecycle state, not policy snapshots or revisions.
   Do not restore access decisions or policy revisions from the filesystem
   snapshot. Missing policy, an empty selector intersection, or an unready
   endpoint keeps access denied.
3. **Activate and reopen.** Apply the normal enforcement-activation checks:
   required policy revisions must be active in authorization and native limit
   profiles must be active in Envoy on every serving path, or that path must be
   gated out. Ingress must resolve the restored endpoint within the same sandbox
   UID. The controller rechecks current lifecycle intent before reopening. A stale
   resume completion must not reopen a sandbox that was paused again, deleted,
   or expired. Report ingress readiness separately from runtime readiness;
   newly authorized requests use the current endpoint lookup.

#### Pool-to-Standalone Policy Handoff

The ingress policy controller creates a `SandboxIngressPolicy` for the pausing
batch before the runtime controller removes `spec.poolRef`. Application ingress
remains gated throughout the transition:

1. **Copy the sandbox restrictions.** Resolve the verified source
   `PoolIngressPolicy` in the batch's tenant and copy its ingress selectors,
   preserving ports, protocols, methods, paths, and empty-list deny behavior.
   Copy the pool's policy intent, not its current intersection with tenant rules.
   Do not copy tenant selectors, tenant-owned limits, credentials, runtime
   bindings, or enforcement readiness into the new policy.
2. **Persist before detachment.** Create `SandboxIngressPolicy` with the batch's
   name and namespace and an owner reference to its `BatchSandbox` UID.
   Admission permits creation for a still-pooled owner only by the authorized
   handoff controller during gated pause, with `spec.replicas: 1` and the copied
   selectors verified against the source policy. Reuse an equivalent policy
   owned by the same batch on retries; do not overwrite a conflicting object or
   substitute deny-all because the source cannot be read. User writes to the
   staged policy remain prohibited while the batch is pool-backed.
3. **Complete runtime detachment.** Only after the new policy is durable may the
   runtime controller save the template, clear `spec.poolRef`, and release the
   pooled runtime. Detachment must wait for every enabled policy handoff,
   including egress where applicable; the ingress controller must not
   independently clear `spec.poolRef` while another handoff still needs it.
4. **Switch to the standalone source.** Before detachment, the pool policy remains
   authoritative; afterward, use only the batch-owned `SandboxIngressPolicy`.
   Keep it unready while paused. Resume activates its own UID/generation with
   the latest tenant guardrail, not the old pool-policy revision. Withdraw the
   batch's old pool-policy consumer reference; the original pool and policy may
   be cleaned up once their remaining consumers are gone. A retry after
   detachment uses the existing standalone policy without rereading the pool.

If creation or detachment fails, keep ingress blocked and retry without reporting
pause complete. Do not merge both policy sources or choose a fallback at request
time. The handoff changes authorization state and policy ownership, not shared
Gateway API routes or native limit profiles. Once standalone, the sandbox's
selectors support the normal live-replacement operation, including while paused;
the source pool's rules remain immutable. This ordered handoff is a required
controller integration, not existing behavior supplied by clearing `spec.poolRef`.

#### Policy and Rate-Limit Continuity

Tenant policy changes made while a sandbox is paused apply before ingress
reactivation. Pause/resume keeps the same sandbox UID and must not deliberately
recreate its local rate bucket or change its native profile to reset the
allowance. Retained buckets refill normally during the pause, so a long pause
may leave a full bucket. Tokens are not included in the sandbox snapshot, and
v1 does not promise token preservation across proxy restarts, cache eviction,
or policy reconfiguration.

#### Failure Handling and Validation

Failure to confirm gating, stream closure, current runtime binding, or policy
activation keeps affected ingress blocked and exposes an actionable status.
If a pause fails and the lifecycle controller confirms the sandbox is still
running, access may reopen only after the normal activation checks; a surviving
Pod or old endpoint credential is not sufficient. A failed resume stays gated.
Retries reconcile the latest intent and must not recreate the sandbox identity
or weaken the current guardrail.
