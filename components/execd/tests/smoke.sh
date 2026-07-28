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

usage() {
	cat <<'EOF'
Usage: ./tests/smoke.sh [--inventory-cap|--inventory-expiry]

Starts isolated local Jupyter and execd processes, waits for /ping, runs the
API smoke once, then stops both processes. The inventory modes require the
matching EXECD_COMMAND_RECOVERY_* settings documented in this script.
EOF
}

mode=""
case "${1:-}" in
	"") ;;
	--inventory-cap) mode="cap" ;;
	--inventory-expiry) mode="expiry" ;;
	-h|--help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
esac

case "$mode" in
	cap)
		[[ "${EXECD_COMMAND_RECOVERY_TTL:-}" == "1h" && "${EXECD_COMMAND_RECOVERY_MAX_TERMINAL:-}" == "2" ]] || {
			echo "--inventory-cap requires EXECD_COMMAND_RECOVERY_TTL=1h EXECD_COMMAND_RECOVERY_MAX_TERMINAL=2" >&2
			exit 2
		}
		;;
	expiry)
		[[ "${EXECD_COMMAND_RECOVERY_TTL:-}" == "2s" && "${EXECD_COMMAND_RECOVERY_MAX_TERMINAL:-}" == "1000" ]] || {
			echo "--inventory-expiry requires EXECD_COMMAND_RECOVERY_TTL=2s EXECD_COMMAND_RECOVERY_MAX_TERMINAL=1000" >&2
			exit 2
		}
		;;
esac

JUPYTER_PID=""
EXECD_PID=""

cleanup() {
	local pid
	for pid in "$EXECD_PID" "$JUPYTER_PID"; do
		if [[ -n "$pid" ]]; then
			kill "$pid" 2>/dev/null || true
			wait "$pid" 2>/dev/null || true
		fi
	done
}
trap cleanup EXIT INT TERM

require_alive() {
	local pid="$1"
	local name="$2"
	if ! kill -0 "$pid" 2>/dev/null; then
		echo "$name process exited unexpectedly (pid=$pid)" >&2
		return 1
	fi
}

curl_ready() {
	curl --fail --silent --show-error --connect-timeout 1 --max-time 2 "$@" >/dev/null
}

wait_for_jupyter() {
	local deadline=$((SECONDS + 30))
	until curl_ready -H "Authorization: token ${JUPYTER_TOKEN}" "http://127.0.0.1:${JUPYTER_PORT}/api"; do
		require_alive "$JUPYTER_PID" "Jupyter" || return 1
		if (( SECONDS >= deadline )); then
			echo "Jupyter did not become ready at /api before the 30s deadline" >&2
			return 1
		fi
		sleep 0.2
	done
	require_alive "$JUPYTER_PID" "Jupyter"
}

wait_for_execd() {
	local deadline=$((SECONDS + 30))
	while true; do
		# Check both owned processes before and after the request. This rejects a
		# successful /ping response from an unrelated listener if our execd failed
		# to bind 44772 or exited during startup.
		require_alive "$JUPYTER_PID" "Jupyter" || return 1
		require_alive "$EXECD_PID" "execd" || return 1
		if curl_ready "http://127.0.0.1:44772/ping"; then
			require_alive "$JUPYTER_PID" "Jupyter" || return 1
			require_alive "$EXECD_PID" "execd" || return 1
			return 0
		fi
		if (( SECONDS >= deadline )); then
			echo "execd did not become healthy at /ping before the 30s deadline" >&2
			return 1
		fi
		sleep 0.2
	done
}

source tests/jupyter.sh
install_jupyter
JUPYTER_PID=$!
require_alive "$JUPYTER_PID" "Jupyter"
wait_for_jupyter

export EXECD_API_GRACE_SHUTDOWN=500ms
export EXECD_LOG_FILE=execd.log
./bin/execd --jupyter-host="http://127.0.0.1:${JUPYTER_PORT}" --jupyter-token="$JUPYTER_TOKEN" --log-level=7 >startup.log 2>&1 &
EXECD_PID=$!
require_alive "$EXECD_PID" "execd"
wait_for_execd

python3 -m unittest discover -s tests -p 'test_smoke_api_inventory.py'
SMOKE_INVENTORY_MODE="$mode" python3 tests/smoke_api.py
