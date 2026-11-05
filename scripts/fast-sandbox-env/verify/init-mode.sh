#!/usr/bin/env bash
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

# verify/init-mode.sh — execd-as-init in-guest process verify (OSEP-0018).
#
# runtime.execd_run_as_init makes templates bake EXECD_INIT=1, so the guest
# boots bootstrap.sh into `execd --init`: execd supervises the workload as a
# subreaper under the fsb guest init (PID 1, no signal shield). This stage
# dumps the full in-guest process table, then pins: PID 1 is the guest init,
# execd carries --init as its child, the entrypoint is execd's child, an
# orphan reparents to execd and gets reaped (no zombies), and the
# capabilities endpoint reports init_mode=subreaper / signal_shield=false.
#
# NOTE: integration-env.sh carries an inline copy of this stage; keep the
# two in sync until the env.sh split is completed.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# init_mode_entrypoint_probe: guest-side probe printing TAIL_PPID=<ppid> for
# the baked entrypoint workload. Selection is the exact comm name (pgrep -x,
# never -f) plus the exact argv, so the probe's own sh -c shell — whose
# cmdline embeds this very text and whose PPid is execd — can never
# self-match, and a same-name process with different args is skipped; it
# exits nonzero when no matching workload exists.
init_mode_entrypoint_probe='
for p in $(pgrep -x tail); do
	argv=$(tr "\0" "\n" </proc/$p/cmdline)
	[ "$argv" = "$(printf "tail\n-f\n/dev/null")" ] || continue
	echo "TAIL_PPID=$(grep "^PPid:" /proc/$p/status | grep -o "[0-9]*")"
	exit 0
done
echo "init-mode probe: no tail -f /dev/null workload process in the guest" >&2
exit 1
'

# guest_out <words...>: run a command in the sandbox via the CLI.
guest_out() {
	osb_api command run "$VERIFY_ID" -- "$@" 2>"$LOGS_DIR/init-mode-cmd.err"
}

# guest_value <key> <words...>: extract the guest's KEY=VALUE line. The full
# guest_out output is captured and its exit status checked before parsing, so
# a CLI failure after the target line cannot satisfy the caller's probe.
guest_value() { # <key> <probe-words...>
	local key="$1" out status line
	shift
	out="$(guest_out "$@")"
	status=$?
	[[ "$status" -eq 0 ]] || return "$status"
	while IFS= read -r line || [[ -n "$line" ]]; do
		case "$line" in
			"$key="*) printf '%s\n' "${line#*=}"; return 0 ;;
		esac
	done <<<"$out"
	return 1
}

