#!/bin/bash
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

# Runs the server-path hardening E2E (execd-as-init, OSEP-0018) against a local
# Docker-bridge server with runtime.execd_run_as_init = true. The hardened
# isolation TOML (hardening + landlock) is injected into every sandbox via
# a config-level bind mount + EXECD_ISOLATION_CONFIG, so the whole
# server -> sandbox -> execd path runs with the floor on.
#
# Phase 1: the floor works end to end (reduced caps/seccomp/NNP, env strip,
#          landlock enforcement, capabilities endpoint).
# Phase 2: fail-open degradation — same server with CAP_SETPCAP dropped from
#          the container ceiling; cap_drop must report degraded while the
#          rest of the floor stays active.
# Phase 3: custom-policy overrides (R-q) — a [seccomp] deny override +
#          keep_capabilities=["CAP_NET_RAW"]; the ceiling keeps NET_RAW so
#          the ambient raise can succeed.
# Phase 4: EXECD_INIT <-> TOML drift pin (R-s) — the hardened TOML with
#          execd_run_as_init = false; the endpoint must report init_mode=none
#          and degrade the enabled layers.
# Phase 5: default-off pin (R-m) — plain server, no isolation TOML, init off;
#          the endpoint must report init_mode=none with every layer disabled
#          and the workload must be unaffected (no floor applied).
#
# Usage: bash scripts/python-execd-hardening-e2e.sh

set -euxo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SERVER_PID=""

cleanup() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

run_server() {
  cd server
  export OPENSANDBOX_INSECURE_SERVER=YES
  uv sync
  uv run python -m opensandbox_server.main > server.log 2>&1 &
  SERVER_PID=$!
  cd "${REPO_ROOT}"
  sleep 10
}

stop_server() {
  if [ -n "${SERVER_PID}" ]; then
    kill "${SERVER_PID}" 2>/dev/null || true
    wait "${SERVER_PID}" 2>/dev/null || true
    SERVER_PID=""
    sleep 2
  fi
}

run_pytest() {
  local selector="$1"
  cd "${REPO_ROOT}/tests/python"
  uv sync --all-extras --refresh
  uv run pytest tests/test_execd_hardening_e2e.py -v -k "${selector}"
  cd "${REPO_ROOT}"
}

# ---------------------------------------------------------------------------
# Images + host workspace + hardened TOML.
# ---------------------------------------------------------------------------
docker build -f components/execd/Dockerfile -t opensandbox/execd:local "${REPO_ROOT}"
docker pull opensandbox/code-interpreter:${TAG:-latest}

# The workload has no CAP_DAC_OVERRIDE under the floor, so the host-side
# workspace dir must be fully accessible to the container uid. The hardened
# TOML is injected via a config-level bind mount (the server does not stage
# isolation configs from the execd image).
mkdir -p /tmp/opensandbox-e2e/workspace /tmp/opensandbox-e2e/logs
chmod 0777 /tmp/opensandbox-e2e/workspace
cp components/execd/configs/isolation.hardened.toml /tmp/opensandbox-e2e/isolation.hardened.toml
cp components/execd/configs/isolation.custom.toml /tmp/opensandbox-e2e/isolation.custom.toml
echo "-------- EXECD HARDENING E2E test logs for execd --------" > /tmp/opensandbox-e2e/logs/execd.log

write_server_config() {
  local drop_capabilities="$1"
  local toml_file="${2:-isolation.hardened.toml}"
  local run_as_init="${3:-true}"
  local inject_toml="${4:-true}"
  local sandbox_env_line=""
  local toml_bind_line=""
  if [ "${inject_toml}" = "true" ]; then
    sandbox_env_line='sandbox_env = { EXECD_ISOLATION_CONFIG = "/etc/opensandbox/isolation.toml" }'
    toml_bind_line="  \"/tmp/opensandbox-e2e/${toml_file}:/etc/opensandbox/isolation.toml\","
  fi
  cat <<EOF > ~/.sandbox.toml
[server]
host = "127.0.0.1"
port = 8080
api_key = ""
[log]
level = "INFO"
[runtime]
type = "docker"
execd_image = "opensandbox/execd:local"
execd_run_as_init = ${run_as_init}
[egress]
image = "opensandbox/egress:local"
mode = "dns+nft"
[docker]
network_mode = "bridge"
# The container baseline must not mask the launcher's own floor: with
# no_new_privileges=true (the server default) or Docker's default seccomp
# profile, a launcher regression would still show NoNewPrivs=1/Seccomp=2
# in the workload. Unset both so the observed values can only come from
# the opensandbox-launcher.
no_new_privileges = false
seccomp_profile = "unconfined"
${sandbox_env_line}
sandbox_binds = [
  "/tmp/opensandbox-e2e/workspace:/workspace",
${toml_bind_line}
]
drop_capabilities = ${drop_capabilities}
[storage]
allowed_host_paths = ["/tmp/opensandbox-e2e"]
EOF
}

# ---------------------------------------------------------------------------
# Phase 1: the floor applies end to end (default ceiling keeps CAP_SETPCAP).
# ---------------------------------------------------------------------------
write_server_config '["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]'
run_server
run_pytest "TestHardeningE2E or TestIsolatedSessionHardeningE2E"
stop_server

# ---------------------------------------------------------------------------
# Phase 2: degradation — CAP_SETPCAP missing from the ceiling.
# ---------------------------------------------------------------------------
write_server_config '["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG", "SETPCAP"]'
run_server
OPENSANDBOX_HARDENING_DEGRADATION=true run_pytest "TestHardeningDegradationE2E"
stop_server

# ---------------------------------------------------------------------------
# Phase 3: custom-policy overrides (R-q) — [seccomp] deny + keep_capabilities.
# The ceiling must KEEP CAP_NET_RAW (not in the drop list) so the launcher's
# ambient raise can succeed; the custom TOML replaces the hardened one.
# ---------------------------------------------------------------------------
write_server_config '["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]' isolation.custom.toml
run_server
run_pytest "TestHardeningCustomPolicyE2E"
stop_server

# ---------------------------------------------------------------------------
# Phase 4: EXECD_INIT <-> TOML drift (R-s) — hardened TOML with init off.
# ---------------------------------------------------------------------------
write_server_config '["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]' isolation.hardened.toml false
run_server
OPENSANDBOX_HARDENING_DRIFT=true run_pytest "TestHardeningDriftE2E"
stop_server

# ---------------------------------------------------------------------------
# Phase 5: default-off pin (R-m) — plain server, no TOML, init off. The
# endpoint must report init_mode=none with every layer disabled, and the
# workload must be unaffected (no floor applied).
# ---------------------------------------------------------------------------
write_server_config '["AUDIT_WRITE", "MKNOD", "NET_ADMIN", "NET_RAW", "SYS_ADMIN", "SYS_MODULE", "SYS_PTRACE", "SYS_TIME", "SYS_TTY_CONFIG"]' isolation.hardened.toml false false
run_server
OPENSANDBOX_HARDENING_DEFAULT_OFF=true run_pytest "TestHardeningDefaultOffE2E"
stop_server

echo "Execd hardening E2E PASSED (phase 1: floor, phase 2: degradation, phase 3: custom policy, phase 4: drift, phase 5: default-off)"
