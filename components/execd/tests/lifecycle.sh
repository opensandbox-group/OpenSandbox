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

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BOOTSTRAP="$ROOT_DIR/bootstrap.sh"
TESTDIR="$(mktemp -d)"
export HOME="$TESTDIR/home"
mkdir -p "$HOME"
BOOTSTRAP_PID=""
cleanup() {
    if [ -n "$BOOTSTRAP_PID" ]; then
        kill -TERM "$BOOTSTRAP_PID" 2>/dev/null || true
        wait "$BOOTSTRAP_PID" 2>/dev/null || true
    fi
    rm -rf "$TESTDIR"
}
trap cleanup EXIT

assert_status_dir_empty() {
    leaked="$(ls -A "$STATUS_DIR")"
    if [ -n "$leaked" ]; then
        echo "FAIL: leaked lifecycle temp entries: $leaked" >&2
        exit 1
    fi
}

EXECD_STUB="$TESTDIR/execd"
cat > "$EXECD_STUB" <<'STUB'
#!/bin/sh
stub_fail() {
    printf 'execd test stub: %s\n' "$1" >&2
    exit 90
}

status_file=""
while [ "$#" -gt 0 ]; do
    case "$1" in
    --lifecycle-startup-status-file)
        if [ -z "${2:-}" ]; then
            stub_fail "missing value for --lifecycle-startup-status-file"
        fi
        status_file="$2"
        shift 2
        ;;
    --lifecycle-startup-status-file=*)
        status_file="${1#*=}"
        if [ -z "$status_file" ]; then
            stub_fail "missing value for --lifecycle-startup-status-file"
        fi
        shift
        ;;
    *)
        stub_fail "unexpected argument: $1"
        ;;
    esac
