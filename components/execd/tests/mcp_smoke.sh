#!/bin/bash
# Copyright 2025 The OpenSandbox Authors
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

# Smoke test for the execd MCP proxy bridge.
#
# Starts a real FastMCP HTTP upstream, a real execd configured with an HTTP
# bridge and a stdio bridge (mcpecho), then drives the full chain with the
# official python MCP SDK (tests/mcp_smoke.py).
#
# Prerequisites: bin/execd (make build); uv (preferred) or pip for the mcp
# python package.

set -euxo pipefail

cd "$(dirname "$0")/.."

EXECD_PORT=44772
export MCP_UPSTREAM_PORT=46001
API_TOKEN=mcp-smoke-token
export BASE_URL="http://127.0.0.1:${EXECD_PORT}"
export API_TOKEN

cleanup() {
	[[ -n "${EXEC_PID:-}" ]] && kill "${EXEC_PID}" 2>/dev/null || true
	[[ -n "${UPSTREAM_PID:-}" ]] && kill "${UPSTREAM_PID}" 2>/dev/null || true
	pkill -f "mcp_upstream_fastmcp.py" 2>/dev/null || true
}
trap cleanup EXIT

# --- python driver (uv preferred, pip fallback) -----------------------------
# The first argument is a version pin: the suite runs once per python SDK
# release line (1.x, 2.x).
run_python_pinned() {
	local pin="$1"
	shift
	if command -v uv >/dev/null 2>&1; then
		uv run --quiet --with "$pin" --with httpx python "$@"
	else
		python3 -m pip install --quiet "$pin" httpx
		python3 "$@"
	fi
}

# --- build ------------------------------------------------------------------
if [[ ! -x bin/execd ]]; then
	make build
fi
echo_bin="$(mktemp -d)/mcpecho"
go build -o "$echo_bin" ./pkg/mcpproxy/testdata/mcpecho

# --- upstream MCP server (Streamable HTTP) ----------------------------------
run_python_pinned "mcp>=1.16,<2" tests/mcp_upstream_fastmcp.py >/tmp/mcp-upstream.log 2>&1 &
UPSTREAM_PID=$!
for _ in $(seq 1 30); do
	if curl -s -o /dev/null "http://127.0.0.1:${MCP_UPSTREAM_PORT}/mcp"; then
		break
	fi
	sleep 0.5
done

# --- execd with MCP bridges --------------------------------------------------
export EXECD_MCP_SERVERS="[
  {\"name\":\"smoke-http\",\"transport\":\"http\",\"url\":\"http://127.0.0.1:${MCP_UPSTREAM_PORT}/mcp\"},
  {\"name\":\"smoke-stdio\",\"transport\":\"stdio\",\"command\":\"$echo_bin\"}
]"
export EXECD_API_GRACE_SHUTDOWN=500ms
./bin/execd --port="${EXECD_PORT}" --access-token="${API_TOKEN}" --log-level=6 >/tmp/mcp-execd.log 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 30); do
	if curl -s -o /dev/null "http://127.0.0.1:${EXECD_PORT}/ping"; then
		break
	fi
	sleep 0.5
done

# --- drive with the official python SDK (1.x and 2.x release lines) ----------
run_python_pinned "mcp>=1.16,<2" tests/mcp_smoke.py
run_python_pinned "mcp>=2" tests/mcp_smoke.py

# --- drive with the official java SDK (optional toolchain) --------------------
bash tests/mcp_smoke_java.sh
