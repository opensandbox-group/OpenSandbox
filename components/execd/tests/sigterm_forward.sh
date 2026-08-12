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

# Test: bootstrap.sh forwards K8s SIGTERM to execd and the user process.
#
# Simulates K8s termination: send SIGTERM to bootstrap process,
# verify both child processes receive it via trap marker files.
#
# Usage:
#   cd components/execd
#   bash tests/test_sigterm_forward.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"

TESTDIR="$(mktemp -d)"
BOOTSTRAP_PID=""
cleanup() {
    if [ -n "$BOOTSTRAP_PID" ]; then
        kill -TERM "$BOOTSTRAP_PID" 2>/dev/null || true
        wait "$BOOTSTRAP_PID" 2>/dev/null || true
    fi
    rm -rf "$TESTDIR"
}
trap cleanup EXIT

MARKER_STARTED="$TESTDIR/started"
MARKER_SIGTERM="$TESTDIR/sigterm_received"
EXECD_MARKER_STARTED="$TESTDIR/execd_started"
EXECD_MARKER_SIGTERM="$TESTDIR/execd_sigterm_received"

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

EXECD_HELPER="$TESTDIR/execd_helper.sh"
cat > "$EXECD_HELPER" << 'EXECD_HELPER_SCRIPT'
#!/bin/bash
trap 'touch "$EXECD_MARKER_SIGTERM"; exit 0' TERM
touch "$EXECD_MARKER_STARTED"
while true; do
    sleep 1
done
EXECD_HELPER_SCRIPT
chmod +x "$EXECD_HELPER"

echo "=== Test: SIGTERM forwarding from bootstrap to child processes ==="

# Start bootstrap with helpers as execd and the user process.
EXECD="$EXECD_HELPER" \
EXECD_MARKER_STARTED="$EXECD_MARKER_STARTED" \
EXECD_MARKER_SIGTERM="$EXECD_MARKER_SIGTERM" \
BOOTSTRAP_SHELL="$(command -v bash)" \
BOOTSTRAP_CMD="$HELPER $MARKER_STARTED $MARKER_SIGTERM" \
bash "$BOOTSTRAP" &
BOOTSTRAP_PID=$!

# Wait for both child processes to signal they are alive.
WAIT_OK=""
for i in $(seq 1 10); do
    if [ -f "$MARKER_STARTED" ] && [ -f "$EXECD_MARKER_STARTED" ]; then
        WAIT_OK="yes"
        break
    fi
    sleep 1
done
if [ -z "$WAIT_OK" ]; then
    echo "FAIL: execd or user process did not start within 10s"
    kill "$BOOTSTRAP_PID" 2>/dev/null || true
    exit 1
fi
echo "OK: execd and user process started (PID: $BOOTSTRAP_PID tree)"

# Simulate K8s: send SIGTERM to bootstrap process
echo "Sending SIGTERM to bootstrap PID $BOOTSTRAP_PID ..."
kill -TERM "$BOOTSTRAP_PID"

# Wait for signal propagation and handler execution
sleep 3

# Verify both child processes received SIGTERM.
if [ -f "$MARKER_SIGTERM" ] && [ -f "$EXECD_MARKER_SIGTERM" ]; then
    echo "PASS: execd and user process received SIGTERM from bootstrap"
else
    echo "FAIL: execd or user process did NOT receive SIGTERM"
    echo "  execd marker: $(test -f "$EXECD_MARKER_SIGTERM" && echo yes || echo no)"
    echo "  user marker: $(test -f "$MARKER_SIGTERM" && echo yes || echo no)"
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

wait "$BOOTSTRAP_PID" 2>/dev/null || true
BOOTSTRAP_PID=""

echo "=== Test: user command exit shuts down execd ==="
NATURAL_EXECD_STARTED="$TESTDIR/natural_execd_started"
NATURAL_EXECD_SIGTERM="$TESTDIR/natural_execd_sigterm_received"
EXECD="$EXECD_HELPER" \
EXECD_MARKER_STARTED="$NATURAL_EXECD_STARTED" \
EXECD_MARKER_SIGTERM="$NATURAL_EXECD_SIGTERM" \
BOOTSTRAP_SHELL="$(command -v bash)" \
BOOTSTRAP_CMD='while [ ! -f "$EXECD_MARKER_STARTED" ]; do sleep 0.1; done; exit 7' \
bash "$BOOTSTRAP" &
BOOTSTRAP_PID=$!

NATURAL_EXITED=""
for i in $(seq 1 100); do
    if ! kill -0 "$BOOTSTRAP_PID" 2>/dev/null; then
        NATURAL_EXITED="yes"
        break
    fi
    sleep 0.1
done
if [ -z "$NATURAL_EXITED" ]; then
    echo "FAIL: bootstrap did not stop after the user command exited"
    kill -TERM "$BOOTSTRAP_PID" 2>/dev/null || true
    wait "$BOOTSTRAP_PID" 2>/dev/null || true
    BOOTSTRAP_PID=""
    exit 1
fi

set +e
wait "$BOOTSTRAP_PID"
BOOTSTRAP_STATUS=$?
set -e
BOOTSTRAP_PID=""

if [ "$BOOTSTRAP_STATUS" -ne 7 ]; then
    echo "FAIL: bootstrap exit status = $BOOTSTRAP_STATUS, want 7"
    exit 1
fi
if [ ! -f "$NATURAL_EXECD_STARTED" ] || [ ! -f "$NATURAL_EXECD_SIGTERM" ]; then
    echo "FAIL: natural user-command exit did not gracefully stop execd"
    exit 1
fi
echo "PASS: natural user-command exit preserved status and stopped execd"
