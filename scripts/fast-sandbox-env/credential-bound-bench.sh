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

# credential-bound-bench.sh — OSEP-0023 credential-bound cost measurements.
#
# Sourced by `integration-env.sh bench-credential-bound` (after up and the
# verify actions) in either leg; EGRESS_INTERCEPTION_MODE selects the leg.
# Every measurement writes one JSON report under $BENCH_DIR, assembled by
# bench-report.py from raw readings kept under $BENCH_DIR/raw:
#
#   M1  create latency: create-bench.py, BENCH_CREATE_ROUNDS rounds of
#       BENCH_CREATE_N creates at BENCH_CREATE_CONCURRENCY, each create
#       carrying a network policy (so every sandbox registers an egress
#       subject). No Vault is written: create-bench has no Vault step, so the
#       bound-create cost is covered by the Vault ACK times recorded in M2/M3
#       and by the M2/M3 measurements themselves.
#   M2  TCP+TLS connect time from inside the guest to the credential-bound
#       TLS origin fixture (verify/credential-bound-origin.py), a new
#       connection per sample (client handshake-bench mode), on an unbound
#       sandbox U and a bound sandbox B on the same Fastlet Pod. Default/all
#       leg: all_unbound (full decrypt) and all_bound (decrypt + inject);
#       credential-bound leg: cb_bound (decrypt + inject) and cb_unbound
#       (passthrough). The other leg's paths are NOT_RUN. Every sample's
#       presented leaf and origin-observed credential are classified, so a
#       path that was not what it claims is INVALID, never a number.
#   M3  credential-bound leg only: idle bound HTTP/1.1 keep-alive
#       connections held from BENCH_MEM_SANDBOXES sandboxes on one Fastlet
#       Pod, cumulative K in BENCH_MEM_STEPS, egress container cgroup memory
#       (memory.current, memory.stat anon) read after each step; bytes per
#       connection fitted by least squares. Runs on a bench-only Pool render
#       (egress memory limit BENCH_EGRESS_MEMORY_LIMIT, per-subject budget
#       BENCH_FSB_TLS_CAPACITY); the default render and the verify lanes are
#       untouched, and the default Pool is re-applied afterwards. A step that
#       cannot be reached is recorded with the largest K and the closed
#       failure kinds, never dropped.
#   M4  egress container cpu.stat usage_usec delta under passthrough-
#       dominated load: BENCH_CPU_UNBOUND short HTTPS requests from U plus
#       BENCH_CPU_BOUND from B (new connection, one request, close),
#       idle-corrected with an idle window of BENCH_CPU_IDLE_SECONDS.
#
# Reports carry closed-vocabulary fields only: no credential value, digest,
# sandbox hostname or subject UID. The fake credentials never reach a log
# (xtrace is refused) and the egress/env logs are scanned for them before
# the bench ends, as in the verify stage (AC9).

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
BENCH_MEM_STEPS="${BENCH_MEM_STEPS:-500 1000 2000}"
BENCH_MEM_SANDBOXES="${BENCH_MEM_SANDBOXES:-4}"
BENCH_MEM_SETTLE_SECONDS="${BENCH_MEM_SETTLE_SECONDS:-20}"
BENCH_EGRESS_MEMORY_LIMIT="${BENCH_EGRESS_MEMORY_LIMIT:-2Gi}"
BENCH_FSB_TLS_CAPACITY="${BENCH_FSB_TLS_CAPACITY:-1024}"
BENCH_CPU_UNBOUND="${BENCH_CPU_UNBOUND:-1900}"
BENCH_CPU_BOUND="${BENCH_CPU_BOUND:-100}"
BENCH_CPU_IDLE_SECONDS="${BENCH_CPU_IDLE_SECONDS:-30}"

BENCH_LEG=""
BENCH_DIR=""
BENCH_PREV_EXIT_TRAP=""
BENCH_RAW=""
BENCH_POLICY=""
BENCH_POOL_RENDERED=0
BENCH_POD=""
BENCH_POD_UID=""

# --- small helpers

