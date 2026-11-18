---
title: Native Kubernetes Jobs and CronJobs
authors:
  - "@zonemeen"
creation-date: 2026-11-16
last-updated: 2026-11-16
status: draft
---

# OSEP-0028: Native Kubernetes Jobs and CronJobs

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

Add an opt-in HTTP API for native Kubernetes `batch/v1` Jobs and CronJobs. Trusted application backends can submit finite workloads and recurring schedules through OpenSandbox while Kubernetes owns execution, retries, scheduling, and cleanup. The existing sandbox lifecycle API and BatchSandbox controller are unchanged.

## Motivation

Applications using OpenSandbox may also need finite background workloads. Running a command in a sandbox does not create a native Job, expose Job completion conditions, or provide CronJob scheduling semantics. Today, those applications need a separate Kubernetes client and credentials.

One motivating use case is archiving a workspace after a sandbox session: mount an existing PVC subdirectory read-only, create and upload an archive, and retrieve a small result before deleting the Job. This is an on-demand Job; it does not require a schedule. CronJobs address separate recurring maintenance or processing workloads that can reuse the same Job template.

### Goals

- Create, list, inspect, and delete native Jobs and CronJobs.
- Update CronJob schedules and suspend or resume future executions.
- Expose native completion/failure conditions, child Jobs, Pod termination results, and bounded logs.
- Reuse API authentication and tenant namespaces, with explicit ownership checks and namespace-scoped batch RBAC.

### Non-Goals

- A new scheduler, controller, CRD, or database-backed lifecycle state machine.
- Docker emulation, workflow DAGs, exactly-once execution, or managing arbitrary existing cluster resources.
- Automatic injection of execd, sandbox network policies, or sandbox runtime settings into native Pods.
- Application-specific archiving logic, object storage management, SDK bindings, or CLI commands in the initial HTTP extension.

## Requirements

- Default the feature off and require API key authentication or a tenant provider.
- Restrict submission to the authenticated tenant's namespace, or the configured Kubernetes namespace in single-tenant deployments.
- Keep request envelopes explicit and pass native specs to Kubernetes for strict validation.
- Keep the API additive and publish its contract alongside the implementation.

## Proposal

Expose `/v1/kubernetes/jobs` and `/v1/kubernetes/cronjobs` as a Kubernetes-only extension, enabled by `kubernetes.batch_api_enabled = true`. Requests contain native `apiVersion`, `kind`, `metadata`, and `spec` fields. Kubernetes remains the source of truth; responses include resource metadata and native status.

Job submission and CronJob scheduling share namespace selection, ownership, validation, and diagnostic behavior. CronJob management adds only native resource operations and Job-template ownership stamping; it does not add a server-side timer.

### Notes/Constraints/Caveats

These are trusted workload-submission APIs. Namespace selection and ownership labels prevent accidental cross-tenant management through these endpoints; they do not constrain what a Pod template can execute or access. Operators must apply appropriate admission policies, Pod Security Admission, quotas, ServiceAccount restrictions, and storage policy before enabling the API. Existing sandbox isolation settings do not apply automatically.

Native CronJobs may miss or repeat an execution, and suspending them does not stop existing Jobs. Tasks must be idempotent. Time-zone support requires Kubernetes 1.27 or later; strict field validation requires a compatible Kubernetes API server.

### Risks and Mitigations

- Privilege escalation through arbitrary Pod templates: keep the feature disabled by default, require authentication, document the trusted-caller scope, and delegate workload restrictions to Kubernetes admission.
- Access to unrelated resources: reserve ownership labels, check labels on reads/mutations, and verify controller owner UIDs for child Jobs and Pod diagnostics.
- Name reuse between authorization and mutation: use UID preconditions on deletion and resourceVersion plus UID on CronJob replacement.
- Secret leakage in errors or diagnostics: do not relay Kubernetes error bodies; diagnostics are available only to the resource owner. Callers remain responsible for excluding credentials from logs and termination messages.
- Resource retention and API load: Kubernetes controls deadlines, retries, TTL, and history limits; use the existing client rate limiter and bounded log requests.

## Design Details

