#!/bin/bash
#
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

: "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER:=opensandbox-e2e-credential-vault-target}"
: "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST:=credential-vault-e2e.opensandbox.test}"
: "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IMAGE:=python:3.11-alpine}"
: "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY:=opensandbox.e2e}"
: "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE:=credential-vault}"

setup_credential_vault_e2e_target() {
  local repo_root="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
  local label="${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY}=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE}"

  docker pull "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IMAGE}"
  docker rm -f "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=${label}" | xargs -r docker rm -f || true

  docker run -d \
    --name "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" \
    --label "${label}" \
    -v "${repo_root}/tests/python/tests/support:/srv:ro" \
    "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IMAGE}" \
    python /srv/credential_vault_echo_server.py >/dev/null

  local target_ready="false"
  for _ in $(seq 1 30); do
    if docker exec "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" python - <<'PY' >/dev/null 2>&1
import urllib.request
urllib.request.urlopen("http://127.0.0.1/healthz", timeout=1).read()
PY
    then
      target_ready="true"
      break
    fi
    sleep 1
  done

  if [ "${target_ready}" != "true" ]; then
    docker logs "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" || true
    echo "Credential Vault E2E target container did not become ready" >&2
    return 1
  fi

  local target_ip
  target_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
    "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}")"
  if [ -z "${target_ip}" ]; then
    docker logs "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" || true
    echo "Failed to determine Credential Vault E2E target container IP" >&2
    return 1
  fi

  export OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST
  export OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP="${target_ip}"
  export OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY
  export OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE
}

cleanup_credential_vault_e2e_target() {
  local label="${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY}=${OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE}"
  docker rm -f "${OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_CONTAINER}" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=${label}" | xargs -r docker rm -f || true
}
