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

# OSEP-0023 M1/M2 cost measurements; sourced by integration-env.sh.
# Method, report fields and BENCH_* parameters are documented in README.md.
# M1 registers subjects without Vault writes; M2 records Vault ACK costs.
# Reports classify paths and omit credentials, digests and subject identities.

CBB_SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	exec "$CBB_SELF_DIR/integration-env.sh" bench-credential-bound
fi
# shellcheck source=verify/credential-bound.sh
source "$CBB_SELF_DIR/verify/credential-bound.sh"

BENCH_REPORT_PY="$CBB_SELF_DIR/bench-report.py"
BENCH_CREATE_N="${BENCH_CREATE_N:-200}"
BENCH_CREATE_CONCURRENCY="${BENCH_CREATE_CONCURRENCY:-10}"
BENCH_CREATE_ROUNDS="${BENCH_CREATE_ROUNDS:-3}"
BENCH_HANDSHAKES="${BENCH_HANDSHAKES:-200}"
BENCH_HANDSHAKE_WARMUP="${BENCH_HANDSHAKE_WARMUP:-10}"

BENCH_LEG=""
BENCH_DIR=""
BENCH_PREV_EXIT_TRAP=""
BENCH_RAW=""
BENCH_POLICY=""
BENCH_POD_UID=""

# --- small helpers

bench_validate_knobs() {
	local name value
	for name in BENCH_CREATE_N BENCH_CREATE_CONCURRENCY BENCH_CREATE_ROUNDS BENCH_HANDSHAKES BENCH_HANDSHAKE_WARMUP; do
		value="${!name}"
		[[ "$value" =~ ^[0-9]+$ ]] || die "$name must be a non-negative integer (got: $value)"
	done
	(( BENCH_HANDSHAKES <= 5000 )) || die "BENCH_HANDSHAKES must be at most 5000 (guest client limit)"
	(( BENCH_HANDSHAKES >= 1 && BENCH_CREATE_ROUNDS >= 1 && BENCH_CREATE_N >= 1 )) \
		|| die "BENCH_HANDSHAKES, BENCH_CREATE_ROUNDS and BENCH_CREATE_N must be at least 1"
}

bench_ns() { now_ms; }   # nanoseconds (GNU date +%s%N)
bench_elapsed_ms() { echo $(( ($(bench_ns) - $1) / 1000000 )); }

# Non-failing bounded wait (the bench records a shortfall instead of dying).
bench_wait() { # <attempts> <command...>
	local attempts="$1" attempt=0
	shift
	until "$@" >/dev/null 2>&1; do
		attempt=$((attempt + 1))
		[[ "$attempt" -lt "$attempts" ]] || return 1
		sleep 2
	done
}