bench_validate_knobs() {
	local name value
	for name in BENCH_CREATE_N BENCH_CREATE_CONCURRENCY BENCH_CREATE_ROUNDS BENCH_HANDSHAKES \
		BENCH_HANDSHAKE_WARMUP BENCH_MEM_SANDBOXES BENCH_MEM_SETTLE_SECONDS BENCH_FSB_TLS_CAPACITY \
		BENCH_CPU_UNBOUND BENCH_CPU_BOUND BENCH_CPU_IDLE_SECONDS; do
		value="${!name}"
		[[ "$value" =~ ^[0-9]+$ ]] || die "$name must be a non-negative integer (got: $value)"
	done
	local prev=0
	for value in $BENCH_MEM_STEPS; do
		if ! [[ "$value" =~ ^[1-9][0-9]*$ ]] || (( value <= prev )); then
			die "BENCH_MEM_STEPS must list increasing positive integers (got: $BENCH_MEM_STEPS)"
		fi
		prev="$value"
	done
	[[ -n "${BENCH_MEM_STEPS// /}" ]] || die "BENCH_MEM_STEPS must not be empty"
	for name in BENCH_HANDSHAKES BENCH_CPU_UNBOUND BENCH_CPU_BOUND; do
		(( ${!name} <= 5000 )) || die "$name must be at most 5000 (guest client limit)"
	done
	(( BENCH_HANDSHAKES >= 1 && BENCH_CREATE_ROUNDS >= 1 && BENCH_CREATE_N >= 1 )) \
		|| die "BENCH_HANDSHAKES, BENCH_CREATE_ROUNDS and BENCH_CREATE_N must be at least 1"
	[[ "$BENCH_EGRESS_MEMORY_LIMIT" =~ ^[1-9][0-9]*(Mi|Gi)$ ]] \
		|| die "BENCH_EGRESS_MEMORY_LIMIT must look like 2Gi or 1536Mi (got: $BENCH_EGRESS_MEMORY_LIMIT)"
	(( BENCH_MEM_SANDBOXES >= 1 && BENCH_MEM_SANDBOXES <= MAX_SANDBOXES_PER_POD )) \
		|| die "BENCH_MEM_SANDBOXES must be 1..MAX_SANDBOXES_PER_POD ($MAX_SANDBOXES_PER_POD)"
	# The per-guest share of the largest step must fit the per-subject budget.
	(( BENCH_FSB_TLS_CAPACITY * BENCH_MEM_SANDBOXES >= prev + BENCH_MEM_SANDBOXES )) \
		|| die "BENCH_FSB_TLS_CAPACITY x BENCH_MEM_SANDBOXES ($BENCH_FSB_TLS_CAPACITY x $BENCH_MEM_SANDBOXES) cannot hold the largest BENCH_MEM_STEPS entry ($prev)"
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

bench_report() { # <m1|m2|m3|m4> <spec-file> <report-file>
	python3 "$BENCH_REPORT_PY" "$1" "$2" "$3" || fail "bench: report assembly for $1 failed"
}

# Egress container cgroup v2 counters of one Pod as one JSON object
# (memory.current, memory.stat anon/file/kernel/sock, memory.events
# oom_kill, cpu.stat usage/throttling); the container's own cgroup is
# resolved from /proc/self/cgroup and falls back to the namespace root.
bench_egress_cgroup() { # <pod>
	kubectl -n "$RESOURCE_NS" exec "$1" -c egress -- sh -c '
		p=$(sed -n "s/^0:://p" /proc/self/cgroup 2>/dev/null)
		d="/sys/fs/cgroup${p%/}"
		[ -r "$d/memory.current" ] || d=/sys/fs/cgroup
		printf "memory_current %s\n" "$(cat "$d/memory.current")"
		sed -n "s/^\(anon\|file\|kernel\|sock\) \([0-9]*\)$/\1 \2/p" "$d/memory.stat"
		sed -n "s/^oom_kill \([0-9]*\)$/oom_kill \1/p" "$d/memory.events"
		sed -n "s/^\(usage_usec\|nr_throttled\|throttled_usec\) \([0-9]*\)$/\1 \2/p" "$d/cpu.stat"' 2>/dev/null \
		| awk 'NF == 2 && $2 ~ /^[0-9]+$/ { printf "%s\"%s\":%s", (n++ ? "," : "{"), $1, $2 } END { if (n) print "}"; else exit 1 }'
}

bench_egress_restarts() { # <pod>
	kubectl -n "$RESOURCE_NS" get pod "$1" \
		-o jsonpath='{.status.containerStatuses[?(@.name=="egress")].restartCount}' 2>/dev/null
}

# "<container-id> <restart-count>" of one Pod's egress container. The id
# stays in the shell: reports carry only whether it changed.
bench_egress_identity() { # <pod>
	kubectl -n "$RESOURCE_NS" get pod "$1" \
		-o jsonpath='{range .status.containerStatuses[?(@.name=="egress")]}{.containerID} {.restartCount}{end}' 2>/dev/null
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

# --- bench-only Pool render (M3): the default render plus two edits inside
# the egress container — the per-subject budget env and the memory limit.

# render_pool keys warmImages on TEMPLATE_ID. bench_pool_apply deletes the
# live Pool before applying a render, so an empty id must stop the bench
# before either render instead of producing `warmImages: [null]`.
bench_require_template_id() {
	[[ "$WARM_IMAGES" != 1 || -n "$TEMPLATE_ID" ]] \
		|| die "bench: WARM_IMAGES=1 needs TEMPLATE_ID for the Pool render (run up first)"
}

# The action runs in its own process: load the template id that `up` wrote,
# for the bench sandboxes (CB_TEMPLATE_ID) and the Pool renders (TEMPLATE_ID).
bench_load_template_id() {
	CB_TEMPLATE_ID="$(cat "$WORK/template-id" 2>/dev/null || true)"
	[[ -n "$CB_TEMPLATE_ID" ]] || die "no template id at $WORK/template-id (run up first)"
	TEMPLATE_ID="$CB_TEMPLATE_ID"
}

bench_render_pool() { # -> $GEN_DIR/$POOL_PROFILE.bench.yaml
	local out="$GEN_DIR/$POOL_PROFILE.bench.yaml"
	bench_require_template_id
	render_pool
	awk -v mem="$BENCH_EGRESS_MEMORY_LIMIT" -v cap="$BENCH_FSB_TLS_CAPACITY" '
		/^      - name: / { egress = ($0 == "      - name: egress"); limits = 0 }
		egress && /^        volumeMounts:$/ && !env_done {
			printf "        - name: OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY\n          value: \"%s\"\n", cap
			env_done = 1 }
		egress && /^          limits:$/ { limits = 1; print; next }
		limits && /^            memory: / { print "            memory: " mem; limits = 0; mem_done = 1; next }
		limits && !/^            / { limits = 0 }
		{ print }
		END { if (!env_done || !mem_done) exit 3 }
	' "$GEN_DIR/$POOL_PROFILE.yaml" > "$out" || die "bench: egress container not found in the rendered Pool"
}

bench_pool_pods_gone() {
	[[ -z "$(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" ]]
}

# Every live pool Pod's egress container carries <memory-limit> and, when
# <capacity> is non-empty, exactly that per-subject budget (empty = unset).
bench_live_egress_is() { # <memory-limit> <capacity|"">
	local pods
	pods="$(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o json 2>/dev/null)" || return 1
	printf '%s' "$pods" | jq -e --arg mem "$1" --arg cap "$2" '(.items | length) > 0 and all(.items[];
		(.spec.containers[] | select(.name == "egress")) as $e
		| $e.resources.limits.memory == $mem
		and ([$e.env[]? | select(.name == "OPENSANDBOX_EGRESS_FSB_TLS_CAPACITY") | .value] == (if $cap == "" then [] else [$cap] end)))' >/dev/null
}

# The SandboxPool object is absent from its namespace. An API error is not
# "absent": --ignore-not-found prints nothing only for NotFound.
bench_pool_object_gone() {
	local out
	out="$(kubectl -n "$RESOURCE_NS" get sandboxpool "$POOL_NAME" --ignore-not-found -o name 2>/dev/null)" || return 1
	[[ -z "$out" ]]
}

# Recreate the pool from <manifest> (a template change must not wait on the
# controller to roll Pods) and wait for the same readiness as pool_up.
# SandboxPool is namespaced: deleting it outside $RESOURCE_NS is a NotFound
# no-op, so the object's absence is checked before waiting on its Pods.
bench_pool_apply() { # <manifest> <memory-limit> <capacity|"">
	kubectl -n "$RESOURCE_NS" delete sandboxpool "$POOL_NAME" --ignore-not-found --wait --timeout=300s >/dev/null 2>&1 \
		|| fail "bench: deleting SandboxPool $POOL_NAME failed"
	wait_for "bench: SandboxPool $POOL_NAME deleted" 30 bench_pool_object_gone
	wait_for "bench: previous pool Pods gone" 150 bench_pool_pods_gone
	kubectl apply -f "$1" >/dev/null || fail "bench: applying $1 failed"
	wait_for "bench: fastlet pods Ready (poolMin=$POOL_MIN)" 300 fastlet_pods_ready
	wait_for "bench: egress container ready in every fastlet pod" 180 egress_containers_ready
	wait_for "bench: pool condition RuntimeReady=True" 120 pool_condition_true RuntimeReady
	wait_for "bench: pool condition InfraReady=True" 120 pool_condition_true InfraReady
	wait_for "bench: egress actions endpoint answering" 60 egress_status_ready
	wait_for "bench: egress MITM addon up with the pinned CA fingerprint" 120 egress_mitm_ca_pinned
	wait_for "bench: live egress memory limit $2${3:+, per-subject budget $3}" 30 bench_live_egress_is "$2" "$3"
}

bench_pool_restore() {
	[[ "$BENCH_POOL_RENDERED" == 1 ]] || return 0
	local mem
	bench_require_template_id
	render_pool
	mem="$(awk '/^      - name: / { e = ($0 == "      - name: egress") } e && /^          limits:$/ { l = 1; next } l && /^            memory: / { print $2; exit }' "$GEN_DIR/$POOL_PROFILE.yaml")"
	bench_pool_apply "$GEN_DIR/$POOL_PROFILE.yaml" "$mem" ""
	BENCH_POOL_RENDERED=0
	pass "bench: default SandboxPool $POOL_NAME re-applied"
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
		  rounds: [], notes: ["no Vault is written during M1; bound-create cost is covered by vault_bind_ack_ms in M2/M3"]}' > "$spec"
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

# --- M2 handshake latency + M4 CPU on a colocated unbound/bound pair

bench_pair() {
	local cred_b="$1" code_b="$2" u b unbound_leaf
	cb_create_sandbox bench-u "$BENCH_POLICY"; u="$CB_LAST_ID"
	cb_placement "$u" || fail "bench: U's placement (fastletPodUID) is not a live pool Pod"
	BENCH_POD="$CB_POD_NAME"; BENCH_POD_UID="$CB_POD_UID"
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

bench_m4() {
	local code_b="$1" spec="$BENCH_RAW/m4-spec.json" idle0 idle1 load0 load1 t0 idle_ms load_ms leaf ident0 ident1
	leaf=mitm
	[[ "$BENCH_LEG" != credential-bound ]] || leaf=origin
	# cgroup counters reset with the container: both windows count only when
	# one egress container instance spans them.
	ident0="$(bench_egress_identity "$BENCH_POD")" || ident0=""
	idle0="$(bench_egress_cgroup "$BENCH_POD")" || idle0=""
	t0="$(bench_ns)"
	sleep "$BENCH_CPU_IDLE_SECONDS"
	idle1="$(bench_egress_cgroup "$BENCH_POD")" || idle1=""
	idle_ms="$(bench_elapsed_ms "$t0")"
	load0="$(bench_egress_cgroup "$BENCH_POD")" || load0=""
	t0="$(bench_ns)"
	(( BENCH_CPU_UNBOUND == 0 )) || bench_handshakes_start "$BENCH_U" m4-unbound "$BENCH_CPU_UNBOUND"
	(( BENCH_CPU_BOUND == 0 )) || bench_handshakes_start "$BENCH_B" m4-bound "$BENCH_CPU_BOUND"
	(( BENCH_CPU_UNBOUND == 0 )) || bench_handshakes_collect "$BENCH_U" m4-unbound "$BENCH_CPU_UNBOUND" "$BENCH_RAW/m4-unbound.json"
	(( BENCH_CPU_BOUND == 0 )) || bench_handshakes_collect "$BENCH_B" m4-bound "$BENCH_CPU_BOUND" "$BENCH_RAW/m4-bound.json"
	load1="$(bench_egress_cgroup "$BENCH_POD")" || load1=""
	load_ms="$(bench_elapsed_ms "$t0")"
	ident1="$(bench_egress_identity "$BENCH_POD")" || ident1=""
	jq -n --arg leg "$BENCH_LEG" --arg leaf "$CB_ORIGIN_LEAF" \
		--arg c0 "${ident0%% *}" --arg c1 "${ident1%% *}" --arg r0 "${ident0##* }" --arg r1 "${ident1##* }" \
		--argjson i0 "${idle0:-null}" --argjson i1 "${idle1:-null}" --argjson im "$idle_ms" \
		--argjson l0 "${load0:-null}" --argjson l1 "${load1:-null}" --argjson lm "$load_ms" \
		--argjson nu "$BENCH_CPU_UNBOUND" --argjson nb "$BENCH_CPU_BOUND" --argjson is "$BENCH_CPU_IDLE_SECONDS" \
		--arg ul "$leaf" --arg code "$code_b" --arg ru "$BENCH_RAW/m4-unbound.json" --arg rb "$BENCH_RAW/m4-bound.json" \
		'{leg: $leg, origin_leaf: $leaf,
		  settings: {unbound_requests: $nu, bound_requests: $nb, idle_window_s: $is,
		    request: "new TCP+TLS connection, one HTTP/1.1 request, close; U and B run concurrently on one Fastlet Pod"},
		  idle: {start: $i0, end: $i1, wall_ms: $im}, load: {start: $l0, end: $l1, wall_ms: $lm},
		  egress_continuity: {restart_count_start: ($r0 | tonumber? // null), restart_count_end: ($r1 | tonumber? // null),
		    same_container: ($c0 != "" and $c0 == $c1)},
		  runs: ([if $nu > 0 then {name: "unbound", description: "unbound sandbox U", requests: $nu, expect_leaf: $ul, expect_cred: "absent", raw: $ru} else empty end]
		    + [if $nb > 0 then {name: "bound", description: "bound sandbox B", requests: $nb, expect_leaf: "mitm", expect_cred: $code, raw: $rb} else empty end])}' > "$spec"
	bench_report m4 "$spec" "$BENCH_DIR/m4-cpu.json"
}

# --- M3 memory per held bound connection (credential-bound leg only)

bench_m3() {
	local spec="$BENCH_RAW/m3-spec.json" i id cred code k inc per rem n held_total=0 actual restarts0 restarts reading
	local -a ids=() codes=() dirs=()
	# shellcheck disable=SC2086 # BENCH_MEM_STEPS is a word list on purpose
	jq -n --arg leg "$BENCH_LEG" --arg leaf "$CB_ORIGIN_LEAF" --argjson s "$BENCH_MEM_SANDBOXES" \
		--argjson steps "$(printf '%s\n' $BENCH_MEM_STEPS | jq -s -c 'map(tonumber)')" \
		--arg mem "$BENCH_EGRESS_MEMORY_LIMIT" --argjson cap "$BENCH_FSB_TLS_CAPACITY" --argjson settle "$BENCH_MEM_SETTLE_SECONDS" \
		'{leg: $leg, origin_leaf: $leaf, settings: {sandboxes: $s, steps: $steps, egress_memory_limit: $mem,
		  fsb_tls_capacity_per_subject: $cap, fsb_process_tls_capacity: "default (32768)", settle_s: $settle,
		  connection: "HTTP/1.1 keep-alive over TLS to a bound host, one request then idle"}, points: [], stopped: null,
		  notes: ["bench-only Pool render; the default render and verify lanes are unchanged"]}' > "$spec"
	cb_kill_sandbox "$BENCH_U"; cb_kill_sandbox "$BENCH_B"
	bench_render_pool
	BENCH_POOL_RENDERED=1
	bench_pool_apply "$GEN_DIR/$POOL_PROFILE.bench.yaml" "$BENCH_EGRESS_MEMORY_LIMIT" "$BENCH_FSB_TLS_CAPACITY"
	for i in $(seq 1 "$BENCH_MEM_SANDBOXES"); do
		if [[ "$i" == 1 ]]; then
			cb_create_sandbox "bench-m$i" "$BENCH_POLICY"; id="$CB_LAST_ID"
			cb_placement "$id" || fail "bench: M3 anchor placement is not a live pool Pod"
			BENCH_POD="$CB_POD_NAME"; BENCH_POD_UID="$CB_POD_UID"
		else
			cb_create_colocated "bench-m$i" "$BENCH_POLICY" "$BENCH_POD_UID"; id="$CB_LAST_ID"
		fi
		cred="$(cb_fake_value "m$i")"; code="$(cb_digest "$cred")"
		printf '%s\n' "$cred" >> "$CB_WORK_DIR/values"
		bench_bind "$id" "$cred" "bench vault for M3 sandbox $i"
		cb_until "bench: M3 sandbox $i injected over a MITM leaf" 60 "$id" "bench-m$i-ready" "$code"
		ids+=("$id"); codes+=("$code")
	done
	sleep "$BENCH_MEM_SETTLE_SECONDS"
	reading="$(bench_egress_cgroup "$BENCH_POD")" || fail "bench: egress cgroup memory counters not readable on the M3 Pod"
	restarts0="$(bench_egress_restarts "$BENCH_POD")" || restarts0=""
	bench_json_set "$spec" '.baseline = $r | .baseline_restart_count = ($c | tonumber? // null)' \
		--argjson r "$reading" --arg c "$restarts0"
	for k in $BENCH_MEM_STEPS; do
		inc=$(( k - held_total ))
		(( inc > 0 )) || continue
		per=$(( inc / BENCH_MEM_SANDBOXES )); rem=$(( inc % BENCH_MEM_SANDBOXES ))
		local -a step_targets=()
		for i in "${!ids[@]}"; do
			n=$(( per + (i < rem ? 1 : 0) ))
			step_targets+=("$n")
			(( n > 0 )) || continue
			bench_guest_start "${ids[$i]}" hold-many --tag "bench-hold-$k" --conns "$n" \
				--dir "$CB_GUEST_DIR/hold-$k" --wait-timeout 3600 \
				|| fail "bench: hold-many for K=$k failed to start in M3 sandbox $((i + 1))"
			dirs+=("${ids[$i]}|$CB_GUEST_DIR/hold-$k")
		done
		local holders='[]' got=0
		actual=0
		for i in "${!ids[@]}"; do
			n="${step_targets[$i]}"
			(( n > 0 )) || continue
			if bench_wait $(( 60 + n / 2 )) bench_guest_fetch "${ids[$i]}" "$CB_GUEST_DIR/hold-$k/status.json" "$BENCH_RAW/m3-$k-$i.json"; then
				got="$(jq -r '.held' "$BENCH_RAW/m3-$k-$i.json")"
			else
				got=0
			fi
			actual=$(( actual + got ))
			holders="$(jq -c --argjson t "$n" --arg c "${codes[$i]}" --arg p "$BENCH_RAW/m3-$k-$i.json" \
				'. + [{target: $t, expect_cred: $c, raw: $p}]' <<<"$holders")"
		done
		held_total=$(( held_total + actual ))
		sleep "$BENCH_MEM_SETTLE_SECONDS"
		if ! reading="$(bench_egress_cgroup "$BENCH_POD")"; then
			# An unread point must never reach the fit as a measured one.
			bench_json_set "$spec" '.stopped = {at_k: $k, reason: "egress cgroup read failed"}' --argjson k "$k"
			break
		fi
		restarts="$(bench_egress_restarts "$BENCH_POD")" || restarts=""
		bench_json_set "$spec" '.points += [{k_target: $k, k_held: $h, reading: $r, restart_count: ($c | tonumber? // null), settle_s: $s, holders: $hs}]' \
			--argjson k "$k" --argjson h "$held_total" --argjson r "$reading" --arg c "$restarts" \
			--argjson s "$BENCH_MEM_SETTLE_SECONDS" --argjson hs "$holders"
		log "bench: M3 K=$k target, $held_total held cumulatively"
		if [[ "$restarts" != "$restarts0" ]] || ! jq -e --argjson b "$(jq -c '.baseline' "$spec")" \
			'(.oom_kill // 0) == ($b.oom_kill // 0)' <<<"$reading" >/dev/null 2>&1; then
			bench_json_set "$spec" '.stopped = {at_k: $k, reason: "egress container restarted or recorded an OOM kill"}' --argjson k "$k"
			break
		fi
		if (( held_total < k )); then
			bench_json_set "$spec" '.stopped = {at_k: $k, reason: "fewer connections opened than targeted; see holders[].errors for the closed failure kinds"}' --argjson k "$k"
			break
		fi
	done
	# Release: one more request per held socket, then close (alive counts).
	local entry sid dir alive=0 release_errors='{}' out
	for entry in ${dirs[@]+"${dirs[@]}"}; do
		IFS='|' read -r sid dir <<<"$entry"
		osb_api command run "$sid" -- touch "$dir/go" >/dev/null 2>>"$LOGS_DIR/bench-guest.err" || true
	done
	for entry in ${dirs[@]+"${dirs[@]}"}; do
		IFS='|' read -r sid dir <<<"$entry"
		out="$BENCH_RAW/m3-release.tmp.json"
		if bench_wait 90 bench_guest_fetch "$sid" "$dir/done.json" "$out"; then
			alive=$(( alive + $(jq -r '.alive_at_release' "$out") ))
			release_errors="$(jq -c --argjson e "$(jq -c '.release_errors' "$out")" \
				'reduce ($e | to_entries[]) as $x (.; .[$x.key] = ((.[$x.key] // 0) + $x.value))' <<<"$release_errors")"
		else
			release_errors="$(jq -c '.no_done_file = ((.no_done_file // 0) + 1)' <<<"$release_errors")"
		fi
	done
	rm -f "$BENCH_RAW/m3-release.tmp.json"
	bench_json_set "$spec" '.release = {held: $h, alive_at_release: $a, errors: $e} | .vault_bind_ack_ms = $b' \
		--argjson h "$held_total" --argjson a "$alive" --argjson e "$release_errors" --argjson b "$(bench_bind_ms_json)"
	bench_report m3 "$spec" "$BENCH_DIR/m3-memory.json"
	for id in "${ids[@]}"; do cb_kill_sandbox "$id"; done
}

