---
title: Managed External Egress Authorization
authors:
  - "@hpliStartAgain"
creation-date: 2026-10-20
last-updated: 2026-10-20
status: draft
---

# OSEP-0026: Managed External Egress Authorization

The number 0026 is provisional pending upstream OSEP numbering review. This document proposes behavior; it does not claim implementation acceptance or production readiness.

<!-- toc -->

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Requirements](#requirements)
- [Proposal](#proposal)
  - [Ownership and integration boundary](#ownership-and-integration-boundary)
  - [Notes/Constraints/Caveats](#notesconstraintscaveats)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Design Details](#design-details)
  - [Administrator-required enrollment](#administrator-required-enrollment)
  - [Runtime identity and credential delivery](#runtime-identity-and-credential-delivery)
  - [File credential source and rotation](#file-credential-source-and-rotation)
  - [CONNECT admission and destination enforcement](#connect-admission-and-destination-enforcement)
  - [Revocation, freshness, and existing tunnels](#revocation-freshness-and-existing-tunnels)
  - [Readiness and diagnostics](#readiness-and-diagnostics)
  - [Relationship to OSEP-0022 and OSEP-0023](#relationship-to-osep-0022-and-osep-0023)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [Questions for Review](#questions-for-review)
- [References](#references)

<!-- /toc -->

## Summary

Add an opt-in, administrator-required enrollment mode and a rotatable, egress-only credential source to OpenSandbox's existing upstream CONNECT support. These primitives let an external platform authorize outbound connections using trusted sandbox identity while OpenSandbox retains local network enforcement. The proposal defines new-connection admission, expiry, failure, and revocation semantics; external policy storage, credential issuance, and gateway products remain outside the core.

## Motivation

The current upstream proxy integration supplies the transport path to an external CONNECT gateway. Its documented server configuration attaches egress when a create request includes `networkPolicy`, and changes to proxy configuration apply to new sandboxes. The current addon loads proxy authorization into process-wide state at startup. These behaviors support administrator-configured chaining, but do not establish mandatory enrollment for a managed population or rotation of short-lived authorization in a running sandbox. See the [configuration][current-config], [upstream guide][upstream-guide], and [addon implementation][upstream-addon] at the review baseline.

A platform may need to apply one external policy across several sandbox instances, rotate gateway credentials without recreating those instances, and stop future CONNECT admissions for one retired instance, subject to the declared credential-expiry and policy-propagation bounds, without disabling the policy shared by others. Reimplementing these requirements independently in each deployment creates ambiguity at the boundary between sandbox lifecycle, credential delivery, and CONNECT admission.

Downstream gateway integration motivates three distinctions. A business policy and a runtime instance have different lifecycles. Delivery of a credential does not prove that an egress process consumed it. Stopping issuance, denying a new CONNECT, and terminating an established tunnel are separate effects. These lessons motivate the contracts below; downstream experience is not a conformance result for this proposal.

### Goals

1. Make required egress enrollment an administrator decision that a sandbox create request cannot omit or weaken.
2. Allow a trusted runtime adapter to provide rotating, instance-scoped gateway authorization to egress without exposing it to the workload.
3. Specify when each new CONNECT uses a credential, how local and external decisions compose, and what happens on expiry or failure.
4. Preserve the responsibilities and compatibility boundaries of upstream CONNECT, OSEP-0022, and OSEP-0023.
5. Provide a small, testable integration contract that multiple external control planes and gateways can implement.

### Non-Goals

- Adding business ownership, quotas, billing, a policy database, or a mandatory external admission service to OpenSandbox.
- Replacing `networkPolicy`, the credential vault, or application credentials with a new policy language.
- Standardizing an enterprise JWT claim schema, Envoy configuration, or an identity issuer implementation.
- Promising authorization for every request inside an admitted tunnel or immediate termination of existing tunnels.
- Claiming initial parity across shared egress, Fast, Pool, gVisor, Kata, IPv6, arbitrary TCP, UDP, or QUIC.

## Requirements

The terms MUST, SHOULD, and MAY describe proposed implementation requirements, not capabilities of the current release.

| ID | Requirement |
| --- | --- |
| R1 | Required enrollment MUST be selected through trusted administrator configuration and enforced before untrusted execution can open a route outside the configured boundary. Unsupported combinations MUST fail creation. |
| R2 | The profile MUST declare proxied traffic, administrator-authorized direct exceptions, and denied traffic. Managed public traffic MUST NOT fall back to a direct route. |
| R3 | Gateway authorization MUST be bound through a trusted runtime adapter to the active sandbox incarnation. Workload-supplied identifiers, headers, metadata, or environment variables MUST NOT establish that binding. |
| R4 | Each new CONNECT attempt MUST select a complete, valid credential for that incarnation. Rotation MUST prevent partial reads, stale replacement, and cross-instance credential selection or upstream-connection reuse within the trusted egress implementation. |
| R5 | An external allow decision MUST NOT override a denial by local network enforcement or an applicable credential admission check. Gateway authentication MUST remain separate from origin-service authentication. |
| R6 | Missing or expired credentials, unavailable authorization, unknown policy, and expired policy state MUST deny new upstream CONNECT admissions. Failure MUST NOT activate a static credential or unmanaged fallback. |
| R7 | Issuance revocation, credential expiry, policy revocation, and established-tunnel termination MUST have separate documented effects and observation points. |
| R8 | Existing behavior MUST remain unchanged when the administrator has not selected the new mode. Broader runtime or interception support MUST require its own acceptance evidence. |
| R9 | Operators MUST be able to distinguish local enrollment, credential consumption, and external policy observation without exposing authorization secrets or claiming a distributed readiness transaction. |

## Proposal

The public data path remains sandbox traffic through local egress, then through the configured external CONNECT gateway to an approved origin. A trusted adapter delivers instance credentials to egress. An external policy service supplies authorization state to the gateway or its authorizer. Business payloads do not pass through the credential issuer or policy distribution API.

The proposed core changes are limited to administrator-required enrollment, a file-backed upstream authorization source, local CONNECT-admission enforcement using the existing network policy, and the validation and diagnostics needed to uphold those contracts. This local check closes the intercepted-traffic enforcement gap; it does not introduce a new policy language or general admission-provider framework. The external platform owns its policy model and runtime-to-policy association. A reference integration demonstrates that boundary without making its database, issuer, or gateway a required OpenSandbox dependency.

### Ownership and integration boundary

| Component | Responsibility |
| --- | --- |
| OpenSandbox server/runtime adapter | Select a trusted profile, install the local enforcement boundary, validate the runtime binding, and provision an egress-only credential source before admitting managed traffic. |
| Egress | Enforce local authority, port, and applicable original-destination constraints before each new CONNECT; read the credential source for that attempt; authenticate only to the configured upstream; expose consumption and rejection diagnostics. |
| External issuer/controller | Authorize runtime enrollment, maintain the runtime-to-policy association, issue and rotate credentials, and stop renewal after binding revocation. |
| External gateway/authorizer | Validate the presented credential and current policy, enforce destination selection, and declare its existing-tunnel behavior. |
| Deployment network boundary | Prevent workload access to credentials, trusted process exemptions, alternate gateways, and direct public routes that bypass local egress. |

### Notes/Constraints/Caveats

The first implementation target is a non-pooled Kubernetes sandbox using the ordinary per-sandbox `runc` egress sidecar, `dns+nft`, and the existing transparent MITM upstream-chain profile. This is a proposed acceptance target, not a claim that all of its managed-enrollment and identity-isolation requirements have already been met. The implementation must name its Kubernetes workload provider; evidence for one provider does not establish acceptance for another. Other runtimes remain outside the initial profile until separately demonstrated.

For that profile, managed public traffic is HTTPS to approved public FQDNs over IPv4 TCP port 443. The outer gateway protocol remains the existing HTTP/1.1 CONNECT path; application HTTP/2 inside that path does not imply HTTP/2 CONNECT support. The [current upstream guide][upstream-guide] documents narrower support for experimental interception, TLS passthrough, Fast server integration, and other combinations. This proposal does not broaden those claims.

The profile explicitly permits only the infrastructure routes needed for operation, such as the configured DNS resolver and upstream endpoint. Any internal direct destinations require administrator-defined rules with a documented scope. All remaining protocols, address families, ports, and destinations are denied in this profile. “Managed egress” describes control of this complete routing partition; it does not mean CONNECT carries every outbound packet.

The existing upstream-chain path also handles HTTP/80, and [port-scoped network rules do not govern intercepted traffic][egress-architecture]. The proposed 443-only profile therefore needs explicit, tested rejection of HTTP/80 and other out-of-profile traffic at the appropriate interception or enforcement boundary. An ordinary port rule or port floor alone must not be assumed to establish this restriction. The current [transparent redirect][transparent-redirect] runs at output priority `-100`, before the [network-policy output hook][policy-output] at priority `0`, whose loopback accept precedes destination and port rules. A workload can therefore dial a hard-coded IPv4 address on 443 without a DNS query and supply an SNI denied by the local baseline but allowed externally. DNS filtering and containment of the upstream endpoint are not proof that this CONNECT authority passed local authorization. The managed profile requires the local admission and original-destination checks below before it can claim R5.

### Risks and Mitigations

| Risk | Required mitigation or acknowledged cost |
| --- | --- |
| Enrollment omitted during creation or recovery | Enforce the profile before untrusted execution; reject incompatible inputs and restored state. |
| Workload obtains an egress credential or impersonates an exempt process | Validate runtime isolation, mount/process visibility, capabilities, and the identity used for network exemptions; a different container alone is insufficient evidence. |
| Rotation race or unavailable identity source | Atomic publication, per-attempt reads, incarnation checks, bounded expiry, and denial on an invalid read; temporary connection failures are an accepted cost. |
| Stale external policy or partial replica rollout | Explicit revision and freshness contracts; report per-replica observation and deny after the validity deadline. |
| Authorization approves one address while the gateway dials another | Bind the decision to the actual destination address and revalidate on a changed dial target. |
| Long-lived connection survives a revoke operation | Declare admission-only semantics; require a separate executor capability for a bounded tunnel lifetime or active termination. |

The threat model includes an untrusted sandbox process attempting to read credentials, forge identity, change proxy settings, or bypass the controlled route. Host, runtime, issuer, and gateway compromise are outside the guarantee. A bearer credential containing sandbox identity is still transferable if stolen; identity claims do not provide proof of possession.

## Design Details

### Administrator-required enrollment

An administrator selects the mode for a deployment or a trusted runtime profile. The preferred behavior is for the server to install the required egress configuration even when the create request omits `networkPolicy`. The first profile uses one explicit administrator-owned restrictive local baseline. A supplied `networkPolicy` must match that baseline after normal request validation, or creation fails before workload execution. The same restriction applies to subsequent policy mutation and restore paths. General composition with caller policies is deferred rather than inferred from rule concatenation. The final configuration field spelling is a review decision; arbitrary request metadata and ordinary environment variables are not configuration authorities.

Managed enrollment does not require an origin Credential Vault or origin credential bindings. The selected runtime profile must provision transparent MITM independently of those optional application capabilities, and must reject requests or environment settings that disable or conflict with the required interception path.

Creation installs a deny-by-default network boundary before starting the untrusted workload. Public admission stays closed until the trusted identity source is usable. Bootstrap exceptions are limited to the configured infrastructure paths and remain destination constrained. Restart, resume, and replacement must either re-establish the same guarantees or reject the operation; they cannot restore a route using credentials or bindings from a previous incarnation. Enabling this mode does not retroactively protect existing sandboxes.

The runtime must prevent the workload from modifying routing rules or impersonating the identity used to exempt trusted egress traffic. For example, an exemption based only on a numeric UID is insufficient when the workload can acquire that UID. A profile that cannot demonstrate credential and routing isolation must be rejected rather than advertised as managed.

### Runtime identity and credential delivery

The external platform maintains the association between its policy subject and a sandbox incarnation. The trusted adapter verifies that association using authoritative runtime ownership and lifecycle information before requesting or installing credentials. A caller-supplied policy reference may be an input to authorization, but possession of that reference does not grant access to the policy.

The initial profile has exactly one active sandbox incarnation per egress credential source. The source is mounted read-only into egress, excluded from the workload's accessible mounts and process interfaces, and writable only by the trusted delivery mechanism. Proxy endpoint, trust roots, expected runtime binding, and credential source are administrator owned. Credentials must not appear in SDK responses, workload environment, image layers, diagnostic bodies, or application logs.

The adapter fences stale delivery work using its authoritative incarnation and update ordering. It must reject an update for a retired runtime and prevent an older controller operation from overwriting a newer credential, including during restart and restore. Egress validates the expected binding and rejects observed revision regression; this local check supplements controller fencing and is not a replacement for it.

In the initial profile, the expected binding is immutable for an egress admission lifecycle; credential rotation does not change it. Assignment to another incarnation requires retiring the previous lifecycle, cancelling its pending CONNECT attempts, and discarding its pooled connection state before activating the replacement. Recovery of an egress process may retain the same still-active sandbox incarnation if the trusted adapter revalidates it. The adapter remains responsible for update fencing across that recovery; an in-memory revision high-water mark is not persistent replay protection.

A later shared-egress integration must reuse OSEP-0022's trusted subject resolution and attachment fencing. It must select authorization from that resolved context and partition connection state accordingly. This proposal adds neither a competing subject registry nor a client-selectable identity header, and its first implementation must reject a shared-egress configuration.

### File credential source and rotation

Add an administrator-configured file source as an alternative to the existing static upstream authorization. Configuring both sources is an error. The proposed record is an internal egress integration format, not a new public sandbox API or a JWT schema:

| Field | Proposed meaning |
| --- | --- |
| `schemaVersion` | Version of this credential-source record; initially `1`. |
| `runtimeBinding` | Opaque value for the expected active incarnation, supplied independently through trusted runtime configuration. It is a comparison value, not proof of ownership or a new policy subject registry. |
| `revision` | Increasing credential-delivery revision within that binding. Reusing a revision with different contents is invalid. |
| `authorization` | Non-empty, single-line value for the outer `Proxy-Authorization` header, treated as secret and otherwise opaque by egress. |
| `expiresAt` | Absolute UTC deadline after which egress must not select this record. The issuer must set it no later than the credential's actual acceptance deadline. |

The writer publishes the complete record using an atomic replacement supported by the delivery mechanism. Kubernetes projected-volume updates must be validated with the actual projection mechanism; a controller's successful write does not prove local visibility. No update may expose a partially written credential to egress.

For every new upstream CONNECT attempt, including a retry, egress opens the configured path and reads a size-bounded complete record. It validates the schema, binding, revision, header syntax, and expiry, then takes an immutable snapshot for that attempt. An equal revision is valid only with identical contents. Revision comparison, advancing the observed revision, and admitting the snapshot must be serialized or atomic per binding: a reader paused on revision 7 cannot select it after another reader has selected revision 8. This admission of the snapshot is the local selection point. A concurrent replacement does not mutate an already selected attempt. The external gateway independently validates the credential at its own admission point.

The initial behavior is strict: a missing, unreadable, malformed, wrong-binding, regressed, or expired record rejects the new attempt. Egress does not use a cached last-known-good record after such a read failure and does not fall back to static authorization. If the controller is unavailable but the file still contains a valid record, that record may be selected only until its original expiry. Reading or observing it again cannot extend the deadline.

The source's expiry metadata comes from the trusted delivery path. Egress does not infer authorization by parsing an arbitrary enterprise token. The gateway remains responsible for issuer trust, audience, signature or equivalent credential verification, and time validity. The deployment must define bounded clock error and reject new admissions when it cannot maintain that bound; credential lifetime must not be extended by a backward clock jump.

In the managed profile, the gateway MUST enforce a finite expiry for every issued credential, including opaque credentials whose expiry is held server-side. The declared clock-error allowance bounds any acceptance beyond that expiry. A local `expiresAt` field alone does not establish a gateway-side expiration bound.

The selected authorization is attached only to the outer CONNECT sent to the configured gateway over verified TLS in the managed profile. It must replace any workload-controlled proxy authorization and must never be forwarded as an origin-service header. Connection reuse must not cross runtime bindings. An established tunnel retains its original admission revision and may continue carrying application traffic under the declared admission-only semantics; observing a new record does not reauthorize or relabel that tunnel. Every actual new CONNECT, including one caused by pooling or a retry, must perform credential selection again. Changing a process-global header is insufficient.

### CONNECT admission and destination enforcement

Each new upstream CONNECT is admitted only when the local route and network policy allow it, the selected instance credential is valid, any applicable credential-bound interception admission permits it, and the external gateway allows the requested destination using valid policy state. Each layer may deny; an allow at one layer does not override another layer.

Before emitting every new upstream CONNECT, including retries and attempts caused by connection pooling, egress MUST normalize and validate the actual CONNECT authority and port and evaluate its normalized hostname against the administrator baseline using the [existing domain-policy first-match and default-action semantics][domain-policy]. This is not a new deny-overrides rule order. Missing, invalid, or conflicting destination identity (including inconsistent SNI, Host, or CONNECT authority when present) MUST fail closed. A change to the authority or port after a check invalidates that result and requires a new check before the CONNECT is emitted; a prior DNS decision or an admission for another authority is insufficient.

The profile MUST also preserve applicable local restrictions on the workload's original destination IP/CIDR and port at a pre-redirect check or an equivalent enforcement point that retains the original destination context. An allowed authority must not erase an original-destination denial, and a pre-redirect IP check alone does not evaluate a denied SNI. These are proposed local enforcement requirements, separate from the gateway's authorization of the address it actually dials. They apply to new CONNECT admission and do not require reauthorization or immediate draining of established tunnels.

The reference gateway must validate the CONNECT authority, port, and destination classification. For the initial profile, IP literals and non-public address results are rejected, and only supported IPv4 FQDN destinations on port 443 are eligible. The authorized address must be the actual address dialed. A cache hit, retry, changed DNS answer, or alternate resolved address cannot substitute an unapproved endpoint. Mixed address results and address-family handling require explicit, restrictive behavior.

An Envoy/ext_authz integration, for example, must demonstrate how the authorization decision constrains the subsequent dynamic-forward-proxy dial and how decision metadata is protected from client injection. Merely checking a hostname before an independent resolver chooses the dial target is insufficient. This is a reference-integration acceptance requirement, not a requirement to embed Envoy in OpenSandbox.

Local containment of the upstream proxy endpoint and remote authorization of the final origin are distinct checks. The local runtime must prevent the workload from using an alternate route or the gateway directly with forged identity. Origin authority and TLS identity remain bound to the requested destination even when dialing a pinned IP. Gateway or authorizer unavailability denies new managed CONNECT attempts; it does not enable a direct connection.

### Revocation, freshness, and existing tunnels

The baseline offers bounded expiration for new-connection credentials and admission-time policy enforcement. It does not offer an atomic fleet-wide revoke operation.

| Event | Required effect on new CONNECT admission | Effect on an established tunnel |
| --- | --- | --- |
| Runtime binding loses issuance permission | The issuer stops issuing or renewing credentials for that incarnation. An already issued credential may remain acceptable until expiry unless the gateway has also observed an explicit runtime revocation. | No automatic termination. |
| Credential source becomes missing or invalid | Once observed by egress on a new read, selection fails. An attempt that already selected a record remains subject to gateway admission. | No automatic termination. |
| Credential expires | Egress stops selecting it; the gateway rejects it at admission according to the declared clock bound. | Expiry alone does not close the tunnel. |
| Shared policy is disabled or narrowed | A gateway that has observed the effective policy denies subsequent disallowed admissions. This may affect every runtime sharing that policy. | No automatic termination in the baseline. |
| Gateway policy state expires or authorization is unavailable | New admission is denied. A still-valid cached policy may be used only under its declared validity contract. | No automatic termination in the baseline. |
| Gateway implements an explicit tunnel lifetime or termination capability | New-admission checks still apply. | Only that executor's separately declared and tested capability provides a termination bound. |

For a stopped issuer, the maximum remaining acceptance window for new connections is bounded by the maximum remaining validity of credentials already issued plus the permitted verifier clock error. A deployment must publish and test those values; the token refresh interval alone is not the revocation bound. Immediate per-runtime denial would additionally require distributing runtime revocation state to every eligible gateway or checking an authoritative online source. That stronger contract is deferred.

This bound starts at a declared effective issuance cutoff, not merely receipt or acknowledgment of a revoke request. The external issuer and adapter must fence in-flight renewal work or explicitly include any permitted post-request issuance and its maximum credential validity in the published bound. A successful delivery after a cutoff cannot silently move the claimed revocation deadline.

External policy state must have identifiable revisions and explicit freshness semantics. A snapshot-based authorizer must atomically publish a validated snapshot, reject stale replacement, and deny new admission after its validity deadline. An online authorizer must deny when the authoritative decision cannot be obtained within its timeout. Neither design may silently renew stale authorization because a synchronization loop is still running.

The external integration owns the relationship between credential revisions and policy revisions. If credentials carry a required policy revision, a mismatch must deny rather than guess compatibility. Asynchronous updates can therefore cause a temporary conservative denial; implementations must document the ordering and recovery behavior. Credential-delivery revision, external policy revision, and an OSEP-0023 binding revision are different version domains.

A control-plane update acknowledgment is not evidence that every gateway has observed the update. Fleet-level completion requires observation from all gateways eligible to receive traffic, or isolation of unconfirmed gateways. The initial core interface does not introduce that distributed transaction. It reports local consumption and leaves convergence reporting to the external integration.

CONNECT admission authorizes establishment of a tunnel, not every HTTP request, HTTP/2 stream, or other exchange inside it. Established tunnels can survive credential expiry, a policy change, or loss of the control plane. This OSEP claims no bounded termination time for an executor without explicit tunnel-lifetime enforcement; evidence from one gateway implementation cannot establish that capability for another.

### Readiness and diagnostics

Local enforcement readiness means that the required routing boundary is installed. Credential readiness means that egress has successfully validated a record for the expected incarnation and observed revision. External policy readiness belongs to the gateway or authorizer and identifies the policy state it can currently enforce. These are separate observations and must not be presented as one atomic “attached” result.

A consumption observation records when and which record was validated; it does not guarantee that the source remains unchanged. Any current readiness claim must account for expiry and the source-validation semantics above. Historical success cannot keep an expired or unreadable source marked ready.

The first implementation should expose these observations through administrator diagnostics and rejection reason codes using existing egress mechanisms where possible. At minimum, distinguish missing identity, invalid source, binding mismatch, expiry, gateway authentication failure, and gateway unavailability. External integrations additionally report policy observation and freshness failures. Do not require a new multi-stage public SDK lifecycle for this proposal.

Diagnostics may expose opaque binding references, revisions, deadlines, and bounded reason codes, but never the authorization value or raw token claims. High-cardinality runtime identifiers belong in access-controlled diagnostic records rather than unbounded metric labels. Best-effort counters do not constitute a durable security audit trail.

### Relationship to OSEP-0022 and OSEP-0023

| Existing work | Responsibility retained | Contribution of this proposal |
| --- | --- | --- |
| [Upstream CONNECT][upstream-guide] | Chaining transport, proxy endpoint containment, and documented runtime/protocol support. | Mandatory enrollment for a selected profile and a rotating source for outer proxy authorization. |
| [OSEP-0022][osep-0022] | Resolving trusted local subjects and fencing runtime attachments when one egress serves multiple sandboxes. | An eventual external-authorization consumer of that context; no replacement subject registry. Shared operation is deferred from the initial profile. |
| [OSEP-0023][osep-0023] | Credential-bound TLS interception, local binding acknowledgment, fencing, and transition behavior. | External gateway admission and its independent credential/policy lifecycle. A local binding acknowledgment does not acknowledge an external policy rollout. |

Removing an origin credential or interception binding and revoking external network access express different intent. The former follows OSEP-0023's local behavior; the latter may deny a new connection at the gateway. A remote policy allow cannot reactivate a locally revoked credential capability.

The initial profile uses the currently documented upstream-chain mode and does not require completion of every OSEP-0023 experimental feature. Combining managed authorization with selective interception or a shared-egress runtime requires a later explicit compatibility profile. Unsupported combinations must be rejected rather than inferred from component-only support.

## Test Plan

This documentation PR provides a proposal and no runtime conformance results. Implementation PRs must identify their runtime and gateway profile and supply evidence for the following acceptance matrix. CI should use synthetic issuers, credentials, and destinations; no private deployment is required to review or reproduce the core contract.

| Acceptance case | Required observation | Requirements |
| --- | --- | --- |
| T1: Enrollment cannot be omitted | Omitting `networkPolicy` or origin bindings does not remove the boundary; conflicting policies, runtime policy mutations, attempts to disable required interception, and unsupported runtimes are rejected. Ordinary environment/metadata cannot bypass the profile. | R1, R8 |
| T2: Routing partition and startup | During creation, restart, and credential absence, direct public access, alternate proxy use, raw IP, HTTP/80, IPv6, UDP/QUIC, and unsupported ports fail; only declared infrastructure/direct exceptions work. | R1, R2, R6 |
| T3: Identity isolation | A workload cannot read the source, impersonate a trusted process exemption, or use another runtime's binding. Egress rejects stale writes and nonmatching records; pending attempts and pooled state cannot pass to a replacement incarnation. Already-issued credentials after issuance revocation follow T7. | R3, R4 |
| T4: Rotation under concurrency | New attempts observe complete revisions; retries reread the source; invalid reads deny. Equal-revision mutation and regression fail, including a paused revision-7 reader after revision 8 is selected. Pooling cannot cross bindings or relabel an existing tunnel's admission revision. | R4, R6 |
| T5: Credential validation and secrecy | Wrong issuer/audience, invalid credential, CR/LF injection, expired record, and invalid gateway TLS fail; gateway authorization is absent from origin requests, workload interfaces, and logs. | R3, R4, R5 |
| T6: Policy and destination composition | With no workload DNS query, dialing a hard-coded IPv4 address on 443 with locally denied SNI but external allow emits zero upstream CONNECTs; a matching both-allow control succeeds. Verify existing first-match/default domain semantics, missing/invalid/conflicting identity rejection, and a fresh local check on every new CONNECT, including retries and authority/port changes. Local original-destination IP/CIDR or port denies still block an allowed authority. External deny wins over local allow; unknown or expired policy fails; DNS rebinding, mixed answers, cache hits, and retries cannot make the gateway dial an unapproved address. | R2, R5, R6 |
| T7: Revocation and replica observation | Race renewal against one instance's binding revocation while another shares its policy. Verify the effective issuance cutoff and latest outstanding expiry; measure policy rollout lag, stale replica behavior, and clock error separately. | R7, R9 |
| T8: Existing-tunnel semantics | A long-lived CONNECT is observed across expiry and policy changes; results match the declared admission-only behavior. A termination capability, if advertised separately, has executor-specific tests. | R7 |
| T9: Compatibility and recovery | Mode off preserves current static behavior; enabling it rejects dual credential sources and unsupported interception/shared profiles; rollback and restore never create an unmanaged fallback. | R1, R8 |
| T10: Readiness and failures | Delayed file projection and policy rollout produce distinct observations; issuer, source, gateway, and authorizer failures yield the documented new-admission decisions without leaking secrets. | R6, R9 |

Core credential-source tests should exercise record parsing, expiry, update ordering, and attempt selection. Runtime integration tests must prove the isolation and routing claims with an untrusted workload; a unit test of configuration injection is insufficient. T1, T2, T3, and T9 must run for each advertised Kubernetes workload provider. Gateway conformance tests must observe actual destination sockets and long-lived tunnels. Broader runtime or gateway support is accepted only with its own evidence for the applicable rows.

## Drawbacks

Required enrollment adds a lifecycle dependency: incomplete identity or policy delivery can delay or deny network access. The first profile also rejects caller policies that differ from its administrator baseline, so adopting it may require changes to existing create or policy-update callers. Strict reads favor a clear security boundary over availability during projection or source failures. Reading a record for each CONNECT also has a cost that implementation benchmarks should measure under realistic connection churn.

Admission-only revocation leaves a long-lived access window for established tunnels. Operators requiring a hard disconnect deadline need an executor that enforces it. Shorter credential lifetimes improve the new-connection bound but increase rotation load and sensitivity to clock or delivery problems.

Even a small core interface imposes compatibility and test obligations on runtime adapters. Keeping policy storage and gateway choice external limits that maintenance burden, but does not eliminate the need to verify destination pinning, credential isolation, and failure behavior for each supported profile.

## Alternatives

| Alternative | Assessment |
| --- | --- |
| Keep static upstream authorization and recreate sandboxes for rotation | Uses existing support and remains appropriate for simple deployments. It does not provide rotation within a running sandbox or a generic per-incarnation delivery contract. |
| Implement the complete feature in an external broker and deployment templates | A valid starting point if the deployment already proves mandatory enrollment and credential isolation. Prefer existing configuration mechanisms where they satisfy the contract; the core addition should cover only gaps requiring stable server or egress behavior. |
| Synchronize all external policy into each sandbox's `networkPolicy` | Useful for local restrictions, but does not by itself define gateway identity, credential rotation, or external CONNECT admission. It also couples external policy distribution to every local runtime. |
| Extend OSEP-0022 as the entire solution | Reuses necessary local identity foundations for shared egress, but does not establish external issuer/gateway lifecycle semantics. This proposal consumes that context when a shared profile is added. |
| Add a general admission-provider framework and central policy platform to core | Creates a much larger API and operational commitment than required for this use case. The initial file source and external reference boundary are smaller to review and maintain. |

## Infrastructure Needed

Core validation needs a synthetic credential writer, a controllable CONNECT gateway/authorizer, and an isolated runtime environment for routing and credential-access tests. A reference deployment may use an external issuer, policy distributor, and Envoy or another CONNECT gateway, but no particular database, service mesh, or hosted service is required by the OSEP.

The managed profile needs verified TLS to the gateway, reliable time bounds, and a credential delivery mechanism that preserves atomic updates and isolation. An external gateway deployment does not require transparent service-mesh sidecar injection into the sandbox Pod; any such combination remains subject to the [existing upstream limitations][upstream-guide].

## Upgrade & Migration Strategy

1. Land the generic credential-source contract and focused egress tests behind administrator configuration. Preserve static authorization when the new mode is not selected; reject dual-source configuration.
2. Add mandatory enrollment and identity delivery for the first runtime profile. Validate the acceptance matrix before advertising that profile as supported.
3. Publish an optional reference integration and its gateway-specific conformance results, including credential lifetime, clock bounds, policy freshness, and existing-tunnel behavior.
4. Add shared-egress or OSEP-0023 selective-interception combinations only through explicit follow-up compatibility work using the existing identity and binding contracts.

Adoption applies to newly created sandboxes in the selected profile. Existing sandboxes must be drained or recreated under an operator-managed migration; they must not be relabeled as protected without verification. Pool adoption and credential-bearing snapshot restore are deferred until their incarnation and secret-handling rules are demonstrated.

Disabling the profile for future creation is an explicit administrator action. Existing managed sandboxes retain their boundary until retired; an unavailable controller or a rolled-back server must not reinterpret them as unmanaged. A downgrade that cannot preserve their configuration requires draining them first. This proposal changes no existing public request schema or default behavior by itself.

## Questions for Review

The draft selects strict per-attempt file reads, denial after a source read failure, and admission-only baseline revocation. Review is requested on these decisions and on the smallest upstream interface needed to support them:

1. Should mandatory enrollment be a server deployment setting or a trusted runtime-profile setting, and can an existing configuration mechanism satisfy it without a new public API? Is a fixed administrator baseline sufficient for v1, with general caller-policy composition deferred?
2. Is the proposed internal credential record the right first interface, or can an existing egress-owned secret/source abstraction provide the same binding, revision, expiry, and consumption semantics?
3. Which Kubernetes workload provider should be validated first, and should the initial scope remain that non-pooled `runc` sidecar profile before adding another provider, Docker, or OSEP-0022 shared egress?
4. Which credential-lifetime and clock-error bounds should the reference integration publish, while keeping immediate tunnel termination a separately negotiated executor capability?

## References

Current-behavior statements use upstream commit [`39bbe518f95f89527e5c906201e7ee7642e1265d`](https://github.com/opensandbox-group/OpenSandbox/commit/39bbe518f95f89527e5c906201e7ee7642e1265d), reviewed on 2026-10-20. Status and support claims should be checked again before upstream submission.

- [Server configuration and upstream proxy settings][current-config]
- [Upstream proxy integration and support boundaries][upstream-guide]
- [Current upstream CONNECT addon][upstream-addon]
- [Egress architecture and intercepted-port limitations][egress-architecture]
- [OSEP-0022: Multi-Sandbox Egress Control Plane][osep-0022]
- [OSEP-0023: Credential-Bound TLS Interception][osep-0023]
- [OSEP contribution process][osep-process]

[current-config]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/server/configuration.md
[upstream-guide]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/docs/guides/egress-upstream-proxy.md
[upstream-addon]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/components/egress/mitmscripts/upstream_proxy.py
[egress-architecture]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/docs/architecture/network/egress.md
[osep-0022]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/oseps/0022-multi-sandbox-egress-control-plane.md
[osep-0023]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/oseps/0023-credential-bound-tls-interception.md
[osep-process]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/oseps/CONTRIBUTING.md
[transparent-redirect]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/components/egress/pkg/iptables/transparent.go#L62-L69
[policy-output]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/components/egress/pkg/nftables/manager.go#L284-L327
[domain-policy]: https://github.com/opensandbox-group/OpenSandbox/blob/39bbe518f95f89527e5c906201e7ee7642e1265d/components/egress/pkg/policy/policy.go#L102-L140