The standalone contract is [specs/kubernetes-batch-api.yml](../specs/kubernetes-batch-api.yml). Thin FastAPI routes call a Kubernetes batch service through the existing client wrapper and Kubernetes Python dependency.

| Operation | Job | CronJob |
|---|---|---|
| Create/list | `POST` / `GET /v1/kubernetes/jobs` | `POST` / `GET /v1/kubernetes/cronjobs` |
| Inspect/delete | `GET` / `DELETE /v1/kubernetes/jobs/{name}` | `GET` / `DELETE /v1/kubernetes/cronjobs/{name}` |
| Replace desired spec | Not exposed | `PUT /v1/kubernetes/cronjobs/{name}` |
| Diagnostics/children | `GET /jobs/{name}/pods` and `/jobs/{name}/pods/{pod_name}/logs`, under `/v1/kubernetes` | `GET /v1/kubernetes/cronjobs/{name}/jobs` |

Top-level request metadata accepts only name, namespace, labels, and annotations. The server sets `opensandbox.io/batch-managed-by=server` and `opensandbox.io/tenant` (empty for single-tenant mode), including on CronJob Job templates. Callers cannot supply these labels or resource owner references. Kubernetes validates `spec` with `fieldValidation=Strict`.

Create returns `201`; callers poll native Job `Complete` / `Failed` conditions. Stable names allow recovery after an uncertain create response; duplicate names return `409`. CronJob replacement requires a full desired request, including schedule and Job template; `spec.suspend` pauses or resumes scheduling. Existing executions retain their original spec. Delete requests background cascading cleanup and returns `202`.

Pod diagnostics verify the current Job UID and return native container termination information. Logs are non-streaming and bounded by `tailLines` and `limitBytes`. An archiver can place a small JSON result in its configured `terminationMessagePath`; consumers must persist it before TTL or history cleanup removes the Pod.

The server Helm chart adds `batchApi.namespaces`, defaulting to an empty list. It creates batch Roles and RoleBindings only in those existing namespaces. This is independent of enabling the API in TOML. No new CRDs, controller changes, dependencies, or persistence schema are needed.

## Test Plan

- Route/service tests using the real request validation and SDK dispatch, mocking only Kubernetes I/O: feature gate, authentication, namespaces, labels, owner UIDs, replacement/deletion preconditions, bounded logs, and error mapping.
- Contract checks for paths, operation IDs, success responses, and request envelopes.
- Real-cluster smoke tests: successful and failed Jobs, deadline expiry, strict spec rejection, duplicate create recovery, logs/results, actual CronJob clock scheduling, suspend/resume, child Job visibility, and cascading deletion.
- PVC archiving verification using disposable data and a temporary namespace.
- Existing server tests, type checks, coverage, docs build, and Helm rendering with the feature both disabled and configured.

## Drawbacks

The extension is Kubernetes-specific and adds a trusted control-plane capability beyond interactive sandbox management. Native manifests expose Kubernetes-version-dependent fields, and initial clients must use HTTP. Completed resources and logs are not an archival store; applications must retain results themselves.

## Alternatives

- Direct Kubernetes access from the application: already possible, but requires distributing and managing a separate Kubernetes credential and client integration.
- Sandbox command execution: suitable for work inside an active sandbox, but does not provide native Job or CronJob resources.
- A new batch CRD/controller or scheduler in OpenSandbox: duplicates Kubernetes lifecycle and scheduling behavior without being needed for these use cases.
- Job-only first iteration: reduces initial review scope. CronJob support can be reviewed separately on top of the shared Job functionality if maintainers prefer staged delivery.

## Infrastructure Needed

A Kubernetes 1.27+ cluster supporting strict field validation and CronJob time zones, authenticated OpenSandbox Server access, permitted workload namespaces, and the documented RBAC. The local validation target is Docker Desktop Kubernetes. No new external service is required by the feature.

## Upgrade & Migration Strategy

Existing deployments retain their behavior because the feature and additional RBAC are off by default. Operators explicitly enable the TOML flag and grant permissions in selected namespaces. Disabling the API stops access through OpenSandbox but does not stop existing native Jobs or CronJobs; operators should suspend or remove schedules before disabling it. Existing sandbox SDK generation inputs are unchanged.