bench_m3_not_run() {
	jq -n --arg leg "$BENCH_LEG" \
		'{leg: $leg, not_run: "per-subject decision-registry memory exists only in the credential-bound leg"}' > "$BENCH_RAW/m3-spec.json"
	bench_report m3 "$BENCH_RAW/m3-spec.json" "$BENCH_DIR/m3-memory.json"
}

bench_summary() {
	[[ -n "$BENCH_DIR" && -d "$BENCH_DIR" ]] || return 0
	python3 "$BENCH_REPORT_PY" summary "$BENCH_DIR/environment.json" "$BENCH_DIR/bench-summary.json" \
		"$BENCH_DIR/m1-create.json" "$BENCH_DIR/m2-handshake.json" "$BENCH_DIR/m3-memory.json" "$BENCH_DIR/m4-cpu.json"
}

bench_cleanup() {
	cb_cleanup
	bench_summary >/dev/null 2>&1 || true
	if [[ "$BENCH_POOL_RENDERED" == 1 ]]; then
		log "bench: SandboxPool $POOL_NAME is still in the bench render; re-applying the default Pool"
		bench_pool_restore || true
	fi
}

# --- the action

credential_bound_bench() {
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
	BENCH_LEG="${EGRESS_INTERCEPTION_MODE:-default}"
	BENCH_DIR="${BENCH_REPORT_DIR:-$WORK/bench/$BENCH_LEG}"
	BENCH_RAW="$BENCH_DIR/raw"
	rm -rf "$BENCH_DIR"
	mkdir -p "$BENCH_RAW" "$GEN_DIR"
	BENCH_BIND_MS=()
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
	run_stage "bench M4: egress CPU under passthrough-dominated load" bench_m4 "$code_b"
	if [[ "$BENCH_LEG" == credential-bound ]]; then
		# M3 recreates the pool; scan the current Pods' egress logs before they go.
		run_stage "bench: no fake credential value in the logs before the M3 pool recreate" cb_log_scan "$CB_WORK_DIR/values" "$since"
		run_stage "bench M3: egress memory per held bound connection (K: $BENCH_MEM_STEPS)" bench_m3
	else
		cb_kill_sandbox "$BENCH_U"; cb_kill_sandbox "$BENCH_B"
		bench_m3_not_run
		stage_unverified NOT_RUN "bench M3 (memory per bound connection) runs in the credential-bound leg only"
	fi

	run_stage "bench: no fake credential value in the egress or env logs" cb_log_scan "$CB_WORK_DIR/values" "$since"
	bench_pool_restore
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
