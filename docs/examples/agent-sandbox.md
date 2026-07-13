---
title: Agent Sandbox
description: Create a kubernetes-sigs/agent-sandbox instance and run a command using the OpenSandbox Python SDK.
---

# Agent-Sandbox Example

This example creates a sandbox backed by `kubernetes-sigs/agent-sandbox` and
executes `echo hello world` via the OpenSandbox Python SDK.

## Prerequisites

- A Kubernetes cluster with the agent-sandbox controller and CRDs installed.
- OpenSandbox server configured with Kubernetes runtime and `workload_provider = "agent-sandbox"`.
- Sandbox image should include `bash` (default example uses `ubuntu:22.04`).

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
execd_image = "opensandbox/execd:v1.0.21"

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