bench_json_set() { # <file> <jq-filter> [jq args...]: in-place update
	local file="$1" filter="$2"
	shift 2
	jq "$@" "$filter" "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

bench_report() { # <m1|m2> <spec-file> <report-file>
	python3 "$BENCH_REPORT_PY" "$1" "$2" "$3" || fail "bench: report assembly for $1 failed"
}

# --- guest runs (the client is uploaded by cb_create_sandbox)

bench_guest_start() { # <sandbox-id> <client args...>
	local id="$1" out
	shift
	out="$(osb_api command run "$id" --background -o json -- python3 "$CB_GUEST_DIR/client.py" "$@" \
		--host "$MITM_ECHO_DOMAIN" --ip "$CB_ORIGIN_IP" --port "$MITM_ECHO_PORT" --cafile "$CB_GUEST_CAFILE" \
		2>>"$LOGS_DIR/bench-guest.err")" || return 1
	[[ -n "$(printf '%s' "$out" | jq -r '.execution_id // empty' 2>/dev/null)" ]]
}

bench_guest_fetch() { # <sandbox-id> <guest-json> <host-json>
	local out
	out="$(osb_api command run "$1" -- cat "$2" 2>/dev/null)" || return 1
	out="$(printf '%s\n' "$out" | grep '^{' | tail -n 1)"
	[[ -n "$out" ]] && printf '%s' "$out" | jq -e '.v == 1' >/dev/null 2>&1 || return 1
	printf '%s\n' "$out" > "$3"
}

# handshake-bench in the background, result copied to <host-json>.
bench_handshakes_start() { # <sandbox-id> <name> <samples>
	bench_guest_start "$1" handshake-bench --tag "bench-$2" --samples "$3" --out "$CB_GUEST_DIR/$2.json" \
		|| fail "bench: guest handshake-bench $2 failed to start"
}

bench_handshakes_collect() { # <sandbox-id> <name> <samples> <host-json>
	bench_wait $(( 60 + $3 / 2 )) bench_guest_fetch "$1" "$CB_GUEST_DIR/$2.json" "$4" \
		|| fail "bench: guest handshake-bench $2 produced no result"
}

bench_handshakes() { # <sandbox-id> <name> <samples> <host-json>
	bench_handshakes_start "$1" "$2" "$3"
	bench_handshakes_collect "$@"
}

# U/B readiness: one fresh connection answered 200 by the origin with the
# leaf class this leg must present and no credential; a credential or the
# other leaf class fails at once (traffic evidence is never retried away).
bench_until_reachable() { # <sandbox-id> <tag> <origin|mitm>
	local attempt leaf
	for attempt in $(seq 1 60); do
		cb_guest_once "$1" "$2-$attempt"
		! cb_result_other_credential absent \
			|| fail "bench: unbound sandbox saw a credential ($(cb_result_summary))"
		if [[ "$CB_RC" == 0 && -n "$CB_RESULT" ]]; then
			leaf="$(printf '%s' "$CB_RESULT" | jq -r --arg o "$CB_ORIGIN_LEAF" 'if .leaf_sha256 == $o then "origin" else "mitm" end')"
			[[ "$leaf" == "$3" ]] || fail "bench: unbound sandbox presented a $leaf leaf, expected $3 in this leg"
			printf '%s' "$CB_RESULT" | jq -e '.last.status == 200' >/dev/null && { pass "bench: unbound sandbox reaches the origin ($3 leaf)"; return 0; }
		fi
		sleep 2
	done
	fail "bench: unbound sandbox never reached the origin ($(cb_result_summary))"
}

bench_bind() { # <sandbox-id> <fake-value> <label> -> appends the ACK ms to BENCH_BIND_MS
	local t0
	t0="$(bench_ns)"
	cb_vault_commit POST "$1" "$(cb_vault_body "$2")" "$3"
	BENCH_BIND_MS+=("$(bench_elapsed_ms "$t0")")
}

bench_bind_ms_json() {
	printf '%s\n' ${BENCH_BIND_MS[@]+"${BENCH_BIND_MS[@]}"} | jq -s -c 'map(tonumber)'
}

# The bench action loads the template id written by `up`.
bench_load_template_id() {
	CB_TEMPLATE_ID="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$CB_TEMPLATE_ID" ]] || die "no template id at $WORK/template-id (run up first)"
}

# --- environment fields