done
[ -n "${EXECD_MARKER:-}" ] || stub_fail "stub requires EXECD_MARKER"
[ -n "${EXECD_READY_MARKER:-}" ] || stub_fail "stub requires EXECD_READY_MARKER"
[ -n "${SEQUENCE_FILE:-}" ] || stub_fail "stub requires SEQUENCE_FILE"
touch "$EXECD_MARKER"
printf 'execd-started\n' >> "$SEQUENCE_FILE"
if [ -n "$status_file" ]; then
    sleep 0.05
    touch "$EXECD_READY_MARKER"
    printf 'execd-ready\n' >> "$SEQUENCE_FILE"
    if [ "${EXECD_DIE_BEFORE_STATUS:-}" = "1" ]; then
        exit 17
    fi
    if [ "${EXECD_HANG_BEFORE_STATUS:-}" = "1" ]; then
        [ -n "${EXECD_HANG_TERMINATED_MARKER:-}" ] \
            || stub_fail "hang case requires EXECD_HANG_TERMINATED_MARKER"
        trap 'touch "$EXECD_HANG_TERMINATED_MARKER"; exit 0' TERM INT
        if [ "${EXECD_REMOVE_STATUS_FILE:-}" = "1" ]; then
            rm -f "$status_file"
        fi
        while true; do sleep 1; done
    fi
    lifecycle_config=""
    lifecycle_transport="$(printf '%s' "${OPENSANDBOX_LIFECYCLE:-}" | tr -d '[:space:]')"
    if [ -z "$lifecycle_transport" ]; then
        lifecycle_config="${EXECD_LIFECYCLE_CONFIG:-}"
        if [ -z "$lifecycle_config" ]; then
            if [ -z "${HOME:-}" ]; then
                stub_fail "stub requires HOME to resolve the default lifecycle config"
            fi
            lifecycle_config="$HOME/.execd/lifecycle.toml"
        fi
    fi
    lifecycle_config_missing=0
    if [ -n "${lifecycle_config:-}" ] && [ ! -e "$lifecycle_config" ]; then
        lifecycle_config_missing=1
    elif [ -n "${lifecycle_config:-}" ] && [ ! -f "$lifecycle_config" ]; then
        exit 1
    fi
    prestart_status=0
    if [ "$lifecycle_config_missing" != "1" ] && [ "${EXECD_NO_PRESTART:-}" != "1" ]; then
        test -f "$status_file" || stub_fail "stub requires lifecycle status file"
        if [ "${EXECD_HANG_AFTER_RUNNING:-}" = "1" ]; then
            [ -n "${EXECD_HANG_TERMINATED_MARKER:-}" ] \
                || stub_fail "hang case requires EXECD_HANG_TERMINATED_MARKER"
            if [ "${EXECD_IGNORE_TERM:-}" = "1" ]; then
                trap 'touch "$EXECD_HANG_TERMINATED_MARKER"' TERM INT
            elif [ "${EXECD_REPORT_SUCCESS_ON_TERM:-}" = "1" ]; then
                trap 'trap - TERM INT; if [ -f "$status_file" ]; then printf "done 0\n" >> "$status_file"; fi; touch "$EXECD_HANG_TERMINATED_MARKER"; sleep 2; exit 0' TERM INT
            else
                trap 'touch "$EXECD_HANG_TERMINATED_MARKER"; exit 0' TERM INT
            fi
        fi
        printf 'running %s\n' "${PRESTART_TIMEOUT_SECONDS:-60}" >> "$status_file"
        if [ -n "${EXECD_RUNNING_MARKER:-}" ]; then
            touch "$EXECD_RUNNING_MARKER"
        fi
        if [ "${EXECD_HANG_AFTER_RUNNING:-}" = "1" ]; then
            while true; do sleep 1; done
        fi
        if [ -n "${PRESTART_DELAY_SECONDS:-}" ]; then
            sleep "$PRESTART_DELAY_SECONDS"
        fi
        if [ "${PRESTART_BLOCK:-}" = "1" ]; then
            trap 'touch "$PRESTART_TERMINATED_MARKER"; exit 0' TERM INT
            touch "$PRESTART_MARKER"
            printf 'preStart\n' >> "$SEQUENCE_FILE"
            while true; do sleep 1; done
        fi
        touch "$PRESTART_MARKER"
        printf 'preStart\n' >> "$SEQUENCE_FILE"
        prestart_status="${PRESTART_EXIT_CODE:-0}"
    fi
    test -f "$status_file" || stub_fail "stub requires lifecycle status file"
    if [ -n "${PRESTART_STATUS_RAW+x}" ]; then
        printf 'done %s\n' "$PRESTART_STATUS_RAW" >> "$status_file"
    else
        printf 'done %s\n' "$prestart_status" >> "$status_file"
    fi
    if [ "${EXECD_STATUS_STAY_ALIVE:-}" = "1" ]; then
        trap 'touch "$STATUS_TERMINATED_MARKER"; exit 0' TERM INT
        while true; do sleep 1; done
    fi
    if [ "$prestart_status" -ne 0 ]; then
        exit "$prestart_status"
    fi
fi
trap 'exit 0' TERM INT
while true; do sleep 1; done
STUB
chmod +x "$EXECD_STUB"

USER_SCRIPT="$TESTDIR/user.sh"
cat > "$USER_SCRIPT" <<'USER'
#!/bin/sh
set -e
if [ "${EXPECT_PRESTART_MARKER:-1}" = "1" ]; then
    test -f "$PRESTART_MARKER"
fi
test -f "$EXECD_READY_MARKER"
test -z "${OPENSANDBOX_LIFECYCLE:-}"
test -z "${EXECD_LIFECYCLE_CONFIG:-}"
test -f "$EXECD_MARKER"
touch "$USER_MARKER"
printf 'user\n' >> "$SEQUENCE_FILE"
USER
chmod +x "$USER_SCRIPT"

PRESTART_MARKER="$TESTDIR/prestart"
EXECD_MARKER="$TESTDIR/execd-started"
EXECD_READY_MARKER="$TESTDIR/execd-ready"
USER_MARKER="$TESTDIR/user-started"
SEQUENCE_FILE="$TESTDIR/sequence"
STATUS_DIR="$TESTDIR/status"
mkdir "$STATUS_DIR"
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"

