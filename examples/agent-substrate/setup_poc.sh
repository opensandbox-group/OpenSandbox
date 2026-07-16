#!/usr/bin/env bash
# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# This installer has two modes:
#   1. render-only (default): fetch, transform, and client-validate manifests
#   2. --apply: install the rendered resources and wait for every POC layer
# It always requires --context so it can never mutate the current context by
# accident. Existing PKI and API-key Secrets are reused on repair runs.
UPSTREAM_REVISION="c1ab0958f85202faf9f87ea66cd98903a9de763b"
UPSTREAM_REPOSITORY="https://github.com/agent-substrate/substrate.git"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KUBE_CONTEXT=""
APPLY=false
KEEP_WORK_DIR=false
WORK_DIR=""

usage() {
  cat <<'EOF'
Usage: setup_poc.sh --context CONTEXT [--apply] [--work-dir DIR] [--keep-work-dir]

Without --apply, the script fetches the pinned upstream source, renders the
AKS-compatible manifests, and runs client-side validation without changing the
cluster. --apply installs or repairs the POC, including cluster-scoped CRDs,
RBAC, and admission policy resources.

Optional image overrides:
  ATEAPI_IMAGE, ATECONTROLLER_IMAGE, ATENET_IMAGE, ATELET_IMAGE
  ATEOM_IMAGE, EXECD_IMAGE, OPENSANDBOX_SERVER_IMAGE
  STORAGE_CLASS (default: managed-csi)
EOF
}

while (($#)); do
  case "$1" in
    --context)
      KUBE_CONTEXT="${2:-}"
      shift 2
      ;;
    --apply)
      APPLY=true
      shift
      ;;
    --work-dir)
      WORK_DIR="${2:-}"
      shift 2
      ;;
    --keep-work-dir)
      KEEP_WORK_DIR=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$KUBE_CONTEXT" ]]; then
  echo "--context is required; the current kubectl context is never used implicitly." >&2
  exit 2
fi

# Fail before network access or rendering if the local toolchain is incomplete.
for command in base64 git go kubectl openssl uv; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Required command not found: $command" >&2
    exit 1
  fi
done

if ! kubectl config get-contexts "$KUBE_CONTEXT" >/dev/null 2>&1; then
  echo "Kubernetes context not found: $KUBE_CONTEXT" >&2
  exit 1
fi

if [[ -z "$WORK_DIR" ]]; then
  WORK_DIR="$(mktemp -d)"
fi
mkdir -p "$WORK_DIR"