bench_environment() {
	local pods egress git_commit
	pods="$(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o json 2>/dev/null || echo '{}')"
	egress="$(printf '%s' "$pods" | jq -c '[.items[]? | .spec.containers[] | select(.name == "egress") | .resources] | unique' 2>/dev/null || echo null)"
	git_commit="${GITHUB_SHA:-$(git -C "$OSB_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)}"
	jq -n \
		--arg leg "$BENCH_LEG" \
		--arg mode "${EGRESS_INTERCEPTION_MODE:-unset (all)}" \
		--arg recorded "$(date -u +%FT%TZ)" \
		--arg commit "$git_commit" \
		--arg fsb "$FSB_COMMIT" \
		--arg pool "$POOL_NAME" --arg pool_mode "$POOL_MODE" \
		--argjson pool_min "$POOL_MIN" --argjson per_pod "$MAX_SANDBOXES_PER_POD" \
		--arg oci "$OCI_STACK" \
		--arg kernel "$(uname -r)" \
		--arg cpu "$(awk -F': ' '/^model name/ { print $2; exit }' /proc/cpuinfo 2>/dev/null || echo unknown)" \
		--arg cpus "$(nproc 2>/dev/null || echo unknown)" \
		--arg mem "$(awk '/^MemTotal:/ { print $2 " " $3; exit }' /proc/meminfo 2>/dev/null || echo unknown)" \
		--arg k8s "$(kubectl version -o json 2>/dev/null | jq -r '.serverVersion.gitVersion // "unknown"' 2>/dev/null || echo unknown)" \
		--arg egress_image "$IMG_EGRESS" \
		--argjson egress "${egress:-null}" \
		--arg template "native (\$WORK/template-id)" \
		'{leg: $leg, interception_mode: $mode, recorded_at: $recorded, opensandbox_commit: $commit,
		  fast_sandbox_commit: $fsb, pool: {name: $pool, mode: $pool_mode, pool_min: $pool_min,
		  max_sandboxes_per_pod: $per_pod}, oci_stack: $oci, host: {kernel: $kernel, cpu_model: $cpu,
		  cpus: $cpus, mem_total: $mem}, kubernetes: $k8s, egress_image: $egress_image,
		  egress_resources_live: $egress, template: $template, topology: "kind on the self-hosted kvm runner"}' \
		> "$BENCH_DIR/environment.json"
}

# --- M1 create latency

bench_m1() {
	local spec="$BENCH_RAW/m1-spec.json" round rc template_id
	template_id="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$template_id" ]] || die "bench: no template id at $WORK/template-id (run up first)"
	jq -n --arg leg "$BENCH_LEG" --argjson n "$BENCH_CREATE_N" --argjson c "$BENCH_CREATE_CONCURRENCY" \
		--argjson r "$BENCH_CREATE_ROUNDS" \
		'{leg: $leg, settings: {n: $n, concurrency: $c, rounds: $r, template: "native", network_policy: true},
		  rounds: [], notes: ["no Vault is written during M1; bound-create cost is covered by vault_bind_ack_ms in M2"]}' > "$spec"
	if ! command -v uv >/dev/null 2>&1; then
		bench_json_set "$spec" '{leg: .leg, not_run: "uv not on PATH (create-bench.py runs in the tests/python environment)"}'
	else
		for round in $(seq 1 "$BENCH_CREATE_ROUNDS"); do
			rc=0
			log "bench: M1 round $round/$BENCH_CREATE_ROUNDS ($BENCH_CREATE_N creates, concurrency $BENCH_CREATE_CONCURRENCY; output in $LOGS_DIR/bench-create-round-$round.log)"
			( cd "$OSB_ROOT/tests/python" \
				&& N="$BENCH_CREATE_N" CONCURRENCY="$BENCH_CREATE_CONCURRENCY" TEMPLATE_ID="$template_id" \
					DOMAIN="127.0.0.1:$SERVER_HOST_PORT" PROTOCOL=http API_KEY="$SERVER_API_KEY" \
					DISABLE_METRICS=1 NETWORK_POLICY_FILE="$BENCH_POLICY" \
					REPORT_PATH="$BENCH_RAW/m1-round-$round.json" \
					uv run python "$CBB_SELF_DIR/create-bench.py" ) \
				> "$LOGS_DIR/bench-create-round-$round.log" 2>&1 || rc=$?
			bench_json_set "$spec" '.rounds += [{round: $r, rc: $rc, report: $p}]' \
				--argjson r "$round" --argjson rc "$rc" --arg p "$BENCH_RAW/m1-round-$round.json"
		done
	fi
	bench_report m1 "$spec" "$BENCH_DIR/m1-create.json"
}

