#!/bin/bash
#
# Copyright 2026 The OpenSandbox Authors
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

# HTTP probe target for the egress-MITM E2E tests: it must LISTEN, so
# "blocked by policy/floor" stays distinguishable from "nothing there".

: "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER:=opensandbox-e2e-egress-mitm-target}"
: "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IMAGE:=python:3.11-alpine}"
: "${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_KEY:=opensandbox.e2e}"
: "${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_VALUE:=egress-mitm}"

# Fixed at 8080 to match the tests' PROBE_PORT; not overridable, so they
# cannot drift.

setup_egress_mitm_e2e_target() {
  local label="${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_KEY}=${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_VALUE}"

  docker pull "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IMAGE}"
  docker rm -f "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=${label}" | xargs -r docker rm -f || true

  docker run -d \
    --name "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" \
    --label "${label}" \
    "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IMAGE}" \
    python -m http.server 8080 --bind 0.0.0.0 >/dev/null

  local target_ready="false"
  for _ in $(seq 1 30); do
    if docker exec "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" python - <<PY >/dev/null 2>&1
import urllib.request
urllib.request.urlopen("http://127.0.0.1:8080/", timeout=1).read()
PY
    then
      target_ready="true"
      break
    fi
    sleep 1
  done

  if [ "${target_ready}" != "true" ]; then
    docker logs "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" || true
    echo "Egress MITM E2E target container did not become ready" >&2
    return 1
  fi

  local target_ip
  target_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
    "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}")"
  if [ -z "${target_ip}" ]; then
    docker logs "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" || true
    echo "Failed to determine Egress MITM E2E target container IP" >&2
    return 1
  fi

  export OPENSANDBOX_EGRESS_MITM_E2E_LABEL_KEY
  export OPENSANDBOX_EGRESS_MITM_E2E_LABEL_VALUE
  export OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IP="${target_ip}"
}

cleanup_egress_mitm_e2e_target() {
  local label="${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_KEY}=${OPENSANDBOX_EGRESS_MITM_E2E_LABEL_VALUE}"
  docker rm -f "${OPENSANDBOX_EGRESS_MITM_E2E_TARGET_CONTAINER}" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=${label}" | xargs -r docker rm -f || true
}
