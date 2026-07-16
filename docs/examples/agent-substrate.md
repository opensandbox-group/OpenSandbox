---
title: Agent Substrate POC
description: Validate the Agent Substrate workload-provider POC through OpenSandbox REST APIs and atenet.
---

# Agent Substrate POC

This guide installs or repairs a single-tenant Agent Substrate POC deployment,
then validates the OpenSandbox REST lifecycle, atenet routing, durable suspend,
automatic resume, execd command execution, and cleanup.

::: warning POC limitations
The POC uses one OpenSandbox server replica and process-local workload records.
Restarting the server loses its sandbox-to-Actor mappings. The deployment also
uses temporary static ateapi TLS material. Do not treat this setup as a
production deployment.
:::

## Prerequisites

- Administrator `kubectl` access to the target test cluster.
- `git`, `go`, `openssl`, and `uv` for setup and validation.
- Linux `amd64` nodes and the `managed-csi` StorageClass.
- Cluster pull access to the default POC images, or equivalent image overrides.
- Three terminals for testing: one for each port-forward and one for the test.

The commands below use the AKS POC context. Override the first variable when
testing another deployment. Every cluster command uses an explicit context and
does not change your global kubeconfig context.

```shell
export KUBE_CONTEXT=testmember-5-admin
export ATE_NAMESPACE=ate-system
export WORKER_NAMESPACE=ate-demo-sandbox
export OPENSANDBOX_NAMESPACE=opensandbox-system
```

## Install or repair the POC

