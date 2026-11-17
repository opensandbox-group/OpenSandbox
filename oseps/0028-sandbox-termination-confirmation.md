---
title: Sandbox Termination Confirmation
authors:
  - "@AdityaxDeore"
creation-date: 2026-11-11
last-updated: 2026-11-11
status: draft
---

# OSEP-0028: Sandbox Termination Confirmation

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
- [Test Plan](#test-plan)
- [Drawbacks](#drawbacks)
- [Alternatives](#alternatives)
- [Infrastructure Needed](#infrastructure-needed)
- [Upgrade & Migration Strategy](#upgrade--migration-strategy)
<!-- /toc -->

## Summary

This proposal adds an opt-in, bounded wait on sandbox deletion so a client can distinguish deletion accepted from sandbox execution confirmed terminated, without changing the default DELETE semantics. The contract is `DELETE /v1/sandboxes/{id}?wait=termination&timeout=<seconds>` (default off): 204 with `X-Sandbox-Termination: terminated` when termination is confirmed, 202 with `X-Sandbox-Termination: unknown` when the wait times out. This is a draft proposal for discussion: the direction is not yet maintainer-approved, and the design it builds on was offered by contributor kittimzhe as a concrete sketch on issue #2396 (2026-11-01) as discussion input, not as an accepted specification. The current boundary (204 means accepted, a later 404 does not prove termination) is documented explicitly in PR #2738.

## Motivation

Today a client that deletes a sandbox gets a 204 once deletion is accepted, and a later 404 for the control object only means the control object is gone. On the Kubernetes BatchSandbox backend the finalizer and deletion propagation run asynchronously after the delete request is accepted, so the workload may still be executing while the client believes cleanup is done. Evaluation runners that execute independent attempts under a concurrency limit need to know when a timed-out or cancelled attempt's workload has actually stopped before they release the attempt's concurrency slot and start the next attempt; otherwise actual resource usage can temporarily exceed the runner's accounting and the caller cannot say when the previous workload stopped. The request comes from issue #2396, filed after downstream NiceEval integration work, and the issue states explicitly that this is a low-priority lifecycle observability improvement, not a blocker, and that normal runs clean up eventually. Documenting the current boundary explicitly helps clients avoid treating acceptance as completion, but it does not give them a way to confirm termination; this proposal adds that way as an opt-in.

### Goals

1. Give clients an explicit, documented way to distinguish deletion accepted from execution confirmed terminated.
2. Keep the default DELETE behavior exactly as it is today (asynchronous acceptance, 204).
3. Make timeout and observation failure report `unknown`, never success: uncertainty must stay uncertainty.
4. Define per-backend what "terminated" can honestly mean, and do not promise more than a backend can observe.
5. Keep the contract backward compatible: existing clients that do not request a wait see no change.
6. Cover the retry case: a client can re-issue the same DELETE after the control object disappears and still get a meaningful answer.

### Non-Goals

1. No mandatory wait on DELETE and no change to the default 204 semantics.
2. No guarantee that storage, sidecar, volume, or metadata cleanup has finished; the contract covers runtime execution termination only.
3. No proof that a physical VM is gone where the backend cannot observe that fact; "terminated" means no observable runtime artifacts at poll time.
4. No new asynchronous operation resource, operation-id system, or persistent deletion-job state.
5. No fast-sandbox confirmation until a Fastlet-side termination signal is agreed with maintainers.

## Requirements

1. The wait is opt-in per request (`wait=termination`), default off; without it, DELETE behaves exactly as today.
2. The wait is bounded by a client-supplied `timeout` in seconds with a server-enforced maximum; there is no unbounded server-side wait.
3. Confirmed termination is `204` with `X-Sandbox-Termination: terminated`; timeout is `202` with `X-Sandbox-Termination: unknown`.
4. On Kubernetes, "terminated" requires both the BatchSandbox control object to be absent AND no pods matching the sandbox's label selector to be observable; control-object absence alone never counts as confirmation.
5. On Docker, the existing synchronous container kill/remove already means the container is gone when DELETE returns, so a wait degenerates to an immediate confirm.
6. On fast-sandbox, the wait returns `unknown` until a Fastlet-side termination signal is agreed and implemented.
7. Retries are idempotent: re-issuing DELETE after the control object disappears must still perform the runtime observation rather than erroring or fabricating confirmation.
8. The existing deletion propagation selection (Background when a taskTemplate is present, Foreground otherwise) is preserved.

## Proposal

Add an optional bounded wait to the existing DELETE endpoint, following the design sketch contributor kittimzhe posted on issue #2396 on 2026-11-01, which this proposal adopts as its starting point for discussion, not as an approved specification. `DELETE /v1/sandboxes/{id}?wait=termination&timeout=<seconds>` keeps today's behavior when the parameters are absent. When `wait=termination` is given, the server submits the deletion as today, then polls the backend's runtime-observation path until either termination is confirmed or the timeout expires. Confirmation returns 204 with `X-Sandbox-Termination: terminated`; expiry returns 202 with `X-Sandbox-Termination: unknown`, and the client may re-issue the same DELETE or fall back to polling GET (Stopping, then 404, as documented today). "Terminated" is defined as no observable runtime artifacts at poll time, and the API documentation must say exactly that. SDKs expose the option as `sandbox.delete(wait=False, timeout=60)` (the Daytona shape: one parameter pair, no second endpoint), with the result distinguishing accepted, terminated, and unknown. Delivery is split per backend, Kubernetes first, because it is the only backend whose 204 is acceptance-only today; Docker and fast-sandbox follow with their own honest semantics rather than a uniform guarantee.

### Notes/Constraints/Caveats

- The retry-after-404 path is the hardest part of this design and is deliberately left open for maintainer input: the current `BatchSandboxProvider.delete_workload()` starts with `get_workload()`, which raises when the BatchSandbox cannot be found, so a confirmation implementation cannot simply reuse that method for retries after the control object disappears. It needs a way to identify and query the sandbox's runtime artifacts independently of the control-object lookup (pod selector construction, sandbox label conventions, namespace handling), and no verified selector that survives control-object deletion has been established in this proposal; do not invent one in implementation.
- The fast-sandbox observation signal is unresolved: candidate signals are a Fastlet Get returning gone after invalidation or a dedicated status field, and neither is established or approved.
- The `timeout` upper bound, the poll interval, and the default `timeout=60` value are proposed starting points for discussion, not decided values.
- This proposal does not change the DELETE route's existing Stopping-then-404 GET behavior or the 404 meaning for a never-existent sandbox.

### Risks and Mitigations

- Control-object 404 mistaken for termination: mitigated by requiring the runtime-artifact observation in addition to control-object absence on Kubernetes; the contract and the tests must cover the case where the control object is gone but pods remain (answer: unknown/pending, never terminated).
- Timeout converted into success: mitigated by the 202 + `unknown` header and by SDKs surfacing unknown as a distinct outcome that callers cannot accidentally treat as success.
- Overclaiming physical termination: mitigated by defining "terminated" as no observable runtime artifacts at poll time and documenting that a Kubernetes API observation is not an independent physical-host observation; Kata VM termination specifically requires a live-cluster integration test and is not claimed from mocked tests.
- Retry after control-object disappearance failing or fabricating: mitigated by the CR-independent observation path (open design point above) and by tests for retries in both states.
- Unbounded waits and polling load: mitigated by a server-enforced timeout maximum and a bounded poll interval; cancellation of a waiting request must not fabricate confirmation.
- Backend unevenness confusing callers: mitigated by the per-backend guarantee matrix in the API documentation and by SDKs exposing the same three outcomes on every backend.

## Design Details

- API: `DELETE /v1/sandboxes/{id}?wait=termination&timeout=<seconds>` in `server/opensandbox_server/api/lifecycle.py`; the existing 204/404/409/500 responses are unchanged, and the new behavior adds the 202 + `X-Sandbox-Termination` header outcomes. The route stays thin; the waiting logic lives in the provider/service layer.
- Kubernetes (`server/opensandbox_server/services/k8s/batchsandbox_provider.py`): a bounded termination-confirmation path that observes the BatchSandbox control object and the pods for the sandbox's label selector independently, polling until both are absent (confirmed) or the deadline (unknown). Deletion propagation selection is unchanged. Observation failures are reported as unknown/error, never as confirmation.
- Docker (`server/opensandbox_server/services/docker/docker_service.py`): `_delete_sandbox_locked` already kills and removes the container synchronously before returning, so a wait on this backend confirms immediately; storage, sidecar, volume, and metadata cleanup still trail container removal and are not part of the termination guarantee.
- Fast-sandbox (`server/opensandbox_server/services/fast_sandbox/service.py`): returns unknown until the Fastlet-side termination signal is agreed; no assumed signal is implemented.
- SDKs: `sandbox.delete(wait=False, timeout=60)` in the sandbox SDKs, with a typed result distinguishing accepted, terminated, and unknown; existing callers that do not pass `wait` see no change.
- Documentation: the API reference and the SDK docs state the per-backend guarantee matrix (Docker: container removed, other cleanup may continue; Kubernetes: control object absent and no matching runtime pods observable, does not establish that every host-level cleanup operation has completed; fast-sandbox: awaiting an agreed Fastlet observation) and the retry behavior, including after the control object disappears.

## Test Plan

- Mocked Kubernetes provider tests in `server/tests/k8s/test_batchsandbox_provider.py`, extending the existing mocked-client suite: immediate termination (control object absent, no matching pods) confirms; normal asynchronous deletion (control object then pods disappear) confirms; control object disappears first with pods still present stays unknown/pending; slow pod termination stays unknown until timeout; Kubernetes API failure on pod-list is unknown/error; delete accepted with artifacts remaining is not confirmed; retry after control-object disappearance with pods remaining is still pending and with pods absent is confirmed; cancellation/timeout at the deadline is unknown; unrelated pods in the namespace are not mistaken for sandbox runtime (wrong-selector test); repeated DELETE keeps its documented behavior; Background and Foreground propagation selection is preserved.
- API lifecycle tests for `delete_sandbox`: default DELETE unchanged without the wait parameters; confirmed termination returns 204 with the terminated header; timeout returns 202 with the unknown header; Kubernetes API errors never become false confirmations; the existing Stopping-then-404 behavior remains valid; an already-absent control object does not prevent runtime-artifact verification.
- SDK tests: default deletion remains backward compatible; the optional wait is serialized correctly; timeout is represented as unknown; headers and statuses are interpreted correctly.
- Integration tests that need live infrastructure and must run in CI, not on a developer VM: actual Kubernetes deletion with a live cluster and installed BatchSandbox CRDs/controller; Kata VM termination; Docker kill/remove behavior with a Docker daemon; Fastlet termination once its signal is agreed. Mocked tests establish state-transition handling only; they cannot establish that a real Kata VM has terminated.

## Drawbacks

- A bounded server-side wait holds a request open for up to the timeout, which adds server-side polling load and long-lived requests for clients that ask for it; the default stays off, so only opting-in clients pay this.
- The guarantee is uneven across backends by design (Docker confirms immediately, Kubernetes waits on two observations, fast-sandbox is unknown until its signal is agreed), which callers must understand; the documentation matrix exists to make that explicit rather than hidden.
- The retry-after-404 observation path is new surface that needs a maintainer decision on the selector; until that is settled, the design is incomplete in exactly the place the issue's acceptance scenario 5 exercises.

## Alternatives

- A separate queryable operation (`POST …/wait-termination`, or an operation-id resource): rejected for this proposal because the operation would have to outlive the sandbox's own control object (the 404-after-accept case), which means new server state to garbage-collect; binding the wait to the DELETE call that triggered it avoids that entirely, and the header keeps the response cache-safe for the no-wait default.
- Polling GET until Stopping becomes 404: insufficient on its own, because a 404 for the control object is acceptance-level and does not establish runtime termination; polling remains a valid fallback for clients that do not want the wait, but it cannot provide the confirmation this proposal adds.
- Mandatory waiting on every DELETE: rejected; it would slow the common path and change the existing contract for all clients.
- Reporting timeout as success (204 on expiry): rejected; a timeout means the requested confirmation was not obtained within the deadline, and converting it to success would fabricate a guarantee.

## Infrastructure Needed

None. The proposal uses the existing Kubernetes client, the existing Docker client, and the existing Fastlet integration points; no new services, storage, or third-party dependencies are required.

## Upgrade & Migration Strategy

Fully backward compatible and additive: the wait parameters are optional and default off, the existing DELETE behavior, status codes, and SDK signatures are unchanged for clients that do not opt in, and no migration is needed for existing deployments. Operators need no configuration change; the server-enforced timeout maximum is a new server-side default that can be tuned later if maintainers want it configurable.
