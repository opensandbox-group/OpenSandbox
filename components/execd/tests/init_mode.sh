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

# Test: bootstrap.sh EXECD_INIT mode (OSEP-0018).
#
# With EXECD_INIT set, bootstrap.sh must exec into execd (--init -- <cmd>)
# instead of backgrounding it, so execd becomes the sandbox init. The user
# command is passed through the concrete argv forms bootstrap.sh supports:
# BOOTSTRAP_CMD, the "-c" form, plain positional args, and the default shell.
#
# Usage:
#   cd components/execd
#   bash tests/init_mode.sh

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"

TESTDIR="$(mktemp -d)"
cleanup() {
    rm -rf "$TESTDIR"
}
trap cleanup EXIT

# Stub execd: records its argv and pid, then sleeps so the test can verify the
# process tree (bootstrap must have replaced itself, not spawned a child).
EXECD_STUB="$TESTDIR/execd_stub.sh"
cat > "$EXECD_STUB" << 'STUB'
#!/bin/sh
printf '%s\n' "$*" > "$EXECD_ARGV_FILE"
printf '%s\n' "$$" > "$EXECD_PID_FILE"
while true; do sleep 1; done
STUB
chmod +x "$EXECD_STUB"

run_bootstrap() {
    ARGV_FILE="$1"
    PID_FILE="$2"
    shift 2
    EXECD="$EXECD_STUB" \
    EXECD_ARGV_FILE="$ARGV_FILE" \
    EXECD_PID_FILE="$PID_FILE" \
    EXECD_INIT=1 \
    "$BOOTSTRAP" "$@" &
    BOOTSTRAP_PID=$!

    for i in $(seq 1 50); do
        [ -f "$PID_FILE" ] && break
        sleep 0.1
    done
    if [ ! -f "$PID_FILE" ]; then
        echo "FAIL: execd stub did not start"
        kill "$BOOTSTRAP_PID" 2>/dev/null || true
        wait "$BOOTSTRAP_PID" 2>/dev/null || true
        exit 1
    fi
}

stop_stub() {
    kill "$BOOTSTRAP_PID" 2>/dev/null || true
    wait "$BOOTSTRAP_PID" 2>/dev/null || true
}

# 1. BOOTSTRAP_CMD form: execd must receive --init -- bash -c '<cmd>'
ARGV_FILE="$TESTDIR/argv1"
PID_FILE="$TESTDIR/pid1"
BOOTSTRAP_CMD="echo init-works" run_bootstrap "$ARGV_FILE" "$PID_FILE" -c "ignored"
STUB_PID="$(cat "$PID_FILE")"
if [ "$STUB_PID" != "$BOOTSTRAP_PID" ]; then
    echo "FAIL: bootstrap did not exec execd (bootstrap pid $BOOTSTRAP_PID != stub pid $STUB_PID)"
    stop_stub
    exit 1
fi
if ! grep -q '^--init -- /[^ ]* -c echo init-works$' "$ARGV_FILE"; then
    echo "FAIL: BOOTSTRAP_CMD argv = $(cat "$ARGV_FILE"), want --init -- <bash> -c <cmd>"
    stop_stub
    exit 1
fi
stop_stub
echo "PASS: EXECD_INIT=1 + BOOTSTRAP_CMD execs execd --init -- <shell> -c <cmd>"

# 2. Positional form: execd must receive --init -- <argv...>
ARGV_FILE="$TESTDIR/argv2"
PID_FILE="$TESTDIR/pid2"
run_bootstrap "$ARGV_FILE" "$PID_FILE" sh -c 'echo positional'
STUB_PID="$(cat "$PID_FILE")"
if [ "$STUB_PID" != "$BOOTSTRAP_PID" ]; then
    echo "FAIL: bootstrap did not exec execd (bootstrap pid $BOOTSTRAP_PID != stub pid $STUB_PID)"
    stop_stub
    exit 1
fi
if ! grep -q '^--init -- sh -c echo positional$' "$ARGV_FILE"; then
    echo "FAIL: positional argv = $(cat "$ARGV_FILE"), want --init -- sh -c echo positional"
    stop_stub
    exit 1
fi
stop_stub
echo "PASS: EXECD_INIT=1 + positional args pass through unchanged"

# 3. No command: execd must receive --init -- <default shell>
ARGV_FILE="$TESTDIR/argv3"
PID_FILE="$TESTDIR/pid3"
run_bootstrap "$ARGV_FILE" "$PID_FILE"
if ! grep -q '^--init -- /' "$ARGV_FILE"; then
    echo "FAIL: default-shell argv = $(cat "$ARGV_FILE"), want --init -- <shell>"
    stop_stub
    exit 1
fi
stop_stub
echo "PASS: EXECD_INIT=1 with no command passes the default shell"

echo "=== all init-mode bootstrap contract tests passed ==="
