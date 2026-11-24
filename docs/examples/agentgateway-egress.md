---
title: Agentgateway Egress
description: Enforce centralized sandbox egress policies with agentgateway and OpenSandbox Egress Forward Mode on Docker and Kubernetes.
---

# Agentgateway Egress Example

This example connects OpenSandbox [Egress Forward Mode](/guides/egress-forward-mode) to [agentgateway](https://github.com/agentgateway/agentgateway) (`v1.7.0+`) so every outbound TCP connection from a sandbox is transparently funneled to agentgateway over HTTP(S) `CONNECT`, authenticated with `Proxy-Authorization`, tagged with OpenSandbox identity headers, and routed by TLS SNI or HTTP `Host`.

```text
sandbox workload (no proxy env vars, no NET_ADMIN/NET_RAW)
   │  nftables transparent redirect
   ▼
opensandbox/egress (mode = "forward")
   │  CONNECT <original-ip>:<port>
   │  Proxy-Authorization: Bearer <token>
   │  OpenSandbox-Sandbox-Id: <sandbox-id>
   │  OpenSandbox-Sandbox-Namespace: <k8s-namespace>   # Kubernetes
   ▼
agentgateway v1.7.0+ (tunnelProtocol: connect / connect.mode: Tunnel)
   │  networkAuthorization + accessLog on source.connectHeaders
   │  TLS SNI passthrough (443) or HTTP Host routing + header injection (80)
   ▼
upstream origin (dialed via DNS on authorized hostname, never the raw CONNECT IP)
```

## Prerequisites

- OpenSandbox `release-1.3.0-rc.3` (or newer) supporting Egress Forward Mode ([OSEP-0030](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0030-egress-upstream-proxy-forward-mode.md)) — see the [1.3.0-rc.3 release manifest](https://github.com/opensandbox-group/OpenSandbox/releases/download/release-1.3.0-rc.3/1.3.0-rc.3.yaml) (`opensandbox/server:release-1.3.0-rc.3`, `opensandbox/execd:release-1.3.0-rc.3`, `opensandbox/egress:release-1.3.0-rc.3`).
  - Note: PyPI `opensandbox-server==1.2.1` rejects `egress.mode = "forward"`. Until `1.3.0` is published to PyPI, run the server from this repository checkout (`uv run --project server opensandbox-server ...`) or deploy the `opensandbox/server:release-1.3.0-rc.3` container image.
- `ghcr.io/agentgateway/agentgateway:v1.7.0` (or newer).
- Python 3.10+ with `opensandbox` SDK (`uv pip install opensandbox`; `1.2.1` works).
- For Docker walkthrough: Docker Engine / Docker Desktop / OrbStack with bridge networking.
- For Kubernetes Gateway API walkthrough: Kubernetes **1.30+**, Gateway API `v1.6.3` standard CRDs, `agentgateway` `v1.7.0+` Helm charts, and OpenSandbox controller (`batchsandbox` or `agent-sandbox` provider).
- For Kubernetes Standalone outer-HTTPS walkthrough: any Kubernetes cluster with OpenSandbox controller and `openssl` (no Gateway API CRDs required).

::: warning
The sample files under `examples/agentgateway-egress/` contain demo credentials (`api_key = "opensandbox-demo-key"` and `Bearer opensandbox-demo-token`) for local walkthroughs. Replace them before exposing the server or gateway outside a local environment.
:::

## Option A: Docker (Standalone agentgateway)

### 1. Start agentgateway

Validate and run agentgateway with [docker/agentgateway.yaml](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/agentgateway-egress/docker/agentgateway.yaml):

```shell
docker run --rm \
  -v "$(pwd)/examples/agentgateway-egress/docker/agentgateway.yaml:/etc/agentgateway/config.yaml:ro" \
  ghcr.io/agentgateway/agentgateway:v1.7.0 \
  -f /etc/agentgateway/config.yaml --validate-only

docker run -d --name agentgateway-egress \
  -p 3128:3128 \
  -v "$(pwd)/examples/agentgateway-egress/docker/agentgateway.yaml:/etc/agentgateway/config.yaml:ro" \
  ghcr.io/agentgateway/agentgateway:v1.7.0 \
  -f /etc/agentgateway/config.yaml
```

### 2. Start OpenSandbox server

[docker/server.toml](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/agentgateway-egress/docker/server.toml) already pins `execd_image` and `egress.image` to `release-1.3.0-rc.3`. On Linux Docker Engine, edit `egress.upstream_proxy.url` to use the `agentgateway-egress` bridge IP or host gateway IP instead of `host.docker.internal`. If your Docker daemon uses a non-default socket (such as OrbStack), export `DOCKER_HOST` before starting the server from the repository checkout:

```shell
# On OrbStack (when using its non-default socket instead of /var/run/docker.sock):
export DOCKER_HOST="unix://${HOME}/.orbstack/run/docker.sock"
# Or resolve the active Docker context endpoint automatically:
# export DOCKER_HOST="$(docker context inspect --format '{{.Endpoints.docker.Host}}')"

uv run --project server opensandbox-server \
  --config examples/agentgateway-egress/docker/server.toml
```

### 3. Run the verification script

```shell
SANDBOX_DOMAIN=127.0.0.1:8080 \
SANDBOX_API_KEY=opensandbox-demo-key \
uv run python examples/agentgateway-egress/main.py
```

## Option B: Kubernetes (Gateway API)

This deployment uses the native `agentgateway` Kubernetes Gateway API controller (`Gateway`, `AgentgatewayParameters`, `AgentgatewayBackend`, `AgentgatewayPolicy`, `TLSRoute`, and `HTTPRoute`).

### 1. Install Gateway API CRDs and agentgateway

```shell
kubectl apply --server-side --force-conflicts \
  -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml

helm upgrade -i agentgateway-crds oci://ghcr.io/agentgateway/charts/agentgateway-crds \
  --create-namespace --namespace agentgateway-system \
  --version v1.7.0

helm upgrade -i agentgateway oci://ghcr.io/agentgateway/charts/agentgateway \
  --namespace agentgateway-system \
  --version v1.7.0 \
  --wait
```

::: note
`agentgateway` `v1.7.0` reconciles `gateway.networking.k8s.io/v1` `TLSRoute`, whose CRD validation requires Kubernetes **1.30+**. On older clusters, use [Option C](#option-c-kubernetes-standalone-with-outer-https) instead.
:::

### 2. Apply the egress Gateway manifests

```shell
kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f examples/agentgateway-egress/kubernetes/gateway-api.yaml
kubectl wait --for=condition=Programmed gateway/opensandbox-egress -n opensandbox --timeout=60s
```

### 3. Start OpenSandbox server and run the verification script

[kubernetes/server.toml](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/agentgateway-egress/kubernetes/server.toml) already pins `execd_image` and `egress.image` to `release-1.3.0-rc.3`.

Because `SANDBOX_USE_SERVER_PROXY=true` proxies SDK calls through `opensandbox-server`, the server itself must be able to reach sandbox Pod IPs directly. On local `kind` clusters (or any cluster whose Pod CIDR is not routable from the host), running `opensandbox-server` on the host will time out waiting for sandbox readiness even when `SANDBOX_USE_SERVER_PROXY=true` is set.

Choose one of the following server setups:

1. **In-cluster + `kubectl port-forward` (recommended; verified on local `kind`)**: Deploy `opensandbox-server` inside the cluster using `opensandbox/server:release-1.3.0-rc.3` (for example with `./manifests/charts/server`, keeping `[server] host = "0.0.0.0"`, `port = 80`, and `[kubernetes].kubeconfig_path` unset in `configToml` while copying `[runtime]`, `[kubernetes]`, `[egress]`, and `[egress.upstream_proxy]` from `examples/agentgateway-egress/kubernetes/server.toml`), then forward port `8080`:

   ```shell
   kubectl port-forward -n opensandbox-system svc/opensandbox-server 8080:80
   ```

2. **Host-side server (only when the host can route directly to sandbox Pod IPs)**: Uncomment and explicitly set `[kubernetes].kubeconfig_path` in [kubernetes/server.toml](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/agentgateway-egress/kubernetes/server.toml) (leaving it unset falls back to in-cluster configuration and fails outside a Pod), then start the server from the repository checkout:

   ```shell
   uv run --project server opensandbox-server \
     --config examples/agentgateway-egress/kubernetes/server.toml
   ```

Run `main.py` with `SANDBOX_USE_SERVER_PROXY=true` and `EXPECTED_NAMESPACE=opensandbox`:

```shell
SANDBOX_DOMAIN=127.0.0.1:8080 \
SANDBOX_API_KEY=opensandbox-demo-key \
SANDBOX_USE_SERVER_PROXY=true \
EXPECTED_NAMESPACE=opensandbox \
uv run python examples/agentgateway-egress/main.py
```

## Option C: Kubernetes (Standalone with Outer HTTPS)

Use this option when you want TLS between the OpenSandbox egress sidecar and agentgateway (`ca_secret_name`), or when running on a Kubernetes cluster without Gateway API `v1` `TLSRoute` CRDs.

### 1. Generate a CA and gateway TLS Secrets

```shell
mkdir -p /tmp/ag-k8s-certs
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout /tmp/ag-k8s-certs/ca.key -out /tmp/ag-k8s-certs/ca.crt \
  -days 30 -subj "/CN=opensandbox-agentgateway-ca"
openssl req -newkey rsa:2048 -nodes \
  -keyout /tmp/ag-k8s-certs/tls.key -out /tmp/ag-k8s-certs/tls.csr \
  -subj "/CN=agentgateway-standalone.opensandbox.svc.cluster.local"
printf "subjectAltName=DNS:agentgateway-standalone,DNS:agentgateway-standalone.opensandbox,DNS:agentgateway-standalone.opensandbox.svc,DNS:agentgateway-standalone.opensandbox.svc.cluster.local\n" \
  > /tmp/ag-k8s-certs/ext.cnf
openssl x509 -req -in /tmp/ag-k8s-certs/tls.csr \
  -CA /tmp/ag-k8s-certs/ca.crt -CAkey /tmp/ag-k8s-certs/ca.key -CAcreateserial \
  -out /tmp/ag-k8s-certs/tls.crt -days 30 -extfile /tmp/ag-k8s-certs/ext.cnf

kubectl create namespace opensandbox --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic gateway-ca -n opensandbox \
  --from-file=ca.crt=/tmp/ag-k8s-certs/ca.crt --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret tls gateway-tls -n opensandbox \
  --cert=/tmp/ag-k8s-certs/tls.crt --key=/tmp/ag-k8s-certs/tls.key --dry-run=client -o yaml | kubectl apply -f -
```

### 2. Deploy standalone agentgateway

```shell
kubectl apply -f examples/agentgateway-egress/kubernetes/standalone-https.yaml
kubectl rollout status deployment/agentgateway-standalone -n opensandbox --timeout=60s
```

### 3. Switch `kubernetes/server.toml` to Option 2 and verify

In `examples/agentgateway-egress/kubernetes/server.toml`, **first comment out Option 1's `url` and `authorization` keys**, and then uncomment the Option 2 `[egress.upstream_proxy]` block (`url = "https://agentgateway-standalone.opensandbox.svc.cluster.local:3128"`, `authorization`, and `ca_secret_name = "gateway-ca"`). Leaving Option 1 active causes a duplicate-key TOML parse error (`Cannot overwrite a value`).

Apply the updated config to your in-cluster server (keeping `kubectl port-forward` active) or restart the host-side server via `uv run --project server opensandbox-server --config examples/agentgateway-egress/kubernetes/server.toml`, then run HTTPS verification:

```shell
SANDBOX_DOMAIN=127.0.0.1:8080 \
SANDBOX_API_KEY=opensandbox-demo-key \
SANDBOX_USE_SERVER_PROXY=true \
VERIFY_HTTP=false \
uv run python examples/agentgateway-egress/main.py
```

## Expected output

Options A and B (HTTPS + HTTP):

```text
Created sandbox: <sandbox-id>
[PASS] HTTPS passthrough allowed: https://example.com -> 200
[PASS] HTTPS denied domain blocked: https://example.org
[PASS] Anti-spoofing SNI routing: 1.1.1.1:443 (SNI=example.com) -> HTTP/1.1 200 OK
[PASS] HTTP allowed + CONNECT identity headers injected: x-opensandbox-sandbox-id=<sandbox-id>
[PASS] HTTP denied domain rejected by agentgateway: http://example.org -> 404
[PASS] Link-local metadata address dropped: 169.254.169.254 (BLOCKED:TimeoutError)
All agentgateway egress checks passed.
Sandbox <sandbox-id> destroyed.
```

Option C (`VERIFY_HTTP=false`):

```text
Created sandbox: <sandbox-id>
[PASS] HTTPS passthrough allowed: https://example.com -> 200
[PASS] HTTPS denied domain blocked: https://example.org
[PASS] Anti-spoofing SNI routing: 1.1.1.1:443 (SNI=example.com) -> HTTP/1.1 200 OK
[PASS] Link-local metadata address dropped: 169.254.169.254 (BLOCKED:TimeoutError)
All agentgateway egress checks passed.
Sandbox <sandbox-id> destroyed.
```

Inspect agentgateway logs to confirm per-sandbox attribution:

```shell
# Docker
docker logs agentgateway-egress

# Kubernetes (Gateway API)
kubectl logs -n opensandbox deploy/opensandbox-egress

# Kubernetes (Standalone HTTPS)
kubectl logs -n opensandbox deploy/agentgateway-standalone
```

Each allowed and denied egress entry carries `sandbox_id="<sandbox-id>"` (and `sandbox_ns="opensandbox"` on Kubernetes).

## Operational notes

- **Use Tunnel mode, not Route mode**: OpenSandbox forward mode sends numeric `CONNECT <IP>:<port>` targets. agentgateway's Route mode (`connect.mode: route`) matches route `hostnames` against the CONNECT authority and cannot allowlist domains for numeric targets; Tunnel mode (`tunnelProtocol: connect` / `connect.mode: Tunnel`) re-enters internal binds by destination port and matches TLS SNI or HTTP `Host`.
- **Outer HTTPS + inner plaintext HTTP (port 80)**: In `agentgateway` `v1.7.0`, terminating outer TLS on a `tunnelProtocol: connect` bind carries outer TLS connection metadata into inner plaintext HTTP binds, causing dynamic HTTP backends on port 80 (when `Host` omits `:80`) to dial upstream port `443`. Keep outer HTTPS deployments on HTTPS-only egress (port `443`, as in `kubernetes/standalone-https.yaml`), or use plaintext inner-network `http://` CONNECT protected by network isolation / Kubernetes `NetworkPolicy`.
- **TLS interception / credential injection**: To inject upstream API keys on HTTPS endpoints (such as LLM APIs), add an `HTTPS` listener with `tls.mode: dynamicCa` (Standalone) or `agentgateway.dev/tls-certificate-source: DYNAMIC_CA` (Kubernetes) and bake or mount the dynamic CA root into the sandbox image.

## Cleanup

```shell
# Docker
docker rm -f agentgateway-egress

# Kubernetes (Gateway API)
kubectl delete -f examples/agentgateway-egress/kubernetes/gateway-api.yaml

# Kubernetes (Standalone HTTPS)
kubectl delete -f examples/agentgateway-egress/kubernetes/standalone-https.yaml
kubectl delete secret gateway-ca gateway-tls -n opensandbox --ignore-not-found
```

## References

- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/agentgateway-egress)
- [Egress Forward Mode guide](/guides/egress-forward-mode)
- [agentgateway egress proxy documentation](https://agentgateway.dev/docs/standalone/latest/documentation/configuration/egress-proxy/)
