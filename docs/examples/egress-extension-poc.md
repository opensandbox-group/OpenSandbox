---
title: Egress Extension API POC
description: Build, deploy, and verify the storage-only egress extension API proof of concept on Kubernetes, including capability discovery, atomic replacement, and optimistic concurrency.
---

# Egress Extension API POC

This example builds the current egress component, deploys it to an isolated Kubernetes namespace, and exercises the versioned extension API through real HTTP requests.

::: warning Storage only
This POC validates the API and transaction model. It stores extension resources in memory but does not translate them to Envoy or enforce their traffic policy. A successful write reports `Programmed=Unknown` with reason `PocStored`.
:::

## What This Verifies

- `GET /capabilities` reports exact `(apiVersion, kind)` support.
- `GET /extensions` returns the current revisioned resource set.
- `PUT /extensions` atomically replaces the complete set.
- Unknown fields inside `metadata` and `spec` survive a round trip.
- A stale `expectedRevision` returns `409 REVISION_CONFLICT`.
- A known but unavailable kind returns `412 CAPABILITY_UNAVAILABLE`.
- Clearing extensions does not modify the core `/policy` API.

The runnable files are under [`examples/egress-extension-poc`](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/egress-extension-poc):

| File | Purpose |
|------|---------|
| `kubernetes.yaml` | Deployment and ClusterIP service for the POC sidecar |
| `verify.sh` | Automated API verification and cleanup of stored extension state |

## Prerequisites

- A Linux `amd64` Kubernetes cluster
- `kubectl`, `az`, `curl`, `jq`, and `sed`
- Permission to create a namespace, Deployment, Service, Secret, and port-forward
- An Azure Container Registry (ACR) where you can build images
- The OpenSandbox repository checked out at the branch containing the POC

The commands below were exercised with the `testmember-3-admin` context. Change the variables for another cluster or registry.

## 1. Set Variables

Run from the repository root:

```bash
export KUBE_CONTEXT=testmember-3-admin
export POC_NAMESPACE=opensandbox-egress-poc
export ACR_NAME=ryanclaw
export POC_TAG="$(date -u +%Y%m%d-%H%M%S)"
export POC_IMAGE="${ACR_NAME}.azurecr.io/opensandbox/egress-extension-poc:${POC_TAG}"

kubectl --context "${KUBE_CONTEXT}" get --raw=/readyz
```

Expected output:

```text
ok
```

Confirm the nodes are compatible:

```bash
kubectl --context "${KUBE_CONTEXT}" get nodes \
  -o custom-columns=NAME:.metadata.name,ARCH:.metadata.labels.kubernetes\.io/arch,OS:.metadata.labels.kubernetes\.io/os
```

## 2. Build the POC Image

ACR Tasks builds the current working tree without requiring a local Docker daemon:

```bash
az acr build \
  --registry "${ACR_NAME}" \
  --image "opensandbox/egress-extension-poc:${POC_TAG}" \
  --file components/egress/Dockerfile \
  .
```

Verify the pushed image:

```bash
az acr repository show \
  --name "${ACR_NAME}" \
  --image "opensandbox/egress-extension-poc:${POC_TAG}" \
  --query '{digest:digest,lastUpdateTime:lastUpdateTime}' \
  --output json
```

## 3. Configure Registry Pull Access

Create the isolated namespace first:

```bash
kubectl --context "${KUBE_CONTEXT}" create namespace "${POC_NAMESPACE}" \
  --dry-run=client -o yaml |
  kubectl --context "${KUBE_CONTEXT}" apply -f -
```

