#!/usr/bin/env bash
# Parent-owned prerequisite provisioning and exact namespace cleanup for the
# ensureSubPathDirectory remote Kubernetes POC.

set -euo pipefail

readonly POC_PREFIX='opensandbox-subpath-poc-'

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

note() {
  printf '%s\n' "$*"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "Required command is unavailable: $1"
}

validate_namespace() {
  [[ "$POC_NAMESPACE" =~ ^opensandbox-subpath-poc-[a-f0-9]{16}$ ]] || die \
    "POC_NAMESPACE must be exactly ${POC_PREFIX}<16 lowercase hex characters>."
  [[ "$POC_NAMESPACE" != 'sandbox-tenant-a' ]] || die 'sandbox-tenant-a is never an allowed POC namespace.'
}

validate_names() {
  [[ "$POC_SERVICE_ACCOUNT" == "${POC_PREFIX}sa-${POC_TOKEN}" ]] || die \
    "POC ServiceAccount must exactly be ${POC_PREFIX}sa-${POC_TOKEN}."
  [[ "$POC_IMAGE_PULL_SECRET" == "${POC_PREFIX}pull-${POC_TOKEN}" ]] || die \
    "POC imagePullSecret must exactly be ${POC_PREFIX}pull-${POC_TOKEN}."
}

secure_remove() {
  local path="$1"
  [[ -e "$path" ]] || return 0
  shred -u -- "$path" || {
    printf 'ERROR: unable to securely erase temporary credential material: %s\n' "$path" >&2
    exit 1
  }
}

credential_file=''
cleanup_local_credentials() {
  local status=$?
  trap - EXIT
  if [[ -n "$credential_file" ]]; then
    secure_remove "$credential_file"
  fi
  exit "$status"
}

write_registry_docker_config() {
  credential_file="$(mktemp "${TMPDIR:-/tmp}/opensandbox-subpath-poc-dockerconfig.XXXXXX")"
  chmod 600 "$credential_file"
  REMOTE_POC_DOCKER_CONFIG="$REMOTE_POC_DOCKER_CONFIG" \
    REMOTE_POC_DOCKER_AUTH_KEY="$REMOTE_POC_DOCKER_AUTH_KEY" python3 - "$credential_file" <<'PY'
import json
import os
import sys

source_path = os.environ["REMOTE_POC_DOCKER_CONFIG"]
auth_key = os.environ["REMOTE_POC_DOCKER_AUTH_KEY"]
target_path = sys.argv[1]
with open(source_path, encoding="utf-8") as source_file:
    source = json.load(source_file)
auths = source.get("auths")
if not isinstance(auths, dict):
    raise SystemExit("Local Docker config has no auths object.")
registry_auth = auths.get(auth_key)
if not isinstance(registry_auth, dict) or not registry_auth:
    raise SystemExit("Local Docker config has no auth entry for REMOTE_POC_DOCKER_AUTH_KEY.")
with open(target_path, "w", encoding="utf-8") as target_file:
    json.dump({"auths": {auth_key: registry_auth}}, target_file, separators=(",", ":"))
PY
}

check_provision_permissions() {
  kubectl --kubeconfig "$KUBECONFIG" auth can-i get namespaces | grep -qx yes || die \
    'The supplied KUBECONFIG cannot determine whether the exact POC Namespace exists.'
  kubectl --kubeconfig "$KUBECONFIG" auth can-i create namespaces | grep -qx yes || die \
    'The supplied KUBECONFIG cannot create the exact POC Namespace.'
  kubectl --kubeconfig "$KUBECONFIG" auth can-i create secrets -n "$POC_NAMESPACE" | grep -qx yes || die \
    'The supplied KUBECONFIG cannot create the exact POC imagePullSecret.'
  kubectl --kubeconfig "$KUBECONFIG" auth can-i create serviceaccounts -n "$POC_NAMESPACE" | grep -qx yes || die \
    'The supplied KUBECONFIG cannot create the exact POC ServiceAccount.'
  kubectl --kubeconfig "$KUBECONFIG" auth can-i patch serviceaccounts -n "$POC_NAMESPACE" | grep -qx yes || die \
    'The supplied KUBECONFIG cannot add the exact imagePullSecret reference to the POC ServiceAccount.'
}

