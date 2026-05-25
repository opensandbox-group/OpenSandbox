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

# Test: bootstrap.sh forwards K8s SIGTERM to user process.
#
# Simulates K8s termination: send SIGTERM to bootstrap process,
# verify user process receives it via trap marker file.
#
# Usage:
#   cd components/execd
#   bash tests/test_sigterm_forward.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"

TESTDIR="$(mktemp -d)"
trap 'rm -rf "$TESTDIR"' EXIT

MARKER_STARTED="$TESTDIR/started"
MARKER_SIGTERM="$TESTDIR/sigterm_received"

# Write test helper: traps SIGTERM, writes marker, then exits.
# Must use shell builtin loop so bash stays alive as PID and trap can fire.
# Without this, bash -c exec's child process and trap handler is lost.
HELPER="$TESTDIR/sigterm_helper.sh"
cat > "$HELPER" << 'HELPER_SCRIPT'
#!/bin/bash
MARKER_STARTED="$1"
MARKER_SIGTERM="$2"
trap 'touch "$MARKER_SIGTERM"; exit 0' TERM
touch "$MARKER_STARTED"
while true; do
    sleep 1
done
HELPER_SCRIPT
chmod +x "$HELPER"

echo "=== Test: SIGTERM forwarding from bootstrap to user process ==="

# Start bootstrap with helper as user process
BOOTSTRAP_SHELL="$(command -v bash)" \
BOOTSTRAP_CMD="$HELPER $MARKER_STARTED $MARKER_SIGTERM" \
bash "$BOOTSTRAP" &
BOOTSTRAP_PID=$!

# Wait for user process to signal it's alive
WAIT_OK=""
for i in $(seq 1 10); do
    if [ -f "$MARKER_STARTED" ]; then
        WAIT_OK="yes"
        break
    fi
    sleep 1
done
if [ -z "$WAIT_OK" ]; then
    echo "FAIL: user process did not start within 10s"
    kill "$BOOTSTRAP_PID" 2>/dev/null || true
    exit 1
fi
echo "OK: user process started (PID: $BOOTSTRAP_PID tree)"

# Simulate K8s: send SIGTERM to bootstrap process
echo "Sending SIGTERM to bootstrap PID $BOOTSTRAP_PID ..."
kill -TERM "$BOOTSTRAP_PID"

# Wait for signal propagation and handler execution
sleep 3

# Verify user process received SIGTERM
if [ -f "$MARKER_SIGTERM" ]; then
    echo "PASS: user process received SIGTERM from bootstrap"
else
    echo "FAIL: user process did NOT receive SIGTERM"
    echo "  Bootstrap PID: $BOOTSTRAP_PID (still running: $(kill -0 "$BOOTSTRAP_PID" 2>/dev/null && echo yes || echo no))"
    echo "  Process tree:"
    pgrep -P "$BOOTSTRAP_PID" 2>/dev/null | while read -r pid; do
        echo "    child PID $pid: $(ps -p "$pid" -o comm= 2>/dev/null || echo dead)"
        pgrep -P "$pid" 2>/dev/null | while read -r child; do
            echo "      grandchild PID $child: $(ps -p "$child" -o comm= 2>/dev/null || echo dead)"
        done
    done
    exit 1
fi
