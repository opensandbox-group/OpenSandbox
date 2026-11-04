---
title: Optional E2B Compatibility for Core Agent Workflows
authors:
  - "@hpliStartAgain"
creation-date: 2026-11-04
last-updated: 2026-11-04
status: draft
---

# OSEP-0026: Optional E2B Compatibility for Core Agent Workflows

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
  - [Compatibility Profiles](#compatibility-profiles)
  - [Control Plane and Resource Identity](#control-plane-and-resource-identity)
  - [Execution and File Data Plane](#execution-and-file-data-plane)
  - [Routing and Security](#routing-and-security)
  - [Errors and Operational Behavior](#errors-and-operational-behavior)
  - [Delivery Gates and Community Work](#delivery-gates-and-community-work)
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
- [References](#references)
<!-- /toc -->

## Summary

This proposal explores an optional E2B-compatible entry point for selected agent workflows backed by OpenSandbox. Compatibility would be defined by explicit SDK versions, supported operations, runtime configurations, and reproducible tests, while OpenSandbox's native APIs remain the source of truth for its own clients. This is a design proposal and an invitation to collaborate: it does not deliver an adapter, announce supported frameworks, or commit the project to complete E2B parity or a release date.

## Motivation

Applications using E2B SDKs can require substantial integration work to adopt a different sandbox backend. A shared compatibility component could reduce repeated adapters and make a small set of common workflows reusable across deployments.

[Issue #800](https://github.com/opensandbox-group/OpenSandbox/issues/800) raised this request. The [maintainer response](https://github.com/opensandbox-group/OpenSandbox/issues/800#issuecomment-4394440123) declined direct core API compatibility because of abstraction differences, maintenance costs, and misleading equivalence. A later [community report](https://github.com/opensandbox-group/OpenSandbox/issues/800#issuecomment-5474176640) described a standalone sidecar PoC; its reported results are useful prior art, not independently verified acceptance evidence for this proposal.

This OSEP narrows the question to an optional, tested compatibility surface with explicit limits. Permission to discuss this direction does not establish design approval, implementation ownership, or ongoing support. E2B is an external product API and SDK ecosystem, not an OpenSandbox-owned interoperability standard.

### Goals

1. Demonstrate at least one agreed agent workflow using an unmodified, pinned official E2B SDK against OpenSandbox, with only documented endpoint, credential, and template configuration changes.
2. Define a minimum sandbox profile covering creation, connection to a running sandbox, inspection, listing, timeout changes, command execution, file transfer, and deletion. Include every additional operation required by the selected workflow before declaring that integration supported.
3. Preserve observable behavior for advertised operations, including errors, authorization, streaming completion, timeouts, and resource cleanup.
4. Publish a compatibility matrix and executable tests that distinguish supported behavior, explicit limitations, unsupported operations, and untested combinations.
5. Provide independent work items for community contributors without making the native server, SDKs, or every runtime depend on the compatibility component.

### Non-Goals

- Full E2B API, SDK, CLI, cloud service, or future-version parity; a claim that all E2B integrations work unchanged.
- Replacing or renaming OpenSandbox's public APIs, or changing native defaults to match E2B.
- E2B account management, billing, managed template builds, hosted domains, or cloud infrastructure provisioning.
- Equating container images with E2B templates, or root filesystem snapshots with E2B process/memory snapshots.
- Initial support for pause/resume, auto-pause, fork, volume management, PTY, filesystem watchers, arbitrary public port exposure, or cloud identity features. A workflow requiring one of these is blocked until that feature is separately designed and tested; it is not silently approximated.
- Promising every runtime, language, SDK release, image, or operating system. Unvalidated combinations remain unsupported.

## Requirements

| ID | Requirement |
|---|---|
| R1 | Compatibility is opt-in and separately addressable; disabling it leaves native OpenSandbox APIs usable without an E2B dependency. |
| R2 | Each advertised profile pins the SDK packages, relevant protocol sources, OpenSandbox version, runtime, image digest, transport configuration, and operation-level tests. No version is supported solely because its source was inspected. |
| R3 | The actual official SDK and a selected real framework workflow must pass against a real OpenSandbox backend before that integration is advertised. Mock or direct HTTP tests alone are insufficient. |
| R4 | Accepted inputs retain their documented semantics. Unsupported operations and non-default options fail explicitly before side effects; no ignored security policy, fabricated metadata, or success-shaped fallback. |
| R5 | Authentication and resource authorization apply to both planes. Routing identifiers never confer access, and native backend credentials never reach clients or sandbox workloads. |
| R6 | Existing OpenSandbox policy and isolation constraints remain effective. Compatibility cannot grant broader images, resources, filesystem identity, network access, or sandbox visibility. |
| R7 | Resource mapping survives adapter restarts; create failures, deletion, expiry, and ambiguous upstream outcomes have bounded reconciliation and cleanup behavior. |
| R8 | Native regression checks and the published compatibility test matrix are release gates for the affected component. Support expansion requires evidence and an explicitly accepted maintainer for that work. |

## Proposal

Use a separately deployable compatibility service as the initial design candidate. In this document, "adapter" and "compatibility component" refer to that same service. It terminates the selected E2B client protocols and uses native OpenSandbox lifecycle and data-plane capabilities. A separate origin or listener avoids collisions between similarly named paths whose schemas and semantics differ. No E2B routes are added to the native server by this proposal.

The service has two responsibilities: translate lifecycle requests into authorized OpenSandbox operations, and serve the execution/file protocols expected by the selected SDK. Sharing an E2B-shaped method name or a lifecycle REST path does not satisfy the second responsibility.

The first implementation should target one reproducible Linux runtime/image combination, proposed to be Docker for local contributor testing. Kubernetes, Fast Sandbox, other images, and additional SDK languages require their own matrix entries and acceptance evidence. This choice limits initial validation; it does not remove any native runtime support.

### Notes/Constraints/Caveats

- This PR is documentation only. Implementation requires the decisions and evidence in the delivery gates below; the status remains `draft` until maintainers change it through the OSEP process.
- "Compatible" must be qualified by a profile and version matrix. Avoid unqualified "drop-in replacement", "100% compatible", or "supports all E2B frameworks" claims.
- The target SDK language and first framework will be selected with interested users in Gate 0. Python is a candidate because the existing PoC report used it; that report does not establish support for Python, JavaScript, or code-interpreter packages here.
- Custom endpoints must work through configuration exposed by the pinned SDK and framework. If the framework hardcodes hosted E2B endpoints, a separate framework contribution is needed and the configuration-only integration remains blocked.
- Repository placement, implementation language, packaging, and release ownership remain review decisions. Proposed separation is an architectural boundary, not a commitment to a new official hosted service or an independently staffed project.

### Risks and Mitigations

| Risk | Mitigation |
|---|---|
| Partial compatibility produces late application failures | Start from a real workflow, inventory its full call graph, and require operation-level positive and negative tests before support claims. |
| SDK or protocol changes break integrations | Pin exact versions, retain fixtures, test upgrades explicitly, and publish supported versions instead of following `latest`. |
| A translation changes execution, file, or timeout semantics | Compare client-observable results, including failure and streaming behavior; block the affected operation rather than approximate it silently. |
| Shared credentials or routing mistakes expose another sandbox | Bind resources and data-plane authorization to the authenticated scope; test conflicting route selectors, wrong tokens, and concurrent clients. |
| Adapter outages or retries leak resources or duplicate execution | Preserve resource identity, reconcile ambiguous outcomes, bound cleanup, and prohibit blind replay of create or command requests. |
| Volunteers become implicit permanent owners | Require explicit work-item and release ownership; no default deadline, SLA, automatic language expansion, or indefinite tracking of upstream E2B. |

## Design Details

### Compatibility Profiles

The following are proposed acceptance targets, not a current support matrix. Gate 0 must replace method groups with the exact methods, routes/RPCs, request options, response fields, errors, and transport behavior emitted by the pinned SDK.

| Surface | Initial target | Boundary |
|---|---|---|
| Lifecycle | Create, connect to a running sandbox, get info, list, set timeout, kill | Specify supported filters/pagination, metadata, readiness, and repeated-delete behavior. Connecting to a paused sandbox is unsupported initially. |
| Commands | Foreground execution, stdout/stderr streaming, exit status, command timeout | Working directory, environment, and execution user must be mapped or rejected explicitly. Background handles, reconnect, stdin, and cancellation need separate entries if used. |
| Files | Read/write and binary upload/download | Define paths, overwrite behavior, permissions, supported users, size bounds, and missing-file errors. Directory operations are included only with explicit tests. |
| Code interpreter | Separate conditional profile | Required if the chosen workflow calls `run_code`/`runCode`. Test kernel/session state, language selection, results, MIME payloads, errors, and interruption; shell execution is not an equivalent substitute. |
| Templates | Operator-managed alias to an approved OpenSandbox image, startup, and resource-limit configuration | No E2B template build compatibility. Unknown aliases are rejected; native template IDs are not assumed interchangeable. |
| Other features | Unsupported unless separately admitted | SDK defaults and implicit calls are part of the inventory; an unsupported implicit dependency blocks the workflow. |

A lifecycle-only prototype is a useful milestone but is not a completed sandbox profile. Passing the sandbox profile does not imply support for the code-interpreter profile or for every workflow in a framework.

The matrix must record an exact package lock and source revisions for the base SDK and any code-interpreter package, the selected framework/version/scenario, and the OpenSandbox deployment/image. It must list configuration changes and identify any dependency on deprecated endpoint overrides. The inspected [E2B Python connection configuration](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/python-sdk/e2b/connection_config.py) marks `api_url` and `sandbox_url` overrides deprecated for a future major version; they must not become an unbounded future compatibility promise.

### Control Plane and Resource Identity

The native [lifecycle contract](https://github.com/opensandbox-group/OpenSandbox/blob/fab314287bded086a5f76b2735935c7f484a0332/specs/sandbox-lifecycle.yml) remains authoritative for backend calls. Its API base is `/v1`; this must not be confused with the client-facing E2B API base.

| E2B operation group | Proposed backend action | Required checks |
|---|---|---|
| Create | Resolve an approved template alias, then create via native `/sandboxes` | Validate all options before allocation; wait for the SDK-required readiness within a bounded deadline. Record allocation early enough to reconcile failure. |
| Connect/get info | Resolve the binding and inspect the native sandbox | Check authenticated scope, actual state, and lifetime deadline; never report paused, failed, or TTL-stopping/terminated sandboxes as running. Apply any requested timeout according to the pinned contract. |
| List | List only resources belonging to the compatibility scope | Preserve supported filtering and pagination semantics; never expose unrelated native sandboxes. |
| Set timeout | Convert the accepted duration to native `/renew-expiration` | Validate units, finite limits, shortening/extension behavior, and timestamp semantics. Enforce adapter policy on renewal as well as creation. |
| Kill | Delete the bound native sandbox | Preserve the pinned client-visible deletion contract and revoke data-plane access at the adapter boundary; keep direct backend access blocked until sandbox termination and reconcile backend completion instead of assuming deletion succeeded. |

Keep a durable binding between compatibility resource identity, authenticated scope, native sandbox ID, selected template configuration revision/image digest, and compatibility profile. Store only adapter-owned metadata; backend state remains authoritative. Native IDs may be reused only if their format and exposure are compatible with the pinned client and authorization model. Alias updates affect future creates; existing bindings retain their original template identity.

The implementation design must choose persistence and concurrency handling before Gate 1 passes. It must cover restart recovery, create responses lost after allocation, expiry, stale records, repeated delete, and concurrent timeout/delete. If the native operation offers no idempotency guarantee, the adapter must not claim exactly-once create: reconcile known allocations, bound orphan cleanup, and expose ambiguous results without blindly allocating again.

### Execution and File Data Plane

The inspected [official Python SDK](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/python-sdk/e2b/sandbox_sync/main.py) creates separate clients for commands, filesystem, and PTY, using envd addressing and access-token metadata. OpenSandbox's [execd contract](https://github.com/opensandbox-group/OpenSandbox/blob/fab314287bded086a5f76b2735935c7f484a0332/specs/execd-api.yaml) has its own execution, file, and code APIs. A lifecycle field translation alone cannot connect these protocols.

The preferred prototype is a bridge from the selected E2B execution/file protocol subset to native execd. Gate 1 must inventory and implement the actual wire encoding and streaming envelopes used by the pinned clients, including any Connect/protobuf methods; an HTTP JSON facade with similar operation names is insufficient. It must preserve stdout/stderr separation, completion/error events, exit status, binary bytes, file semantics, and timeout units. Advertised envd version/capability fields must be backed by the tested subset and must not unlock unimplemented client behavior.

An alternative is an explicitly approved image containing a pinned envd implementation, with the service authorizing and routing to it. This may reuse more protocol behavior but adds image, process supervision, authentication, dependency/license, and update responsibilities. The prior PoC reported this approach. It must not bypass native network/security policy, expose envd directly, or become a mandatory dependency of ordinary OpenSandbox images.

If the selected workflow requires the code-interpreter profile, the comparison must also cover its separate code-execution service protocol and native execd code contexts. The inspected [official code-interpreter client](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/code-interpreter-python/e2b_code_interpreter/code_interpreter_sync.py) uses Jupyter `/execute` and `/contexts` endpoints; validating envd command/file access alone does not validate those operations.

These are alternatives for a bounded feasibility comparison, not two implementations promised by this OSEP. Gate 1 selects one based on executable SDK evidence and records the choice in this document before its status becomes `implementable`. If neither approach preserves the selected workflow's semantics within an acceptable maintenance scope, the work stops at a documented experiment or the scope is revised explicitly.

### Routing and Security

- Separate control-plane credentials from per-sandbox data-plane authorization. Define the concrete token format, expiry, revocation, and binding checks in the Gate 1 design. Do not return an OpenSandbox server key or reuse one unrestricted token for every sandbox.
- Bind any E2B-shaped sandbox ID, hostname, port, and routing header to the same authorized resource. Reject conflicting selectors. Headers such as `E2b-Sandbox-Id` are routing hints, never proof of ownership.
- Resolve backend addresses only through the configured native service and validated bindings, using native `GET /sandboxes/{sandboxId}/endpoints/{port}` where applicable. Preserve its required access headers and signed-route expiry; a signed ingress route does not replace execution-service authentication. Do not accept arbitrary client-supplied upstream URLs, forward client authorization blindly, or permit the gateway to become an SSRF/open-proxy endpoint.
- The deployment must prevent direct bypass of the gateway to backend execution endpoints. Strip external routing/credential headers before constructing trusted backend requests, and keep native credentials outside sandbox workloads.
- Use TLS for remote client traffic and validate upstream TLS. Describe control-plane and sandbox hostnames, DNS, certificates, proxy headers, and supported HTTP transport in the matrix. A custom API URL alone does not establish working data-plane routing.
- Initial data-plane routing is limited to the execution/file services required by the profile. User port URLs and browser access are separate capabilities; do not return an unusable or unauthenticated URL as success.
- Reuse native authorization boundaries where available. Where the backend credential represents an entire deployment, either restrict the compatibility deployment to one explicitly trusted scope or implement and test an independent authorization boundary before accepting mutually untrusted users. This proposal does not assert native per-user tenancy from the presence of an API key.
- Preserve operator-enforced resource/image limits and egress policy. E2B network options require explicit semantic mappings; unsupported non-default requests are rejected before creation rather than ignored or translated into broader access.

### Errors and Operational Behavior

The compatibility contract must specify lifecycle HTTP status/body mappings and data-plane protocol error mappings separately, verified through the actual SDK's exception behavior. An unsupported request returns a stable explicit error without side effects; its exact HTTP/RPC representation is selected from the pinned client's behavior, not a blanket assumption that every client handles HTTP 501 correctly. Do not expose native internal endpoints, credentials, or raw infrastructure errors.

Keep sandbox lifetime, command execution timeout, client request timeout, and stream idle timeout distinct. A nonzero process exit, a transport failure, and a sandbox expiry must not collapse into the same successful result. Disconnect handling, bounded buffering, backpressure, and cleanup must be specified for each supported stream. The service must not transparently replay a command after it may have executed.

Record profile/version, operation, result class, latency, and a safe correlation ID. Bound metric label cardinality; do not log API keys, access tokens, command output, file contents, or arbitrary environment/metadata by default. Readiness must fail when required backend connectivity or binding storage is unavailable. Document resource handling and stream termination during adapter shutdown; stopping the adapter must not disable the native API.

### Delivery Gates and Community Work

| Gate | Deliverable | Exit condition |
|---|---|---|
| 0 — Contract and workflow selection | Exact SDK/framework versions, minimal workflow, operation/options inventory, proposed compatibility matrix | Agreement on one bounded target is recorded in the OSEP PR or a linked tracking issue, with maintainer confirmation and explicit contributor opt-in; required code-interpreter or other dependencies are included or the target remains blocked. |
| 1 — Design and feasibility | Control/data-plane spike, chosen execution strategy, persistence/auth/routing design, dependency review, test fixtures | Unmodified pinned SDK demonstrates create → command → file round trip → delete against a real backend; design gaps and security requirements are resolved before marking the OSEP `implementable`. Add code execution if required by the selected target. |
| 2 — Experimental implementation | Small reviewable implementation PRs, deterministic tests, deployment example, negative tests, native regression results | All requirements for the chosen profile pass; at least one pinned real framework workflow passes. Publish evidence with limitations and an experimental label. |
| 3 — Supported release decision | Compatibility matrix, upgrade/deprecation policy, release artifact, explicit maintainer agreement | Maintainers accept ownership and support scope. Experimental success does not automatically establish GA status or an SLA. |

Contributors can volunteer for SDK/framework inventory, lifecycle translation and resource bookkeeping, execution/file bridging, code-interpreter compatibility, authentication/routing, or integration tests and documentation. Work items must identify their deliverable, dependencies, accepting reviewer, and test evidence before implementation. No participant is assigned by this proposal, and no single volunteer is assumed to maintain all profiles indefinitely.

The proposal author can coordinate discussion and the first contract inventory. Implementation ownership, schedules, review capacity, and release responsibility are accepted separately. The existing PoC contributor should be invited to share reusable code and tests with attribution and license review; access to that implementation is not assumed or a dependency of this proposal.

## Test Plan

These are future implementation acceptance criteria, not results of this documentation PR.

| ID | Evidence required |
|---|---|
| T1 — Contract mapping | Pinned SDK fixtures for every supported request/response/error, options/defaults, units, pagination/filtering, and template resolution; explicit rejection of unsupported options before backend calls. |
| T2 — Real SDK lifecycle | Create and readiness, reconnect to a running sandbox, get/list, shorten and extend timeout, expiry, kill/repeated kill, missing sandbox, and invalid credentials against a real OpenSandbox deployment. |
| T3 — Real data plane | Commands with interleaved stdout/stderr, nonzero exit, timeout and disconnect; text/binary file round trips, overwrite, missing paths, permissions, and configured bounds. Code-interpreter profile adds stateful multi-call execution, rich results, and execution errors. |
| T4 — Authorization | Two independent scopes/sandboxes in parallel; wrong/missing/revoked tokens; forged IDs, conflicting header/host selectors, forbidden ports, direct-backend bypass attempts, and unauthorized template/network options. No cross-sandbox output or file access. |
| T5 — Recovery | Adapter restart with existing sandboxes, storage/backend unavailability, lost create response, partial allocation, concurrent timeout/delete, expiry cleanup, and stream shutdown. Demonstrate no blind command replay and the documented orphan cleanup bound. |
| T6 — Framework integration | Reproduce the agreed workflow through the framework's normal E2B integration, pin its dependencies/configuration, and record the complete call path. No SDK fork or monkeypatch; any necessary framework change is separately declared and accepted before claiming configuration-only support. |
| T7 — Native regression and packaging | Native clients continue to work with the adapter both enabled and stopped; deployment example, TLS routing, image digests, dependency checks, and the runtime-specific matrix all match the tested release. |

Gate 0 defines concrete resource/stream limits and test expectations rather than arbitrary performance promises. CI should run contract tests on relevant changes and real SDK/runtime tests for every advertised profile before release. New profiles or SDK upgrades require new evidence; one passing language/runtime does not validate another. Performance measurements should describe the test setup and added overhead, not invent a latency SLA.

## Drawbacks

A separate service adds deployment, routing, persistence, credential management, and troubleshooting work. Protocol translation can be more expensive to maintain than native SDK integrations, especially when stream semantics and code-interpreter state differ. A deliberately limited compatibility profile can still be unsuitable for applications requiring unsupported features. The community may decide that native framework integrations are the better use of available maintainers.

## Alternatives

1. **Native OpenSandbox integration per framework.** Keeps native semantics and may be the simplest route for a small number of frameworks, but requires separate integration work and does not reuse an existing E2B client unchanged.
2. **An E2B-style SDK wrapper.** Can reduce migration edits but does not satisfy the unmodified official SDK/wire compatibility goal; it should be labeled a migration helper.
3. **Direct E2B routes in the core server.** Avoids a separate service but couples native releases and security surfaces to external protocol evolution. The optional service is preferred for the initial experiment; any later in-process placement needs an explicit design review.
4. **Pinned envd in an opt-in image.** A data-plane alternative described above, to be compared with execd translation during Gate 1. Reuse must account for supervision, image updates, authentication, and licensing.
5. **Migration documentation only.** Remains a valid outcome if a reliable profile cannot be maintained. Publish useful mappings and limitations without labeling them protocol compatibility.

## Infrastructure Needed

The initial experiment needs a reproducible local OpenSandbox environment, an approved image, locked official SDK dependencies, a minimal selected-framework fixture, and test credentials scoped to disposable sandboxes. A remote test deployment additionally needs controlled DNS/TLS routing and protected backend access. Persistence, packaging, and CI resources are specified by the Gate 1 design; no production deployment, cloud account, new public service, or recurring infrastructure budget is authorized by this OSEP.

The eventual compatibility documentation belongs under `docs/guides/`, architecture under `docs/architecture/`, and runnable examples under `examples/`. The OSEP remains the design/discussion record; it does not replace a maintained support matrix or executable contract tests. Native public contract additions, if proven necessary, require separate aligned spec/server/SDK review.

## Upgrade & Migration Strategy

Native OpenSandbox deployments require no migration. Operators opt into the compatibility component and approved template mappings only after selecting a tested profile. Applications retain the official E2B SDK package; endpoint/credential/template configuration and any deployment prerequisites are documented per profile. Existing E2B cloud sandboxes, templates, snapshots, or data are not automatically imported.

Releases publish their exact supported matrix. SDK updates are opt-in and pass the same gates; unknown versions are untested, not implicitly compatible. Where client version detection is available, use it for diagnostics without treating a spoofable version header as authorization. If a future SDK removes needed endpoint customization or changes semantics, retain the documented supported version until an upgrade path is validated; do not promise automatic forward compatibility.

Before any supported release, agree on maintenance ownership, removal notice, and a rollback procedure covering adapter storage schema and live resource bindings. Rollback stops new compatibility allocations and drains or explicitly terminates affected sessions according to the documented policy, while preserving native API access and a cleanup inventory. Never downgrade binding state blindly or imply that removing the adapter migrates applications back to E2B.

## References

- [Issue #800: E2B interface compatibility request](https://github.com/opensandbox-group/OpenSandbox/issues/800), including the maintainer decision and subsequent community PoC report.
- [OpenSandbox lifecycle API at the proposal's inspected baseline](https://github.com/opensandbox-group/OpenSandbox/blob/fab314287bded086a5f76b2735935c7f484a0332/specs/sandbox-lifecycle.yml).
- [OpenSandbox execd API at the same baseline](https://github.com/opensandbox-group/OpenSandbox/blob/fab314287bded086a5f76b2735935c7f484a0332/specs/execd-api.yaml).
- [Official E2B Python SDK connection configuration](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/python-sdk/e2b/connection_config.py) and [sandbox implementation](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/python-sdk/e2b/sandbox_sync/main.py).
- [Official E2B JavaScript SDK connection configuration](https://github.com/e2b-dev/E2B/blob/9301cd84757b0e3f3fccbd349e85ff2bd403a1f4/packages/js-sdk/src/connectionConfig.ts).

The source revisions above were inspected to identify protocol boundaries. They are research references, not a tested SDK support baseline; Gate 0 must select released packages and Gate 2 must establish compatibility evidence.
