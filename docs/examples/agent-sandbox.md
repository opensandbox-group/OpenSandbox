---
title: Agent Sandbox
description: Create a kubernetes-sigs/agent-sandbox instance and run a command using the OpenSandbox Python SDK.
---

# Agent-Sandbox Example

This example creates a sandbox backed by `kubernetes-sigs/agent-sandbox` and executes `echo hello world` via the OpenSandbox Python SDK.

## Prerequisites

- A Kubernetes cluster with agent-sandbox >= v0.5.0 controller and CRDs installed, serving `agents.x-k8s.io/v1beta1`.
- OpenSandbox server configured with Kubernetes runtime and `workload_provider = "agent-sandbox"`.
- Sandbox image should include `bash` (default example uses `ubuntu:22.04`).

## Compatibility and migration

The server provider and ingress informer use `agents.x-k8s.io/v1beta1` only. This API is available starting in agent-sandbox v0.5.0; v1.0.0 removes `v1alpha1`. Upgrade older controller and CRD installations before using this integration; there is no `v1alpha1` fallback.

For an existing installation, first upgrade agent-sandbox to a release that serves both versions (such as v0.5.0), with its conversion webhook healthy, then upgrade the OpenSandbox server and ingress together. Complete the upstream resource/storage migration before installing an agent-sandbox release that removes the alpha API.

Update custom AgentSandbox templates to use `apiVersion: agents.x-k8s.io/v1beta1` and remove `spec.replicas`. The server sets `spec.operatingMode: Running` and `spec.service: true`, including when merging a custom template. Service creation is required for ingress routing through `status.serviceFQDN`.

The server applies these fields when creating new sandboxes; it does not rewrite existing resources. On v0.5.0, alpha-to-beta conversion preserves an omitted `service` field. If an existing sandbox has no Service/FQDN, explicitly enable `spec.service: true` on that resource and wait for `Ready=True` and a populated `status.serviceFQDN` before using ingress. This can restore routing without recreating the Sandbox. Keep intentionally service-free resources unchanged.

The configured `shutdownPolicy`, pod template merging, and absolute `shutdownTime` semantics are preserved. Requests without an expiration omit `shutdownTime`, including any value supplied by a template. Status and endpoint resolution still use `Ready`, `selector`, `podIPs`, and `serviceFQDN`.

OpenSandbox's own BatchSandbox API remains `sandbox.opensandbox.io/v1alpha1`.

## Pause and resume

This provider supports the standard `POST /v1/sandboxes/{id}/pause` and `POST /v1/sandboxes/{id}/resume` endpoints. Both are implemented by patching the `Sandbox` CRD's `spec.operatingMode` field.

Pause patches `spec.operatingMode: Suspended`. The agent-sandbox controller terminates and destroys the Pod but retains the `Sandbox` object, so the `sandboxId` stays stable across pause/resume cycles. The sandbox reports `Running` → `Pausing` → `Paused`; while the Pod is terminating, the status `reason` comes from the `Suspended` condition (e.g. `PodTerminating`).

Resume patches `spec.operatingMode: Running`. The controller recreates the Pod from the Sandbox spec, so expect scheduling and image-pull latency before the sandbox is usable again. The sandbox reports `Paused` → `Resuming` → `Pending` → `Running`.

| | Preserved? |
|--|-----------|
| Persistent volume (PVC) data | ✅ Yes — volumes are retained with the Sandbox object |
| `sandboxId` and sandbox metadata | ✅ Yes |
| Memory / running processes | ❌ No — the Pod is destroyed |
| Root filesystem runtime state | ❌ No — the Pod is recreated from the original image on resume |

Anything not written to a persistent volume is lost on pause.

A paused sandbox still expires at its original timeout; a retained expired sandbox reports `Terminated` and can no longer be paused or resumed.

## Start OpenSandbox server

1. Install the server package and fetch the example config for agent-sandbox:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
```

2. Update `~/.sandbox.toml` with the following sections:

```toml
[runtime]
type = "kubernetes"
execd_image = "opensandbox/execd:v1.1.0"

[kubernetes]
namespace = "default"
# kubeconfig_path = "/absolute/path/to/kubeconfig"  # optional if running in-cluster
workload_provider = "agent-sandbox"

[agent_sandbox]
shutdown_policy = "Delete"
```

3. Start the server:

```shell
opensandbox-server
```

## Run the example

```shell
uv pip install opensandbox
uv run python examples/agent-sandbox/main.py
```

## Expected output

```text
command output: hello world
```

## References

- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/agent-sandbox)
