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

# verify/hardening.sh — execd hardening floor verify on fsb (OSEP-0018).
#
# init-mode.sh pins the execd-as-init topology (subreaper under the fsb
# guest init) on the plain template, where the floor layers stay disabled.
# This stage owns the hardened tier: a dedicated overlaybd template carries
# EXECD_ISOLATION_CONFIG=/opt/opensandbox/isolation.toml (built by
# template_up_overlaybd_hardening in integration-env.sh, whose hardened
# source image is built from THIS checkout and pushed to the in-cluster
# registry with the launcher prelude + TOML baked at execd's trusted
# runtime paths), so execd loads the isolation TOML, finds the launcher,
# and arms the pre-exec privilege floor for every user-code launch.
#
# Pinned (mirroring the docker/k8s legs in
# tests/python/tests/test_execd_hardening_e2e.py):
#
#   1. launcher present and executable at /opt/opensandbox/opensandbox-launcher
#   2. isolation TOML readable and hardening-gated ([hardening] enabled)
#   3. capabilities endpoint: init_mode=subreaper, signal_shield=false,
#      cap_drop + seccomp active (the TOML-load + launcher-found proof),
#      landlock active|unsupported (honest), ebpf disabled
#   4. the /command workload runs through the launcher: CapEff=0, CapBnd=0,
#      Seccomp=2, NoNewPrivs=1, uid root (fsb execd is root by construction;
#      the launcher drops to execd's own uid, never a foreign identity),
#      execd's config/credential env stripped from its environ
#   5. Landlock (when active): /tmp writable, /etc/passwd read-only, a
#      script written to /tmp still executes
#   6. sustained fork-heavy mix keeps a bounded, zombie-free process table
#      under the fsb subreaper: 20 tracked sleepers stay `kill -0`-alive
#      under /command churn, then fully disappear (a zombie would still
#      answer kill -0 — disappearance is the reap proof; kill(2) is outside
#      Landlock's handled accesses and the seccomp deny list)
#
# Landlock observation limit (documented, matching the docker/k8s hardening
# e2e): the launcher's ruleset grants read only beneath the launcher-wrapped
# process's own /proc/<pid> directory — anchored at its execve'd PID — so a
# forked helper cannot even read its own /proc/self, and no confined probe
# can read another process's /proc entries. The /command shell IS the
# launcher-wrapped process (same PID across the launcher's execve), so the
# floor is pinned from inside that anchored process; the baked `tail -f
# /dev/null` entrypoint is uninstrumentable on fsb (it rides the same
# execd -> launcher path the capabilities report arms, and init-mode.sh pins
# it as execd's supervised child) — its direct per-process pin stays on the
# docker/k8s legs, where the test entrypoint self-dumps.
#
# Known fsb gaps, deliberately not pinned here (the docker/k8s legs own
# them): the custom [seccomp] deny + keep_capabilities override (a second
# hardened template would cost another full OCI build), the EXECD_INIT
# drift tier (the fsb server always bakes EXECD_INIT=1), and the
# missing-CAP_SETPCAP degradation (a VM guest has no container cap ceiling
# to drop it from).
#
# Runs as part of `integration-env.sh verify-overlaybd` (run_stage executes
# this script) or standalone against a live stack from `integration-env.sh
# up`; standalone runs resolve the hardened template id at
# $WORK/hardening-template-id (written by verify-overlaybd, or create a
# template with the env yourself and point HARDENING_TEMPLATE_ID_FILE at it).

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

HARDENING_TEMPLATE_ID_FILE="${HARDENING_TEMPLATE_ID_FILE:-$WORK/hardening-template-id}"
LAUNCHER_GUEST_PATH="/opt/opensandbox/opensandbox-launcher"
ISOLATION_TOML_GUEST_PATH="/opt/opensandbox/isolation.toml"
CAPABILITIES_URL="http://127.0.0.1:44772/v1/isolated/capabilities"

# execd's own configuration env (isolation.ExecdConfigEnvBlacklist) — the
# launcher unsets this exact set before execve. JUPYTER_HOST/JUPYTER_TOKEN
# are not set on the fsb golden image but the strip must hold regardless.
HARDENING_ENV_BLACKLIST="EXECD_ACCESS_TOKEN EXECD_ISOLATION_CONFIG EXECD_ENVS EXECD_LIFECYCLE_CONFIG EXECD_RUNTIME_INIT OPENSANDBOX_LIFECYCLE JUPYTER_HOST JUPYTER_TOKEN"
# Pid files for the fork-heavy mix: the /command shell records the sleeper
# pids it spawned (kill(2) needs no /proc enumeration, which Landlock denies).
HARDENING_SLEEPER_DIR="/tmp/hardening-sleepers"

