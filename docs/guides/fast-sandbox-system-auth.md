---
title: Fast Sandbox System Authentication
description: Step-by-step tutorial to enable verified TLS and Kubernetes TokenRequest system authentication between the OpenSandbox Server, Ingress Gateway, and FastPath.
---

# Fast Sandbox System Authentication

Fast Sandbox System Authentication protects the control-plane gRPC link between OpenSandbox (`opensandbox-server` and `opensandbox-ingress-gateway`) and the Fast Sandbox control plane (`fast-sandbox-fastpath`). When enabled, every FastPath business RPC requires:

- **Verified TLS transport** signed by a deployment-managed `cert-manager` Issuer/ClusterIssuer or an existing TLS Secret.
- **Short-lived Kubernetes ServiceAccount tokens** minted via the `TokenRequest` API with the dedicated audience `fast-sandbox-fastpath` (maximum lifetime 3,600 seconds), verified locally via OIDC discovery and JWKS, and authorized against an explicit RPC profile (`server` vs. `gateway`) and workload namespace list.

::: info Disabled by default for zero-prerequisite compatibility
Fast Sandbox system authentication is **disabled by default** (`enabled: false`), allowing existing installations and local development clusters to run over plaintext gRPC without `cert-manager`. Once enabled on FastPath and its callers, authentication is strictly fail-closed with no plaintext or anonymous fallback.
:::

## Prerequisites