test -f "$PRESTART_MARKER"
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test -f "$USER_MARKER"
test "$(cat "$SEQUENCE_FILE")" = "$(printf 'execd-started\nexecd-ready\npreStart\nuser')"
assert_status_dir_empty
echo "PASS: preStart completed before the user entrypoint"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
OPENSANDBOX_LIFECYCLE='{"periodic":[{"name":"sync","schedule":"@hourly","command":["true"]}]}' \
EXECD="$EXECD_STUB" \
EXECD_NO_PRESTART=1 \
EXPECT_PRESTART_MARKER=0 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"

test ! -f "$PRESTART_MARKER"
test -f "$USER_MARKER"
test "$(cat "$SEQUENCE_FILE")" = "$(printf 'execd-started\nexecd-ready\nuser')"
assert_status_dir_empty
echo "PASS: periodic-only lifecycle starts without a preStart running status"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
# Keep this delay above bootstrap's 10-second initial startup watchdog.
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
PRESTART_TIMEOUT_SECONDS=30 \
PRESTART_DELAY_SECONDS=11 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"

test -f "$PRESTART_MARKER"
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test -f "$USER_MARKER"
test "$(cat "$SEQUENCE_FILE")" = "$(printf 'execd-started\nexecd-ready\npreStart\nuser')"
assert_status_dir_empty
echo "PASS: preStart completion may exceed the initial startup watchdog"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
PRESTART_EXIT_CODE=42 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 42
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: preStart failure stops execd and prevents the user entrypoint from starting"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
PRESTART_TERMINATED_MARKER="$TESTDIR/prestart-terminated"
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
PRESTART_BLOCK=1 \
PRESTART_TIMEOUT_SECONDS=300 \
PRESTART_MARKER="$PRESTART_MARKER" \
PRESTART_TERMINATED_MARKER="$PRESTART_TERMINATED_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP" &
BOOTSTRAP_PID=$!

