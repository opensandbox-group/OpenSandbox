#!/bin/bash
# Copyright 2025 Alibaba Group Holding Ltd.
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

color() {
  if [[ -t 1 ]] && command -v tput >/dev/null 2>&1; then
    tput setaf "$1"
  fi
}

reset_color() {
  if [[ -t 1 ]] && command -v tput >/dev/null 2>&1; then
    tput sgr0
  fi
}

STEP_COLOR=6   # cyan
INFO_COLOR=2   # green
WARN_COLOR=3   # yellow
ERR_COLOR=1    # red

step() {
  printf "\n%s==== %s ====%s\n" "$(color ${STEP_COLOR})" "$1" "$(reset_color)"
}

info() {
  printf "%s%s%s\n" "$(color ${INFO_COLOR})" "$1" "$(reset_color)"
}

warn() {
  printf "%s%s%s\n" "$(color ${WARN_COLOR})" "$1" "$(reset_color)" >&2
}

error() {
  printf "%s%s%s\n" "$(color ${ERR_COLOR})" "$1" "$(reset_color)" >&2
}

BASE_URL="${BASE_URL:-http://localhost:32888}"
BASE_API_URL="${BASE_URL}/v1"
curl_json() {
  curl -sfSL "$@"
}

curl_json_status() {
  # Returns body + trailing status code line to allow non-2xx handling.
  curl -sSL -w "\n%{http_code}" "$@"
}

wait_for_running() {
  local sandbox_id="${1:-${SANDBOX_ID}}"
  local deadline=$((SECONDS + 30))
  while true; do
    local result
    result=$(curl -sSL -w "\n%{http_code}" \
      "${BASE_API_URL}/sandboxes/${sandbox_id}" \
      | python3 -c '
import json, sys
raw = sys.stdin.read()
lines = raw.rsplit("\n", 1)
http_code = lines[-1].strip() if len(lines) > 1 else "000"
body_text = lines[0] if len(lines) > 1 else ""
if http_code == "404":
    print("ERROR:not found (404) — may have failed during provisioning")
elif http_code != "200":
    print(f"RETRY:HTTP {http_code}")
elif not body_text.strip():
    print("RETRY:empty body")
else:
    try:
        data = json.loads(body_text)
        state = data.get("status", {}).get("state", "")
        print(f"STATE:{state}")
        print(body_text)
    except json.JSONDecodeError as exc:
        print(f"RETRY:invalid JSON: {exc}")
') || true

    local tag="${result%%:*}"
    local detail="${result#*:}"

    case "${tag}" in
      ERROR)
        error "Sandbox ${sandbox_id}: ${detail}"
        return 1
        ;;
      RETRY)
        warn "GET sandbox ${sandbox_id}: ${detail}, retrying..."
        if (( SECONDS >= deadline )); then
          error "Sandbox ${sandbox_id} did not reach Running within 30s."
          return 1
        fi
        sleep 1
        continue
        ;;
      STATE)
        local state="${detail%%$'\n'*}"
        local body="${detail#*$'\n'}"
        if [[ "${state}" == "Running" ]]; then
          printf '%s' "${body}"
          return 0
        fi
        if [[ "${state}" == "Failed" || "${state}" == "Terminated" ]]; then
          error "Sandbox ${sandbox_id} entered terminal state '${state}'."
          return 1
        fi
        if (( SECONDS >= deadline )); then
          error "Sandbox ${sandbox_id} did not reach Running within 30s (last: ${state})."
          return 1
        fi
        sleep 1
        ;;
      *)
        warn "GET sandbox ${sandbox_id}: unexpected output, retrying..."
        if (( SECONDS >= deadline )); then
          error "Sandbox ${sandbox_id} did not reach Running within 30s."
          return 1
        fi
        sleep 1
        ;;
    esac
  done
}

wait_for_expired() {
  local sandbox_id=$1
  local deadline=$((SECONDS + 90))
  while true; do
    local resp body status
    resp=$(curl_json_status "${BASE_API_URL}/sandboxes/${sandbox_id}")
    status="${resp##*$'\n'}"
    body="${resp%$'\n'*}"
    if [[ "${status}" == "404" ]]; then
      info "Sandbox ${sandbox_id} expired as expected."
      return 0
    fi
    if (( SECONDS >= deadline )); then
      error "Sandbox ${sandbox_id} did not expire within expected window (last status ${status})."
      echo "${body}"
      return 1
    fi
    sleep 2
  done
}

wait_for_sidecar_gone() {
  local sandbox_id=$1
  local deadline=$((SECONDS + 20))
  while true; do
    if ! docker ps -a --filter "label=opensandbox.io/egress-sidecar-for=${sandbox_id}" -q | grep -q .; then
      info "No sidecar remaining for sandbox ${sandbox_id}"
      return 0
    fi
    if (( SECONDS >= deadline )); then
      error "Sidecar for sandbox ${sandbox_id} still present after timeout"
      docker ps -a --filter "label=opensandbox.io/egress-sidecar-for=${sandbox_id}"
      return 1
    fi
    sleep 2
  done
}

