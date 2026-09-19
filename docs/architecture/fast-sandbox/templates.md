---
title: Fast Sandbox Templates
description: Template-backed sandboxes — the server-side catalog, SandboxTemplate golden-image builds, and content-addressed artifact publication.
---

# Templates

Templates are how Fast Sandbox fixes a workload before any request exists: runtime, image, entrypoint, and resources are decided at build time, so the create API only needs to express intent — which template, whose sandbox, for how long. `templateId` is mutually exclusive with `image`/`snapshotId`, and per-request workload fields (entrypoint, env, resources, volumes) are rejected.

## Two layers, one template

A template exists in two places, on purpose:

- **Server-persisted metadata** (SQLite/PostgreSQL) is the source of truth for template definitions and tenant ownership. Templates are tenant-private; the catalog is served through the same server that serves sandboxes, so SDKs manage templates with the same authentication and tenancy as everything else.
- **`SandboxTemplate` CRs** drive the actual build on the fast-sandbox side. The server persists the definition and creates the CR; the platform's builder turns it into a golden-image artifact asynchronously. Template state converges from CR observations — the same pattern as sandbox state.

Builds are asynchronous by design: the API returns an accepted state immediately, and the template becomes usable when the build reports readiness.

## What a build produces

A `SandboxTemplate` describes a conversion, not just a copy: an ordinary OCI image plus the OpenSandbox guest wiring, compiled into an immutable golden image.

- **Guest wiring is injected**: an init path (PID 1, carrying the readiness marker and heartbeat) and the execd runtime files always come from the template — every sandbox built from it is execd-ready by construction.
- **Business entrypoint and envs** are baked in: an empty entrypoint defaults to a keep-alive; envs are injected literally into the guest (no runtime indirection).
- **Runtime shape is declared**: for microVM runtimes the template names the guest kernel and reference machine; readiness defines when the guest counts as up.
- **Output is content-addressed**: the build publishes artifacts to the store and records a manifest reference plus artifact digest in the CR status — builds are immutable and verifiable, never mutated in place.

## Using templates

Sandbox creation with a `templateId` resolves to the recorded artifact; capacity comes from the referenced pool, not from the template. Because the template pins the workload, per-sandbox behavior is limited to intent: identity, TTL, metadata, network policy, and an optional pool reference (see [Scheduling](/architecture/fast-sandbox/scheduling)). Updating a template rebuilds the artifact and yields a new digest — existing sandboxes keep running their admitted revision; new creations pick up the new build.