# --- M2 handshake latency on a colocated unbound/bound pair

bench_pair() {
	local cred_b="$1" code_b="$2" u b unbound_leaf
	cb_create_sandbox bench-u "$BENCH_POLICY"; u="$CB_LAST_ID"
	cb_placement "$u" || fail "bench: U's placement (fastletPodUID) is not a live pool Pod"
	BENCH_POD_UID="$CB_POD_UID"
	cb_create_colocated bench-b "$BENCH_POLICY" "$BENCH_POD_UID"; b="$CB_LAST_ID"
	bench_bind "$b" "$cred_b" "bench vault for B"
	cb_until "bench: bound sandbox B injected over a MITM leaf" 60 "$b" bench-b-ready "$code_b"
	unbound_leaf=mitm
	[[ "$BENCH_LEG" != credential-bound ]] || unbound_leaf=origin
	bench_until_reachable "$u" bench-u-ready "$unbound_leaf"
	BENCH_U="$u"; BENCH_B="$b"
}

bench_m2() {
	local code_b="$1" spec="$BENCH_RAW/m2-spec.json" name sbx leaf cred desc
	jq -n --arg leg "$BENCH_LEG" --arg leaf "$CB_ORIGIN_LEAF" --argjson s "$BENCH_HANDSHAKES" \
		--argjson w "$BENCH_HANDSHAKE_WARMUP" --argjson bind "$(bench_bind_ms_json)" \
		'{leg: $leg, origin_leaf: $leaf, settings: {samples: $s, warmup: $w, sequential: true,
		  connection: "new TCP+TLS connection per sample, one HTTP/1.1 request, close",
		  sandboxes: "unbound U and bound B on the same Fastlet Pod"}, paths: [], vault_bind_ack_ms: $bind,
		  notes: ["warmup samples are discarded (first-use MITM leaf generation and caches)"]}' > "$spec"
	local -a run=() skip=()
	if [[ "$BENCH_LEG" == credential-bound ]]; then
		run=("cb_bound|$BENCH_B|mitm|$code_b|credential-bound, bound host: decrypt + inject"
			"cb_unbound|$BENCH_U|origin|absent|credential-bound, unbound host: opaque passthrough")
		skip=("all_unbound|interception all, unbound host: decrypt" "all_bound|interception all, bound host: decrypt + inject")
	else
		run=("all_unbound|$BENCH_U|mitm|absent|interception all, unbound host: decrypt"
			"all_bound|$BENCH_B|mitm|$code_b|interception all, bound host: decrypt + inject")
		skip=("cb_bound|credential-bound, bound host: decrypt + inject" "cb_unbound|credential-bound, unbound host: opaque passthrough")
	fi
	local entry
	for entry in "${run[@]}"; do
		IFS='|' read -r name sbx leaf cred desc <<<"$entry"
		if (( BENCH_HANDSHAKE_WARMUP > 0 )); then
			bench_handshakes "$sbx" "m2-warmup-$name" "$BENCH_HANDSHAKE_WARMUP" "$BENCH_RAW/m2-warmup-$name.json"
		fi
		bench_handshakes "$sbx" "m2-$name" "$BENCH_HANDSHAKES" "$BENCH_RAW/m2-$name.json"
		bench_json_set "$spec" '.paths += [{name: $n, description: $d, expect_leaf: $l, expect_cred: $c, raw: $p}]' \
			--arg n "$name" --arg d "$desc" --arg l "$leaf" --arg c "$cred" --arg p "$BENCH_RAW/m2-$name.json"
	done
	for entry in "${skip[@]}"; do
		IFS='|' read -r name desc <<<"$entry"
		bench_json_set "$spec" '.paths += [{name: $n, description: $d, not_run: $r}]' \
			--arg n "$name" --arg d "$desc" --arg r "measured in the other leg (this leg: $BENCH_LEG)"
	done
	bench_report m2 "$spec" "$BENCH_DIR/m2-handshake.json"
}