# guest_out <words...>: run a command in the sandbox via the CLI.
guest_out() {
	osb_api command run "$VERIFY_ID" -- "$@" 2>"$LOGS_DIR/hardening-cmd.err"
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

# hardening_status_fields <words...>: print the requested /proc/<pid>/status
# lines as KEY=VALUE pairs (the proc layout is "CapEff:0000..."; only the
# first colon splits, so multi-field lines like Uid keep their tab-separated
# fields intact).
hardening_status_fields() { # <probe-words...>
	local out status line key value
	out="$(guest_out "$@")"
	status=$?
	[[ "$status" -eq 0 ]] || return "$status"
	while IFS= read -r line || [[ -n "$line" ]]; do
		case "$line" in
			[A-Z]*:*)
				key="${line%%:*}"
				value="${line#*:}"
				# Strip the leading whitespace proc puts after the colon
				# ("Seccomp:\t2"), keeping inner field tabs (Uid) intact.
				value="${value#"${value%%[![:space:]]*}"}"
				printf '%s=%s\n' "$key" "$value"
				;;
		esac
	done <<<"$out"
	return 0
}

hardening_field() { # <KEY> <KEY=VALUE blob (multi-line)>
	local key="$1" blob="$2" line
	while IFS= read -r line || [[ -n "$line" ]]; do
		case "$line" in
			"$key="*) printf '%s\n' "${line#*=}"; return 0 ;;
		esac
	done <<<"$blob"
	return 1
}

# hardening_template_id: resolve the hardened template id (argv > env file).
hardening_template_id() { # [template-id]
	local template_id="${1:-}"
	if [[ -z "$template_id" && -s "$HARDENING_TEMPLATE_ID_FILE" ]]; then
		template_id="$(cat "$HARDENING_TEMPLATE_ID_FILE" 2>/dev/null || true)"
	fi
	[[ -n "$template_id" ]] \
		|| die "no hardened template id at $HARDENING_TEMPLATE_ID_FILE (run 'integration-env.sh verify-overlaybd' first, or pass a template id)"
	printf '%s' "$template_id"
}

