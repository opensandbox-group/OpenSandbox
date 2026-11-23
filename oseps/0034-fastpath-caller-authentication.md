---
title: FastPath Caller Authentication
description: Unified Kubernetes-backed authentication for in-cluster and external OpenSandbox servers and the in-cluster ingress gateway.
authors:
  - "@Pangjiping"
creation-date: 2026-11-20
last-updated: 2026-11-23
status: implementing
---

# OSEP-0034: FastPath Caller Authentication

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
  - [Deployment and identity model](#deployment-and-identity-model)
  - [Token acquisition and rotation](#token-acquisition-and-rotation)
  - [FastPath authentication](#fastpath-authentication)
  - [FastPath authorization](#fastpath-authorization)
  - [Transport security](#transport-security)
  - [Configuration and packaging](#configuration-and-packaging)
  - [Errors and observability](#errors-and-observability)
  - [Implementation ownership](#implementation-ownership)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

Authenticate every FastPath business RPC by default using a short-lived Kubernetes ServiceAccount token with a dedicated FastPath audience, local JWT verification, and explicit caller authorization over verified TLS, while allowing operators to explicitly opt out (`enabled = false`) for smooth rolling upgrades or isolated non-TLS environments. An OpenSandbox server running inside or outside Kubernetes uses the same protocol; only its Kubernetes bootstrap credentials differ, while the ingress gateway always runs inside the cluster. Reuse the mechanism already implemented for FastPath-to-Fastlet authentication without reusing that hop's tokens, identities, or authorization policy, and require protected transport whenever bearer credentials are enabled.

## Motivation

The current [Python FastPath client](../server/opensandbox_server/services/fast_sandbox/fastpath_client.py) uses `grpc.insecure_channel` and supplies no caller credential. The [ingress gateway FastPath client](../components/ingress/pkg/sandbox/fastsandbox_provider.go) likewise uses plaintext gRPC with no authentication metadata. Network placement is therefore the principal boundary around lifecycle operations and `ResolveEndpoint`, which can issue data-plane route credentials.

The lower hop already has a Kubernetes-backed authentication mechanism: [FSEP-0003: Fastlet Control Authentication](https://github.com/opensandbox-group/fast-sandbox/blob/96e41031cb091d099ab6f15d4da5528835d065b0/fseps/0003-fastlet-control-auth.md) defines TokenRequest-issued ServiceAccount tokens, automatic renewal, issuer/JWKS discovery, local verification, and an exact subject allow list. Its implementation exists in the fast-sandbox version recorded by [the source pin](../manifests/third-party/fast-sandbox.commit). This proposal extends that identity model to the missing upper hop rather than introducing another signing key or identity provider.

The [server's Kubernetes client](../server/opensandbox_server/services/k8s/client.py) already loads either in-cluster credentials or a kubeconfig. An external server is assumed to have a working kubeconfig for the target cluster; making that assumption explicit removes the need for a separate external enrollment protocol.

### Goals

- Cover in-cluster server, external server with kubeconfig, and in-cluster ingress gateway with one FastPath authentication contract.
- Automatically acquire, cache, and refresh new FastPath credentials without manually distributing tokens or maintaining application signing keys.
- Authenticate callers before any business handler runs, and authorize their RPC and resource namespace scope separately.
- Keep the steady-state RPC path independent of Kubernetes API availability and avoid per-request TokenReview or SubjectAccessReview calls.
- Preserve the existing Fastlet control and data-plane route credential boundaries.

### Non-Goals

- Running ingress gateway outside Kubernetes, or supporting an external server that cannot access the target cluster's API server.
- Replacing end-user authentication, implementing user impersonation, or redesigning tenant authorization in the lifecycle server.
- Forwarding caller credentials to Fastlets, sandbox workloads, or external handlers.
- Changing FastPath protobuf messages, public OpenSandbox APIs, CRDs, or SDK authentication contracts.
- Building a new CA, certificate controller, service mesh, or kubeconfig renewal service.
- Immediate revocation of already-issued tokens following ServiceAccount deletion or removal of token-minting RBAC.

## Requirements

| ID | Requirement |
|---|---|
| R1 | When enabled (the default), every FastPath business RPC requires a valid token and an explicit authorization decision, including reads, route resolution, and the optional gRPC-Web surface. |
| R2 | Both server deployment modes produce the same FastPath identity and token format; FastPath does not branch on caller network location. |
| R3 | Server and gateway use distinct caller identities, distinct from FastPath/controller identities accepted by Fastlet. |
| R4 | Token minting is namespaced and restricted to named target ServiceAccounts; normal operation needs neither cluster-admin nor access to signing private keys. |
| R5 | Token expiry, rotation, process restart, and Kubernetes signing-key rotation require no manual token replacement. |
| R6 | When enabled, authentication or authorization failure never falls back to anonymous traffic; new RPCs are denied until mapped to a policy. An explicit opt-out switch disables caller authentication and TLS together as a deliberate operator choice for staged migration. |
| R7 | Warm token acquisition and verification perform no network I/O; cache misses and refresh work are bounded and deduplicated. |
| R8 | Bearer credentials travel only over authenticated, confidential transport, with no certificate-verification bypass. |
| R9 | Removing FastPath access must be possible through its caller policy without waiting for Kubernetes token expiry. |

## Proposal

Clients obtain a token from the target cluster's TokenRequest API for a designated FastPath caller ServiceAccount, request the audience `fast-sandbox-fastpath`, and attach the cached token as gRPC `authorization: Bearer <token>` metadata. FastPath validates it against the trusted cluster issuer and authorizes the authenticated subject against a deployment-owned policy. Kubernetes manages the signing keys; client code manages token refresh.

This is the same authentication mechanism as the Fastlet hop, with different credentials and protocol adapters:

| Property | Server/gateway to FastPath | FastPath/controller to Fastlet |
|---|---|---|
| Issuer | Target cluster's ServiceAccount issuer | Target cluster's ServiceAccount issuer |
| Audience | `fast-sandbox-fastpath` | Existing `fast-sandbox-fastlet` audience |
| Subjects | Dedicated server and gateway FastPath caller SAs | Existing FastPath/controller SAs |
| Carrier | gRPC metadata; equivalent header for gRPC-Web | Existing HTTP authorization header |
| Receiver policy | Explicit RPC profile and resource namespaces | Existing Fastlet control caller allow list |
| Renewal | TokenRequest, cached and refreshed automatically | Existing TokenRequest source |

A valid token for one hop is not a credential for the other. FastPath calls Fastlet using its own existing token source, never by forwarding the incoming server or gateway token. Data-plane route credentials returned by `ResolveEndpoint` remain unchanged.

### Notes/Constraints/Caveats

“Zero maintenance” means no new operator-managed authentication token or signing-key lifecycle. It does not mean that kubeconfig credentials never expire, TLS certificates appear automatically, or network connectivity needs no configuration. In-cluster bootstrap credentials rotate through Kubernetes; an external server needs a kubeconfig credential provider usable without interactive login for unattended operation. A static or interactive kubeconfig remains usable only while its credentials remain valid.

Issuer discovery is a FastPath-side responsibility. The external server needs the Kubernetes API endpoint and FastPath endpoint, not direct access to an issuer URL that may only resolve inside the cluster. FastPath must have a trusted, reachable discovery/JWKS source; an unreachable configured issuer is an observable deployment failure, not a reason to skip verification.

Local JWT verification validates cryptographic identity and time bounds, not the current existence of a ServiceAccount or a bound Pod. Already-issued tokens may remain acceptable until expiry unless FastPath policy removes their authority. This limitation is explicit rather than claiming the revocation semantics of a fresh TokenReview.

### Risks and Mitigations

| Risk | Mitigation or accepted boundary |
|---|---|
| A stolen bearer token is replayed | Require protected transport, keep tokens in memory, never log them, and bound their lifetime and privileges. Tokens are not proof-of-possession credentials. |
| A token minter requests a different audience | Use authentication-only target SAs with no workload-resource RBAC; standard RBAC does not constrain TokenRequest audiences. |
| A compromised bootstrap identity requests a longer-lived token | FastPath requires `iat` and enforces `0 < exp - iat <= 3600` seconds even if Kubernetes permits longer tokens. Removing minting RBAC prevents new issuance, not use of existing tokens; removing the shared server subject from FastPath policy revokes both server deployment modes. |
| Server identity is used against Fastlet | Separate exact subject lists as well as audiences; never accept the server/gateway caller SAs as Fastlet control callers. |
| A gateway compromise gains lifecycle-admin authority | Grant only route resolution and the read/resume operations required for wake-on-access, scoped to permitted namespaces. |
| API outage prevents refresh | Continue using an already cached, still-valid token; retry refresh with bounded backoff and fail closed when no valid token remains. Cold starts remain dependent on Kubernetes. |
| An attacker forces repeated JWKS fetches | Bound caches and token size, deduplicate refresh, and throttle unknown-key refresh attempts. |
| Authorization changes are hidden by cached verdicts | Cache identity verification, not the final authorization decision; evaluate the current policy for every call. |
| Name reuse restores a previously removed identity | Treat re-allowlisting the same SA subject within the old token lifetime as a security-relevant administrative action; deleting and recreating an SA is not an immediate revocation mechanism here. |

## Design Details

### Deployment and identity model

Separate the credential used to call the Kubernetes API from the identity asserted to FastPath. The API server authenticates the former and checks whether it may mint for the latter.

Proposed default FastPath caller ServiceAccounts are `opensandbox-server-fastpath-client` and `opensandbox-ingress-fastpath-client`, both in the platform system namespace. These are authentication-only identities, not replacements for the existing server or gateway Pod ServiceAccounts. They receive no workload-resource permissions and do not need legacy ServiceAccount token Secrets or their own token-minting grants.

| Deployment | Kubernetes bootstrap identity | TokenRequest target | FastPath authority |
|---|---|---|---|
| In-cluster server | Existing server Pod SA via in-cluster configuration | Server FastPath caller SA | Server RPC profile and configured resource namespace scope |
| External server | Identity selected by the existing kubeconfig | The same server FastPath caller SA | The same server profile and scope |
| In-cluster ingress gateway | Existing gateway Pod SA via in-cluster configuration | Gateway FastPath caller SA | Gateway RPC profile and configured resource namespace scope |

The target SA namespace is the platform identity namespace, not a sandbox namespace supplied by an end user. Never select a minting identity from a sandbox request, tenant header, or metadata field.

For each target SA, a Role in its namespace grants `create` on `serviceaccounts/token` with `resourceNames` containing only that target's name. Its RoleBinding lists the corresponding Pod SA; the server binding may additionally list explicitly configured subjects representing the external kubeconfig identity. Do not bind an entire authenticated-users or serviceaccounts group. An administrator configures the external subject once; the application neither derives RBAC subjects from an arbitrary kubeconfig context name nor grants itself permissions.

A dedicated target SA matters because Kubernetes RBAC restricts which SA a caller can mint for, but not the requested audience. Allowing a restricted external identity to mint for a powerful controller SA would also let it request an API-server-audience token carrying that SA's Kubernetes permissions. The authentication-only identities avoid adding that escalation path; existing permissions of the bootstrap identity are otherwise unchanged.

### Token acquisition and rotation

Use `authentication.k8s.io/v1` TokenRequest through the existing authenticated Kubernetes client, with one audience, `fast-sandbox-fastpath`, and a requested lifetime of 3,600 seconds. Do not send the kubeconfig credential or the Pod's default API-server token to FastPath.

Each process maintains an in-memory token source keyed by target cluster, target SA, and audience. Use the returned `status.expirationTimestamp` rather than assuming the requested lifetime was granted. Reject empty or already-expired responses. Start refresh when the remaining lifetime falls below twenty minutes, clamped to at most half the granted lifetime for short-lived tokens, and add bounded jitter so replicas do not refresh together.

Concurrent calls share one in-flight mint; refresh has a timeout and bounded retry backoff. A failed refresh may retain the previous token only while it is still valid with an expiry safety margin. Initial mint failure or exhaustion of the valid-token window fails the call locally without opening an anonymous RPC. Do not persist the token to a Secret, environment variable, configuration file, or log.

Attach credentials per RPC through Python and Go client hooks, including background renewals and gateway wake-on-access calls. Rotating a token must not require reconnecting the gRPC channel. Existing kubeconfig credential refresh and kubelet token-file rotation must remain effective rather than being copied into a permanent bootstrap bearer string.

### FastPath authentication

Install a common authentication layer before business-handler dispatch, covering every registered FastPath service and its gRPC-Web adapter. Any future streaming RPC must pass the same gate and cannot rely on an unprotected registration path. Existing dedicated health probes may remain unauthenticated if they expose only health; no business read RPC is exempt, and reflection is disabled on exposed production listeners unless separately protected.

Resolve the trusted issuer and verification CA from deployment-owned Kubernetes configuration, normally the FastPath Pod's projected identity and CA. The incoming JWT never chooses the issuer, discovery URL, JWKS URL, or CA. Fetch keys using FastPath's own renewable Kubernetes credential over verified HTTPS; give that identity discovery access on hardened clusters without introducing anonymous bindings. Do not forward a Kubernetes bearer token to arbitrary cross-origin discovery redirects or key URLs; any separate key endpoint must be explicitly trusted with its own credential-forwarding policy.

Require exactly one bearer credential and validate the JWT signature using the allowed ServiceAccount algorithms, initially RS256 and ES256 P-256, together with `iss`, `iat`, `exp`, `nbf` when present, and the exact FastPath audience contract. Require numeric `iat` and `exp` claims and enforce `0 < exp - iat <= 3600` seconds independently of the client's TokenRequest settings and the cluster's maximum token lifetime. Reject an `iat` more than thirty seconds ahead of the verifier's clock; this clock-skew allowance does not extend the maximum lifetime or permit acceptance at or after `exp`. Reject missing or invalid time claims, unsupported algorithms, malformed tokens, and tokens intended for the Kubernetes API or Fastlet. Tokens issued by these clients have a single audience; multi-audience tokens are not part of this contract. The verified `sub` must identify an exact ServiceAccount subject, not a namespace prefix or an arbitrary user claim.

Reuse the upstream verifier's bounded-cache and key-refresh approach where suitable: positive identity verdicts live no longer than one minute or token expiry, whichever comes first; negative verdicts live at most five seconds; unknown-key refresh is single-flight and throttled to at most one attempt per thirty seconds. Keep a bounded periodic JWKS refresh as well so removal of old keys is eventually observed, with a finite maximum key-cache age and invalidation of affected positive verdicts on key-set replacement. Warm valid keys may serve during a transient refresh outage only within that bound. Choose and document the key-cache freshness limits in the implementation; indefinite stale-key acceptance is not supported.

Authentication results attach the verified subject to the RPC context. They never confer privileges by themselves. A policy removal affects the next call after that policy revision is installed even if the token's identity verdict is still cached.

### FastPath authorization

FastPath owns a static deployment policy mapping exact SA subjects to an explicit RPC profile and allowed resource namespaces. This is service-to-service authorization, not Kubernetes RBAC evaluation for each sandbox operation and not end-user tenant authorization. The lifecycle server remains responsible for deciding which operations its end users may request.

| Profile | Allowed RPCs |
|---|---|
| Server | `CreateSandbox`, `CreateRootfsSandbox`, `GetSandbox`, `DeleteSandbox`, `PauseSandbox`, `ResumeSandbox`, `ListSandboxes`, `UpdateSandbox`, `GetSandboxDiagnostics`, `CreateSandboxSnapshot`, `GetSandboxSnapshot`, `DeleteSandboxSnapshot`, `ResolveEndpoint`, `InvokeHandlerAction`, `GetPool`, `ListPools` |
| Gateway | `ResolveEndpoint`, `GetSandbox`, `ResumeSandbox` |

These lists reflect the current clients, including the gateway's wake-on-access path. There is no wildcard method grant: adding a FastPath RPC requires an explicit policy mapping and a test. In particular, gateway does not receive create, delete, snapshot, update, or handler-invocation authority.

Bind each profile to the namespaces served by the deployment. A deployment intentionally serving all namespaces may declare an explicit all-namespaces scope; an absent or empty namespace list must not silently mean unrestricted access. The caller SA's namespace does not determine its resource scope.

Authorize the canonical target namespace before exposing resource state or performing side effects. Define method-specific extraction for direct namespace fields, Sandbox references, Snapshot references, and create requests; check cross-resource references such as pools or source snapshots against the same scope. Reject unauthorized namespaces rather than silently filtering list results or interpreting an empty list namespace as cluster-wide. Preserve existing UID and generation fencing after authorization. References that cannot be scoped safely must not bypass the gate.

Policy comes from trusted deployment configuration, not JWT custom claims, request metadata, Pool templates, or user-controlled labels. Initial delivery may install policy changes through a normal FastPath rollout; runtime policy watching is not required. The rollout completion bounds revocation latency, and identity-cache TTL adds no further authorization delay. Audit the subject, method, decision, and configured scope without recording credentials.

### Transport security

Token authentication and TLS answer different questions: the former establishes caller identity and authority, while the latter protects the credential and authenticates the endpoint receiving it. FSEP-0003 explicitly excludes transport encryption; reusing its token mechanism does not imply that the current Fastlet link already has TLS, and this proposal does not change that link.

For the new FastPath connection, use verified TLS for both deployment modes. Native FastPath TLS or a deployment-managed TLS proxy is acceptable; a non-loopback proxy-to-FastPath segment must also have authenticated confidential transport. The plain backend listener, if used behind a colocated proxy, must not remain independently reachable. Protect the optional gRPC-Web listener identically. NetworkPolicy remains defense in depth, not a substitute for transport protection.

Clients validate the server certificate chain and intended DNS identity; no `insecure_skip_tls_verify`, plaintext fallback, or trust-on-first-use is introduced. A Kubernetes API CA is not automatically a CA for arbitrary FastPath serving certificates. Likewise, a `ClusterIP` Service does not automatically receive a serving certificate.

Reuse an existing deployment-managed certificate issuer and trust-distribution mechanism where available. Automatic certificate renewal and reload, plus client trust refresh when needed, are requirements for claiming unattended transport operation. This OSEP does not introduce a built-in PKI or promise certificate automation on clusters that have none. The reference implementation uses native FastPath TLS and a cert-manager `Certificate` referencing an existing deployment-owned Issuer or ClusterIssuer. It does not install cert-manager or create a CA. Mount serving certificate files without `subPath` for handshake-time reload; distribute only the public CA bundle to clients through the deployment's trust-management process. The server replaces its channel when its CA file changes, and the gateway rereads its CA file on new TLS handshakes. Root rotations require overlapping trust bundles; immediate revocation of established TLS sessions requires reconnecting clients.

### Configuration and packaging

Configuration has three responsibilities; concrete flag and Helm value names should be finalized together with the upstream FastPath implementation rather than presenting non-existent settings as usable commands.

| Owner | Required configuration | Defaulting and lifecycle |
|---|---|---|
| Server | `fastpath_auth_enabled` (default `true`); existing Kubernetes connection; FastPath target SA namespace/name; TLS endpoint and trust | Enabled by default. Both deployment modes use the same target SA. Setting `fastpath_auth_enabled = false` switches the client back to plaintext gRPC without TokenRequest preflight or bearer metadata. |
| Gateway | `--fastpath-auth-enabled` (default `true`); FastPath target SA namespace/name; TLS endpoint and trust | Enabled by default; Helm derives identity and trust settings from the installation. Setting `--fastpath-auth-enabled=false` uses plaintext gRPC without TokenRequest. |
| FastPath | `--fastpath-auth-enabled` (default `true`); trusted issuer/key source; subject/profile/namespace policy; TLS serving configuration | Enabled by default (fail-closed if policy or TLS files are missing). Setting `--fastpath-auth-enabled=false` serves plaintext gRPC/gRPC-Web without caller authentication or cert-manager Certificate creation. |
| Base chart | `fastSandbox.callerAuth.enabled` (default `true`); two authentication-only SAs, narrowly scoped token-minter Roles/RoleBindings | Enabled by default; reuse installation namespace and configured Pod SA names; support explicit external Kubernetes subjects without granting cluster-admin. |

The audience and maximum accepted token lifetime of 3,600 seconds are protocol constants for this hop, not user-selected values copied from incoming requests or relaxed to match cluster TokenRequest limits. Refresh values have safe defaults and do not need separate operator tuning in the initial configuration surface. Existing Fastlet audience, token source, and accepted subjects remain unchanged.

When caller authentication is enabled (the default), empty caller policy, missing target identity, malformed TLS trust, or missing bootstrap credentials must produce directed startup/preflight errors. Readiness must distinguish “TCP channel connected” from “credentials and verification prerequisites ready.” The gateway checks TLS connectivity and token minting during startup. Because the server constructs the optional FastPath backend even in ordinary Kubernetes deployments, its first FastPath connection performs credential preflight instead of making FastPath a universal startup dependency; the server's general health endpoint does not certify FastPath readiness. Deployment validation includes an authenticated read-only RPC in the intended namespace. Temporary dependency failure never automatically downgrades an enabled endpoint to plaintext or anonymous access; opting out requires setting the explicit configuration flag on both FastPath and its callers.

### Errors and observability

| Condition | FastPath/client outcome |
|---|---|
| Missing, malformed, expired, wrong-issuer/audience, or invalid-signature token | gRPC `Unauthenticated`; business handler is not invoked |
| Valid identity without the required subject/profile/namespace grant | gRPC `PermissionDenied`; business handler is not invoked |
| Required verification material unavailable with no usable bounded cache | gRPC `Unavailable`; business handler is not invoked |
| Client cannot obtain a usable token | Local dependency failure identifying TokenRequest or bootstrap failure; no anonymous RPC |
| TLS trust, hostname, or transport failure | Connection failure; do not retry over plaintext |

An unknown `kid` after a successful JWKS refresh is invalid authentication; inability to fetch required verification material is dependency unavailability. Preserve the equivalent gRPC status through gRPC-Web. Do not translate upstream credential rejection into Sandbox NotFound, a runtime admission failure, or an end-user API-key error: the failed identity belongs to the OpenSandbox deployment, not the SDK caller.

Keep refresh failures, verifier dependency failures, and policy denials distinguishable in logs and bounded-cardinality metrics. Never log token contents, authorization metadata, kubeconfig secrets, or private keys. Report authenticated service subjects in audit records rather than high-cardinality metric labels. A generic transport `Unavailable` may still leave a mutation outcome unknown; do not add unconditional mutation retries merely because an authentication-layer rejection would have been safe to retry.

### Implementation ownership

| Repository/component | Changes |
|---|---|
| fast-sandbox | FastPath authentication adapters and authorization gate; reusable JWT verification/token primitives where appropriate; native TLS or protected-proxy wiring; upstream configuration, deployment, and tests. The existing Fastlet policy is not widened. |
| OpenSandbox server | TokenRequest-backed Python credential source using the existing Kubernetes client; per-RPC metadata, verified TLS, configuration, and dependency-error mapping. |
| OpenSandbox ingress gateway | TokenRequest-backed Go credential source; per-RPC credentials, verified TLS, and routing/wake-on-access error handling. |
| OpenSandbox manifests | Authentication identities/RBAC, policy wiring, TLS integration, source-pin update, and companion images synchronized with the upstream implementation. |
| OpenSandbox documentation and integration tests | In-cluster and external setup, permission preflight, rotation/revocation semantics, coordinated upgrade, and both deployment-mode acceptance tests. |

Go packages under fast-sandbox `internal/` cannot be imported directly by the OpenSandbox gateway. Reuse the contract and test vectors, not an invalid cross-repository import; any shared export must be an explicit upstream API decision. Python implements the same TokenRequest lifecycle using its existing Kubernetes dependency. Generated protobuf clients do not acquire a new authentication field.

## Test Plan

These are implementation acceptance criteria, not claims that executable changes accompany this draft.

- **Identity matrix:** Run a real in-cluster server, an external server using kubeconfig, and an in-cluster gateway against the same authenticated FastPath; verify that both server modes resolve to the same authorized subject and gateway remains distinct. The external test must not depend on local access to cluster-only issuer DNS.
- **RBAC:** Verify each bootstrap identity can mint only for its configured caller SA in the configured identity namespace; it cannot mint for the other caller, FastPath, or controller. Verify the auth-only targets carry no workload-resource grants, including when a caller requests an API-server audience. Test a restricted non-admin external kubeconfig, not only an administrator fixture.
- **Token source:** Test first mint, cache reuse, shortened granted lifetime, refresh margins, concurrent refresh deduplication, process restart, kubelet/bootstrap credential rotation, kubeconfig credential-provider renewal, transient refresh failure with a still-valid token, and failure once it expires. Assert that no unauthenticated RPC is sent.
- **Verifier:** Cover supported algorithms; missing or nonnumeric `iat`/`exp`; zero, negative, exactly 3,600-second, and overlong `exp - iat`; future `iat` within and beyond the thirty-second skew allowance; rejection at `exp` even with skew allowance or a warm positive cache; malformed, expired, future, wrong-issuer/audience, multi-audience, and forged tokens; exact SA subject parsing; trusted key-fetch destinations; bounded positive/negative caches; known-key removal; unknown-key throttling; issuer outage; and signing-key rotation without restart. On a cluster permitting TokenRequest lifetimes above one hour, mint an otherwise valid overlong token using the external bootstrap identity and verify that both gRPC and gRPC-Web reject it before handler dispatch, including after its minting RoleBinding is removed.
- **Authorization:** Exercise every registered business method through the policy gate, including the optional gRPC-Web listener. Verify gateway wake-on-access succeeds but create/delete/snapshot/handler calls fail; verify cross-namespace and cross-resource references fail; verify empty namespaces cannot request cluster-wide reads. Removing a subject or scope must take effect after policy installation even with a warm identity cache.
- **Hop isolation:** Present Fastlet tokens to FastPath, FastPath tokens to Fastlet, and default API-server tokens to both; all must fail. Even a Fastlet-audience token minted for a server/gateway auth-only SA must fail the Fastlet subject check. Ensure ingress authorization metadata never reaches a Fastlet handler or sandbox workload.
- **Transport:** Test correct TLS identity, untrusted CA, wrong hostname, expired certificate, automatic certificate renewal/reload, client trust refresh, direct backend bypass attempts, and consistent gRPC-Web protection. Neither client may fall back to plaintext.
- **Lifecycle regression:** Exercise create/get/list/update/delete, pause/resume, expiration renewal, snapshots, diagnostics, pool lookup, route resolution, and handler invocation through authenticated server calls; exercise cached routes and wake-on-access through the gateway.
- **Availability and performance:** Measure warm-call authentication overhead, TokenRequest counts under concurrency, and refresh behavior across replicas. Confirm warm calls generate no Kubernetes API traffic; test separate cold-start, token-expiry, and verifier-key-expiry outages rather than claiming unlimited offline availability.
- **Packaging and migration:** Render charts for non-default namespaces, custom Pod SA names, in-cluster-only and external-server bindings, and single/all-namespace policies. Validate upgrade failure modes and ensure old clients receive explicit rejection rather than silently retaining access.

## Drawbacks

The design adds a Kubernetes dependency to initial credential acquisition and key discovery, and an outage can stop traffic once valid cached material expires. External bootstrap credentials and TLS still need a functioning lifecycle supplied by the deployment. Namespace-aware service authorization adds a method-to-resource-scope mapping that must stay aligned with the FastPath API. Offline JWT verification deliberately sacrifices immediate Kubernetes object revocation for low and predictable steady-state overhead.

## Alternatives

- **Static API key/shared Secret:** Simple but introduces manual credential distribution, rotation, and overlap handling across the external server and cluster; it does not meet the credential-maintenance goal.
- **Forward the existing kubeconfig or default Pod token:** Avoids TokenRequest but exposes general Kubernetes credentials to FastPath, loses dedicated audience isolation, and does not give certificate- or exec-based kubeconfigs one common bearer format.
- **Use workload SAs directly as FastPath caller identities:** Fewer SAs, but granting external mint access may convey the target workload SA's Kubernetes permissions through other audiences. Authentication-only targets make the delegated authority explicit.
- **TokenReview plus SubjectAccessReview:** Supports Kubernetes-backed online checks but adds network-dependent decisions and RBAC for reviews; caching trades away immediate revocation again. Retain local verification and a small explicit service policy for this path.
- **mTLS workload identity/SPIFFE:** Appropriate for deployments with an existing identity plane, but not the baseline because enrolling the external server and operating a new certificate identity plane exceeds this scope. Existing TLS infrastructure may still provide transport protection.
- **External OIDC or cloud-specific IAM:** Adds a separate trust and enrollment system despite both server modes already having access to the target Kubernetes API; cloud-specific credentials also reduce portability.
- **NetworkPolicy alone:** Useful defense in depth, but authenticates neither the caller nor the endpoint and does not protect credentials on the wire.

## Infrastructure Needed

No new authentication service, application signing-key Secret, or external identity provider is required. The target Kubernetes cluster must support stable TokenRequest and authenticated ServiceAccount issuer discovery/JWKS access. Reuse the project's supported Kubernetes version range rather than introducing a lower-version compatibility path in this proposal.

Integration coverage needs a cluster, a host-side server with restricted kubeconfig credentials, the in-cluster gateway, and the deployment's managed TLS facility. A deployment without automatic certificate management cannot claim fully unattended transport from this OSEP alone; the reference integration requires an existing cert-manager issuer and deployment-managed public CA distribution.

## Upgrade & Migration Strategy

This proposal enables FastPath caller authentication and TLS by default while providing an explicit opt-out switch (`fastpath_auth_enabled = false` / `--fastpath-auth-enabled=false` / Helm `callerAuth.enabled=false`) so existing deployments can upgrade binaries first and enable transport/caller security in a controlled step. It changes no persisted Sandbox data, public protobuf fields, CRDs, or SDK credentials. There is no automatic runtime downgrade when authentication is enabled: both sides of the connection must agree on whether caller authentication and TLS are enabled.

1. **Zero-prerequisite binary upgrade (optional transitional step):** Deployments that have not yet provisioned cert-manager certificates, CA distribution, or external kubeconfig RBAC can upgrade FastPath, server, and gateway binaries with caller authentication explicitly disabled (`fastpath.callerAuth.enabled=false`, `server` `fastpath_auth_enabled = false`, `gateway.fastpathAuth.enabled=false`). This preserves legacy plaintext gRPC behavior while rolling out the new release.
2. **Provision identities and TLS prerequisites:** Enable `fastSandbox.callerAuth.enabled=true` in the base chart to provision the caller SAs and narrow minting bindings, including any external kubeconfig subject; provision FastPath issuer access, caller policy, and verified TLS serving certificates/client CA trust. Validate TokenRequest under the actual runtime identities, not an administrator's identity, and verify issuer/JWKS access from the actual FastPath environment.
3. **Enable authenticated TLS (default):** Switch FastPath, server, and gateway to the default enabled mode (`enabled=true`); close any old bypass listener. Run the identity matrix, lifecycle, route resolution, and gateway wake-on-access checks before reopening normal traffic.
4. **Steady-state verification:** Validate token refresh and TLS renewal without manual token replacement. Keep failures observable and fail closed while enabled; if an emergency rollback to legacy plaintext is required during migration, explicitly set the opt-out switch on both FastPath and its clients. This proposal does not require rolling Fastlet Pools or changing their token audience or allow list.