docker pull ubuntu:latest

create_payload='{
  "image": { "uri": "ubuntu" },
  "env": { "HELLO": "WORLD" },
  "metadata": { "hello": "world" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "500m", "memory": "512Mi" },
  "timeout": 60
}'

step "Create sandbox (60s TTL)"
create_resp=$(curl_json \
  -H 'Content-Type: application/json' \
  -d "${create_payload}" \
  "${BASE_API_URL}/sandboxes")

SANDBOX_ID=$(python3 - <<'PY' "${create_resp}"
import json,sys
data=json.loads(sys.argv[1])
sid=str(data.get("id","")).strip()
if not sid:
    raise SystemExit("Failed to parse sandbox id from response")
print(sid,end="")
PY
)

echo "Sandbox created: id=${SANDBOX_ID}"

step "Wait for sandbox to reach Running"
get_resp=$(wait_for_running)
state=$(python3 - <<'PY' "${get_resp}"
import json,sys
body=json.loads(sys.argv[1])
print(body.get("status",{}).get("state"))
PY
)
echo "Sandbox state: ${state}"

python3 - <<'PY' "${get_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
expected=sys.argv[2]
assert str(body.get("id"))==expected, "Sandbox ID mismatch in GET response"
assert body.get("status",{}).get("state") in {"Pending","Running","Unknown","Paused","Terminated","Failed"}, "Unexpected state"
PY

step "List sandboxes (metadata filter)"
list_resp=$(curl_json \
  -G \
  --data-urlencode "metadata=hello=world" \
  --data-urlencode "page=1" \
  --data-urlencode "pageSize=10" \
  "${BASE_API_URL}/sandboxes")

python3 - <<'PY' "${list_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
sid=sys.argv[2]
ids=[item.get("id") for item in body.get("items",[])]
assert sid in ids, "Sandbox ID not found in list response"
assert body.get("pagination",{}).get("page") == 1, "Unexpected pagination page"
PY
echo "List check passed (found sandbox, pagination ok)"

step "Renew sandbox expiration (+10m)"
new_expiration=$(python3 - <<'PY'
from datetime import datetime, timedelta, timezone
print((datetime.now(timezone.utc) + timedelta(minutes=10)).isoformat())
PY
)

renew_payload=$(cat <<JSON
{
  "expiresAt": "${new_expiration}"
}
JSON
)

renew_resp=$(curl_json \
  -X POST \
  -H 'Content-Type: application/json' \
  -d "${renew_payload}" \
  "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/renew-expiration")
renewed=$(python3 - <<'PY' "${renew_resp}"
import json,sys
body=json.loads(sys.argv[1])
print(body.get("expiresAt"))
PY
)
echo "Expiration renewed to: ${renewed}"

curl_patch() {
  # Like curl_json but dumps response body on HTTP errors for debugging.
  local result headers http_code
  result=$(curl -sSL -D /dev/stderr -w "\n%{http_code}" "$@") || true
  http_code="${result##*$'\n'}"
  if [[ "${http_code}" -ge 400 ]]; then
    echo ""
    echo "PATCH HTTP ${http_code} — response body:" >&2
    echo "${result%$'\n'*}" >&2
    return 1
  fi
  printf '%s' "${result%$'\n'*}"
}

step "Patch sandbox metadata — add key"
patch_resp=$(curl_patch \
  -X PATCH \
  -H 'Content-Type: application/json' \
  -d '{"team": "platform", "version": "2.0"}' \
  "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/metadata")

python3 - <<'PY' "${patch_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
sid=sys.argv[2]
assert str(body.get("id"))==sid, "Sandbox ID mismatch in PATCH response"
md=body.get("metadata") or {}
# Original key still present
assert md.get("hello")=="world", f"Expected hello=world, got {md.get('hello')}"
# New keys added
assert md.get("team")=="platform", f"Expected team=platform, got {md.get('team')}"
assert md.get("version")=="2.0", f"Expected version=2.0, got {md.get('version')}"
print("PASS: metadata after add: " + json.dumps(md))
PY
info "PATCH add keys OK"

step "Patch sandbox metadata — delete key"
patch_resp=$(curl_patch \
  -X PATCH \
  -H 'Content-Type: application/json' \
  -d '{"version": null}' \
  "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/metadata")

python3 - <<'PY' "${patch_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
sid=sys.argv[2]
md=body.get("metadata") or {}
assert "version" not in md, f"version should be deleted, got {md.get('version')}"
assert md.get("team")=="platform", f"Expected team=platform, got {md.get('team')}"
assert md.get("hello")=="world", f"Expected hello=world, got {md.get('hello')}"
print("PASS: metadata after delete: " + json.dumps(md))
PY
info "PATCH delete key OK"

