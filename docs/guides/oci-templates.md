---
title: Publish Templates to an OCI Registry
description: Build Golden Image templates in an OCI registry and create Fast Sandbox instances from immutable artifacts.
---

# Publish templates to an OCI registry

`osb template create` can publish Golden Image artifacts to an OCI registry. Set `--publish` to a repository-only address such as `oci://registry.example.com/sandbox/templates`; omit tags and digests. The builder generates a UUID for each build and publishes paired rootfs and snapshot images. Once the build succeeds, `status.manifestRef` contains the immutable `oci://registry.example.com/sandbox/templates@sha256:...` reference used to restore that build.

## Prepare the runtime

Use the fast-sandbox companion images built from the source pinned in `manifests/third-party/fast-sandbox.commit`. OCI templates require the OverlayBD node daemon, shared state and device mounts, and `FAST_SANDBOX_OCI_ENABLED=true` on the runtime agent, node daemon, and Fastlet pool. Enable `runtime.oci.enabled` in the fast-sandbox Helm chart to install the node OCI stack. Follow the upstream [OverlayBD OCI deployment guide](https://github.com/opensandbox-group/fast-sandbox/blob/4c389ae1b0e3823d59eee2ade4fba36682984590/docs/guides/overlaybd-oci-sandboxes.md) to configure the pool and its registry credentials.

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

Non-empty `env` requires the selected pool to declare a `runtime-init` handler on `sandbox.data-plane-ready` and the template guest to support `/internal/init`. A pool without the handler returns HTTP 400; the SDK forwards the server error rather than checking pool capabilities itself. Empty or omitted env does not trigger runtime initialization unless server-managed execd authentication is enabled.

With `[execd].auth_signing_key_file` configured on the server, every template create requires that handler and binds a distinct execd token, including creates without env. The endpoint response for port 44772 includes `X-EXECD-ACCESS-TOKEN` for SDK authentication; pause/resume retains the same token. Keep old signing keys until all sandboxes using them have terminated. Server proxy access to execd is not supported with this configuration. Snapshot-derived token rebinding is not implemented in this increment.

Keep the guest's `EXECD_INIT_AUTH=false` for the current near-end handler: ordinary execd APIs are token-protected after binding, but authenticated initialization retries and interrupted-init recovery are separate follow-up work. Do not bake a shared access token into the template, and keep `/internal/init` blocked at external ingress.

The prewarmed entrypoint is not restarted, and existing processes retain their original environment. The supplied values apply to subsequent commands and sessions started by execd. Null values, reserved execd variable names, invalid keys, and runtime-init payloads exceeding 64 KiB are rejected by the server. See [template runtime initialization](/architecture/fast-sandbox/templates) for the runtime contract.

## Other publication formats

OCI publishing requires `overlaybd`, which remains the default format. For S3-compatible publication, explicitly select `native`:

```bash
osb template create --image python:3.12 --publish s3://bucket/publish --format native -o json
```

Deleting a template removes its catalog entry and Kubernetes build resource. Registry artifacts remain subject to the registry's retention and garbage collection policies.
