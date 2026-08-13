#!/usr/bin/env bash
# Safe remote Kubernetes POC for ensureSubPathDirectory.
#
# This runner is deliberately dry-run by default. It creates workloads only
# through the OpenSandbox lifecycle API and uses kubectl only for approved
# prerequisite inspection, observation, terminal verification, and cleanup of
# the exact sandbox/PVC created by this runner's lifecycle request.

set -euo pipefail

readonly POC_PREFIX='opensandbox-subpath-poc-'
readonly POC_LABEL_KEY='opensandbox.io/id'
readonly POC_MOUNT_PATH='/mnt/opensandbox-subpath-poc'

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
    "POC_NAMESPACE must be a random namespace in the exact form ${POC_PREFIX}<16 lowercase hex characters>."
  [[ "$POC_NAMESPACE" != 'sandbox-tenant-a' ]] || die 'sandbox-tenant-a is never an allowed POC namespace.'
}

validate_base_url() {
  python3 - "$REMOTE_POC_BASE_URL" <<'PY'
import sys
from urllib.parse import urlparse

value = sys.argv[1]
parsed = urlparse(value)
if parsed.scheme not in {"http", "https"} or not parsed.hostname:
    raise SystemExit("REMOTE_POC_BASE_URL must be an absolute http(s) URL.")
if parsed.username or parsed.password or parsed.query or parsed.fragment:
    raise SystemExit("REMOTE_POC_BASE_URL must not embed credentials, a query, or a fragment.")
if parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
    raise SystemExit("REMOTE_POC_BASE_URL must target an isolated local OpenSandbox Server on a loopback host.")
try:
    port = parsed.port
except ValueError as exc:
    raise SystemExit(f"REMOTE_POC_BASE_URL has an invalid port: {exc}")
if port in {18080, 18090}:
    raise SystemExit("REMOTE_POC_BASE_URL must not use formal service ports 18080 or 18090.")
PY
}

validate_storage_class() {
  python3 - "$REMOTE_POC_STORAGE_CLASS" <<'PY'
import re
import sys

storage_class = sys.argv[1]
dns1123_subdomain = re.compile(
    r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*"
)
if len(storage_class) > 253 or not dns1123_subdomain.fullmatch(storage_class):
    raise SystemExit(
        "REMOTE_POC_STORAGE_CLASS must be a nonempty Kubernetes DNS-1123 subdomain suitable for storageClassName."
    )
PY
}

