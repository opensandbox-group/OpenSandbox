---
title: Fast Sandbox Templates
description: Template-backed sandboxes — the server-side catalog, SandboxTemplate golden-image builds, and content-addressed artifact publication.
---

# Templates

Templates are how Fast Sandbox fixes a workload before any request exists: runtime, image, entrypoint, and resources are decided at build time, so the create API only needs to express intent — which template, whose sandbox, for how long. `templateId` is mutually exclusive with `image`/`snapshotId`, and per-request workload fields (entrypoint, resources, volumes) are rejected. Non-empty per-create `env` passes validation only when the selected pool declares a `runtime-init` action handler; otherwise the server returns `400 INVALID_PARAMETER`. Empty or omitted env needs no such capability unless server-managed execd authentication is configured.

::: warning Template runtime initialization
Template creates with non-empty `env` or server-managed execd authentication attach a `runtime-init` binding after egress. The pool must declare `runtime-init` on `sandbox.data-plane-ready`, using near-end `curl` with the payload on stdin, and the guest must already support `/internal/init`. Missing pool capability returns HTTP 400, including empty-env creates when authentication is configured. Without authentication, empty or omitted env preserves the existing behavior without a binding. High-level SDK template-create APIs accept optional per-create env; pool capability validation remains server-side.

The binding uses the new sandbox's `request_id`, `generation: 1`, `entrypointPolicy: "keep"`, `preserveRuntimeState: false`, and the requested env. `keep` does not restart the prewarmed entrypoint: env applies only to new commands/sessions subsequently started by execd, not existing processes. Full initialization with `preserveRuntimeState: false` resets old sessions. Deferred-template classification/startup and image/snapshot paths are unchanged; KVM E2E acceptance is not claimed.
:::

When `[execd].auth_signing_key_file` is configured, each template create mints a new token verifier once and persists it in the same FastPath create intent: `accessTokenHash` contains `sha256:<digest>` for guest authentication, while `accessTokenVerifier` contains `sha256:<kid>:<nonce>:<digest>` for server-side token derivation. Ordinary binding input contains no plaintext token. `GetEndpoint(44772)` reads that binding in the current namespace and returns `X-EXECD-ACCESS-TOKEN` alongside any ingress routing header; other ports receive no execd credential. Existing env-only bindings remain unauthenticated. A token-bearing binding with missing signing configuration, an unavailable key, or inconsistent verifier metadata fails closed. Server proxy access to execd is unavailable when signing is configured.

Pause/resume retains the same sandbox identity, persisted verifier, and guest-memory token; the server sends only the resume intent and does not mint or rebind a token. Retain old signing keys for the lifetime of their sandboxes. This does not implement token rotation for snapshot-derived new sandboxes.

The near-end handler still requires `EXECD_INIT_AUTH=false`: enabling init authentication also requires credential-aware retries after an initialization response is lost and recovery handling for interrupted initialization. This is distinct from authentication of ordinary execd APIs, which becomes active after the token hash is bound. Keep `/internal/init` inaccessible through external ingress and do not expose the sandbox before initialization completes. Deployment configuration and the fast-sandbox pin are not changed automatically.

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

Sandbox creation with a `templateId` resolves to the recorded artifact; capacity comes from the referenced pool, not from the template. On a `ResourceAware` pool, the server forwards the template's recorded machine shape as the sandbox's per-sandbox resource limits (the pool's overcommit ratios still own request derivation), so the template — not only the pool defaults — shapes the sandbox; on a `LegacySlot` pool the pool's fixed shape applies. Because the template pins the workload, per-sandbox behavior is limited to intent: identity, TTL, metadata, network policy, optional runtime env on capable pools, and an optional pool reference (see [Scheduling](/architecture/fast-sandbox/scheduling)). Updating a template rebuilds the artifact and yields a new digest — existing sandboxes keep running their admitted revision; new creations pick up the new build.
