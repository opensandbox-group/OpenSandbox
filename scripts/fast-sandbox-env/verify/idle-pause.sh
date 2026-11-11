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

# verify/idle-pause.sh — idle-pause / wake-on-access verify (OSEP-0024 profile)
#
# Requires the idle-pause profile: IDLE_PAUSE=1 up (or the $WORK/idle-pause
# marker it leaves) — Redis + the server [automation.idle_pause] sweeper +
# the gateway activity/wake flags. The opted-in sandbox is created through
# the raw lifecycle API because the CLI has no lifecycle.idlePolicy yet;
# idleTimeoutSeconds=30 (the server rejects X < 30) with onIdle=pause.
#
# Coverage:
#
#   1. Auto-pause + wake-on-access, IDLE_PAUSE_ROUNDS rounds (default 3):
#      with zero traffic the sandbox must reach Paused within X + sweep
#      interval + checkpoint window (240s per round); the next gateway
#      request must then WAKE it — traffic is driven at the paused
#      sandbox until /ping is served again, with no resume API call
#      (10min wake budget, marked 503s tolerated). Per-round pause/wake
#      latencies and the CR placement.attempt counter are logged.
#   2. wakeOnAccess=false invariant: such a sandbox is auto-paused as
#      well, but gateway traffic must NEVER wake it (a 200 — or even a
#      wake-503 — after traffic fails the round) and the state must stay
#      Paused until a MANUAL resume brings it back to Running.
#   3. Redis outage: scaling idle-pause-redis to 0 must suppress pausing
#      entirely — a sandbox must not be paused while the sweeper cannot
#      reach Redis. After Redis is scaled back, the missing-activity
#      tombstone produces a pause only after a full idle window.
#   4. Redis data loss: FLUSHALL must not accelerate the pause — activity
#      keys disappearing must be treated as "activity unknown", so the
#      pause happens only after a complete fresh idle window (a pause
#      within X-5s of the flush fails the round; the pause must still
#      arrive within a 300s budget).
#   5. Cleanup: kill the sandbox and wait until GET 404s.
#
# Housekeeping: the artifact store is pruned at entry — stale checkpoint /
# template sets are removed, the baseline golden template set and the
# indexes pointing into it are preserved.
#
# Runs as part of `integration.sh up` (the entry chains every verify
# script) or standalone against a live stack from `env.sh up`.
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- stage: idle-pause / wake-on-access verify (OSEP-0024 profile) --------------

# Poll the gateway until /ping returns 200 (marked 503s tolerated; the
# resume is never canceled). Fails on budget exhaustion.
idle_wake_wait() { # <label> [sandbox_id]
	local label="$1" id="${2:-$VERIFY_ID}" code
	local deadline=$(( $(now_ms) + 600000000000 )) wake_seen=0 next_state_log=0
	while :; do
		code="$(idle_ping_code "$id")"
		case "$code" in
			200) break ;;
			wake-503) wake_seen=1 ;;
		esac
		if (( $(now_ms) >= next_state_log )); then
			log "verify (idle-pause): $label still waiting (last=$code, state=$(sandbox_state_get))"
			next_state_log=$(( $(now_ms) + 30000000000 ))
		fi
		[[ "$(now_ms)" -ge "$deadline" ]] && fail "$label: wake /ping never returned 200 (last=$code)"
		sleep 1
	done
}

# Gateway answer for one /ping attempt: "200", "wake-503", or "other-<code>".
idle_ping_code() { # [sandbox_id] (default $VERIFY_ID)
	local id="${1:-$VERIFY_ID}" route headers code marker
	route="$(osb_api sandbox endpoint "$id" --port 44772 -o json 2>/dev/null \
		| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
	[[ -n "$route" ]] || { echo "noroute"; return 0; }
	headers="$(mktemp)"
	curl -sS -m 10 -D "$headers" -o /dev/null \
		-H "OpenSandbox-Ingress-To: $route" "$GATEWAY_URL/ping" >/dev/null 2>&1 || true
	code="$(tr -d '\r' < "$headers" | head -1 | awk '{print $2}')" || true
	marker="$(tr -d '\r' < "$headers" | grep -i '^OpenSandbox-Wake:' | awk '{print $2}')" || true
	rm -f "$headers"
	if [[ "$code" == "200" ]]; then
		echo "200"
	elif [[ "$code" == "503" && "$marker" == "expired" ]]; then
		echo "wake-503"
	else
		echo "other-${code:-none}"
	fi
}