init_mode_verify() { # [template-id]
	local template_id="${1:-$TEMPLATE_ID}"
	local created execd_pid probe

	log "verify (init-mode): creating a sandbox via the opensandbox CLI (templateId=$template_id)"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-init-mode -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "init-mode target Running" 600 sandbox_running

	# Full process table first: ground truth + failure evidence.
	log "verify (init-mode): in-guest process table (pid ppid pgid sid stat args):"
	guest_out sh -c 'ps -eo pid,ppid,pgid,sid,stat,args || ps -ef' \
		| tee "$LOGS_DIR/init-mode-processes.txt" >&2 \
		|| log "verify (init-mode): process table dump failed (continuing; the probes below carry the diagnostics)"

	# 1. PID 1 is the guest init.
	probe="$(guest_value PID1 sh -c 'echo "PID1=$(tr "\0" " " < /proc/1/cmdline)"')" \
		|| fail "init-mode: /proc/1/cmdline probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	case "$probe" in
		*sandbox-init*) pass "guest PID 1 is the fsb guest init ($probe)" ;;
		*) fail "guest PID 1 is not the guest init: /proc/1/cmdline = $probe" ;;
	esac

	# 2. execd armed with --init.
	execd_pid="$(guest_value EXECD_PID sh -c 'echo "EXECD_PID=$(pgrep -x execd | head -1)"')" \
		|| fail "init-mode: execd pgrep probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	case "$execd_pid" in
		''|*[!0-9]*) fail "execd process not found in the guest (EXECD_PID='$execd_pid')" ;;
	esac
	probe="$(guest_value INIT_FLAG sh -c "tr \"\\0\" \"\\n\" < /proc/$execd_pid/cmdline | grep -qx -- --init && echo INIT_FLAG=1")" \
		|| fail "init-mode: execd cmdline probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	[[ "$probe" == "1" ]] \
		|| fail "execd (pid $execd_pid) does not carry --init (cmdline probe output: $probe)"
	pass "execd runs with --init (pid $execd_pid)"

	# 3. execd is PID 1's child (bootstrap exec'd away).
	probe="$(guest_value EXECD_PPID sh -c "echo \"EXECD_PPID=\$(grep '^PPid:' /proc/$execd_pid/status | grep -o '[0-9]*')\"")" \
		|| fail "init-mode: execd PPid probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	[[ "$probe" == "1" ]] \
		|| fail "execd (pid $execd_pid) is not a child of PID 1: PPid=$probe"
	pass "execd is a direct child of the guest init (PPid=1)"

	# 4. The entrypoint is execd's child. Exact name + argv selection: a
	# fuzzy pgrep -f would match the probe's own sh -c shell (the search
	# text sits in its cmdline) and pass vacuously on its execd PPid.
	probe="$(guest_value TAIL_PPID sh -c "$init_mode_entrypoint_probe")" \
		|| fail "init-mode: entrypoint probe failed (no tail -f /dev/null workload in the guest?): $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	[[ "$probe" == "$execd_pid" ]] \
		|| fail "the entrypoint is not a child of execd (pid $execd_pid): entrypoint PPid=$probe"
	pass "the baked entrypoint is supervised by execd (entrypoint PPid=$execd_pid)"

	# 5. The orphan reparents to execd (subreaper); left to exit for #6.
	guest_out sh -c 'sleep 6 >/dev/null 2>&1 & echo $! > /tmp/osb-init-mode-orphan.pid' >/dev/null \
		|| fail "init-mode: orphan spawn failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	probe="$(guest_value ORPHAN_PPID sh -c 'p=$(cat /tmp/osb-init-mode-orphan.pid); echo "ORPHAN_PPID=$(grep "^PPid:" /proc/$p/status | grep -o "[0-9]*")"')" \
		|| fail "init-mode: orphan PPid probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	[[ "$probe" == "$execd_pid" ]] \
		|| fail "the orphan did not reparent to execd (pid $execd_pid): orphan PPid=$probe"
	pass "orphaned processes reparent to execd (subreaper; orphan PPid=$execd_pid)"

	# 6. execd reaps the exited orphan (else it stays a Z child forever).
	init_mode_orphan_reaped() {
		[[ "$(guest_value REAPED sh -c 'p=$(cat /tmp/osb-init-mode-orphan.pid 2>/dev/null); if [ -n "$p" ] && [ ! -e "/proc/$p" ]; then echo "REAPED=1"; else echo "REAPED=0"; fi')" == "1" ]]
	}
	wait_for "execd reaps the exited orphan (pid gone from /proc)" 20 init_mode_orphan_reaped
	guest_out sh -c 'rm -f /tmp/osb-init-mode-orphan.pid' >/dev/null 2>&1 || true

	# 7. No zombies left.
	probe="$(guest_value ZOMBIES sh -c 'echo "ZOMBIES=$(ps -eo stat | grep -c "^Z" || true)"')" \
		|| fail "init-mode: zombie count probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	[[ "$probe" == "0" ]] \
		|| fail "the guest process table carries zombies: $probe in Z state"
	pass "guest process table is zombie-free after the orphan round trip"

	# 8. Report: subreaper tier, no signal shield. Capture the full response
	# and check the producer status before scanning for .hardening.
	local hardening_line capabilities_out
	capabilities_out="$(guest_out curl -fsS http://127.0.0.1:44772/v1/isolated/capabilities 2>/dev/null)" \
		|| fail "init-mode: capabilities probe failed: $(head -c 300 "$LOGS_DIR/init-mode-cmd.err" 2>/dev/null)"
	hardening_line="$(printf '%s' "$capabilities_out" | while IFS= read -r line || [[ -n "$line" ]]; do
		printf '%s' "$line" | jq -ce '.hardening // empty' 2>/dev/null && break
	done)" \
		|| fail "init-mode: capabilities probe failed (no .hardening in the response)"
	log "verify (init-mode): hardening report: $hardening_line"
	[[ "$(printf '%s' "$hardening_line" | jq -r '.init_mode')" == "subreaper" ]] \
		|| fail "hardening.init_mode is not subreaper: $hardening_line"
	[[ "$(printf '%s' "$hardening_line" | jq -r '.signal_shield')" == "false" ]] \
		|| fail "hardening.signal_shield is not false (fsb guest init owns PID 1): $hardening_line"
	pass "capabilities: hardening.init_mode=subreaper, signal_shield=false (fsb/Pool tier; no isolation TOML, floor layers stay disabled)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "init-mode sandbox deleted" 120 verify_sandbox_gone
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error verify-init-mode' ERR
	run_stage "execd-as-init verify (in-guest process topology)" init_mode_verify
fi