If the AKS kubelet identity already has `AcrPull` on the registry, skip to [Deploy the POC](#deploy-the-poc). For a temporary test without changing cluster-wide role assignments, create a namespace-scoped pull secret from a short-lived ACR token:

```bash
ACR_TOKEN="$(az acr login \
  --name "${ACR_NAME}" \
  --expose-token \
  --query accessToken \
  --output tsv)"

kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  create secret docker-registry acr-pull \
  --docker-server="${ACR_NAME}.azurecr.io" \
  --docker-username=00000000-0000-0000-0000-000000000000 \
  --docker-password="${ACR_TOKEN}" \
  --dry-run=client -o yaml |
  kubectl --context "${KUBE_CONTEXT}" apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  patch serviceaccount default --type=merge \
  --patch '{"imagePullSecrets":[{"name":"acr-pull"}]}'

unset ACR_TOKEN
```

::: tip Permanent AKS attachment
For a maintained environment, attach the registry to AKS instead of using an expiring pull secret:

```bash
az aks update \
  --resource-group <aks-resource-group> \
  --name <aks-cluster-name> \
  --attach-acr "${ACR_NAME}"
```
:::

## 4. Deploy the POC

Replace the manifest's image placeholder while applying it:

```bash
sed \
  -e "s|POC_IMAGE|${POC_IMAGE}|g" \
  -e "s|POC_NAMESPACE|${POC_NAMESPACE}|g" \
  examples/egress-extension-poc/kubernetes.yaml |
  kubectl --context "${KUBE_CONTEXT}" apply -f -

kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  rollout status deployment/egress-extension-poc --timeout=180s
```

Confirm that the explicit storage-only flag is present:

```bash
kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  get deployment egress-extension-poc \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="OPENSANDBOX_EGRESS_EXTENSIONS_POC")].value}{"\n"}'
```

Expected output:

```text
true
```

Inspect startup logs if the rollout does not complete:

```bash
kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  logs deployment/egress-extension-poc --all-containers
```

The Pod needs `CAP_NET_ADMIN` because the normal egress process installs its DNS redirect in the Pod network namespace.

## 5. Port-Forward the API

Keep this command running in a separate terminal:

```bash
kubectl --context "${KUBE_CONTEXT}" --namespace "${POC_NAMESPACE}" \
  port-forward service/egress-extension-poc 28080:18080
```

The manifest uses the demonstration token `poc-token`. The API is reachable only through the local port-forward unless you deliberately expose the Service. Local port `28080` avoids conflicts with OpenSandbox services that commonly use `18080`.

## 6. Run the Automated Verification

From another terminal at the repository root:

```bash
POC_BASE_URL=http://127.0.0.1:28080 \
POC_AUTH_TOKEN=poc-token \
bash examples/egress-extension-poc/verify.sh
```

Expected final output:

```text
POC verification passed (final revision 2)
```

The final revision can be greater than `2` when the script is run more than once. The verifier reads the current revision before each replacement.

## 7. Inspect the API Manually

Set a reusable header:

```bash
export POC_AUTH_HEADER='OPENSANDBOX-EGRESS-AUTH: poc-token'
```

Discover capabilities:

```bash
curl --silent --show-error \
  -H "${POC_AUTH_HEADER}" \
  http://127.0.0.1:28080/capabilities | jq
```

The `HTTPRoute` entry should include:

```json
{
  "apiVersion": "gateway.networking.k8s.io/v1",
  "kind": "HTTPRoute",
  "available": true,
  "operations": ["read", "replace"],
  "features": ["extensions.opensandbox.io/StorageOnlyPOC"]
}
```

Replace the extension set:

```bash
REVISION="$(curl --silent --show-error \
  -H "${POC_AUTH_HEADER}" \
  http://127.0.0.1:28080/extensions | jq -r '.revision')"

curl --silent --show-error --fail-with-body \
  -X PUT \
  -H "${POC_AUTH_HEADER}" \
  -H 'Content-Type: application/json' \
  http://127.0.0.1:28080/extensions \
  --data "$(jq -n --argjson revision "${REVISION}" '{
    expectedRevision: $revision,
    resources: [{
      apiVersion: "gateway.networking.k8s.io/v1",
      kind: "HTTPRoute",
      metadata: {
        name: "api-example",
        futureMetadata: {preserved: true}
      },
      spec: {
        hostnames: ["api.example.com"],
        futureField: {preserved: [1, 2, 3]}
      }
    }]
  }')" | jq
```

The response must contain both conditions:

```json
[
  {
    "type": "Accepted",
    "status": "True",
    "reason": "Valid"
  },
  {
    "type": "Programmed",
    "status": "Unknown",
    "reason": "PocStored"
  }
]
```

`Unknown` is intentional. It proves that the API does not claim traffic enforcement before an Envoy applier exists.

## Failure Cases

### Missing or Wrong Token

Requests return `401 Unauthorized` when `OPENSANDBOX_EGRESS_TOKEN` is configured and the header is missing or incorrect.

### POC Flag Not Enabled

`GET /capabilities` remains available, but Gateway capabilities report `available: false`. `PUT /extensions` returns:

```json
{
  "code": "CAPABILITY_UNAVAILABLE",
  "message": "extension applier is not configured"
}
```

### Stale Revision

A request whose `expectedRevision` is not current returns HTTP `409`:

```json
{
  "code": "REVISION_CONFLICT",
  "message": "expected revision 0, current revision is 1"
}
```

### Known but Unavailable Resource

`gateway.networking.k8s.io/v1alpha2` `TLSRoute` returns HTTP `412` because TLS route translation is not part of this POC.

## Cleanup

Stop the port-forward with `Ctrl+C`, then delete the isolated namespace:

```bash
kubectl --context "${KUBE_CONTEXT}" delete namespace "${POC_NAMESPACE}" --wait=true
```

Optionally remove the temporary ACR image:

```bash
az acr repository delete \
  --name "${ACR_NAME}" \
  --image "opensandbox/egress-extension-poc:${POC_TAG}" \
  --yes
```

## Testmember-3 Validation Record

This walkthrough was validated on `testmember-3-admin` on July 16, 2026. The validation covered image build and pull, rollout readiness, authentication rejection, capability discovery, authenticated replacement, unknown-field round-tripping, stale-revision rejection, unavailable-kind rejection, state clearing, and namespace cleanup.