step "Patch sandbox metadata — mixed add and delete"
patch_resp=$(curl_patch \
  -X PATCH \
  -H 'Content-Type: application/json' \
  -d '{"team": null, "env": "production"}' \
  "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/metadata")

python3 - <<'PY' "${patch_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
sid=sys.argv[2]
md=body.get("metadata") or {}
assert "team" not in md, f"team should be deleted, got {md.get('team')}"
assert md.get("env")=="production", f"Expected env=production, got {md.get('env')}"
assert md.get("hello")=="world", f"Expected hello=world, got {md.get('hello')}"
print("PASS: metadata after mixed: " + json.dumps(md))
PY
info "PATCH mixed operations OK"

step "Patch sandbox metadata — empty body noop"
patch_resp=$(curl_patch \
  -X PATCH \
  -H 'Content-Type: application/json' \
  -d '{}' \
  "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/metadata")

python3 - <<'PY' "${patch_resp}" "${SANDBOX_ID}"
import json,sys
body=json.loads(sys.argv[1])
sid=sys.argv[2]
md=body.get("metadata") or {}
assert md.get("hello")=="world", f"Expected hello=world, got {md.get('hello')}"
assert md.get("env")=="production", f"Expected env=production, got {md.get('env')}"
print("PASS: empty body noop: " + json.dumps(md))
PY
info "PATCH empty body noop OK"

step "Request endpoint on port 8080"
endpoint_resp=$(curl_json "${BASE_API_URL}/sandboxes/${SANDBOX_ID}/endpoints/8080")
endpoint=$(python3 - <<'PY' "${endpoint_resp}"
import json,sys
body=json.loads(sys.argv[1])
print(body.get("endpoint"))
PY
)
echo "Endpoint: ${endpoint}"

step "Delete sandbox"
curl_json -X DELETE "${BASE_API_URL}/sandboxes/${SANDBOX_ID}"
echo "Sandbox ${SANDBOX_ID} deleted."

step "Create sandbox with networkPolicy (egress sidecar)"
egress_payload='{
  "image": { "uri": "ubuntu" },
  "env": {},
  "metadata": { "egress": "on" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "500m", "memory": "512Mi" },
  "timeout": 60,
  "networkPolicy": {
    "defaultAction": "deny",
    "egress": [
      { "action": "allow", "target": "pypi.org" }
    ]
  }
}'

create_resp_with_status=$(curl_json_status \
  -H 'Content-Type: application/json' \
  -d "${egress_payload}" \
  "${BASE_API_URL}/sandboxes")

status_code="${create_resp_with_status##*$'\n'}"
create_resp_body="${create_resp_with_status%$'\n'*}"

if [[ "${status_code}" != "202" ]]; then
  warn "Skip egress sidecar smoke (status ${status_code}). Body: ${create_resp_body}"
  warn "Likely network_mode=host or egress.image unset."
else
  SANDBOX_ID=$(python3 - <<'PY' "${create_resp_body}"
import json,sys
data=json.loads(sys.argv[1])
sid=str(data.get("id","")).strip()
if not sid:
    raise SystemExit("Failed to parse sandbox id from response")
print(sid,end="")
PY
)
  echo "Egress sandbox created: id=${SANDBOX_ID}"

  step "Wait for egress sandbox to reach Running"
  wait_for_running "${SANDBOX_ID}" >/dev/null

  step "Verify egress sidecar is running"
  SIDECAR_ID=$(docker ps -a --filter "label=opensandbox.io/egress-sidecar-for=${SANDBOX_ID}" -q | head -n1 || true)
  if [[ -z "${SIDECAR_ID}" ]]; then
    error "Expected egress sidecar for sandbox ${SANDBOX_ID}, but none found."
    exit 1
  fi
  info "Sidecar ${SIDECAR_ID} detected for sandbox ${SANDBOX_ID}"

  step "Delete egress sandbox and ensure sidecar cleanup"
  curl_json -X DELETE "${BASE_API_URL}/sandboxes/${SANDBOX_ID}"
  wait_for_sidecar_gone "${SANDBOX_ID}"
fi

step "Create sandbox with host volume mount"
# Prepare the host volume test directory
mkdir -p /tmp/opensandbox-e2e/host-volume-test
echo "opensandbox-e2e-marker" > /tmp/opensandbox-e2e/host-volume-test/marker.txt
chmod -R 755 /tmp/opensandbox-e2e

volume_payload='{
  "image": { "uri": "ubuntu" },
  "env": {},
  "metadata": { "volume": "host-test" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "500m", "memory": "512Mi" },
  "timeout": 60,
  "volumes": [
    {
      "name": "test-host-vol",
      "host": { "path": "/tmp/opensandbox-e2e/host-volume-test" },
      "mountPath": "/mnt/host-data",
      "readOnly": false
    }
  ]
}'

