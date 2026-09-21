#!/bin/bash
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

# Smoke test for the execd runtime-init API (POST /init, GET /ready).
#
# Starts two throwaway execd instances on dedicated ports and asserts the
# full one-shot init contract:
#   Phase 1 (gated, EXECD_RUNTIME_INIT=1): /ready 503 and business APIs 503
#     until /init; malformed /init does not consume the slot; after a valid
#     /init the token hash replaces the legacy container token and /init
#     envs reach user processes; any second /init conflicts (409).
#   Phase 2 (legacy fallback): /ready turns 200 after the template-driven
#     startup; a late /init is accepted once, switches auth to the binding
#     hash, and further calls conflict.
#
# Prerequisites: ./bin/execd (run `make build` first), python3 + requests.
#
# Usage: bash tests/runtime_init.sh
#
# Exit 0 on success, non-zero on failure.

set -euo pipefail

cd "$(dirname "$0")/.."

if [ ! -x ./bin/execd ]; then
    echo "error: ./bin/execd not found; run 'make build' first" >&2
    exit 1
fi

LEGACY_TOKEN="legacy-container-token"
NEW_TOKEN="runtime-init-smoke-new-token"

GATED_PORT="${GATED_PORT:-44773}"
LEGACY_PORT="${LEGACY_PORT:-44774}"

TESTDIR="$(mktemp -d)"
GATED_PID=""
LEGACY_PID=""
cleanup() {
    if [ -n "$GATED_PID" ]; then
        kill -TERM "$GATED_PID" 2>/dev/null || true
        wait "$GATED_PID" 2>/dev/null || true
    fi
    if [ -n "$LEGACY_PID" ]; then
        kill -TERM "$LEGACY_PID" 2>/dev/null || true
        wait "$LEGACY_PID" 2>/dev/null || true
    fi
    rm -rf "$TESTDIR"
}
trap cleanup EXIT

start_execd() {
    local port="$1" log="$2"
    shift 2
    EXECD_LOG_FILE="$TESTDIR/$log" ./bin/execd \
        --port="$port" \
        --access-token="$LEGACY_TOKEN" \
        "$@" >"$TESTDIR/$log.stdout" 2>&1 &
    echo $!
}

# Phase 1: gated mode waits for POST /init before anything user-owned runs.
GATED_PID="$(start_execd "$GATED_PORT" gated.log --runtime-init)"
MODE=gated \
BASE_URL="http://localhost:$GATED_PORT" \
LEGACY_TOKEN="$LEGACY_TOKEN" \
NEW_TOKEN="$NEW_TOKEN" \
python3 tests/runtime_init_smoke.py

# Phase 2: legacy fallback — the template-driven startup owns readiness and
# a late /init is accepted exactly once.
LEGACY_PID="$(start_execd "$LEGACY_PORT" legacy.log)"
MODE=legacy \
BASE_URL="http://localhost:$LEGACY_PORT" \
LEGACY_TOKEN="$LEGACY_TOKEN" \
NEW_TOKEN="$NEW_TOKEN" \
python3 tests/runtime_init_smoke.py

echo "runtime-init smoke OK"