idle_pause_verify() {
	# Raw curl create: the CLI has no lifecycle.idlePolicy parameter yet.
	# Marker-aware so a standalone run against an IDLE_PAUSE=1 stack works
	# without re-exporting the flag (down() removes the marker).
	[[ "$IDLE_PAUSE" == "1" || -f "$WORK/idle-pause" ]] \
		|| { log "idle-pause profile off (IDLE_PAUSE=0); skipping verify"; return 0; }
	local created t0 t1 code i
	local x=30
	local rounds="${IDLE_PAUSE_ROUNDS:-3}"
	# Every auto-pause round publishes a checkpoint set: drop stale ones
	# from earlier runs before this verify adds its own.
	prune_artifact_store
	[[ "$rounds" =~ ^[1-9][0-9]*$ ]] || die "IDLE_PAUSE_ROUNDS must be a positive integer (got $rounds)"
	log "verify (idle-pause): creating an opted-in sandbox (idleTimeoutSeconds=$x, rounds=$rounds) via the lifecycle API"
	t0="$(now_ms)"
	created="$(server_api POST /sandboxes "$(jq -n \
		--arg template "$TEMPLATE_ID" \
		--argjson timeout 3600 --argjson x "$x" \
		'{templateId: $template, timeout: $timeout,
		  lifecycle: {idlePolicy: {idleTimeoutSeconds: $x, onIdle: "pause", wakeOnAccess: true}},
		  metadata: {origin: "fast-sandbox-env-idle-pause"}}')" 2>/dev/null)" \
		|| fail "idle-pause create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "idle-pause target Running" 300 sandbox_running
	log "verify (idle-pause): $VERIFY_ID create->Running $(( ($(now_ms) - t0) / 1000000 ))ms"

	wait_for "pre-idle execd /ping 200 through the gateway (records activity)" 100 execd_ping_ok

	local -a pause_ms=() wake_ms=() attempts=()
	for ((i = 1; i <= rounds; i++)); do
		t0="$(now_ms)"
		wait_for "round $i/$rounds auto-paused within X + sweep + checkpoint" 240 sandbox_paused
		pause_ms+=("$(( ($(now_ms) - t0) / 1000000 ))")
		pass "round $i/$rounds: auto-paused in ${pause_ms[-1]}ms"

		t1="$(now_ms)"
		idle_wake_wait "round $i wake"
		wake_ms+=("$(( ($(now_ms) - t1) / 1000000 ))")
		attempts+=("$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" \
			-o jsonpath='{.status.placement.attempt}' 2>/dev/null || echo '?')")
		pass "round $i/$rounds: woke in ${wake_ms[-1]}ms (placement.attempt=${attempts[-1]})"
	done

	local joined_pause joined_wake joined_att
	printf -v joined_pause '%sms/' "${pause_ms[@]}"
	printf -v joined_wake '%sms/' "${wake_ms[@]}"
	printf -v joined_att '%s/' "${attempts[@]}"
	pass "idle-pause ${rounds} rounds: pause=${joined_pause%?} wake=${joined_wake%?} placement.attempt=${joined_att%?}"

	local main_id="$VERIFY_ID" created2 id2 payload
	log "verify (idle-pause): scenario wakeOnAccess=false"
	created2="$(server_api POST /sandboxes "$(jq -n \
		--arg template "$TEMPLATE_ID" --argjson timeout 3600 --argjson x "$x" \
		'{templateId: $template, timeout: $timeout,
		  lifecycle: {idlePolicy: {idleTimeoutSeconds: $x, onIdle: "pause", wakeOnAccess: false}},
		  metadata: {origin: "fast-sandbox-env-idle-pause"}}')" 2>/dev/null)" \
		|| fail "wakeOnAccess=false create failed"
	id2="$(printf '%s' "$created2" | jq -r '.id')"
	[[ -n "$id2" && "$id2" != "null" ]] || fail "wakeOnAccess=false create carried no id"
	VERIFY_ID="$id2"
	wait_for "wakeOnAccess=false target Running" 300 sandbox_running
	wait_for "wakeOnAccess=false auto-paused" 240 sandbox_paused
	code="$(idle_ping_code "$id2")"
	[[ "$code" == "200" || "$code" == "wake-503" ]] \
		&& fail "wakeOnAccess=false sandbox was woken by traffic (code=$code)"
	sleep 10
	sandbox_paused || fail "wakeOnAccess=false sandbox changed state without resume API"
	osb_api sandbox resume "$id2" --skip-health-check >/dev/null 2>&1 || fail "manual resume failed"
	wait_for "wakeOnAccess=false resumed manually" 240 sandbox_running
	pass "wakeOnAccess=false: traffic never woke it, manual resume did"
	osb_api sandbox kill "$id2" >/dev/null 2>&1 || true
	wait_for "wakeOnAccess=false sandbox deleted" 120 verify_sandbox_gone
	VERIFY_ID="$main_id"

	log "verify (idle-pause): scenario redis down"
	# The previous scenario may have left it Paused.
	idle_wake_wait "pre-redis-down wake"
	t0="$(now_ms)"
	kubectl -n "$OSB_NS" scale deploy/idle-pause-redis --replicas=0 >/dev/null
	while sandbox_running; do
		(( $(now_ms) - t0 >= 90000000000 )) && break
		sleep 5
	done
	sandbox_running || fail "sandbox paused while Redis was scaled down"
	pass "redis down: no pauses while Redis unreachable"
	kubectl -n "$OSB_NS" scale deploy/idle-pause-redis --replicas=1 >/dev/null
	wait_for "redis back ready" 120 \
		kubectl -n "$OSB_NS" rollout status deploy/idle-pause-redis --timeout=10s
	wait_for "tombstone pause after redis recovery" 300 sandbox_paused
	pass "redis recovery: missing-key tombstone paused after the full idle window"

	idle_wake_wait "post-flushall wake"
	t0="$(now_ms)"
	kubectl -n "$OSB_NS" exec deploy/idle-pause-redis -- redis-cli FLUSHALL >/dev/null 2>&1 \
		|| fail "FLUSHALL failed"
	while :; do
		if sandbox_paused; then
			(( $(now_ms) - t0 < (x - 5) * 1000000000 )) \
				&& fail "FLUSHALL accelerated the pause ($(( ($(now_ms) - t0) / 1000000 ))ms after flush)"
			break
		fi
		(( $(now_ms) - t0 >= 300000000000 )) && fail "no pause within budget after FLUSHALL"
		sleep 2
	done
	pass "flushall: pause delayed to a full idle window after data loss ($(( ($(now_ms) - t0) / 1000000 ))ms)"


	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "idle-pause sandbox deleted" 120 verify_sandbox_gone
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	# Same profile check as the entry script's idle-e2e action: the marker is
	# left by an IDLE_PAUSE=1 up and removed by down.
	if [[ "$IDLE_PAUSE" != "1" && ! -f "$WORK/idle-pause" ]]; then
		die "idle-pause verify requires the idle-pause profile (IDLE_PAUSE=1 up)"
	fi
	resolve_osb
	require_template_id
	trap 'on_error idle-pause-verify' ERR
	run_stage "idle-pause / wake-on-access verify (OSEP-0024 profile)" idle_pause_verify
fi