cleanup() {
  if [[ "$KEEP_WORK_DIR" == true ]]; then
    echo "Kept generated files in $WORK_DIR"
  else
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

UPSTREAM_DIR="$WORK_DIR/substrate"
RENDERED_MANIFEST="$WORK_DIR/upstream.yaml"
SYSTEM_MANIFEST="$WORK_DIR/system.yaml"
WORKLOAD_MANIFEST="$WORK_DIR/workload.yaml"
SERVER_MANIFEST="$WORK_DIR/server.yaml"
PKI_DIR="$WORK_DIR/pki"

# Phase 1: fetch the exact upstream source used by the generated gRPC client.
# Checking the detached HEAD guards against accidentally following moving main.
echo "Fetching Agent Substrate revision $UPSTREAM_REVISION"
git clone --quiet --filter=blob:none --no-checkout "$UPSTREAM_REPOSITORY" "$UPSTREAM_DIR"
git -C "$UPSTREAM_DIR" fetch --quiet --depth=1 origin "$UPSTREAM_REVISION"
git -C "$UPSTREAM_DIR" checkout --quiet --detach FETCH_HEAD
test "$(git -C "$UPSTREAM_DIR" rev-parse HEAD)" = "$UPSTREAM_REVISION"

# Phase 2: render upstream's JWT-enabled Kind overlay, then convert it for AKS.
# prepare_manifests.py removes unsupported Pod Certificate, local DNS, and
# observability resources; replaces ko:// images; and selects managed storage.
kubectl kustomize \
  "$UPSTREAM_DIR/manifests/ate-install/kind-jwt" \
  --load-restrictor LoadRestrictionsNone >"$RENDERED_MANIFEST"

uv run --quiet --with pyyaml python "$SCRIPT_DIR/prepare_manifests.py" \
  --source "$RENDERED_MANIFEST" \
  --system-output "$SYSTEM_MANIFEST" \
  --workload-template "$SCRIPT_DIR/manifests/workload.yaml" \
  --workload-output "$WORKLOAD_MANIFEST" \
  --server-template "$SCRIPT_DIR/manifests/server.yaml" \
  --server-output "$SERVER_MANIFEST"

# This validation is intentionally before the --apply branch. The default mode
# proves all generated YAML is structurally valid without changing the cluster.
kubectl --context "$KUBE_CONTEXT" apply --dry-run=client \
  -f "$SYSTEM_MANIFEST" \
  -f "$WORKLOAD_MANIFEST" \
  -f "$SERVER_MANIFEST" >/dev/null

if [[ "$APPLY" != true ]]; then
  echo "Render and client-side validation passed for context $KUBE_CONTEXT."
  echo "Rerun with --apply to install or repair the cluster resources."
  exit 0
fi

# Phase 3: create three POC-owned namespaces. The remaining apply path includes
# cluster-scoped CRDs/RBAC, so --apply should only be used on a test cluster.
echo "Installing cluster-scoped POC resources into context $KUBE_CONTEXT"
for namespace in ate-system ate-demo-sandbox opensandbox-system; do
  kubectl --context "$KUBE_CONTEXT" create namespace "$namespace" \
    --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -
  kubectl --context "$KUBE_CONTEXT" label namespace "$namespace" \
    app.kubernetes.io/part-of=opensandbox-substrate-poc \
    opensandbox.io/poc=agent-substrate --overwrite
done

# Phase 4: establish static POC TLS. A repair run exports and reuses the current
# Secret, preserving trust and avoiding an uncoordinated certificate rotation.
# A fresh installation receives a 30-day CA/server certificate covering ateapi,
# atenet, and every Valkey service DNS name used by the control plane.
mkdir -p "$PKI_DIR"
if kubectl --context "$KUBE_CONTEXT" -n ate-system \
  get secret substrate-static-pki >/dev/null 2>&1; then
  echo "Reusing existing substrate-static-pki Secret"
  for key in ca.crt tls.crt tls.key credential-bundle.pem; do
    kubectl --context "$KUBE_CONTEXT" -n ate-system \
      get secret substrate-static-pki \
      -o "jsonpath={.data.${key//./\\.}}" | base64 --decode >"$PKI_DIR/$key"
  done
else
  openssl req -x509 -newkey rsa:2048 -nodes -days 30 \
    -subj "/CN=OpenSandbox Substrate POC CA" \
    -keyout "$PKI_DIR/ca.key" -out "$PKI_DIR/ca.crt"
  openssl req -newkey rsa:2048 -nodes \
    -subj "/CN=api.ate-system.svc" \
    -addext "subjectAltName=DNS:api,DNS:api.ate-system,DNS:api.ate-system.svc,DNS:api.ate-system.svc.cluster.local,DNS:atenet-router,DNS:atenet-router.ate-system,DNS:atenet-router.ate-system.svc,DNS:atenet-router.ate-system.svc.cluster.local,DNS:valkey-cluster,DNS:valkey-cluster.ate-system,DNS:valkey-cluster.ate-system.svc,DNS:valkey-cluster.ate-system.svc.cluster.local,DNS:*.valkey-cluster-service.ate-system.svc,DNS:*.valkey-cluster-service.ate-system.svc.cluster.local" \
    -keyout "$PKI_DIR/tls.key" -out "$PKI_DIR/tls.csr"
  openssl x509 -req -in "$PKI_DIR/tls.csr" \
    -CA "$PKI_DIR/ca.crt" -CAkey "$PKI_DIR/ca.key" -CAcreateserial \
    -days 30 -sha256 -copy_extensions copy -out "$PKI_DIR/tls.crt"
  cat "$PKI_DIR/tls.key" "$PKI_DIR/tls.crt" >"$PKI_DIR/credential-bundle.pem"
fi
openssl verify -CAfile "$PKI_DIR/ca.crt" "$PKI_DIR/tls.crt"

# ateapi, atenet, and Valkey mount the same certificate bundle. This is an
# explicit POC substitution for upstream Pod Certificate APIs unavailable here.
kubectl --context "$KUBE_CONTEXT" -n ate-system create secret generic \
  substrate-static-pki \
  --from-file=credential-bundle.pem="$PKI_DIR/credential-bundle.pem" \
  --from-file=ca.crt="$PKI_DIR/ca.crt" \
  --from-file=tls.crt="$PKI_DIR/tls.crt" \
  --from-file=tls.key="$PKI_DIR/tls.key" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -
kubectl --context "$KUBE_CONTEXT" -n ate-system create secret generic \
  valkey-ca-certs --from-file=ca.crt="$PKI_DIR/ca.crt" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -

# Phase 5: configure ateapi to authenticate Kubernetes ServiceAccount JWTs and
# connect to the in-cluster TLS-enabled Valkey cluster without cloud IAM auth.
OIDC_ISSUER="$(
  kubectl --context "$KUBE_CONTEXT" get --raw /.well-known/openid-configuration |
    uv run --quiet python -c 'import json,sys; print(json.load(sys.stdin)["issuer"])'
)"
kubectl --context "$KUBE_CONTEXT" -n ate-system create configmap \
  ate-api-server-envvars \
  --from-literal=ATE_API_REDIS_ADDRESS=valkey-cluster.ate-system.svc:6379 \
  --from-literal=ATE_API_REDIS_USE_IAM_AUTH=false \
  --from-literal=ATE_API_REDIS_TLS_SERVER_NAME=valkey-cluster.ate-system.svc \
  --from-literal=ATE_API_REDIS_CLIENT_CERT=/run/servicedns.podcert.ate.dev/credential-bundle.pem \
  --from-literal=ATE_API_K8SJWT_ISSUER="$OIDC_ISSUER" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -

