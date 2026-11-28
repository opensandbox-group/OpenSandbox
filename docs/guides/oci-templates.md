---
title: Publish Templates to an OCI Registry
description: Build Golden Image templates in an OCI registry and create Fast Sandbox instances from immutable artifacts.
---

# Publish templates to an OCI registry

`osb template create` can publish Golden Image artifacts to an OCI registry. Set `--publish` to a repository-only address such as `oci://registry.example.com/sandbox/templates`; omit tags and digests. The builder generates a UUID for each build and publishes paired rootfs and snapshot images. Once the build succeeds, `status.manifestRef` contains the immutable `oci://registry.example.com/sandbox/templates@sha256:...` reference used to restore that build.

## Prepare the runtime

Use the fast-sandbox companion images built from the source pinned in `manifests/third-party/fast-sandbox.commit`. OCI templates require the OverlayBD node daemon, shared state and device mounts, and `FAST_SANDBOX_OCI_ENABLED=true` on the runtime agent, node daemon, and Fastlet pool. Enable `runtime.oci.enabled` in the fast-sandbox Helm chart to install the node OCI stack. Follow the upstream [OverlayBD OCI deployment guide](https://github.com/opensandbox-group/fast-sandbox/blob/4c389ae1b0e3823d59eee2ade4fba36682984590/docs/guides/overlaybd-oci-sandboxes.md) to configure the pool and its registry credentials.

Before using template or Full/RootfsOnly snapshot creation, upgrade the pool's runtime-init handler support and ensure the guest supports `/internal/init`; configure the handler on `sandbox.data-plane-ready` to read the payload from stdin. Every such create binds the new sandbox ID, even without env or signing. This increment does not change `manifests/third-party/fast-sandbox.commit` or deployment configuration: current deployments must prepare these prerequisites themselves rather than assuming the existing pin supplies them.

## Configure build credentials

Create a Docker Registry Secret in each template namespace. Set `REGISTRY_USER` and `REGISTRY_PASSWORD` to an account with push and pull access:

```bash
kubectl -n <template-namespace> create secret docker-registry sandbox-registry-credentials \
  --docker-server=registry.example.com \
  --docker-username="$REGISTRY_USER" \
  --docker-password="$REGISTRY_PASSWORD"
```

Configure the server to use this Secret; OCI publication requires an explicit Secret name:

```toml
[kubernetes]
template_oci_publish_secret = "sandbox-registry-credentials"
```

The Secret must contain `.dockerconfigjson`, including credentials for private source and execd images when needed. Anonymous access can use a Secret containing `{"auths":{}}`. Registry authentication supports anonymous or username/password credentials; identity-token-only credentials are unsupported. Configure the pool's registry credentials separately as described in the upstream deployment guide; the build Secret is not copied into running sandboxes.

## Build and use a template

```bash
osb template create \
  --image python:3.12 \
  --publish oci://registry.example.com/sandbox/templates \
  --format overlaybd \
  -o json

osb template get <template-id> -o json
```

Wait for `status.phase` to become `Succeeded`, then create a sandbox:

```bash
osb sandbox create --template <template-id> --timeout 30m -o json
```

The server resolves the template ID to the recorded immutable OCI reference, so builds from the same source image remain independent. A repository-only publish target is a build destination; it is not a runnable artifact reference.

### Per-create environment variables

The Python (async/sync), JavaScript/TypeScript, Go, Kotlin, and C# SDK template-create APIs accept optional environment variables for new commands and sessions:

```python
from datetime import timedelta
from opensandbox import Sandbox

sandbox = await Sandbox.create_from_template(
    "<template-id>",
    timeout=timedelta(minutes=30),
    env={"APP_ENV": "production"},
)
```

Every template create requires the selected pool to declare a `runtime-init` handler on `sandbox.data-plane-ready` and the template guest to support `/internal/init`. The binding always follows egress, uses `entrypointPolicy: "keep"` and `preserveRuntimeState: false`, and sends `envs: request.env or {}`. Missing or unknown pool capability, including after a pool-read RBAC `403`, returns HTTP 400; there is no legacy or empty-env bypass. The SDK forwards the server error rather than checking pool capabilities itself.

With `[execd].auth_signing_key_file` configured on the server, every template or snapshot create mints and binds a distinct execd token, including creates without env; without signing, the binding omits token fields. The endpoint response for port 44772 includes `X-EXECD-ACCESS-TOKEN` for SDK authentication when signing is configured; pause/resume retains the same token without reinitialization. Keep old signing keys until all sandboxes using them have terminated. Server proxy access to execd is not supported with this configuration.

New Full and RootfsOnly snapshots record the source's declared runtime env and authentication requirement. Creating a sandbox from either snapshot always binds the new sandbox ID after egress, inherits the recorded runtime env with caller-supplied overrides, and uses `keep` / `preserveRuntimeState: true` without an additional entrypoint launch. RootfsOnly cold-boots its captured startup configuration; overrides apply only to new execd commands/sessions. The default pool must support runtime-init regardless of env or signing; missing or unknown capability (including RBAC `403`) returns HTTP 400, with no legacy bypass. Source sandbox deletion does not prevent restore. Captured runtime env `{}` is known-empty and allows identity-only initialization without overrides or signing. Older snapshots with absent/`None` runtime env are always rejected with HTTP 400: capture a new snapshot first, because runtime-init replaces runtime env and must not silently clear unknown values. Restoring an authenticated source without server signing configuration returns HTTP 503 rather than disabling authentication.

Keep the guest's `EXECD_INIT_AUTH=false` for the current near-end handler: ordinary execd APIs are token-protected after binding, but authenticated initialization retries and interrupted-init recovery are separate follow-up work. Do not bake a shared access token into the template, and keep `/internal/init` blocked at external ingress.

The prewarmed entrypoint is not restarted, and existing processes and restored old sessions retain their original environment and identity. The supplied values and `OPENSANDBOX_ID` derived from the new sandbox binding apply only to new commands and sessions started by execd. Template initialization resets old sessions; snapshot initialization preserves them. Ordinary image creation and pause/resume are unchanged. Null values, reserved execd variable names, invalid keys, and runtime-init payloads exceeding 64 KiB are rejected by the server. See [template runtime initialization](/architecture/fast-sandbox/templates) for the runtime contract.

## Other publication formats

OCI publishing requires `overlaybd`, which remains the default format. For S3-compatible publication, explicitly select `native`:

```bash
osb template create --image python:3.12 --publish s3://bucket/publish --format native -o json
```

Deleting a template removes its catalog entry and Kubernetes build resource. Registry artifacts remain subject to the registry's retention and garbage collection policies.
