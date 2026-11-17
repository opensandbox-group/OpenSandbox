# EKS Nitro Example

Deploy OpenSandbox onto an Amazon EKS cluster and run sandboxes inside
hardware-isolated **Nitro** guest VMs using the [EKS Pod Isolation](https://docs.aws.amazon.com/eks/)
`isolated-sandbox` (or `confidential-sandbox`) RuntimeClass.

This is the AWS analog of the [`aks-kata`](../aks-kata) example. The only server
change is the secure runtime block:

```toml
[secure_runtime]
type = "nitro"
k8s_runtime_class = "isolated-sandbox"   # or "confidential-sandbox"
```

`type = "nitro"` is Kubernetes-only and defaults `k8s_runtime_class` to
`isolated-sandbox`, so the line above can be shortened to just `type = "nitro"`.

## Why the batchsandbox template has no nodeSelector/tolerations

Setting `runtimeClassName` is the entire opt-in for EKS Pod Isolation:

- The `isolated-sandbox` RuntimeClass carries
  `scheduling.nodeSelector: {eks.amazonaws.com/nitro-sandbox: "true"}` and a pod
  overhead of `{eks.amazonaws.com/nitro-sandbox: "1", memory: 256Mi}`.
- The cluster's `add-sandbox-toleration` admission policy injects the
  `workload=isolated:NoSchedule` toleration the sandbox NodePool taint requires.

So `batchsandbox-template-configmap.yaml` deliberately omits the
`nodeSelector`/`tolerations` that the Kata example needs. Adding them is
redundant and can pin pods to the wrong nodes.

## Prerequisites

- An Amazon EKS cluster running in EKS Auto Mode. Isolated and confidential pods
  need no additional cluster configuration. See *Isolated and confidential pods*
  in the Amazon EKS User Guide.
- Confirm the runtime classes exist: `kubectl get runtimeclass` should list
  `isolated-sandbox` and `confidential-sandbox` (hyphens, not underscores).

## Sandbox resource minimums (important)

The runtime enforces **silent minimums of 1 vCPU and 1024 Mi per sandbox**
(256 Mi of that is guest overhead, so the minimum *pod* memory request is
768 Mi). Best-Effort QoS is not supported — every container must set CPU and
memory requests. When creating a sandbox, request at least:

```jsonc
// SDK / CreateSandbox request
"resources": {
  "requests": { "cpu": "1",   "memory": "768Mi" },
  "limits":   { "cpu": "1",   "memory": "1Gi"   }
}
```

A sandbox that requests less still consumes the full minimum on the node while
the scheduler only accounts for what was requested, which later starves the
node and makes unrelated pods fail to start with no obvious cause.

## Deploy

```bash
# Server (lifecycle API + ingress gateway)
helm upgrade --install opensandbox-server \
  oci://ghcr.io/opensandbox-group/charts/opensandbox-server \
  -n opensandbox-system --create-namespace \
  -f server-values.yaml

# Controller (snapshots)
helm upgrade --install opensandbox-controller \
  oci://ghcr.io/opensandbox-group/charts/opensandbox-controller \
  -n opensandbox-system \
  -f controller-values.yaml

# Batchsandbox pod template
kubectl apply -f batchsandbox-template-configmap.yaml
```

Then port-forward and drive the API exactly as in the
[`aks-kata` walkthrough](../../docs/examples/aks-kata.md) — the client flow is
identical; only the isolation runtime differs.

> **Note:** `server-values.yaml` ships with insecure demo credentials (`api_key`
> and a fixed `secureAccess` signing key) so the local port-forward walkthrough
> works out of the box. Change them before exposing the server or gateway beyond
> `127.0.0.1`.

## IAM inside the sandbox

The IMDS surrogate inside a Nitro sandbox serves only `region`,
`availability-zone`, and `az-id`; every other path (including credentials)
returns 404. Workloads that need AWS credentials must use **EKS Pod Identity** —
any code path expecting node credentials from IMDS will fail.