i=0
while [ ! -f "$PRESTART_MARKER" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
test -f "$PRESTART_MARKER"
kill -TERM "$BOOTSTRAP_PID"
wait "$BOOTSTRAP_PID" || true
BOOTSTRAP_PID=""
test -f "$PRESTART_TERMINATED_MARKER"
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: termination during preStart is forwarded to the hook"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
EXECD_RUNNING_MARKER="$TESTDIR/execd-running"
EXECD_IGNORED_TERM_MARKER="$TESTDIR/execd-ignored-term"
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_HANG_AFTER_RUNNING=1 \
EXECD_IGNORE_TERM=1 \
EXECD_HANG_TERMINATED_MARKER="$EXECD_IGNORED_TERM_MARKER" \
EXECD_RUNNING_MARKER="$EXECD_RUNNING_MARKER" \
PRESTART_TIMEOUT_SECONDS=300 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP" &
BOOTSTRAP_PID=$!

i=0
while [ ! -f "$EXECD_RUNNING_MARKER" ] && [ "$i" -lt 50 ]; do
    sleep 0.1
    i=$((i + 1))
done
test -f "$EXECD_RUNNING_MARKER"
kill -TERM "$BOOTSTRAP_PID"
i=0
# bootstrap gives execd 10 seconds before KILL; allow ample CI scheduling slack.
while kill -0 "$BOOTSTRAP_PID" 2>/dev/null && [ "$i" -lt 300 ]; do
    sleep 0.1
    i=$((i + 1))
done
if kill -0 "$BOOTSTRAP_PID" 2>/dev/null; then
    kill -KILL "$BOOTSTRAP_PID" 2>/dev/null || true
    wait "$BOOTSTRAP_PID" 2>/dev/null || true
    BOOTSTRAP_PID=""
    echo "FAIL: bootstrap did not bound execd shutdown after TERM" >&2
    exit 1
fi
wait "$BOOTSTRAP_PID" || true
BOOTSTRAP_PID=""
test -f "$EXECD_IGNORED_TERM_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: termination during preStart kills an unresponsive execd"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_DIE_BEFORE_STATUS=1 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 17
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test ! -f "$PRESTART_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: execd exit before lifecycle status fails startup without leaking the status file"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
EXECD_HANG_TERMINATED_MARKER="$TESTDIR/execd-hang-terminated"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_HANG_BEFORE_STATUS=1 \
EXECD_HANG_TERMINATED_MARKER="$EXECD_HANG_TERMINATED_MARKER" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 1
test -f "$EXECD_HANG_TERMINATED_MARKER"
test ! -f "$PRESTART_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: lifecycle startup watchdog terminates a hung execd"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
EXECD_RUNNING_HANG_TERMINATED_MARKER="$TESTDIR/execd-running-hang-terminated"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_HANG_AFTER_RUNNING=1 \
EXECD_HANG_TERMINATED_MARKER="$EXECD_RUNNING_HANG_TERMINATED_MARKER" \
EXECD_REPORT_SUCCESS_ON_TERM=1 \
PRESTART_TIMEOUT_SECONDS=1 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 1
test -f "$EXECD_RUNNING_HANG_TERMINATED_MARKER"
test ! -f "$PRESTART_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: lifecycle hook watchdog timeout cannot be overwritten by a late success"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
EXECD_INVALID_RUNNING_TERMINATED_MARKER="$TESTDIR/execd-invalid-running-terminated"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_HANG_AFTER_RUNNING=1 \
EXECD_HANG_TERMINATED_MARKER="$EXECD_INVALID_RUNNING_TERMINATED_MARKER" \
PRESTART_TIMEOUT_SECONDS=10000000000 \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 1
test -f "$EXECD_INVALID_RUNNING_TERMINATED_MARKER"
test ! -f "$PRESTART_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: malformed lifecycle running status fails closed"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
EXECD_MISSING_STATUS_TERMINATED_MARKER="$TESTDIR/execd-missing-status-terminated"
set +e
OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
EXECD="$EXECD_STUB" \
EXECD_HANG_BEFORE_STATUS=1 \
EXECD_REMOVE_STATUS_FILE=1 \
EXECD_HANG_TERMINATED_MARKER="$EXECD_MISSING_STATUS_TERMINATED_MARKER" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 1
test -f "$EXECD_MISSING_STATUS_TERMINATED_MARKER"
test ! -f "$PRESTART_MARKER"
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: missing lifecycle status file fails closed and terminates execd"

STATUS_TERMINATED_MARKER="$TESTDIR/status-terminated"
for invalid_status in garbled 999 999999999999999999999999; do
    rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE" "$STATUS_TERMINATED_MARKER"
    set +e
    OPENSANDBOX_LIFECYCLE='{"preStart":{"command":["true"]}}' \
    EXECD="$EXECD_STUB" \
    PRESTART_STATUS_RAW="$invalid_status" \
    EXECD_STATUS_STAY_ALIVE=1 \
    STATUS_TERMINATED_MARKER="$STATUS_TERMINATED_MARKER" \
    PRESTART_MARKER="$PRESTART_MARKER" \
    EXECD_MARKER="$EXECD_MARKER" \
    EXECD_READY_MARKER="$EXECD_READY_MARKER" \
    USER_MARKER="$USER_MARKER" \
    SEQUENCE_FILE="$SEQUENCE_FILE" \
    TMPDIR="$STATUS_DIR" \
    BOOTSTRAP_CMD="$USER_SCRIPT" \
    "$BOOTSTRAP"
    status=$?
    set -e

    test "$status" -eq 1
    test -f "$STATUS_TERMINATED_MARKER"
    test ! -f "$USER_MARKER"
    assert_status_dir_empty
done
echo "PASS: malformed lifecycle status fails closed and terminates a still-running execd"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
PERSISTED_CONFIG="$HOME/.execd/lifecycle.toml"
mkdir -p "$(dirname "$PERSISTED_CONFIG")"
printf 'version = 1\n[preStart]\ncommand = ["true"]\n' > "$PERSISTED_CONFIG"
EXECD_LIFECYCLE_CONFIG='' \
EXECD="$EXECD_STUB" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$USER_SCRIPT" \
"$BOOTSTRAP"

test -f "$PRESTART_MARKER"
test -f "$EXECD_MARKER"
test -f "$EXECD_READY_MARKER"
test -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: persisted lifecycle config triggers preStart"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
SANITIZE_USER_SCRIPT="$TESTDIR/sanitize-user.sh"
cat > "$SANITIZE_USER_SCRIPT" <<'USER'
#!/bin/sh
set -e
test -z "${OPENSANDBOX_LIFECYCLE:-}"
test -z "${EXECD_LIFECYCLE_CONFIG:-}"
i=0
while [ ! -f "$EXECD_MARKER" ] && [ "$i" -lt 50 ]; do
    sleep 0.1 2>/dev/null || sleep 1
    i=$((i + 1))
done
test -f "$EXECD_MARKER"
touch "$USER_MARKER"
USER
chmod +x "$SANITIZE_USER_SCRIPT"
test -f "$PERSISTED_CONFIG"
OPENSANDBOX_LIFECYCLE='' \
EXECD_LIFECYCLE_CONFIG="$TESTDIR/missing-lifecycle.toml" \
EXECD="$EXECD_STUB" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$SANITIZE_USER_SCRIPT" \
"$BOOTSTRAP"

test -f "$USER_MARKER"
test ! -f "$PRESTART_MARKER"
assert_status_dir_empty
echo "PASS: explicit missing lifecycle config does not fall back and internal environment is stripped"

rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
CONFIG_DIR_PATH="$TESTDIR/config-as-dir"
mkdir -p "$CONFIG_DIR_PATH"
set +e
OPENSANDBOX_LIFECYCLE='' \
EXECD_LIFECYCLE_CONFIG="$CONFIG_DIR_PATH" \
EXECD="$EXECD_STUB" \
PRESTART_MARKER="$PRESTART_MARKER" \
EXECD_MARKER="$EXECD_MARKER" \
EXECD_READY_MARKER="$EXECD_READY_MARKER" \
USER_MARKER="$USER_MARKER" \
SEQUENCE_FILE="$SEQUENCE_FILE" \
TMPDIR="$STATUS_DIR" \
BOOTSTRAP_CMD="$SANITIZE_USER_SCRIPT" \
"$BOOTSTRAP"
status=$?
set -e

test "$status" -eq 1
test ! -f "$USER_MARKER"
assert_status_dir_empty
echo "PASS: invalid lifecycle config path fails before the user entrypoint starts"

rm -f "$PERSISTED_CONFIG"
for lifecycle_home in "$HOME" ''; do
    lifecycle_transport=''
    [ -z "$lifecycle_home" ] || lifecycle_transport='   '
    rm -f "$PRESTART_MARKER" "$EXECD_MARKER" "$EXECD_READY_MARKER" "$USER_MARKER" "$SEQUENCE_FILE"
    HOME="$lifecycle_home" \
    OPENSANDBOX_LIFECYCLE="$lifecycle_transport" \
    EXECD_LIFECYCLE_CONFIG='' \
    EXECD="$EXECD_STUB" \
    PRESTART_MARKER="$PRESTART_MARKER" \
    EXECD_MARKER="$EXECD_MARKER" \
    EXECD_READY_MARKER="$EXECD_READY_MARKER" \
    USER_MARKER="$USER_MARKER" \
    SEQUENCE_FILE="$SEQUENCE_FILE" \
    TMPDIR="$STATUS_DIR" \
    BOOTSTRAP_CMD="$SANITIZE_USER_SCRIPT" \
    "$BOOTSTRAP"

    test -f "$USER_MARKER"
    test ! -f "$PRESTART_MARKER"
    assert_status_dir_empty
done
echo "PASS: missing default config does not affect sandboxes without lifecycle hooks"
