---
title: EKS + Nitro
description: Deploy OpenSandbox onto an Amazon EKS Auto Mode cluster and run each sandbox in an AWS Nitro Sandbox through the isolated-sandbox RuntimeClass.
---

# EKS Nitro Example

Deploy OpenSandbox onto an Amazon EKS Auto Mode cluster and run each sandbox in an
AWS Nitro Sandbox: the sandbox pod's containers run in a lightweight VM with its own
kernel, using the `isolated-sandbox` (or `confidential-sandbox`) RuntimeClass.

The only server change is the secure runtime block:

```toml
[secure_runtime]
type = "nitro"
k8s_runtime_class = "isolated-sandbox"   # or "confidential-sandbox"
```

`type = "nitro"` is Kubernetes-only and defaults `k8s_runtime_class` to
`isolated-sandbox`, so `type = "nitro"` alone is enough.

::: warning Demo credentials
The Helm values in this example use **insecure, well-known demo credentials**
(`api_key = "eks-nitro-demo-key"` and a fixed `secureAccess` signing key shared by
`server-values.yaml` and `gateway-values.yaml`) so the walkthrough works with local
`kubectl port-forward`. **Change them before exposing the server or gateway beyond
`127.0.0.1`.**
:::

## Prerequisites

- An Amazon EKS cluster in EKS Auto Mode. Nitro Sandboxes need no extra cluster
  configuration there; see *Isolated and confidential pods* in the Amazon EKS User
  Guide.
- `kubectl get runtimeclass` lists `isolated-sandbox` and `confidential-sandbox`.
- Helm 3, and the `osb` CLI (`pip install opensandbox-cli`).

## Files

The example lives in [`examples/eks-nitro`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/eks-nitro):

| File | Purpose |
|------|---------|
| `server-values.yaml` | Helm values for the lifecycle server, with `[secure_runtime] type = "nitro"` |
| `gateway-values.yaml` | Helm values for the ingress gateway (signing key matches the server) |
| `controller-values.yaml` | Helm values for the OpenSandbox controller |
| `batchsandbox-template-configmap.yaml` | BatchSandbox template, with no `nodeSelector` or `tolerations` |

## Why the template has no nodeSelector or tolerations

Setting `runtimeClassName` is the whole opt-in. The `isolated-sandbox` RuntimeClass
carries the scheduling rule and the per-pod overhead:

```yaml
overhead:
  podFixed:
    eks.amazonaws.com/nitro-sandbox: "1"
    memory: 256Mi
scheduling:
  nodeSelector:
    eks.amazonaws.com/nitro-sandbox: "true"
```

Kubernetes merges that `nodeSelector` into every pod that names the class, and EKS
Auto Mode provisions a node that can run Nitro Sandboxes when none is free. Adding
your own `nodeSelector` or `tolerations` to the template is unnecessary.

## 1. Install OpenSandbox

From the repository root. The `base` chart installs the CRDs and creates the
`opensandbox-system` namespace; create the `opensandbox` workload namespace yourself.

```bash
helm upgrade --install base ./manifests/charts/base
kubectl create namespace opensandbox

kubectl apply -f examples/eks-nitro/batchsandbox-template-configmap.yaml

helm upgrade --install opensandbox-controller ./manifests/charts/controller \
  --namespace opensandbox-system \
  -f examples/eks-nitro/controller-values.yaml

helm upgrade --install opensandbox-server ./manifests/charts/server \
  --namespace opensandbox-system \
  -f examples/eks-nitro/server-values.yaml

helm upgrade --install opensandbox-ingress-gateway ./manifests/charts/ingress-gateway \
  --namespace opensandbox-system \
  -f examples/eks-nitro/gateway-values.yaml
```

Wait for the deployments:

```bash
kubectl rollout status deploy/opensandbox-controller-manager -n opensandbox-system --timeout=180s
kubectl rollout status deploy/opensandbox-server -n opensandbox-system --timeout=180s
kubectl rollout status deploy/opensandbox-ingress-gateway -n opensandbox-system --timeout=180s
```