validate_create_request_timeout() {
  [[ "$REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS" =~ ^[0-9]+$ ]] || die \
    'REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS must be an integer from 61 through 300.'
  (( 10#$REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS >= 61 && 10#$REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS <= 300 )) || die \
    'REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS must be an integer from 61 through 300.'
}

validate_local_server_config() {
  python3 - "$REMOTE_POC_SERVER_CONFIG" "$POC_NAMESPACE" "$REMOTE_POC_SERVICE_ACCOUNT" <<'PY'
import os
import re
import sys
try:
    import tomllib
except ModuleNotFoundError as exc:
    raise SystemExit("python3 >= 3.11 with tomllib is required to validate REMOTE_POC_SERVER_CONFIG.") from exc

config_path, namespace, service_account = sys.argv[1:]
with open(config_path, "rb") as config_file:
    config = tomllib.load(config_file)

runtime = config.get("runtime", {})
kubernetes = config.get("kubernetes", {})
if runtime.get("type") != "kubernetes":
    raise SystemExit("REMOTE_POC_SERVER_CONFIG must set [runtime].type to kubernetes.")
if kubernetes.get("namespace") != namespace:
    raise SystemExit("REMOTE_POC_SERVER_CONFIG [kubernetes].namespace must exactly match POC_NAMESPACE.")
if kubernetes.get("workload_provider") != "batchsandbox":
    raise SystemExit("REMOTE_POC_SERVER_CONFIG [kubernetes].workload_provider must be batchsandbox.")
execd_image = runtime.get("execd_image", "")
if not isinstance(execd_image, str) or not re.search(r"@sha256:[0-9a-f]{64}$", execd_image):
    raise SystemExit("REMOTE_POC_SERVER_CONFIG [runtime].execd_image must use a pinned @sha256 digest.")
template_path = kubernetes.get("batchsandbox_template_file")
if not isinstance(template_path, str) or not template_path.strip():
    raise SystemExit("REMOTE_POC_SERVER_CONFIG must set [kubernetes].batchsandbox_template_file.")
template_path = os.path.expanduser(template_path)
if not os.path.isfile(template_path) or not os.access(template_path, os.R_OK):
    raise SystemExit("Configured BatchSandbox template is not a readable regular file.")
with open(template_path, encoding="utf-8") as template_file:
    template = template_file.read()
matches = re.findall(r"(?m)^\s*serviceAccountName:\s*['\"]?([^\s'\"#]+)", template)
if matches != [service_account]:
    raise SystemExit("Configured BatchSandbox template must contain exactly one matching serviceAccountName.")
PY
}

validate_poc_resource_names() {
  [[ "$REMOTE_POC_SERVICE_ACCOUNT" == "${POC_PREFIX}sa-${POC_TOKEN}" ]] || die \
    "REMOTE_POC_SERVICE_ACCOUNT must exactly be ${POC_PREFIX}sa-${POC_TOKEN}."
  [[ "$REMOTE_POC_IMAGE_PULL_SECRET" == "${POC_PREFIX}pull-${POC_TOKEN}" ]] || die \
    "REMOTE_POC_IMAGE_PULL_SECRET must exactly be ${POC_PREFIX}pull-${POC_TOKEN}."
}

api_headers=()
api_headers=(-H 'Accept: application/json')
if [[ -n "${REMOTE_POC_API_KEY:-}" ]]; then
  # Deliberately do not print this environment value.
  api_headers+=(-H "OPEN-SANDBOX-API-KEY: ${REMOTE_POC_API_KEY}")
fi

sandbox_id=''

api_delete_sandbox() {
  local sandbox_id="$1"
  curl -sS --connect-timeout 10 --max-time 60 \
    -X DELETE \
    "${api_headers[@]}" \
    -o /dev/null \
    -w '%{http_code}' \
    "${REMOTE_POC_BASE_URL}/v1/sandboxes/${sandbox_id}" || true
}

cleanup() {
  local status=$?
  trap - EXIT

  if [[ "${REMOTE_POC_CONFIRM:-}" == '1' ]]; then
    note "Cleanup: only the exact lifecycle sandbox and its exact POC PVC will be removed."
    if [[ -n "$sandbox_id" ]]; then
      local delete_status
      delete_status="$(api_delete_sandbox "$sandbox_id")"
      if [[ "$delete_status" != '200' && "$delete_status" != '202' && "$delete_status" != '204' && "$delete_status" != '404' ]]; then
        printf 'WARN: lifecycle deletion for exact sandbox %s returned HTTP %s.\n' \
          "$sandbox_id" "$delete_status" >&2
      fi
    fi

    kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
      delete pvc "$POC_PVC_NAME" --ignore-not-found --wait=false >/dev/null 2>&1 || \
      printf 'WARN: exact PVC cleanup failed: %s/%s\n' "$POC_NAMESPACE" "$POC_PVC_NAME" >&2
  fi

  exit "$status"
}

require_command kubectl
require_command curl
require_command python3

[[ -n "${KUBECONFIG:-}" ]] || die 'KUBECONFIG must be explicitly set to one readable kubeconfig file.'
[[ -f "$KUBECONFIG" && -r "$KUBECONFIG" ]] || die 'KUBECONFIG must name one readable regular file.'
[[ -n "${REMOTE_POC_BASE_URL:-}" ]] || die 'REMOTE_POC_BASE_URL is required.'
[[ -n "${REMOTE_POC_SANDBOX_IMAGE:-}" ]] || die \
  'REMOTE_POC_SANDBOX_IMAGE is required; supply an image already reachable by the target cluster.'
[[ -n "${REMOTE_POC_SERVER_CONFIG:-}" ]] || die \
  'REMOTE_POC_SERVER_CONFIG must name the isolated local OpenSandbox Server configuration file.'
[[ -f "$REMOTE_POC_SERVER_CONFIG" && -r "$REMOTE_POC_SERVER_CONFIG" ]] || die \
  'REMOTE_POC_SERVER_CONFIG must name one readable regular file.'
[[ "${REMOTE_POC_LOCAL_SERVER_CONFIRM:-}" == '1' ]] || die \
  'Set REMOTE_POC_LOCAL_SERVER_CONFIRM=1 only after confirming the loopback URL is the isolated local process using REMOTE_POC_SERVER_CONFIG.'
[[ -n "${REMOTE_POC_SERVICE_ACCOUNT:-}" ]] || die 'REMOTE_POC_SERVICE_ACCOUNT is required.'
[[ -n "${REMOTE_POC_IMAGE_PULL_SECRET:-}" ]] || die 'REMOTE_POC_IMAGE_PULL_SECRET is required.'
[[ -n "${REMOTE_POC_STORAGE_CLASS:-}" ]] || die 'REMOTE_POC_STORAGE_CLASS is required.'
[[ -n "${REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS:-}" ]] || die \
  'REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS is required.'

[[ -n "${POC_NAMESPACE:-}" ]] || die \
  'POC_NAMESPACE must be explicitly set to a newly generated random POC namespace.'
POC_TOKEN="${POC_NAMESPACE#"$POC_PREFIX"}"
POC_PVC_NAME="${POC_PREFIX}pvc-${POC_TOKEN}"
POC_SUBPATH="${POC_PREFIX}data-${POC_TOKEN}"
validate_namespace
validate_base_url
validate_storage_class
validate_create_request_timeout
validate_poc_resource_names
validate_local_server_config

request_body="$(POC_NAMESPACE="$POC_NAMESPACE" POC_PVC_NAME="$POC_PVC_NAME" POC_SUBPATH="$POC_SUBPATH" \
  REMOTE_POC_SANDBOX_IMAGE="$REMOTE_POC_SANDBOX_IMAGE" REMOTE_POC_STORAGE_CLASS="$REMOTE_POC_STORAGE_CLASS" \
  python3 - "$POC_MOUNT_PATH" <<'PY'
import json
import os
import sys

mount_path = sys.argv[1]
print(json.dumps({
    "image": {"uri": os.environ["REMOTE_POC_SANDBOX_IMAGE"]},
    "entrypoint": ["/bin/sh", "-c", "sleep 300"],
    "resourceLimits": {"cpu": "250m", "memory": "256Mi"},
    "timeout": 300,
    "metadata": {
        "poc": "opensandbox-subpath-poc",
        "pocNamespace": os.environ["POC_NAMESPACE"],
    },
    "volumes": [{
        "name": "opensandbox-subpath-poc-volume",
        "pvc": {
            "claimName": os.environ["POC_PVC_NAME"],
            "storageClass": os.environ["REMOTE_POC_STORAGE_CLASS"],
            "createIfNotExists": True,
            "deleteOnSandboxTermination": True,
        },
        "mountPath": mount_path,
        "readOnly": False,
        "subPath": os.environ["POC_SUBPATH"],
        "ensureSubPathDirectory": True,
    }],
}, separators=(",", ":")))
PY
)"

note 'OpenSandbox ensureSubPathDirectory remote Kubernetes POC'
note "Target namespace: ${POC_NAMESPACE}"
note "Target PVC:       ${POC_PVC_NAME}"
note "StorageClass:     ${REMOTE_POC_STORAGE_CLASS}"
note "Create wait:      ${REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS}s (local curl only)"
note "Target subPath:   ${POC_SUBPATH}"
note "Lifecycle URL:    ${REMOTE_POC_BASE_URL}"
note 'The generated subPath is new and random. This POC never deletes, repairs, or empties a shared backing path.'
note 'Formal service ports 18080 and 18090 are refused. No port-forward is used.'
note 'Exact planned remote resources/actions:'
note "  - Namespace/${POC_NAMESPACE} (pre-provisioned and owned by the parent orchestration; never deleted here)"
note "  - ServiceAccount/${REMOTE_POC_SERVICE_ACCOUNT} and Secret/${REMOTE_POC_IMAGE_PULL_SECRET} in ${POC_NAMESPACE} (pre-provisioned; metadata inspected only)"
note "  - PersistentVolumeClaim/${POC_PVC_NAME} in ${POC_NAMESPACE} (created by the lifecycle request with StorageClass/${REMOTE_POC_STORAGE_CLASS})"
note "  - BatchSandbox/<server-generated UUID> in ${POC_NAMESPACE} (created/deleted only through /v1/sandboxes)"
note "  - Pod/<controller-generated name> in ${POC_NAMESPACE}, selected only by ${POC_LABEL_KEY}=<returned UUID>"
note "  - POST ${REMOTE_POC_BASE_URL}/v1/sandboxes with the following body:"
note "$request_body"

if [[ "${REMOTE_POC_CONFIRM:-}" != '1' ]]; then
  note 'Dry run only: no Kubernetes API or lifecycle API request was sent.'
  note 'To execute, set REMOTE_POC_CONFIRM=1 and REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1 after completing the runbook prerequisites.'
  exit 0
fi

[[ "${REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM:-}" == '1' ]] || die \
  'Set REMOTE_POC_SHARED_STORAGE_DELETE_CONFIRM=1 only after confirming POC PVC deletion cannot affect shared backing data.'

trap cleanup EXIT

kubectl --kubeconfig "$KUBECONFIG" auth can-i get namespaces | grep -qx yes || die \
  'The supplied KUBECONFIG cannot read the exact POC namespace.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i get serviceaccounts -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot inspect the exact POC ServiceAccount.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i get secrets -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot inspect exact POC Secret metadata.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i get persistentvolumeclaims -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot verify the exact POC PVC is absent.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i delete persistentvolumeclaims -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot clean up the exact POC PVC.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i get batchsandboxes.sandbox.opensandbox.io -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot inspect BatchSandbox resources in the POC namespace.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i get pods -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot inspect the exact POC sandbox Pod.'
kubectl --kubeconfig "$KUBECONFIG" auth can-i create pods/exec -n "$POC_NAMESPACE" | grep -qx yes || die \
  'The supplied KUBECONFIG cannot perform the exact POC Pod exec verification.'

kubectl --kubeconfig "$KUBECONFIG" get namespace "$POC_NAMESPACE" >/dev/null || die \
  "Required pre-provisioned POC namespace is absent: ${POC_NAMESPACE}"
service_account_pull_secrets="$(kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
  get serviceaccount "$REMOTE_POC_SERVICE_ACCOUNT" \
  -o jsonpath='{.imagePullSecrets[*].name}')" || die \
  "Required pre-provisioned POC ServiceAccount is absent: ${POC_NAMESPACE}/${REMOTE_POC_SERVICE_ACCOUNT}"