kubectl --context "$KUBE_CONTEXT" -n ate-system label \
  secret substrate-static-pki valkey-ca-certs \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite
kubectl --context "$KUBE_CONTEXT" -n ate-system label \
  configmap ate-api-server-envvars \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite

# Phase 6: install the pinned public APIs and validation policy first, then the
# transformed control plane. Server-side dry-run lets admission policy reject
# privileged/hostPath resources before the actual system workloads are applied.
kubectl --context "$KUBE_CONTEXT" apply --dry-run=server -f "$SYSTEM_MANIFEST" >/dev/null
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$UPSTREAM_DIR/manifests/ate-install/generated"
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$UPSTREAM_DIR/manifests/ate-install/sandboxconfig-validation.yaml"
kubectl --context "$KUBE_CONTEXT" apply \
  -f "$UPSTREAM_DIR/manifests/ate-install/sandboxconfig-gvisor.yaml"

kubectl --context "$KUBE_CONTEXT" label crd \
  actortemplates.ate.dev workerpools.ate.dev sandboxconfigs.ate.dev \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite
kubectl --context "$KUBE_CONTEXT" label validatingadmissionpolicy \
  sandboxconfig-assets app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite
kubectl --context "$KUBE_CONTEXT" label validatingadmissionpolicybinding \
  sandboxconfig-assets app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite
kubectl --context "$KUBE_CONTEXT" label sandboxconfig gvisor-default \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite

# Wait for storage, control, routing, and node-runtime layers independently so
# a failed installation points to the component that did not become healthy.
kubectl --context "$KUBE_CONTEXT" apply -f "$SYSTEM_MANIFEST"
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status deployment/ate-api-server-deployment --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status deployment/ate-controller --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status deployment/atenet-router --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status deployment/rustfs --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status statefulset/valkey-cluster --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  rollout status daemonset/atelet --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  wait --for=condition=complete job/rustfs-bucket-init --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-system \
  wait --for=condition=complete job/valkey-cluster-init --timeout=10m

# Phase 7: create two warm gVisor Workers and the administrator-owned execd
# ActorTemplate. Ready=True means its golden Actor snapshot is usable.
kubectl --context "$KUBE_CONTEXT" apply --dry-run=server -f "$WORKLOAD_MANIFEST" >/dev/null
kubectl --context "$KUBE_CONTEXT" apply -f "$WORKLOAD_MANIFEST"
kubectl --context "$KUBE_CONTEXT" -n ate-demo-sandbox \
  rollout status deployment/sandbox-workerpool-deployment --timeout=10m
kubectl --context "$KUBE_CONTEXT" -n ate-demo-sandbox \
  wait --for=condition=Ready actortemplate/opensandbox-execd --timeout=10m

# Phase 8: Actors require a pre-created Atespace. Build the CLI from the same
# pinned source and create the single-tenant boundary only when it is absent.
KUBECTL_ATE="$WORK_DIR/kubectl-ate"
(
  cd "$UPSTREAM_DIR"
  go build -o "$KUBECTL_ATE" ./cmd/kubectl-ate
)
if ! "$KUBECTL_ATE" --context "$KUBE_CONTEXT" \
  get atespace opensandbox -o json >/dev/null 2>&1; then
  "$KUBECTL_ATE" --context "$KUBE_CONTEXT" create atespace opensandbox
fi

# Phase 9: keep the OpenSandbox API key stable across repair runs, publish the
# ateapi CA to the server namespace, and deploy the single-replica POC server.
if ! kubectl --context "$KUBE_CONTEXT" -n opensandbox-system \
  get secret opensandbox-api-key >/dev/null 2>&1; then
  API_KEY="$(openssl rand -hex 24)"
  kubectl --context "$KUBE_CONTEXT" -n opensandbox-system \
    create secret generic opensandbox-api-key \
    --from-literal=api-key="$API_KEY"
fi
kubectl --context "$KUBE_CONTEXT" -n opensandbox-system create configmap \
  agent-substrate-ca --from-file=ca.crt="$PKI_DIR/ca.crt" \
  --dry-run=client -o yaml | kubectl --context "$KUBE_CONTEXT" apply -f -
kubectl --context "$KUBE_CONTEXT" -n opensandbox-system label \
  secret opensandbox-api-key \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite
kubectl --context "$KUBE_CONTEXT" -n opensandbox-system label \
  configmap agent-substrate-ca \
  app.kubernetes.io/part-of=opensandbox-substrate-poc \
  opensandbox.io/poc=agent-substrate --overwrite

kubectl --context "$KUBE_CONTEXT" apply --dry-run=server -f "$SERVER_MANIFEST" >/dev/null
kubectl --context "$KUBE_CONTEXT" apply -f "$SERVER_MANIFEST"
kubectl --context "$KUBE_CONTEXT" -n opensandbox-system \
  rollout status deployment/opensandbox-server --timeout=10m

# At this point the control plane is ready; test_poc.py validates actual Actor
# create, routing, suspend, automatic resume, command execution, and deletion.
echo "Agent Substrate POC is ready on context $KUBE_CONTEXT."
echo "Run the test steps in docs/examples/agent-substrate.md."