check_cleanup_permissions() {
  kubectl --kubeconfig "$KUBECONFIG" auth can-i get namespaces | grep -qx yes || die \
    'The supplied KUBECONFIG cannot inspect the exact POC Namespace.'
  kubectl --kubeconfig "$KUBECONFIG" auth can-i delete namespaces | grep -qx yes || die \
    'The supplied KUBECONFIG cannot delete the exact POC Namespace.'
}

assert_cleanup_namespace_is_exactly_prerequisites() {
  local resource object
  while IFS= read -r resource; do
    [[ -n "$resource" ]] || continue
    kubectl --kubeconfig "$KUBECONFIG" auth can-i list "$resource" -n "$POC_NAMESPACE" | grep -qx yes || die \
      "Cannot list ${resource} in ${POC_NAMESPACE}; refusing namespace cleanup."
    object="$(kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
      get "$resource" -o name)" || die \
      "Cannot list ${resource} in ${POC_NAMESPACE}; refusing namespace cleanup."
    [[ -n "$object" ]] || continue

    case "$resource" in
      events|events.events.k8s.io)
        # The exact, random POC Namespace may contain ordinary runner Events.
        ;;
      serviceaccounts)
        while IFS= read -r object; do
          [[ -z "$object" ]] && continue
          [[ "$object" == 'serviceaccount/default' || "$object" == "serviceaccount/${POC_SERVICE_ACCOUNT}" ]] || die \
            "Unexpected resource ${object}; refusing namespace cleanup."
        done <<<"$object"
        ;;
      secrets)
        while IFS= read -r object; do
          [[ -z "$object" ]] && continue
          [[ "$object" == "secret/${POC_IMAGE_PULL_SECRET}" \
            || "$object" =~ ^secret/default-token-[a-z0-9]{5}$ \
            || "$object" =~ ^secret/"${POC_SERVICE_ACCOUNT}"-token-[a-z0-9]{5}$ ]] || die \
            "Unexpected resource ${object}; refusing namespace cleanup."
        done <<<"$object"
        ;;
      configmaps)
        while IFS= read -r object; do
          [[ -z "$object" ]] && continue
          [[ "$object" == 'configmap/kube-root-ca.crt' || "$object" == 'configmap/tapp-env-config' ]] || die \
            "Unexpected resource ${object}; refusing namespace cleanup."
        done <<<"$object"
        ;;
      *)
        die "Unexpected POC Namespace content (${resource}: ${object}); refusing namespace cleanup."
        ;;
    esac
  done < <(kubectl --kubeconfig "$KUBECONFIG" api-resources --namespaced=true --verbs=list -o name)
}

require_command kubectl
require_command python3
require_command mktemp
require_command chmod
require_command shred

[[ -n "${KUBECONFIG:-}" ]] || die 'KUBECONFIG must be explicitly set to one readable kubeconfig file.'
[[ -f "$KUBECONFIG" && -r "$KUBECONFIG" ]] || die 'KUBECONFIG must name one readable regular file.'
[[ -n "${POC_NAMESPACE:-}" ]] || die \
  'POC_NAMESPACE must be explicitly set to a newly generated random POC Namespace.'

POC_TOKEN="${POC_NAMESPACE#"$POC_PREFIX"}"
POC_SERVICE_ACCOUNT="${REMOTE_POC_SERVICE_ACCOUNT:-${POC_PREFIX}sa-${POC_TOKEN}}"
POC_IMAGE_PULL_SECRET="${REMOTE_POC_IMAGE_PULL_SECRET:-${POC_PREFIX}pull-${POC_TOKEN}}"
validate_namespace
validate_names