python3 - "$REMOTE_POC_IMAGE_PULL_SECRET" "$service_account_pull_secrets" <<'PY'
import sys

if sys.argv[1] not in sys.argv[2].split():
    raise SystemExit("Required POC ServiceAccount does not reference REMOTE_POC_IMAGE_PULL_SECRET.")
PY
secret_metadata="$(kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
  get secret "$REMOTE_POC_IMAGE_PULL_SECRET" \
  -o jsonpath='{.metadata.name}:{.type}')" || die \
  "Required pre-provisioned POC image pull Secret is absent: ${POC_NAMESPACE}/${REMOTE_POC_IMAGE_PULL_SECRET}"
[[ "$secret_metadata" == "${REMOTE_POC_IMAGE_PULL_SECRET}:kubernetes.io/dockerconfigjson" ]] || die \
  'Required POC image pull Secret must have the exact name and type kubernetes.io/dockerconfigjson; Secret data is not read.'

if kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" get pvc "$POC_PVC_NAME" >/dev/null 2>&1; then
  die "Refusing to use existing target PVC: ${POC_NAMESPACE}/${POC_PVC_NAME}"
fi
if kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
  get batchsandboxes.sandbox.opensandbox.io >/dev/null 2>&1; then
  [[ "$(kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
    get batchsandboxes.sandbox.opensandbox.io -o name)" == '' ]] || die \
    "Refusing a non-empty target namespace: ${POC_NAMESPACE}"
else
  die 'The supplied KUBECONFIG cannot read BatchSandbox resources in the target namespace.'
fi

create_response="$(curl -sS --connect-timeout 10 --max-time "$REMOTE_POC_CREATE_REQUEST_TIMEOUT_SECONDS" \
  -w $'\n%{http_code}' \
  -X POST \
  "${api_headers[@]}" \
  -H 'Content-Type: application/json' \
  --data "$request_body" \
  "${REMOTE_POC_BASE_URL}/v1/sandboxes")" || die 'Lifecycle API create request failed.'
http_status="${create_response##*$'\n'}"
create_body="${create_response%$'\n'*}"
[[ "$http_status" == '202' ]] || die "Lifecycle API create returned HTTP ${http_status}; no response body is printed."
sandbox_id="$(python3 - "$create_body" <<'PY'
import json
import sys

value = json.loads(sys.argv[1]).get("id", "")
if not isinstance(value, str) or not value:
    raise SystemExit("Lifecycle create response has no sandbox id.")
print(value)
PY
)"

