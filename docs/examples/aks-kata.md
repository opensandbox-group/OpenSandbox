---
title: AKS + Kata
description: Deploy OpenSandbox onto an AKS cluster with Kata VM isolation and interact with a Kata-isolated sandbox step by step, covering ingress, egress, and Credential Vault.
---

# AKS Kata Example

Deploy OpenSandbox onto an AKS cluster with `kata-vm-isolation`, then interact with a Kata-isolated sandbox step by step.

::: warning Demo credentials
The Helm values in this example use **insecure, well-known demo credentials**
(`api_key = "aks-kata-demo-key"` and a fixed `secureAccess` signing key) so the
walkthrough works out of the box with local `kubectl port-forward`. **Change
them before exposing the server or gateway beyond local port-forwarding.** See
[Security Model](#security-model).
:::

## Prerequisites

- AKS cluster with `kubectl get runtimeclass` showing `kata-vm-isolation`
- Helm 3
- Python 3.10+ with `pip install opensandbox requests`
- Azure OpenAI resource (endpoint + API key)

## Files

The example lives in [`examples/aks-kata`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/aks-kata):

| File | Purpose |
|------|---------|
| `main.py` | CLI tool — create, inspect, and operate on sandboxes one step at a time |
| `controller-values.yaml` | Helm values for the OpenSandbox controller |
| `server-values.yaml` | Helm values for the lifecycle server and ingress gateway |
| `batchsandbox-template-configmap.yaml` | BatchSandbox template that pins pods onto Kata nodes |

## 1. Install OpenSandbox

From the repo root:

```bash
kubectl create namespace opensandbox-system --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -

kubectl apply -f examples/aks-kata/batchsandbox-template-configmap.yaml

helm upgrade --install opensandbox-controller ./kubernetes/charts/opensandbox-controller \
  --namespace opensandbox-system \
  -f examples/aks-kata/controller-values.yaml

helm upgrade --install opensandbox-server ./kubernetes/charts/opensandbox-server \
  --namespace opensandbox-system \
  -f examples/aks-kata/server-values.yaml
```

Wait for the deployments:

```bash
kubectl rollout status deploy/opensandbox-controller-manager -n opensandbox-system --timeout=180s
kubectl rollout status deploy/opensandbox-server -n opensandbox-system --timeout=180s
kubectl rollout status deploy/opensandbox-ingress-gateway -n opensandbox-system --timeout=180s
```

## 2. Start port-forwards

Open two separate terminals:

```bash
# Terminal 1 — lifecycle server
kubectl port-forward -n opensandbox-system svc/opensandbox-server 18080:80

# Terminal 2 — ingress gateway
kubectl port-forward -n opensandbox-system svc/opensandbox-ingress-gateway 28080:80
```

## 3. Set environment variables

```bash
export SANDBOX_DOMAIN=http://127.0.0.1:18080
export SANDBOX_API_KEY=aks-kata-demo-key

export AZURE_OPENAI_ENDPOINT=https://your-resource.openai.azure.com
export AZURE_OPENAI_API_KEY=your-real-key
# Optional:
# export AZURE_OPENAI_DEPLOYMENT=gpt-4o-mini
```

## 4. Enable Pause/Resume (Optional)

Pause commits the sandbox rootfs as an OCI image and pushes it to a registry. Resume recreates the sandbox from that image. Without a registry, the `pause` and `resume` steps will log an error and continue.

The steps below use Azure Container Registry (ACR). Any OCI registry works — see the [Pause / Resume guide](/guides/pause-resume) for the full guide.

### Step 1: Grant AcrPush to the kubelet identity

```bash
KUBELET_ID=$(az aks show \
  -g <resource-group> -n <cluster-name> \
  --query "identityProfile.kubeletidentity.clientId" -o tsv)

ACR_ID=$(az acr show --name <acr-name> --query id -o tsv)

az role assignment create --assignee "$KUBELET_ID" --role AcrPush --scope "$ACR_ID"
```

### Step 2: Create the push/pull secret

```bash
ACR_PASSWORD=$(az acr credential show --name <acr-name> \
  --query "passwords[0].value" -o tsv)

kubectl create secret docker-registry acr-snapshot-push-secret \
  --docker-server=<acr-name>.azurecr.io \
  --docker-username=<acr-name> \
  --docker-password="$ACR_PASSWORD" \
  --namespace=opensandbox
```

### Step 3: Upgrade the controller

```bash
helm upgrade opensandbox-controller ./kubernetes/charts/opensandbox-controller \
  --namespace opensandbox-system \
  --reuse-values \
  --set controller.snapshot.registry=<acr-name>.azurecr.io/opensandbox-snapshots \
  --set controller.snapshot.snapshotPushSecret=acr-snapshot-push-secret \
  --set controller.snapshot.resumePullSecret=acr-snapshot-push-secret

kubectl rollout status deploy/opensandbox-controller-manager \
  -n opensandbox-system --timeout=90s
```

::: tip
This example clears `controller.snapshot.containerdSocketPath` because the pinned controller image (`controller:v0.2.0`) does not accept the `--containerd-socket-path` flag. Current controller builds **do** accept it (see [`kubernetes/cmd/controller/main.go`](https://github.com/opensandbox-group/OpenSandbox/blob/main/kubernetes/cmd/controller/main.go)); if your nodes use a non-default containerd socket and you deploy a controller image that supports the flag, set this value accordingly.
:::

## 5. Use `main.py`

All `main.py` commands below are run from the example directory:

```bash
cd examples/aks-kata
```

### Run everything end to end

```bash
python3 main.py all
```

This creates a sandbox, runs every demo step, and deletes it at the end.

### Run one step at a time

#### Create a sandbox

```bash
python3 main.py create
```

Expected output:

```
Creating Kata-isolated sandbox on AKS...
  OpenSandbox API: http://127.0.0.1:18080
  Sandbox image:   sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

SANDBOX_ID=96045ee2-6614-435c-9fa8-d4b6f9592598

Use --sandbox-id 96045ee2-6614-435c-9fa8-d4b6f9592598 for subsequent steps.
```

Save the ID for the following steps:

```bash
export SANDBOX_ID=96045ee2-6614-435c-9fa8-d4b6f9592598
```

Verify the pod is running on a Kata node:

```bash
kubectl get pod -n opensandbox -o wide
# NODE column should show aks-sandboxagent-*

kubectl get pod -n opensandbox -o jsonpath='{.items[0].spec.runtimeClassName}'
# Expected: kata-vm-isolation

python3 main.py exec --sandbox-id $SANDBOX_ID -c "uname -r"
# Expected: 6.6.137.mshv1-1.azl3 (mshv = Microsoft Hypervisor = Kata VM guest kernel)
```

#### Set up Credential Vault

```bash
python3 main.py credentials --sandbox-id $SANDBOX_ID
```

Expected output:

```
[credentials] Credential Vault configured.
```

#### Ask the LLM a question

```bash
python3 main.py llm --sandbox-id $SANDBOX_ID -q "What is the capital of France? Reply in one word."
```

Expected output:

```
[llm] Question: What is the capital of France? Reply in one word.
[llm] Model:    gpt-4o-mini
[llm] Answer:   Paris
```

The sandbox only has a fake API key — the real key is injected by the egress sidecar via Credential Vault. You can verify:

```bash
python3 main.py exec --sandbox-id $SANDBOX_ID -c "echo \$AZURE_OPENAI_API_KEY"
# Expected: fake-key-inside-sandbox
```

#### Run commands inside the sandbox

```bash
python3 main.py exec --sandbox-id $SANDBOX_ID -c "uname -a"
# [exec][stdout] Linux ...-0 6.6.137.mshv1-1.azl3 ... x86_64 GNU/Linux

python3 main.py exec --sandbox-id $SANDBOX_ID -c "cat /etc/os-release | head -3"
# [exec][stdout] PRETTY_NAME="Ubuntu 24.04.4 LTS"
# [exec][stdout] NAME="Ubuntu"
# [exec][stdout] VERSION_ID="24.04"

python3 main.py exec --sandbox-id $SANDBOX_ID -c "ls -la /tmp/www/"
# Shows the HTTP server root directory
```

#### Fetch from the sandbox HTTP server

The sandbox runs `python3 -m http.server 8080` serving `/tmp/www/`. Traffic goes through the ingress gateway with secure-access headers.

```bash
# Write a file into the sandbox, then fetch it via the gateway
python3 main.py exec --sandbox-id $SANDBOX_ID -c "echo hello > /tmp/www/greeting.txt"
python3 main.py http --sandbox-id $SANDBOX_ID -p /greeting.txt
# [http] GET /greeting.txt -> 200 (6 bytes)
# hello

# Directory listing
python3 main.py http --sandbox-id $SANDBOX_ID -p /
# Shows HTML directory listing

# 404 for missing file
python3 main.py http --sandbox-id $SANDBOX_ID -p /nonexistent.txt
# [http] GET /nonexistent.txt -> 404
```

#### Check sandbox status

```bash
python3 main.py status --sandbox-id $SANDBOX_ID
# Sandbox:  96045ee2-6614-435c-9fa8-d4b6f9592598
# State:    Running
# Image:    ...code-interpreter:v1.1.0
```

#### Pause and resume

Pause snapshots the container rootfs to the registry (1–5 min). Resume recreates the sandbox from that snapshot. Requires step 4 above.

```bash
python3 main.py pause --sandbox-id $SANDBOX_ID
# [lifecycle] pausing sandbox...
# [lifecycle] sandbox is PAUSED

python3 main.py status --sandbox-id $SANDBOX_ID
# State: Paused

# Pod is gone, but the BatchSandbox CR and snapshot remain
kubectl get pods -n opensandbox
# No resources found
kubectl get sandboxsnapshot -n opensandbox
# Shows snapshot with phase Succeed

python3 main.py resume --sandbox-id $SANDBOX_ID
# [lifecycle] resuming sandbox...
# [setup] sandbox state: Resuming, waiting...
# [lifecycle] state after resume: Running
# [http] GET /index.html -> 200 (filesystem preserved!)

python3 main.py status --sandbox-id $SANDBOX_ID
# State: Running
```

#### Delete the sandbox

```bash
python3 main.py delete --sandbox-id $SANDBOX_ID
# [lifecycle] sandbox 96045ee2-... deleted.

kubectl get pods -n opensandbox
# No resources found
```

Without `-c`, `-q`, or `-p`, the `exec`, `llm`, and `http` steps run their built-in demo sequences.

## What gets installed

| Component | Namespace | Purpose |
|-----------|-----------|---------|
| `opensandbox-controller` | `opensandbox-system` | Kubernetes operator — manages BatchSandbox CRDs, pools, and snapshots |
| `opensandbox-server` | `opensandbox-system` | Lifecycle API — creates/deletes sandboxes, proxies SDK calls |
| `opensandbox-ingress-gateway` | `opensandbox-system` | Routes external HTTP traffic into sandbox pods with auth |
| BatchSandbox template | `opensandbox-system` | Pins sandbox pods onto Kata-capable AKS nodes via `nodeSelector` |

The server is configured with:

- `workload_provider = "batchsandbox"`
- `secure_runtime.type = "kata"` / `k8s_runtime_class = "kata-vm-isolation"`
- egress sidecar (`dns+nft` mode)
- ingress gateway (`header` routing with `secureAccess`)

## Security Model

::: warning
`server-values.yaml` ships with **demo-only** credentials: `api_key =
"aks-kata-demo-key"` and a fixed base64 `secureAccess` signing key. They exist so
the local port-forward walkthrough works without extra setup. Replace both with
unique secret values (for example a random key and
`openssl rand -base64 32` for the signing key) before exposing the server or
gateway to anything other than `127.0.0.1`.
:::

### Kata VM isolation

Each sandbox runs inside a dedicated Kata VM (`runtimeClassName: kata-vm-isolation`) on an AKS Kata node pool (`nodeSelector: kubernetes.azure.com/kata-vm-isolation: "true"`). The sandbox sees its own guest kernel, not the host kernel.

### Egress isolation

The create request sets a deny-by-default outbound policy. Only explicitly allowed hosts (Azure OpenAI, pypi.org, files.pythonhosted.org) can be reached.

### Credential Vault

The sandbox only sees a fake `AZURE_OPENAI_API_KEY`. The real key is stored in Credential Vault and injected by the egress sidecar as the `api-key` header only for matching outbound requests to `https://<your-resource>.openai.azure.com/openai/*`.

### Ingress gateway + secureAccess

All external traffic to sandbox services flows through the ingress gateway. The gateway validates the `OpenSandbox-Secure-Access` token and routes via the `OpenSandbox-Ingress-To` header. Both are returned by the SDK's `get_endpoint()` call — callers never need to construct them manually.

### Per-service auth

Even if someone bypasses the gateway and reaches the pod directly:
- **execd** (port 44772) requires its own access token header
- **egress sidecar** (port 18080) requires the `OPENSANDBOX-EGRESS-AUTH` header
- Only the user's HTTP server (port 8080) has no built-in auth — that's what secureAccess protects

## Cleanup

Remove everything installed by this example:

```bash
# 1) Delete any running sandboxes
kubectl delete batchsandbox --all -n opensandbox

# 2) Uninstall Helm releases
helm uninstall opensandbox-server -n opensandbox-system
helm uninstall opensandbox-controller -n opensandbox-system

# 3) Remove the BatchSandbox template
kubectl delete configmap aks-kata-batchsandbox-template -n opensandbox-system

# 4) Remove the snapshot push/pull secret (if created)
kubectl delete secret acr-snapshot-push-secret -n opensandbox --ignore-not-found

# 5) Delete the namespaces
kubectl delete namespace opensandbox
kubectl delete namespace opensandbox-system
```