bench_summary() {
	[[ -n "$BENCH_DIR" && -d "$BENCH_DIR" ]] || return 0
	python3 "$BENCH_REPORT_PY" summary "$BENCH_DIR/environment.json" "$BENCH_DIR/bench-summary.json" \
		"$BENCH_DIR/m1-create.json" "$BENCH_DIR/m2-handshake.json"
}

bench_cleanup() {
	cb_cleanup
	bench_summary >/dev/null 2>&1 || true
}

# --- the action

credential_bound_bench() {
	BENCH_LEG="${EGRESS_INTERCEPTION_MODE:-default}"
	BENCH_DIR="${BENCH_REPORT_DIR:-$WORK/bench/$BENCH_LEG}"
	BENCH_RAW="$BENCH_DIR/raw"
	rm -rf "$BENCH_DIR"
	[[ "$-" != *x* ]] || fail "credential-bound bench refuses to run under xtrace"
	bench_validate_knobs
	load_mitm_ca_fingerprint
	CB_NET="$(kind_network)" || true
	[[ -n "$CB_NET" ]] || die "kind network not found (run up first)"
	local f
	for f in tls.crt echo.crt echo.key; do
		[[ -s "$MITM_CA_DIR/$f" ]] || die "missing $MITM_CA_DIR/$f (run up first)"
	done
	bench_load_template_id
	mkdir -p "$BENCH_RAW" "$GEN_DIR"
	BENCH_BIND_MS=()
	# Shared with the sourced credential-bound verify and bench cleanup.
	# shellcheck disable=SC2034
	CB_SANDBOXES=()
	BENCH_PREV_EXIT_TRAP="$(trap -p EXIT)"
	trap 'bench_cleanup' EXIT
	CB_WORK_DIR="$(mktemp -d "$GEN_DIR/credential-bound-bench.XXXXXX")"
	local since cred_b code_b
	since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	cred_b="$(cb_fake_value bench-b)"; code_b="$(cb_digest "$cred_b")"
	( umask 077; printf '%s\n' "$cred_b" > "$CB_WORK_DIR/values" )
	bench_environment
	log "bench: leg $BENCH_LEG, reports in $BENCH_DIR"

	cb_origin_up
	BENCH_POLICY="$CB_WORK_DIR/policy.json"
	jq -n --arg ip "$CB_ORIGIN_IP" --arg domain "$MITM_ECHO_DOMAIN" \
		'{defaultAction: "deny", egress: [{action: "allow", target: $ip}, {action: "allow", target: $domain}]}' > "$BENCH_POLICY"

	run_stage "bench M1: sandbox create latency ($BENCH_CREATE_ROUNDS x $BENCH_CREATE_N, concurrency $BENCH_CREATE_CONCURRENCY)" bench_m1
	run_stage "bench: colocated unbound/bound sandbox pair" bench_pair "$cred_b" "$code_b"
	run_stage "bench M2: guest TLS connect latency ($BENCH_HANDSHAKES per path)" bench_m2 "$code_b"
	cb_kill_sandbox "$BENCH_U"; cb_kill_sandbox "$BENCH_B"

	run_stage "bench: no fake credential value in the egress or env logs" cb_log_scan "$CB_WORK_DIR/values" "$since"
	cb_cleanup
	if [[ -n "$BENCH_PREV_EXIT_TRAP" ]]; then eval "$BENCH_PREV_EXIT_TRAP"; else trap - EXIT; fi
	local verdict
	verdict="$(bench_summary)" || fail "bench: summary assembly failed"
	log "bench: $verdict (reports in $BENCH_DIR)"
	case "$verdict" in
		"summary OK"|"summary PARTIAL") ;;
		*) fail "bench: at least one measurement is FAILED, INVALID or MISSING (see $BENCH_DIR/bench-summary.json)" ;;
	esac
}