- A Kubernetes cluster with the `TokenRequest` API and ServiceAccount OIDC discovery enabled (Kubernetes 1.22+).
- A TLS serving certificate for `fast-sandbox-fastpath` and its **public CA certificate** (`ca.crt`), provisioned either via [cert-manager](https://cert-manager.io/) (`fastpath.tls.issuerRef`, using an existing `Issuer` or `ClusterIssuer` such as `fast-sandbox-serving`) or as a pre-created TLS Secret (`fastpath.tls.existingSecret`).
- A compatible `fsb-controller` companion image that includes the `--fastpath-auth-enabled` and `--fastpath-tls-*` flags.

---

## Scenario 1: In-Cluster Server and Ingress Gateway

In an in-cluster deployment, both `opensandbox-server` and `opensandbox-ingress-gateway` run inside the `opensandbox-system` namespace and use their own Pod ServiceAccounts to mint `fast-sandbox-fastpath` tokens.

### Step 1: Distribute the public serving CA

Create a `ConfigMap` in `opensandbox-system` containing **only** the public CA certificate (`ca.crt`) that signs your `cert-manager` issuer (never include the private key):

```bash
kubectl create configmap fastpath-serving-ca \
  --namespace opensandbox-system \
  --from-file=ca.crt=/path/to/serving-ca.crt \
  --dry-run=client -o yaml | kubectl apply -f -
```

### Step 2: Enable caller token-minting RBAC in the `base` chart

Enable `fastSandbox.callerAuth.enabled=true` on the `base` chart. This installs the `opensandbox-server-token-minter` and `opensandbox-ingress-gateway-token-minter` Roles and RoleBindings in `opensandbox-system`:

```bash
helm upgrade --install base manifests/charts/base \
  --set fastSandbox.callerAuth.enabled=true
```

Verify that both ServiceAccounts can mint tokens for themselves:

```bash
kubectl auth can-i create serviceaccounts/opensandbox-server \
  -n opensandbox-system \
  --subresource=token \
  --as=system:serviceaccount:opensandbox-system:opensandbox-server

kubectl auth can-i create serviceaccounts/opensandbox-ingress-gateway \
  -n opensandbox-system \
  --subresource=token \
  --as=system:serviceaccount:opensandbox-system:opensandbox-ingress-gateway
```

Both commands must output `yes`.

### Step 3: Enable TLS and caller policy on `fast-sandbox`

Upgrade the `fast-sandbox` chart with `fastpath.callerAuth.enabled=true` and point `fastpath.tls.issuerRef.name` to your `cert-manager` issuer (or set `fastpath.tls.existingSecret` if you pre-created the TLS Secret without `cert-manager`):

```yaml
# values-fast-sandbox.yaml
fastpath:
  callerAuth:
    enabled: true
    # Defaults to [resourceNamespace] ("opensandbox-dataplane").
    # List additional tenant namespaces if needed, or ["*"] for cluster-wide access.
    namespaces:
      - opensandbox-dataplane
  tls:
    issuerRef:
      name: fast-sandbox-serving
      kind: ClusterIssuer
```

```bash
helm upgrade --install fast-sandbox manifests/charts/fast-sandbox \
  --namespace opensandbox-system \
  --values values-fast-sandbox.yaml
```

Wait for `cert-manager` to issue the `fast-sandbox-fastpath-tls` Secret and for the controller to roll out:

```bash
kubectl -n opensandbox-system wait --for=condition=Ready certificate/fast-sandbox-fastpath --timeout=60s
kubectl -n opensandbox-system rollout status deploy/fast-sandbox-controller --timeout=120s
```

### Step 4: Enable caller authentication on `ingress-gateway`

Mount the `fastpath-serving-ca` ConfigMap and enable `gateway.fastpathAuth.enabled=true`:

```yaml
# values-ingress.yaml
gateway:
  fastpathEndpoint: fast-sandbox-fastpath.opensandbox-system.svc:9090
  fastpathAuth:
    enabled: true
    caConfigMap: fastpath-serving-ca
```

```bash
helm upgrade --install ingress-gateway manifests/charts/ingress-gateway \
  --namespace opensandbox-system \
  --values values-ingress.yaml
```

### Step 5: Enable caller authentication on `opensandbox-server`

Mount `fastpath-serving-ca` via `server.fastpath.caConfigMap` and set `fastpath_auth_enabled = true` and `fastpath_tls_ca_file` under `[kubernetes]` in `configToml`:

```yaml
# values-server.yaml
server:
  fastpath:
    caConfigMap: fastpath-serving-ca

configToml: |
  # ... your existing server configuration ...
  [kubernetes]
  namespace = "opensandbox-dataplane"
  fastpath_endpoint = "fast-sandbox-fastpath.opensandbox-system.svc:9090"
  fastpath_auth_enabled = true
  fastpath_tls_ca_file = "/etc/opensandbox/fastpath-ca/ca.crt"
```

```bash
helm upgrade --install opensandbox-server manifests/charts/server \
  --namespace opensandbox-system \
  --values values-server.yaml
```

---

## Scenario 2: External Server with Kubeconfig

When `opensandbox-server` runs outside the Kubernetes cluster using a `[kubernetes].kubeconfig_path`, it uses the same protocol and mints tokens for the same `opensandbox-server` ServiceAccount in the target cluster.

### Step 1: Authorize the external kubeconfig identity in `base`

Identify the Kubernetes `User`, `Group`, or `ServiceAccount` authenticated by your external `kubeconfig`, and bind it in the `base` chart. If you did not install the in-cluster `opensandbox-server` Helm chart, also set `createServerServiceAccount: true` so `base` creates a bare `opensandbox-server` ServiceAccount in `opensandbox-system`:

```yaml
# values-base-external.yaml
fastSandbox:
  callerAuth:
    enabled: true
    createServerServiceAccount: true
    externalServerSubjects:
      - kind: User
        apiGroup: rbac.authorization.k8s.io
        name: external-opensandbox-server
```

```bash
helm upgrade --install base manifests/charts/base \
  --values values-base-external.yaml
```

### Step 2: Add the external DNS name to the FastPath certificate

Include the externally reachable DNS name of FastPath in `fastpath.tls.dnsNames` so TLS hostname verification succeeds:

```yaml
# values-fast-sandbox-external.yaml
fastpath:
  callerAuth:
    enabled: true
  tls:
    issuerRef:
      name: fast-sandbox-serving
      kind: ClusterIssuer
    dnsNames:
      - fastpath.example.com
```

### Step 3: Configure the external Server's `config.toml`

Copy the public `ca.crt` to the external server host and enable `fastpath_auth_enabled = true` in `config.toml`:

```toml
[kubernetes]
kubeconfig_path = "/etc/opensandbox/kubeconfig"
namespace = "opensandbox-dataplane"
fastpath_endpoint = "fastpath.example.com:9090"
fastpath_auth_enabled = true
fastpath_tls_ca_file = "/etc/opensandbox/fastpath-ca/ca.crt"
# Optional: when connecting through a local port-forward (127.0.0.1:9090),
# set fastpath_tls_server_name to a DNS name present on the serving certificate:
# fastpath_tls_server_name = "fast-sandbox-fastpath.opensandbox-system.svc"
```

::: tip Custom system namespace
The server resolves the target ServiceAccount namespace from the `POD_NAMESPACE` environment variable (falling back to `opensandbox-system`). If your target cluster uses a custom `fastSandbox.namespaces.system`, export `POD_NAMESPACE=<target-system-namespace>` in the external server's environment.
:::

---

## Upgrading from Plaintext

Because authenticated FastPath is strictly fail-closed (no plaintext or anonymous fallback) and both `fast-sandbox-controller` and `opensandbox-server` use `Recreate` deployments, switching an existing deployment from plaintext to authenticated TLS causes a brief control-plane interruption while the three releases roll out. Follow this sequence during a brief maintenance window to avoid flag mismatches:

1. **Upgrade binaries first (default disabled):** Roll out the new `fast-sandbox`, `opensandbox-server`, and `ingress-gateway` releases with `callerAuth.enabled=false` (the default). Traffic continues over plaintext gRPC while images update.
2. **Provision certificates and RBAC:** Create the `fastpath-serving-ca` ConfigMap (and TLS Secret if using `fastpath.tls.existingSecret`) and enable `fastSandbox.callerAuth.enabled=true` in the `base` chart.
3. **Switch to authenticated TLS:** Enable `fastpath.callerAuth.enabled=true` on `fast-sandbox`, `gateway.fastpathAuth.enabled=true` on `ingress-gateway`, and `fastpath_auth_enabled = true` on `opensandbox-server`, then wait for all three Deployments to finish rolling out.

## Configuration Reference

| Component | Key / Flag | Default | Description |
|---|---|---|---|
| `base` chart | `fastSandbox.callerAuth.enabled` | `false` | Create `opensandbox-server-token-minter` and `opensandbox-ingress-gateway-token-minter` Roles and RoleBindings |
| `base` chart | `fastSandbox.callerAuth.createServerServiceAccount` | `false` | Create the `opensandbox-server` ServiceAccount in the system namespace (for external Server setups without the `server` chart) |
| `base` chart | `fastSandbox.callerAuth.externalServerSubjects` | `[]` | Extra RBAC subjects (`User`, `Group`, `ServiceAccount`) allowed to mint tokens for `opensandbox-server` |
| `fast-sandbox` chart | `fastpath.callerAuth.enabled` | `false` | Enable FastPath TLS and caller authentication policy |
| `fast-sandbox` chart | `fastpath.callerAuth.namespaces` | `[]` | Allowed workload namespaces (empty defaults to `[resourceNamespace]`; `["*"]` grants all namespaces) |
| `fast-sandbox` chart | `fastpath.tls.existingSecret` | `""` | Existing TLS Secret (`tls.crt` / `tls.key`) to mount instead of rendering a `cert-manager` Certificate |
| `fast-sandbox` chart | `fastpath.tls.issuerRef.name` | `"fast-sandbox-serving"` | Existing `cert-manager` Issuer or ClusterIssuer name (required when `existingSecret` is empty) |
| `fast-sandbox` chart | `fastpath.tls.issuerRef.kind` | `"ClusterIssuer"` | `Issuer` or `ClusterIssuer` |
| `fast-sandbox` chart | `fastpath.tls.dnsNames` | `[]` | Extra DNS SANs on the FastPath serving certificate |
| `ingress-gateway` chart | `gateway.fastpathAuth.enabled` | `false` | Pass `--fastpath-auth-enabled=true` to the gateway |
| `ingress-gateway` chart | `gateway.fastpathAuth.caConfigMap` | `""` | ConfigMap with `ca.crt` mounted at `/etc/opensandbox/fastpath-ca/ca.crt` |
| `ingress-gateway` chart | `gateway.fastpathAuth.serverName` | `""` | Override expected TLS certificate DNS name (`--fastpath-tls-server-name`) |
| `server` chart | `server.fastpath.caConfigMap` | `""` | ConfigMap with `ca.crt` mounted at `/etc/opensandbox/fastpath-ca/ca.crt` |
| Server `config.toml` | `[kubernetes].fastpath_auth_enabled` | `false` | Enable TLS and per-RPC TokenRequest bearer credentials on FastPath calls |
| Server `config.toml` | `[kubernetes].fastpath_tls_ca_file` | `null` | Path to trusted FastPath serving CA PEM bundle (empty uses system roots) |
| Server `config.toml` | `[kubernetes].fastpath_tls_server_name` | `null` | Override expected TLS DNS name (for example, over `kubectl port-forward`) |
