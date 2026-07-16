# Agent Substrate POC

Install or repair the AKS POC with an explicit Kubernetes context:

```shell
# Render and validate without changing the cluster.
./examples/agent-substrate/setup_poc.sh --context testmember-5-admin

# Install or repair the cluster resources.
./examples/agent-substrate/setup_poc.sh \
  --context testmember-5-admin \
  --apply
```

## Manual staged setup

To inspect and apply each layer separately:

1. Render the pinned upstream manifests into a retained work directory.
2. Create the namespaces, static POC PKI, and ateapi OIDC/Valkey configuration.
3. Install the Agent Substrate CRDs, validation policy, and gVisor SandboxConfig.
4. Apply and verify the control plane, storage, router, and `atelet` DaemonSet.
5. Apply the WorkerPool and administrator-owned execd ActorTemplate.
6. Create the `opensandbox` Atespace, API key, CA ConfigMap, and server.

The copy/paste commands are in
[Manual staged installation](../../docs/examples/agent-substrate.md#manual-staged-installation).
The numbered comments in [`setup_poc.sh`](setup_poc.sh) explain the same phases
and remain the source of truth for the automated path.

Then validate the Agent Substrate workload provider through the public
OpenSandbox REST API and atenet.

> **Full step-by-step guide**: [docs/examples/agent-substrate.md](../../docs/examples/agent-substrate.md)

The setup entry point is [`setup_poc.sh`](setup_poc.sh), and the runnable
lifecycle check is [`test_poc.py`](test_poc.py).