volume_resp_with_status=$(curl_json_status \
  -H 'Content-Type: application/json' \
  -d "${volume_payload}" \
  "${BASE_API_URL}/sandboxes")

volume_status="${volume_resp_with_status##*$'\n'}"
volume_body="${volume_resp_with_status%$'\n'*}"

if [[ "${volume_status}" != "202" ]]; then
  warn "Skip host volume smoke (status ${volume_status}). Body: ${volume_body}"
  warn "Likely host path validation or storage config issue."
else
  VOLUME_SANDBOX_ID=$(python3 - <<'PY' "${volume_body}"
import json,sys
data=json.loads(sys.argv[1])
sid=str(data.get("id","")).strip()
if not sid:
    raise SystemExit("Failed to parse sandbox id from response")
print(sid,end="")
PY
)
  echo "Volume sandbox created: id=${VOLUME_SANDBOX_ID}"

  step "Wait for volume sandbox to reach Running"
  wait_for_running "${VOLUME_SANDBOX_ID}" >/dev/null

  # --- Verify the bind mount is actually effective ---
  # Resolve the Docker container ID from the sandbox API response.
  volume_sandbox_resp=$(curl_json "${BASE_API_URL}/sandboxes/${VOLUME_SANDBOX_ID}")
  container_id=$(python3 -c '
import json, sys
body = json.loads(sys.argv[1])
print(body.get("containerId", body.get("container_id", "")), end="")
' "${volume_sandbox_resp}")
  # Fallback: if the API doesn't expose container_id, search by label.
  if [[ -z "${container_id}" ]]; then
    container_id=$(docker ps -qf "label=sandbox_id=${VOLUME_SANDBOX_ID}" | head -1)
  fi

  if [[ -n "${container_id}" ]]; then
    step "Verify host volume bind mount content inside container"
    # 1. Read the marker file written on the host
    marker_content=$(docker exec "${container_id}" cat /mnt/host-data/marker.txt 2>&1) || true
    if [[ "${marker_content}" == "opensandbox-e2e-marker" ]]; then
      info "PASS: marker.txt content matches expected value."
    else
      error "FAIL: marker.txt content='${marker_content}', expected='opensandbox-e2e-marker'"
      exit 1
    fi

    # 2. Write a file from inside the container and verify it on the host
    docker exec "${container_id}" sh -c 'echo "written-from-sandbox" > /mnt/host-data/sandbox-output.txt'
    host_content=$(cat /tmp/opensandbox-e2e/host-volume-test/sandbox-output.txt 2>&1) || true
    if [[ "${host_content}" == "written-from-sandbox" ]]; then
      info "PASS: file written inside container is visible on host."
    else
      error "FAIL: sandbox-output.txt on host='${host_content}', expected='written-from-sandbox'"
      exit 1
    fi
  else
    warn "Skip bind-mount verification: could not resolve container ID for sandbox ${VOLUME_SANDBOX_ID}."
  fi

  step "Delete volume sandbox"
  curl_json -X DELETE "${BASE_API_URL}/sandboxes/${VOLUME_SANDBOX_ID}"
  echo "Volume sandbox ${VOLUME_SANDBOX_ID} deleted."
fi

step "Create short-lived sandbox (60s TTL) for auto-expiration"
create_payload_short='{
  "image": { "uri": "ubuntu" },
  "env": {},
  "metadata": { "lifecycle": "short" },
  "entrypoint": ["tail", "-f", "/dev/null"],
  "resourceLimits": { "cpu": "1", "memory": "2Gi" },
  "timeout": 60
}'

create_resp_short=$(curl_json \
  -H 'Content-Type: application/json' \
  -d "${create_payload_short}" \
  "${BASE_API_URL}/sandboxes")

SANDBOX_ID=$(python3 - <<'PY' "${create_resp_short}"
import json,sys
data=json.loads(sys.argv[1])
sid=str(data.get("id","")).strip()
if not sid:
    raise SystemExit("Failed to parse sandbox id from response")
print(sid,end="")
PY
)

echo "Short-lived sandbox created: id=${SANDBOX_ID}"

step "Wait for short-lived sandbox to reach Running"
get_resp_short=$(wait_for_running "${SANDBOX_ID}")
state_short=$(python3 - <<'PY' "${get_resp_short}"
import json,sys
body=json.loads(sys.argv[1])
print(body.get("status",{}).get("state"))
PY
)
echo "Sandbox state: ${state_short}"

step "Wait for sandbox ${SANDBOX_ID} to auto-expire (expect 404)"
wait_for_expired "${SANDBOX_ID}"

step "server Lifecycle API smoke test completed successfully"