The setup entry point is
[`examples/agent-substrate/setup_poc.sh`](https://github.com/opensandbox-group/OpenSandbox/blob/main/examples/agent-substrate/setup_poc.sh).
It fetches Agent Substrate commit
`c1ab0958f85202faf9f87ea66cd98903a9de763b`, renders an AKS-compatible
manifest, creates temporary static TLS material on a fresh installation, and
waits for the control plane, Workers, ActorTemplate, Atespace, and OpenSandbox
server to become ready.

Run the safe render and client-validation path first. It does not change the
cluster:

```shell
./examples/agent-substrate/setup_poc.sh --context "$KUBE_CONTEXT"
```

Install or repair the POC only after reviewing the target context:

```shell
./examples/agent-substrate/setup_poc.sh \
  --context "$KUBE_CONTEXT" \
  --apply
```

::: warning Cluster-scoped resources
`--apply` creates or updates Agent Substrate CRDs, ClusterRoles,
ClusterRoleBindings, a ValidatingAdmissionPolicy, and a SandboxConfig. It also
creates seven 1 Gi PVCs and a privileged `atelet` DaemonSet. Use a dedicated
test cluster and an administrator context.
:::

The defaults reference the immutable POC images in
`ryanclaw.azurecr.io/opensandbox-substrate-poc`. The target cluster must have
pull access to that registry. To use another registry, set the image variables
shown by `setup_poc.sh --help` to equivalent immutable image references.

The script is idempotent for this POC. On an existing installation it reuses
`substrate-static-pki` and `opensandbox-api-key` instead of rotating credentials.
On a fresh installation the generated static certificate is valid for 30 days.

### Manual staged installation

The automated installer is the canonical and idempotent path. Use the following
procedure on a fresh, dedicated test cluster when you need to inspect each
boundary before applying it.

First render and retain the exact manifests without changing the cluster:

```shell
export WORK_DIR="$(mktemp -d)"

./examples/agent-substrate/setup_poc.sh \
  --context "$KUBE_CONTEXT" \
  --work-dir "$WORK_DIR" \
  --keep-work-dir

ls "$WORK_DIR"/{system,workload,server}.yaml
```

Create the three namespaces:

```shell
for namespace in ate-system ate-demo-sandbox opensandbox-system; do
  kubectl --context "$KUBE_CONTEXT" create namespace "$namespace" \
    --dry-run=client -o yaml |
    kubectl --context "$KUBE_CONTEXT" apply -f -
done
```

Generate the 30-day static POC CA and service certificate. The SANs cover every
service name used by ateapi, atenet, and Valkey:

```shell
mkdir -p "$WORK_DIR/pki"

openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
  -subj "/CN=OpenSandbox Substrate POC CA" \
  -keyout "$WORK_DIR/pki/ca.key" \
  -out "$WORK_DIR/pki/ca.crt"

openssl req -newkey rsa:2048 -nodes \
  -subj "/CN=api.ate-system.svc" \
  -addext "subjectAltName=DNS:api,DNS:api.ate-system,DNS:api.ate-system.svc,DNS:api.ate-system.svc.cluster.local,DNS:atenet-router,DNS:atenet-router.ate-system,DNS:atenet-router.ate-system.svc,DNS:atenet-router.ate-system.svc.cluster.local,DNS:valkey-cluster,DNS:valkey-cluster.ate-system,DNS:valkey-cluster.ate-system.svc,DNS:valkey-cluster.ate-system.svc.cluster.local,DNS:*.valkey-cluster-service.ate-system.svc,DNS:*.valkey-cluster-service.ate-system.svc.cluster.local" \
  -keyout "$WORK_DIR/pki/tls.key" \
  -out "$WORK_DIR/pki/tls.csr"

openssl x509 -req \
  -in "$WORK_DIR/pki/tls.csr" \
  -CA "$WORK_DIR/pki/ca.crt" \
  -CAkey "$WORK_DIR/pki/ca.key" \
  -CAcreateserial -days 30 -sha256 -copy_extensions copy \
  -out "$WORK_DIR/pki/tls.crt"

cat "$WORK_DIR/pki/tls.key" "$WORK_DIR/pki/tls.crt" \
  > "$WORK_DIR/pki/credential-bundle.pem"
openssl verify -CAfile "$WORK_DIR/pki/ca.crt" "$WORK_DIR/pki/tls.crt"
```

Create the TLS inputs and configure ateapi for the cluster's ServiceAccount
OIDC issuer and the in-cluster Valkey service:

```shell
kubectl --context "$KUBE_CONTEXT" -n ate-system create secret generic \
  substrate-static-pki \
  --from-file=credential-bundle.pem="$WORK_DIR/pki/credential-bundle.pem" \
  --from-file=ca.crt="$WORK_DIR/pki/ca.crt" \
  --from-file=tls.crt="$WORK_DIR/pki/tls.crt" \
  --from-file=tls.key="$WORK_DIR/pki/tls.key"

kubectl --context "$KUBE_CONTEXT" -n ate-system create secret generic \
  valkey-ca-certs --from-file=ca.crt="$WORK_DIR/pki/ca.crt"

export OIDC_ISSUER="$(
  kubectl --context "$KUBE_CONTEXT" \
    get --raw /.well-known/openid-configuration |
    uv run --quiet python -c 'import json,sys; print(json.load(sys.stdin)["issuer"])'
)"

kubectl --context "$KUBE_CONTEXT" -n ate-system create configmap \
  ate-api-server-envvars \
  --from-literal=ATE_API_REDIS_ADDRESS=valkey-cluster.ate-system.svc:6379 \
  --from-literal=ATE_API_REDIS_USE_IAM_AUTH=false \
  --from-literal=ATE_API_REDIS_TLS_SERVER_NAME=valkey-cluster.ate-system.svc \
  --from-literal=ATE_API_REDIS_CLIENT_CERT=/run/servicedns.podcert.ate.dev/credential-bundle.pem \
  --from-literal=ATE_API_K8SJWT_ISSUER="$OIDC_ISSUER"
```

Install the pinned APIs and validate the transformed system bundle through AKS
admission before applying it:

```shell
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$WORK_DIR/substrate/manifests/ate-install/generated"
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$WORK_DIR/substrate/manifests/ate-install/sandboxconfig-validation.yaml"
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$WORK_DIR/substrate/manifests/ate-install/sandboxconfig-gvisor.yaml"

kubectl --context "$KUBE_CONTEXT" apply --dry-run=server \
  -f "$WORK_DIR/system.yaml"
kubectl --context "$KUBE_CONTEXT" apply -f "$WORK_DIR/system.yaml"

kubectl --context "$KUBE_CONTEXT" -n ate-system \
  wait --for=condition=Available deployment --all --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status statefulset/valkey-cluster --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status daemonset/atelet --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  wait --for=condition=complete job --all --timeout=10m
```

Create and verify the two warm Workers and the execd golden snapshot:

```shell
kubectl --context "$KUBE_CONTEXT" apply --dry-run=server \
  -f "$WORK_DIR/workload.yaml"
kubectl --context "$KUBE_CONTEXT" apply -f "$WORK_DIR/workload.yaml"
kubectl --context "$KUBE_CONTEXT" -n ate-demo-sandbox \
  rollout status deployment/sandbox-workerpool-deployment --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-demo-sandbox \
  wait --for=condition=Ready actortemplate/opensandbox-execd --timeout=10m
```

Build the pinned administrative CLI and create the single-tenant Atespace:

```shell
(
  cd "$WORK_DIR/substrate"
  go build -o "$WORK_DIR/kubectl-ate" ./cmd/kubectl-ate
)

"$WORK_DIR/kubectl-ate" \
  --context "$KUBE_CONTEXT" \
  create atespace opensandbox
```

Finally, create an OpenSandbox API key, publish the ateapi CA to the server
namespace, and apply the server resources:

```shell
export POC_API_KEY="$(openssl rand -hex 24)"

kubectl --context "$KUBE_CONTEXT" -n opensandbox-system \
  create secret generic opensandbox-api-key \
  --from-literal=api-key="$POC_API_KEY"

kubectl --context "$KUBE_CONTEXT" -n opensandbox-system create configmap \
  agent-substrate-ca --from-file=ca.crt="$WORK_DIR/pki/ca.crt"

kubectl --context "$KUBE_CONTEXT" apply --dry-run=server \
  -f "$WORK_DIR/server.yaml"
kubectl --context "$KUBE_CONTEXT" apply -f "$WORK_DIR/server.yaml"
kubectl --context "$KUBE_CONTEXT" -n opensandbox-system \
  rollout status deployment/opensandbox-server --timeout=10m

unset POC_API_KEY
```

The manual commands intentionally target a fresh installation. Use
`setup_poc.sh --apply` for repair runs because it preserves existing PKI and API
keys and reapplies ownership labels.

## 1. Check the deployment

Confirm the OpenSandbox server, Agent Substrate control plane, WorkerPool, and
ActorTemplate are healthy:

```shell
kubectl --context "$KUBE_CONTEXT" \
  -n "$OPENSANDBOX_NAMESPACE" \
  rollout status deployment/opensandbox-server --timeout=2m

kubectl --context "$KUBE_CONTEXT" \
  -n "$ATE_NAMESPACE" \
  rollout status deployment/ate-api-server-deployment --timeout=2m

kubectl --context "$KUBE_CONTEXT" \
  -n "$ATE_NAMESPACE" \
  rollout status deployment/atenet-router --timeout=2m

kubectl --context "$KUBE_CONTEXT" \
  -n "$ATE_NAMESPACE" \
  rollout status daemonset/atelet --timeout=2m

kubectl --context "$KUBE_CONTEXT" \
  -n "$WORKER_NAMESPACE" \
  wait --for=condition=Ready actortemplate/opensandbox-execd --timeout=2m

kubectl --context "$KUBE_CONTEXT" \
  -n "$WORKER_NAMESPACE" \
  get workerpool/sandbox-workerpool actortemplate/opensandbox-execd

kubectl --context "$KUBE_CONTEXT" \
  -n "$WORKER_NAMESPACE" \
  get pods -l 'ate.dev/worker-pool=sandbox-workerpool'
```

Expected state:

- `opensandbox-server`, `ate-api-server-deployment`, and `atenet-router` are
  available.
- every `atelet` DaemonSet pod is Ready.
- `opensandbox-execd` reports `Ready=True`.
- exactly two `sandbox-workerpool` pods are Running before the test.

Record the Worker pod count. The same two pods should remain after sandbox
creation because an Actor is assigned to an existing Worker rather than getting
a per-sandbox Pod. The `workload=sandbox` label belongs to the WorkerPool and is
used by the ActorTemplate's Worker selector; the generated Worker Pods use the
controller-owned `ate.dev/worker-pool=sandbox-workerpool` label.

```shell
export WORKER_PODS_BEFORE="$(
  kubectl --context "$KUBE_CONTEXT" \
    -n "$WORKER_NAMESPACE" \
    get pods -l 'ate.dev/worker-pool=sandbox-workerpool' --no-headers | wc -l
)"
test "$WORKER_PODS_BEFORE" -eq 2
```

## 2. Forward the OpenSandbox API

In the first terminal, expose the ClusterIP service on localhost:

```shell
kubectl --context "$KUBE_CONTEXT" \
  -n "$OPENSANDBOX_NAMESPACE" \
  port-forward --address 127.0.0.1 service/opensandbox-server 8080:80
```

Keep this command running. In the test terminal, verify the unauthenticated
health endpoint:

```shell
curl --fail --silent --show-error http://127.0.0.1:8080/health
```

Expected response:

```json
{"status":"healthy"}
```

## 3. Forward atenet

In the second terminal, expose the atenet router on localhost:

```shell
kubectl --context "$KUBE_CONTEXT" \
  -n "$ATE_NAMESPACE" \
  port-forward --address 127.0.0.1 service/atenet-router 8000:80
```

Keep this command running. The test sends Actor traffic to this local address
while preserving the Actor `Host` header returned by OpenSandbox.

## 4. Load the API key

In the test terminal, read the namespaced API key without printing it:

```shell
export OPEN_SANDBOX_API_KEY="$(
  kubectl --context "$KUBE_CONTEXT" \
    -n "$OPENSANDBOX_NAMESPACE" \
    get secret opensandbox-api-key \
    -o jsonpath='{.data.api-key}' | base64 --decode
)"

export OPENSANDBOX_API_URL=http://127.0.0.1:8080
export ATENET_URL=http://127.0.0.1:8000
export AGENT_SUBSTRATE_TEMPLATE_KEY=execd
```

## 5. Run the lifecycle check

From the repository root, run:

```shell
uv run --with httpx python examples/agent-substrate/test_poc.py
```

The script performs these operations through public interfaces:

1. creates a sandbox with the administrator-registered `execd` template key
2. confirms the sandbox is Running
3. resolves logical port `44772` to an atenet Actor Host
4. reaches execd through atenet
5. pauses the sandbox using durable `SuspendActor`
6. sends atenet traffic and confirms the Actor automatically resumes
7. executes a command through execd
8. deletes the sandbox and confirms it is no longer available

Expected output resembles:

```text
[1/8] created <sandbox-id>
[2/8] fetched running sandbox
[3/8] resolved atenet host osb-<sandbox-id>.opensandbox.actors.resources.substrate.ate.dev
[4/8] reached execd through atenet
[5/8] durably suspended sandbox
[6/8] atenet auto-resumed sandbox
[7/8] executed command through execd
[8/8] deleted <sandbox-id>
```

The script deletes its test sandbox in a `finally` block when a sandbox ID was
successfully allocated.

## 6. Verify cleanup and multiplexing

Confirm the Worker pod count is unchanged and the OpenSandbox server has no
remaining sandbox record:

```shell
export WORKER_PODS_AFTER="$(
  kubectl --context "$KUBE_CONTEXT" \
    -n "$WORKER_NAMESPACE" \
    get pods -l 'ate.dev/worker-pool=sandbox-workerpool' --no-headers | wc -l
)"
test "$WORKER_PODS_AFTER" -eq "$WORKER_PODS_BEFORE"

curl --fail --silent --show-error \
  -H "OPEN-SANDBOX-API-KEY: $OPEN_SANDBOX_API_KEY" \
  "$OPENSANDBOX_API_URL/v1/sandboxes"
```

The list must not contain the sandbox ID printed by the smoke test. No new Pod,
BatchSandbox, or kubernetes-sigs Sandbox should have been created for it.
Deleting the test Actor releases its assigned Worker back to the pool; the
WorkerPool controller continues to maintain its two Worker Pods.

Stop both port-forwards with `Ctrl+C`, then remove the API key from the shell:

```shell
unset OPEN_SANDBOX_API_KEY
```

## Troubleshooting

### HTTP 401 from OpenSandbox

Reload `OPEN_SANDBOX_API_KEY` from the `opensandbox-api-key` Secret and ensure
the header name is exactly `OPEN-SANDBOX-API-KEY`.

### Connection refused on ports 8080 or 8000

Check that both port-forward commands are still running and that no other local
process is using either port.

### Actor creation reports unavailable capacity

Check the two Worker pods and recent events:

```shell
kubectl --context "$KUBE_CONTEXT" \
  -n "$WORKER_NAMESPACE" get pods -o wide
kubectl --context "$KUBE_CONTEXT" \
  -n "$WORKER_NAMESPACE" get events --sort-by=.lastTimestamp
```

### TLS verification or certificate errors

Inspect the temporary ateapi certificate validity window:

```shell
kubectl --context "$KUBE_CONTEXT" \
  -n "$ATE_NAMESPACE" \
  get secret substrate-static-pki \
  -o jsonpath='{.data.tls\.crt}' |
  base64 --decode |
  openssl x509 -noout -subject -issuer -dates
```

Replace the static POC certificate and restart dependent components before it
expires. Production deployment requires managed certificate rotation.

## References

- [OSEP-0017](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0017-agent-substrate-workload-provider.md)
- [Agent Substrate configuration](/getting-started/configuration#agent-substrate-poc)
- [Runnable smoke test](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/agent-substrate)