note "Lifecycle created exact sandbox ID: ${sandbox_id}"
note "Observed BatchSandbox: ${POC_NAMESPACE}/${sandbox_id}"
kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
  get batchsandboxes.sandbox.opensandbox.io "$sandbox_id" -o json | \
  POC_PVC_NAME="$POC_PVC_NAME" POC_SUBPATH="$POC_SUBPATH" EXPECTED_POC_MOUNT_PATH="$POC_MOUNT_PATH" python3 -c '
import json
import os
import sys

pod_spec = json.load(sys.stdin)["spec"]["template"]["spec"]
expected_pvc = os.environ["POC_PVC_NAME"]
expected_subpath = os.environ["POC_SUBPATH"]
expected_mount = os.environ["EXPECTED_POC_MOUNT_PATH"]
volumes = pod_spec.get("volumes", [])
if not any(v.get("persistentVolumeClaim", {}).get("claimName") == expected_pvc for v in volumes):
    raise SystemExit("BatchSandbox does not reference the exact POC PVC.")
main = next((c for c in pod_spec.get("containers", []) if c.get("name") == "sandbox"), None)
if main is None or not any(
    m.get("mountPath") == expected_mount and m.get("subPath") == expected_subpath
    for m in main.get("volumeMounts", [])
):
    raise SystemExit("BatchSandbox main container lacks the exact POC subPath mount.")
