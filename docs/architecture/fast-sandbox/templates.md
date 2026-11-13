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

- **Guest wiring is injected**: an init path (PID 1, carrying the readiness marker and heartbeat) and the execd runtime files always come from the template — every sandbox built from it is execd-ready by construction. With the server's `runtime.execd_run_as_init` enabled, the server bakes `EXECD_INIT=1` into the template envs, so the baked `bootstrap.sh` execs into `execd --init` and execd becomes the root of the workload tree as a subreaper under the guest init (child reaping, signal forwarding, and the optional hardening floor; the PID 1 signal shield stays with the guest init).
- **Hardening floor delivery (OSEP-0018)**: the floor arms only when execd finds an isolation TOML through `EXECD_ISOLATION_CONFIG` and the `opensandbox-launcher` prelude in the guest. fsb has no ConfigMap/bind equivalent, so the delivery route is the golden image itself: the source image carries both at execd's trusted runtime paths (`/opt/opensandbox/isolation.toml`, `/opt/opensandbox/opensandbox-launcher` — the fsb builder stages only `execd`/`bootstrap.sh`/`prepare.sh`/`bwrap` from the execd image), and a template opts in per build with an `EXECD_ISOLATION_CONFIG` env pointing at the baked path. Without that env the floor stays disabled and the capabilities report says so. The fast-sandbox-env integration environment wires and pins this end to end (`scripts/fast-sandbox-env/verify/hardening.sh`, `verify-overlaybd` lane: the lane builds + pushes the hardened golden image from its own checkout and the stage pins the cap-drop/seccomp/no-new-privs floor, the credential env strip, and the capabilities report). Verified-on-fsb reality: the current fsb guest kernel reports **Landlock ABI 0**, so that layer reports `unsupported` with the kernel reason and the floor runs without filesystem confinement (the fail-open contract) — upgrading the guest kernel's `CONFIG_SECURITY_LANDLOCK` is the fsb-side follow-up to enable the Landlock layer. The long-term home for the launcher/TOML staging is builder-side in fast-sandbox, which would make the floor available to every golden image without a source-image bake.
- **Business entrypoint and envs** are baked in: an empty entrypoint defaults to a keep-alive; envs are injected literally into the guest (no runtime indirection).
- **Runtime shape is declared**: the template names the Firecracker guest kernel and reference machine; readiness defines when the guest counts as up.
- **Output is content-addressed**: the build publishes artifacts to the store and records a manifest reference plus artifact digest in the CR status — builds are immutable and verifiable, never mutated in place.

Native templates publish to S3-compatible storage and resolve through the per-template image index. OverlayBD templates publish paired rootfs and snapshot images to an OCI registry; the catalog records the immutable OCI manifest reference, which becomes the runtime image at sandbox creation. Build credentials are selected by destination through server configuration. See [OCI template setup](/guides/oci-templates) for deployment and usage.

## Using templates

Sandbox creation with a `templateId` resolves to the recorded artifact; capacity comes from the referenced pool, not from the template. On a `ResourceAware` pool, the server forwards the template's recorded machine shape as the sandbox's per-sandbox resource limits (the pool's overcommit ratios still own request derivation), so the template — not only the pool defaults — shapes the sandbox; on a `LegacySlot` pool the pool's fixed shape applies. Because the template pins the workload, per-sandbox behavior is limited to intent: identity, TTL, metadata, network policy, and an optional pool reference (see [Scheduling](/architecture/fast-sandbox/scheduling)). Updating a template rebuilds the artifact and yields a new digest — existing sandboxes keep running their admitted revision; new creations pick up the new build.