hardening_floor_verify() { # [template-id]
	local template_id
	template_id="$(hardening_template_id "$@")"
	local probe

	log "verify (hardening): creating a sandbox via the opensandbox CLI (templateId=$template_id)"
	local created
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-hardening -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "hardening target Running" 600 sandbox_running

	# Ground truth + failure evidence first. Best-effort: under an active
	# Landlock ruleset ps cannot enumerate other processes' /proc entries,
	# so the dump may be partial (the launcher's rules grant read only
	# beneath /proc/self + /proc/sys); the pins below carry the evidence.
	log "verify (hardening): in-guest process table (pid ppid stat args):"
	guest_out sh -c 'ps -eo pid,ppid,stat,args || ps -ef' \
		| tee "$LOGS_DIR/hardening-processes.txt" >&2 \
		|| log "verify (hardening): process table dump failed (continuing; the probes below carry the diagnostics)"

	# 1. The launcher prelude is staged in the golden image at execd's
	#    trusted runtime path (findLauncher() stats exactly this path before
	#    any $PATH fallback).
	probe="$(guest_value LAUNCHER sh -c '[ -x /opt/opensandbox/opensandbox-launcher ] && echo LAUNCHER=1 || echo LAUNCHER=0')" \
		|| fail "hardening: launcher probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	[[ "$probe" == "1" ]] \
		|| fail "opensandbox-launcher is not present/executable at $LAUNCHER_GUEST_PATH — the hardened source image lacks the bake. The lane builds + pushes it from this checkout (template_up_overlaybd_hardening); for standalone templates, build + publish the golden image per scripts/fast-sandbox-env/Dockerfile.sandbox-test-image first."
	pass "launcher present at $LAUNCHER_GUEST_PATH"

	# 2. The isolation TOML is staged and hardening-gated.
	probe="$(guest_value TOML sh -c "grep -q '^\[hardening\]' $ISOLATION_TOML_GUEST_PATH && grep -q '^enabled = true' $ISOLATION_TOML_GUEST_PATH && echo TOML=1 || echo TOML=0")" \
		|| fail "hardening: isolation TOML probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	[[ "$probe" == "1" ]] \
		|| fail "$ISOLATION_TOML_GUEST_PATH missing or not hardening-gated ([hardening] enabled = true)"
	pass "isolation TOML staged at $ISOLATION_TOML_GUEST_PATH ([hardening] enabled)"

	# 3. The capabilities report — captured BEFORE the behavioral probes so
	#    the Landlock tier gates them. cap_drop=active doubles as the proof
	#    execd actually loaded this TOML (the floor only arms on
	#    [hardening] enabled) and found the launcher (missing -> degraded).
	local hardening_line capabilities_out
	capabilities_out="$(guest_out curl -fsS $CAPABILITIES_URL 2>/dev/null)" \
		|| fail "hardening: capabilities probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	printf '%s\n' "$capabilities_out" > "$LOGS_DIR/hardening-report.json"
	hardening_line="$(printf '%s' "$capabilities_out" | while IFS= read -r line || [[ -n "$line" ]]; do
		printf '%s' "$line" | jq -ce '.hardening // empty' 2>/dev/null && break
	done)" \
		|| fail "hardening: capabilities probe failed (no .hardening in the response)"
	log "verify (hardening): hardening report: $hardening_line"
	[[ "$(printf '%s' "$hardening_line" | jq -r '.init_mode')" == "subreaper" ]] \
		|| fail "hardening.init_mode is not subreaper: $hardening_line"
	[[ "$(printf '%s' "$hardening_line" | jq -r '.signal_shield')" == "false" ]] \
		|| fail "hardening.signal_shield is not false (fsb guest init owns PID 1): $hardening_line"
	for probe in cap_drop seccomp; do
		[[ "$(printf '%s' "$hardening_line" | jq -r ".$probe.state")" == "active" ]] \
			|| fail "hardening.$probe is not active (TOML not loaded or launcher missing): $hardening_line"
	done
	pass "capabilities: cap_drop + seccomp active — execd loaded the TOML and armed the floor"
	local landlock_state landlock_message
	landlock_state="$(printf '%s' "$hardening_line" | jq -r '.landlock.state')"
	landlock_message="$(printf '%s' "$hardening_line" | jq -r '.landlock.message // empty')"
	case "$landlock_state" in
		active)
			pass "capabilities: landlock active"
			;;
		unsupported)
			pass "capabilities: landlock unsupported (honest fail-open; guest kernel: $landlock_message)"
			;;
		*)
			fail "hardening.landlock state is '$landlock_state' (want active|unsupported): $hardening_line"
			;;
	esac
	[[ "$(printf '%s' "$hardening_line" | jq -r '.ebpf.state')" == "disabled" ]] \
		|| fail "hardening.ebpf is not disabled (eBPF observation is not part of the floor): $hardening_line"

	# 4. The /command workload runs through the launcher (it IS the
	#    launcher-exec'd process — same PID across the launcher's execve, so
	#    its /proc/<pid> dir is the anchored Landlock grant). CapEff/CapBnd
	#    zero, seccomp filter mode, no_new_privs, identity not relocated.
	#    The forked grep reads the anchored pid dir via /proc/$$ — allowed
	#    by the grant (its own /proc/self would not be).
	local cmd_fields cmd_uid
	cmd_fields="$(hardening_status_fields sh -c 'grep -E "^(CapEff|CapBnd|Seccomp|NoNewPrivs|Uid):" /proc/$$/status')" \
		|| fail "hardening: /command status probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	for probe in CapEff CapBnd; do
		[[ "$(hardening_field "$probe" "$cmd_fields")" == "0000000000000000" ]] \
			|| fail "/command $probe is not zero: $cmd_fields"
	done
	[[ "$(hardening_field Seccomp "$cmd_fields")" == "2" ]] \
		|| fail "/command Seccomp is not 2 (seccomp filter mode): $cmd_fields"
	[[ "$(hardening_field NoNewPrivs "$cmd_fields")" == "1" ]] \
		|| fail "/command NoNewPrivs is not 1: $cmd_fields"
	# The launcher drops to execd's own uid; the fsb guest execd is root by
	# construction (the kernel boots the guest init as root), so root it is.
	cmd_uid="$(hardening_field Uid "$cmd_fields" | awk '{print $1}')"
	[[ -n "$cmd_uid" && "$cmd_uid" == "0" ]] \
		|| fail "/command uid ($cmd_uid) is not execd's identity (root on fsb): the floor must not relocate identity"
	pass "/command floor: CapEff=0 CapBnd=0 Seccomp=2 NoNewPrivs=1 uid=$cmd_uid (execd's uid)"

	# 5. Env strip: the launcher unsets execd's config/credential env before
	#    execve — absent from the launcher-wrapped shell's environ.
	#    /proc/*/environ is NUL-separated, so the probe reads it through
	#    `tr '\0' '\n'` before matching.
	local strip_probe
	strip_probe='f=""; for v in '"$HARDENING_ENV_BLACKLIST"'; do tr "\0" "\n" < /proc/$$/environ | grep -q "^$v=" && { f="$v"; break; }; done; echo "STRIP_FAIL=$f"'
	probe="$(guest_value STRIP_FAIL sh -c "$strip_probe")" \
		|| fail "hardening: env strip probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	[[ -z "$probe" ]] \
		|| fail "/command env carries execd config env: STRIP_FAIL=$probe"
	pass "env strip: execd config env absent from the launcher-wrapped workload"

	# 6. Landlock behavior — only meaningful when the layer reports active;
	#    an unsupported guest kernel fail-opens honestly (handled above).
	if [[ "$landlock_state" == "active" ]]; then
		probe="$(guest_value TMP_WRITABLE sh -c 'if echo ok > /tmp/landlock-probe 2>/dev/null; then echo TMP_WRITABLE=1; else echo TMP_WRITABLE=0; fi')" \
			|| fail "hardening: /tmp write probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
		[[ "$probe" == "1" ]] || fail "Landlock: /tmp is not writable under confinement"
		probe="$(guest_value ETC_WRITABLE sh -c 'if echo x >> /etc/passwd 2>/dev/null; then echo ETC_WRITABLE=1; else echo ETC_WRITABLE=0; fi')" \
			|| fail "hardening: /etc/passwd write probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
		[[ "$probe" == "0" ]] || fail "Landlock: /etc/passwd is writable under confinement"
		probe="$(guest_value EXEC_OK sh -c 'printf "#!/bin/sh\necho exec-ok\n" > /tmp/landlock-exec.sh && chmod +x /tmp/landlock-exec.sh && /tmp/landlock-exec.sh > /dev/null 2>&1 && echo EXEC_OK=1 || echo EXEC_OK=0')" \
			|| fail "hardening: /tmp exec probe failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
		[[ "$probe" == "1" ]] || fail "Landlock: a script written to /tmp cannot execute (no execute grant)"
		pass "Landlock: /tmp writable, /etc/passwd read-only, /tmp script executes"
	fi

	# 7. Sustained fork-heavy mix under the fsb subreaper, confined-safe: 20
	#    tracked sleepers orphan into execd while /command churns (execd
	#    stays responsive = the table is not wedged), then FULLY disappear —
	#    a zombie still answers kill -0, so disappearance is the reap proof.
	#    kill(2) is outside Landlock's handled accesses and the seccomp deny
	#    list, and the pids travel through /tmp files (no cross-pid /proc
	#    enumeration, which Landlock denies).
	guest_out sh -c "rm -rf $HARDENING_SLEEPER_DIR; mkdir -p $HARDENING_SLEEPER_DIR; i=0; while [ \$i -lt 20 ]; do sleep 25 & echo \$! > $HARDENING_SLEEPER_DIR/\$i; i=\$((i+1)); done; echo SLEEPERS=20" >/dev/null \
		|| fail "hardening: sleeper spawn failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	[[ "$(guest_value ALIVE sh -c "n=0; for f in $HARDENING_SLEEPER_DIR/*; do kill -0 \$(cat \$f) 2>/dev/null && n=\$((n+1)); done; echo ALIVE=\$n")" == "20" ]] \
		|| fail "fork-heavy mix: the 20 tracked sleepers are not all alive under the subreaper"
	local i
	for i in 1 2 3 4 5; do
		guest_out sh -c 'echo churn' >/dev/null \
			|| fail "hardening: /command churn round $i failed: $(head -c 300 "$LOGS_DIR/hardening-cmd.err" 2>/dev/null)"
	done
	pass "fork-heavy mix: 20 sleepers alive, /command churns served (bounded, responsive)"
	wait_for "fork-heavy sleepers fully reaped by the subreaper" 25 hardening_sleepers_reaped
	pass "fork-heavy mix: sleepers fully disappeared (zombies would still answer kill -0)"

	log "verify (hardening): skipping by design on fsb — custom [seccomp]/keep_capabilities override (second OCI template build), EXECD_INIT drift (the fsb server always bakes EXECD_INIT=1), missing-CAP_SETPCAP degradation (no VM cap ceiling); the docker/k8s hardening legs pin these"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "hardening sandbox deleted" 120 verify_sandbox_gone
}

hardening_sleepers_reaped() {
	[[ "$(guest_value REAPED sh -c "r=20; for f in $HARDENING_SLEEPER_DIR/*; do kill -0 \$(cat \$f) 2>/dev/null && r=\$((r-1)); done; echo REAPED=\$r")" == "20" ]]
}

# --- standalone entry

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	trap 'on_error verify-hardening' ERR
	run_stage "execd hardening floor verify (hardened overlaybd template, OSEP-0018)" hardening_floor_verify
fi