initializer = next((c for c in pod_spec.get("initContainers", []) if c.get("name") == "volume-subpath-initializer"), None)
if initializer is None or initializer.get("command") != ["/opensandbox-subpath-initializer"]:
    raise SystemExit("BatchSandbox lacks the expected subPath initializer command.")
print("Verified exact PVC mount and volume-subpath-initializer in BatchSandbox.")
'

kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" wait \
  --for=condition=Ready pod -l "${POC_LABEL_KEY}=${sandbox_id}" --timeout=120s
pod_name="$(kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" \
  get pods -l "${POC_LABEL_KEY}=${sandbox_id}" -o json | python3 -c '
import json
import sys
items = json.load(sys.stdin).get("items", [])
if len(items) != 1:
    raise SystemExit(f"Expected exactly one sandbox pod, found {len(items)}.")
print(items[0]["metadata"]["name"])
')"
note "Observed exact pod: ${POC_NAMESPACE}/${pod_name}"
kubectl --kubeconfig "$KUBECONFIG" -n "$POC_NAMESPACE" exec "$pod_name" -c sandbox -- \
  /bin/sh -ceu "test -d '${POC_MOUNT_PATH}' && printf '%s\\n' 'ensure-subpath-poc' > '${POC_MOUNT_PATH}/opensandbox-subpath-poc-proof.txt' && test \"\$(cat '${POC_MOUNT_PATH}/opensandbox-subpath-poc-proof.txt')\" = ensure-subpath-poc"
note 'PASS: the initializer-created subPath is mounted read-write in the sandbox.'