## 2. Start port-forwards

In two separate terminals:

```bash
kubectl port-forward -n opensandbox-system svc/opensandbox-server 18080:80
kubectl port-forward -n opensandbox-system svc/opensandbox-ingress-gateway 28080:80
```

## 3. Create a sandbox

Create a sandbox. The first one can wait for EKS Auto Mode to start a node, so
allow a few minutes:

```bash
export OSB="osb --domain 127.0.0.1:18080 --api-key eks-nitro-demo-key --protocol http"

$OSB --request-timeout 330 sandbox create --image python:3.11 \
  --resource cpu=1 --resource memory=1Gi \
  --entrypoint tail --entrypoint -f --entrypoint /dev/null \
  --ready-timeout 5m -o json
```

```json
{
  "id": "2b286dd4-6450-4f9e-a6e6-581763eeadaf",
  "status": "created",
  "timeout": "sdk-default",
  "image": "python:3.11"
}
```

The pod runs with the RuntimeClass, the merged `nodeSelector`, and the entrypoint
wrapper described in [Entrypoint wrapper](#entrypoint-wrapper):

```bash
kubectl get pod -n opensandbox -o jsonpath='{.items[0].spec.runtimeClassName}{"\n"}{.items[0].spec.nodeSelector}{"\n"}{.items[0].spec.containers[0].command}{"\n"}'
```

```
isolated-sandbox
{"eks.amazonaws.com/nitro-sandbox":"true"}
["/bin/sh","-c","exec \"$@\"","sh","/opt/opensandbox/bootstrap.sh","tail","-f","/dev/null"]
```

## 4. Run commands

```bash
ID=<sandbox id from step 3>
$OSB command run $ID -- sh -c 'echo hello world; uname -r; id'
```

```
hello world
6.18.62
uid=0(root) gid=0(root) groups=0(root)
```

## 5. Clean up

```bash
$OSB sandbox kill $ID
helm uninstall opensandbox-ingress-gateway opensandbox-server opensandbox-controller -n opensandbox-system
kubectl delete -f examples/eks-nitro/batchsandbox-template-configmap.yaml
kubectl delete namespace opensandbox
helm uninstall base
```

The `base` chart keeps its CRDs and the `opensandbox-system` and `opensandbox-dataplane`
namespaces on uninstall (`helm.sh/resource-policy: keep`); delete them by hand when
nothing else uses them.

## Sizing

The RuntimeClass adds a fixed memory overhead to each sandbox pod, on top of what the
pod requests; the Amazon EKS User Guide (*Isolated and confidential pods
limitations*) describes it and the other resource limits of a Nitro Sandbox.

With the lifecycle API, set the top-level `resourceLimits` and, optionally,
`resourceRequests` (when omitted, requests equal limits):

```json
{
  "image": { "uri": "python:3.11" },
  "resourceLimits": { "cpu": "1", "memory": "1Gi" }
}
```

## Entrypoint wrapper

Nitro Sandboxes give each container its own SELinux MCS category rather than one per
pod. The server therefore starts the main container as
`/bin/sh -c 'exec "$@"' sh /opt/opensandbox/bootstrap.sh ...`, so the OCI entrypoint
is `/bin/sh` from the image instead of `bootstrap.sh`, which the `execd-installer`
init container wrote.

`[kubernetes].execd_init_security_context` can pin `seLinuxOptions` (or
`capabilities`, `privileged`) on that init container. Use it only for execd's own
files. **Do not** pin a matching `seLinuxOptions.level` to share a ReadWriteOnce
volume between containers: a runtime that mounts the volume once per container can
then corrupt the file system instead of failing with `permission denied`. Mount a
ReadWriteOnce volume into one container per pod, and use `emptyDir` to share between
containers.

## AWS credentials inside the sandbox

The sandbox has no access to the node's instance credentials. Code that needs AWS
credentials must get them through EKS Pod Identity.