mode="${REMOTE_POC_PROVISION_MODE:-provision}"
case "$mode" in
  provision)
    note 'OpenSandbox remote Kubernetes POC prerequisite provisioning'
    note "Target Namespace:       ${POC_NAMESPACE}"
    note "Target ServiceAccount:  ${POC_SERVICE_ACCOUNT}"
    note "Target imagePullSecret: ${POC_IMAGE_PULL_SECRET}"
    note 'Planned writes: exact Namespace, exact dockerconfigjson Secret, exact ServiceAccount only.'
    note 'The Secret is derived only from the configured local Docker config auth entry and is never printed.'
    if [[ "${REMOTE_POC_PROVISION_CONFIRM:-}" != '1' ]]; then
      note 'Dry run only: no Kubernetes API request and no local Docker credential read was made.'
      note 'To provision, set REMOTE_POC_PROVISION_CONFIRM=1 after parent approval.'
      exit 0
    fi
    [[ -n "${REMOTE_POC_DOCKER_CONFIG:-}" ]] || die \
      'REMOTE_POC_DOCKER_CONFIG must name the local Docker config containing the selected registry auth.'
    [[ -f "$REMOTE_POC_DOCKER_CONFIG" && -r "$REMOTE_POC_DOCKER_CONFIG" ]] || die \
      'REMOTE_POC_DOCKER_CONFIG must name one readable regular file.'
    [[ -n "${REMOTE_POC_DOCKER_AUTH_KEY:-}" ]] || die \
      'REMOTE_POC_DOCKER_AUTH_KEY must name the exact auths key to copy from the local Docker config.'
    check_provision_permissions
    if kubectl --kubeconfig "$KUBECONFIG" get namespace "$POC_NAMESPACE" >/dev/null 2>&1; then
      die "Refusing to provision an existing Namespace: ${POC_NAMESPACE}"
    fi
    trap cleanup_local_credentials EXIT
    write_registry_docker_config
    kubectl --kubeconfig "$KUBECONFIG" create namespace "$POC_NAMESPACE"
    kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" create secret generic "$POC_IMAGE_PULL_SECRET" \
      --type=kubernetes.io/dockerconfigjson \
      --from-file=.dockerconfigjson="$credential_file"
    secure_remove "$credential_file"
    credential_file=''
    kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" create serviceaccount "$POC_SERVICE_ACCOUNT"
    kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" patch serviceaccount "$POC_SERVICE_ACCOUNT" \
      --type=merge \
      -p "{\"imagePullSecrets\":[{\"name\":\"${POC_IMAGE_PULL_SECRET}\"}]}"
    note 'Provisioning completed. Parent owns the Namespace and prerequisite cleanup.'
    ;;
  cleanup)
    note 'OpenSandbox remote Kubernetes POC parent cleanup'
    note "Target Namespace: ${POC_NAMESPACE}"
    note 'Cleanup permits only default ServiceAccount, exact POC ServiceAccount, exact POC imagePullSecret, generated default and POC ServiceAccount token Secrets, configmap/kube-root-ca.crt, configmap/tapp-env-config, and Events only because this is the exact random POC Namespace.'
    note 'It refuses cleanup if any PVC, BatchSandbox, Pod, or other namespaced object remains.'
    if [[ "${REMOTE_POC_CLEANUP_CONFIRM:-}" != '1' ]]; then
      note 'Dry run only: no Kubernetes API request was made.'
      note 'To clean up, set REMOTE_POC_CLEANUP_CONFIRM=1 after the runner has removed its sandbox and PVC and shared-storage impact is confirmed.'
      exit 0
    fi
    [[ "${REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM:-}" == '1' ]] || die \
      'Set REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1 only after confirming Namespace deletion cannot affect shared backing data.'
    check_cleanup_permissions
    kubectl --kubeconfig "$KUBECONFIG" get namespace "$POC_NAMESPACE" >/dev/null || die \
      "Required POC Namespace is absent: ${POC_NAMESPACE}"
    assert_cleanup_namespace_is_exactly_prerequisites
    kubectl --kubeconfig "$KUBECONFIG" delete namespace "$POC_NAMESPACE" --wait=false
    note "Requested deletion of exact POC Namespace: ${POC_NAMESPACE}"
    ;;
  *)
    die 'REMOTE_POC_PROVISION_MODE must be provision (default) or cleanup.'
    ;;
esac
