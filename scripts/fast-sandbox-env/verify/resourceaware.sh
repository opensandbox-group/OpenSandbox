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

# verify/resourceaware.sh — OSEP-0026 ResourceAware resource accounting
# (POOL_MODE=resource-aware stack only). Split-layout copy of the block in
# integration-env.sh; keep the two in sync until the env.sh split completes.

# --- OSEP-0026: ResourceAware resource accounting (POOL_MODE=resource-aware) ---------------
#
# Coverage:
#   1. distinctly-shaped template (2 vCPU / 1Gi) -> create forwards the recorded
#      machine as per-sandbox limits: Sandbox CR spec.resources.limits, cgroup
#      leaf cpu.max (from the limit) and cpu.weight (from the pool-derived
#      request ceil(2000m/4)=500m), aggregate memory budget (derived from
#      the rendered maxSandboxesPerPod).
#   2. oversized template (8 vCPU > 1250m per-fastlet budget) -> permanent 400.
#   3. capacity saturation (5 concurrent 2vCPU sandboxes vs 2x2 slots) -> 429.
#   4. template -> snapshot -> restore round-trip: recorded limits replayed
#      through the composite production path; restore CR carries the same
#      limits; guest answers /ping.
#   LegacySlot regression is covered by the default stages on the legacy stack
#   (byte-identical requests, no CR resources) — this stage requires the
#   resource-aware pool profile.

ra_expected_cpu_weight() { # <milli> — mirrors fastlet cpuWeight (shares [2,262144] -> weight [1,10000])
	local milli="$1" shares
	shares=$(( milli * 1024 / 1000 ))
	(( shares < 2 )) && shares=2
	(( shares > 262144 )) && shares=262144
	echo $(( 1 + (shares - 2) * 9999 / 262142 ))
}

# Fixture pool defaults: limits 1 CPU / 512Mi, overcommit ratios 4 / 2.
RA_DEFAULT_REQ_CPU_MILLI=$(( 1000 / 4 ))
RA_DEFAULT_REQ_MEM_BYTES=$(( 512 * 1024 * 1024 / 2 ))

ra_aggregate_memory_budget() { # max(defaultRequest * maxSandboxesPerPod, defaultLimit)
	local req=$(( RA_DEFAULT_REQ_MEM_BYTES * MAX_SANDBOXES_PER_POD ))
	local limit=$(( 512 * 1024 * 1024 ))
	(( req >= limit )) && { echo "$req"; return 0; }
	echo "$limit"
}

ra_cg_read() { # <fastlet-pod> <rel-path-under-pod-cgroup> <file>
	local pod="$1" rel="$2" file="$3" node uid out base
	node="$(kubectl -n "$RESOURCE_NS" get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null)" || return 1
	uid="$(kubectl -n "$RESOURCE_NS" get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)" || return 1
	# The egress sidecar's unequal requests/limits make every fastlet Pod
	# Burstable; try the Burstable and Guaranteed forms on both drivers.
	for base in \
		"kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod${uid//-/_}.slice" \
		"kubepods.slice/kubepods-pod${uid//-/_}.slice" \
		"kubepods/burstable/pod${uid}" \
		"kubepods/pod${uid}"; do
		out="$(docker exec "$node" cat "/sys/fs/cgroup/$base/$rel/$file" 2>/dev/null)" && { printf '%s' "$out"; return 0; }
	done
	return 1
}

ra_leaf_read() { # <sandbox-id> <cgroup-file> — from the fastlet hosting the sandbox
	local sid="$1" file="$2" pod out
	local leaf="fsb-$(printf '%s' "$sid" | openssl dgst -sha256 -hex | awk '{print substr($2,1,16)}')"
	for pod in $(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
		out="$(ra_cg_read "$pod" "fast-sandbox/$leaf" "$file")" && { printf '%s' "$out"; return 0; }
	done
	return 1
}

ra_aggregate_read() { # <cgroup-file> — from the first fastlet's VM pool aggregate
	local file="$1" pod out
	for pod in $(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
		out="$(ra_cg_read "$pod" "fast-sandbox" "$file")" && { printf '%s' "$out"; return 0; }
	done
	return 1
}

_ra_template_phase() { osb_api template get "$RA_TEMPLATE_ID" -o json 2>/dev/null | jq -r '.status.phase // empty'; }

ra_template_up() { # <cpu> <memory> -> RA_TEMPLATE_ID (cached per shape under $WORK)
	local cpu="$1" memory="$2" created phase attempts=0
	local cache="$WORK/ra-template-$cpu-$memory.id"
	if [[ -s "$cache" ]]; then
		RA_TEMPLATE_ID="$(<"$cache")"
		if [[ "$(_ra_template_phase)" == "Succeeded" ]]; then
			log "reusing Succeeded template ($cpu/$memory): $RA_TEMPLATE_ID"
			return 0
		fi
	fi
	created="$(osb_api template create \
		--image "$SBX_IMAGE" \
		--publish "s3://$RUSTFS_BUCKET/publish" \
		--format native \
		--resource "cpu=$cpu" --resource "memory=$memory" --resource "disk=2Gi" \
		--warmup-seconds 15 \
		--metadata "origin=osep-0026-resourceaware" \
		-o json 2>/dev/null)" \
		|| fail "template create ($cpu/$memory) failed: $(osb_api template get 2>&1 | head -c 200)"
	RA_TEMPLATE_ID="$(printf '%s' "$created" | jq -r '.template_id')"
	[[ -n "$RA_TEMPLATE_ID" && "$RA_TEMPLATE_ID" != "null" ]] || fail "template create ($cpu/$memory) response carried no templateId"
	printf '%s' "$RA_TEMPLATE_ID" > "$cache"
	while (( attempts++ < 300 )); do
		phase="$(_ra_template_phase)"
		[[ "$phase" == "Succeeded" ]] && { pass "SandboxTemplate build ($cpu/$memory): $RA_TEMPLATE_ID"; return 0; }
		[[ "$phase" == "Failed" ]] && fail "template $RA_TEMPLATE_ID build failed"
		sleep 2
	done
	fail "template $RA_TEMPLATE_ID did not reach Succeeded in 600s (last phase: ${phase:-unknown})"
}

ra_sandbox_create() { # -> id (from RA_TEMPLATE_ID)
	osb_api sandbox create --template "$RA_TEMPLATE_ID" --timeout 30m \
		--metadata origin=osep-0026 --skip-health-check -o json 2>/dev/null | jq -r '.id'
}

ra_kill_all() { # kill every osep-0026 sandbox still around
	local id
	for id in "${RA_VERIFY_IDS[@]:-}"; do
		[[ -n "$id" ]] && osb_api sandbox kill "$id" >/dev/null 2>&1
	done
	RA_VERIFY_IDS=()
}

ra_forward_verify() {
	local id cpu memory weight
	id="$(ra_sandbox_create)"
	[[ -n "$id" && "$id" != "null" ]] || fail "template-mode create (2 vCPU / 1Gi) failed"
	RA_VERIFY_IDS+=("$id")
	VERIFY_ID="$id"
	wait_for "resource-aware sandbox Running" 90 sandbox_running

	cpu="$(kubectl -n "$RESOURCE_NS" get sandbox "$id" -o jsonpath='{.spec.resources.limits.cpu}')"
	memory="$(kubectl -n "$RESOURCE_NS" get sandbox "$id" -o jsonpath='{.spec.resources.limits.memory}')"
	[[ "$cpu" == "2" && "$memory" == "1Gi" ]] \
		|| fail "Sandbox CR spec.resources.limits mismatch: cpu=$cpu memory=$memory (want 2 / 1Gi)"
	pass "Sandbox CR carries the forwarded limits (cpu=2, memory=1Gi)"

	weight="$(ra_expected_cpu_weight 500)" # request = ceil(2000m / ratio 4) = 500m
	[[ "$(ra_leaf_read "$id" cpu.max)" == "200000 100000" ]] \
		|| fail "cgroup leaf cpu.max mismatch: $(ra_leaf_read "$id" cpu.max 2>/dev/null) (want '200000 100000' for 2 cores)"
	[[ "$(ra_leaf_read "$id" cpu.weight)" == "$weight" ]] \
		|| fail "cgroup leaf cpu.weight mismatch: $(ra_leaf_read "$id" cpu.weight 2>/dev/null) (want $weight for the 500m derived request)"
	[[ "$(ra_leaf_read "$id" pids.max)" == "128" ]] \
		|| fail "cgroup leaf pids.max mismatch (want pool pids 128)"
	local want_mem_budget
	want_mem_budget="$(ra_aggregate_memory_budget)"
	[[ "$(ra_aggregate_read memory.max)" == "$want_mem_budget" ]] \
		|| fail "aggregate memory.max mismatch: $(ra_aggregate_read memory.max 2>/dev/null) (want $want_mem_budget = max(256Mi*$MAX_SANDBOXES_PER_POD, 512Mi))"
	pass "cgroup enforcement matches: leaf cpu.max/weight/pids from limit+derived request, aggregate memory budget"

	wait_for "resource-aware sandbox execd /ping" 60 execd_ping_ok
	ra_kill_all
	wait_for "resource-aware sandbox gone" 60 verify_sandbox_gone
}

ra_oversized_verify() {
	local code
	ra_template_up 8 1Gi # > 1250m per-fastlet sandbox budget
	code="$(server_api_status POST "/sandboxes" "{\"templateId\": \"$RA_TEMPLATE_ID\", \"timeout\": 600}")"
	[[ "$code" == "400" ]] \
		|| fail "oversized template (8 vCPU) create returned HTTP $code (want permanent 400)"
	pass "oversized template create -> permanent 400"
}

ra_capacity_verify() {
	local -a ids=()
	local i id code total
	# Committed per sandbox: cpu ceil(2000m/4)=500m, memory ceil(1Gi/2)=512Mi.
	# Per-fastlet slots derive from the RENDERED density (budget =
	# max(defaultReq * maxSandboxesPerPod, defaultLimit)), not the manifest
	# literal; total = slots * POOL_MIN fastlets, then one more must 429.
	local budget_cpu=$(( RA_DEFAULT_REQ_CPU_MILLI * MAX_SANDBOXES_PER_POD ))
	(( budget_cpu < 1000 )) && budget_cpu=1000
	local budget_mem
	budget_mem="$(ra_aggregate_memory_budget)"
	local per=$(( budget_cpu / 500 ))
	local per_mem=$(( budget_mem / (512 * 1024 * 1024) ))
	(( per_mem < per )) && per=$per_mem
	(( per > MAX_SANDBOXES_PER_POD )) && per=$MAX_SANDBOXES_PER_POD
	total=$(( per * POOL_MIN ))
	log "capacity: $per slots/fastlet (budget ${budget_cpu}m/$(printf '%d' "$budget_mem")B) x $POOL_MIN fastlets -> saturating at $total"
	ra_template_up 2 1Gi
	for i in $(seq 1 "$total"); do
		id="$(ra_sandbox_create)"
		[[ -n "$id" && "$id" != "null" ]] || fail "capacity pre-fill create #$i failed"
		RA_VERIFY_IDS+=("$id"); ids+=("$id")
		VERIFY_ID="$id"
		wait_for "capacity pre-fill sandbox #$i Running" 90 sandbox_running
	done
	code="$(server_api_status POST "/sandboxes" "{\"templateId\": \"$RA_TEMPLATE_ID\", \"timeout\": 600}")"
	ra_kill_all
	for id in "${ids[@]}"; do VERIFY_ID="$id"; wait_for "saturated sandbox $id gone" 60 verify_sandbox_gone; done
	[[ "$code" == "429" ]] \
		|| fail "capacity-exhausted create returned HTTP $code (want retryable 429)"
	pass "capacity-exhausted create -> retryable 429"
}

ra_snapshot_verify() {
	local source_id restore_id snap_id state attempts=0
	ra_template_up 2 1Gi
	source_id="$(ra_sandbox_create)"
	[[ -n "$source_id" && "$source_id" != "null" ]] || fail "snapshot source create failed"
	RA_VERIFY_IDS+=("$source_id")
	VERIFY_ID="$source_id"
	wait_for "snapshot source Running" 90 sandbox_running

	snap_id="$(osb_api snapshot create "$source_id" --name osep-0026-ra -o json 2>/dev/null | jq -r '.snapshot_id // .id')"
	[[ -n "$snap_id" && "$snap_id" != "null" ]] || fail "snapshot create failed"
	while (( attempts++ < 150 )); do
		state="$(osb_api snapshot get "$snap_id" -o json 2>/dev/null | jq -r '.status.state // empty')"
		[[ "$state" == "Ready" ]] && break
		[[ "$state" == "Failed" ]] && fail "snapshot $snap_id failed"
		sleep 2
	done
	[[ "$state" == "Ready" ]] || fail "snapshot $snap_id did not reach Ready in 300s (last: ${state:-unknown})"

	restore_id="$(osb_api sandbox create --snapshot-id "$snap_id" --timeout 30m \
		--resource cpu=2 --resource memory=1Gi \
		--skip-health-check -o json 2>/dev/null | jq -r '.id')"
	[[ -n "$restore_id" && "$restore_id" != "null" ]] || fail "snapshot restore create failed"
	RA_VERIFY_IDS+=("$restore_id")
	VERIFY_ID="$restore_id"
	wait_for "restored sandbox Running" 120 sandbox_running
	[[ "$(kubectl -n "$RESOURCE_NS" get sandbox "$restore_id" -o jsonpath='{.spec.resources.limits.cpu}')" == "2" \
		&& "$(kubectl -n "$RESOURCE_NS" get sandbox "$restore_id" -o jsonpath='{.spec.resources.limits.memory}')" == "1Gi" ]] \
		|| fail "restore did not replay the recorded limits (want cpu=2 memory=1Gi)"
	wait_for "restored sandbox execd /ping" 60 execd_ping_ok
	osb_api snapshot delete "$snap_id" >/dev/null 2>&1 || true
	ra_kill_all
	for id in "$source_id" "$restore_id"; do VERIFY_ID="$id"; wait_for "snapshot chain sandbox $id gone" 60 verify_sandbox_gone; done
	pass "snapshot round-trip replays recorded limits through the composite path"
}

verify_resourceaware() {
	require_live_stack
	[[ "$POOL_MODE" == "resource-aware" ]] \
		|| die "verify-resourceaware requires POOL_MODE=resource-aware (stack was brought up with: $POOL_MODE)"
	run_stage "SandboxTemplate build (2 vCPU / 1Gi, distinctly shaped)" ra_template_up 2 1Gi
	run_stage "per-sandbox limit forwarding + cgroup enforcement" ra_forward_verify
	run_stage "oversized template -> 400" ra_oversized_verify
	run_stage "template -> snapshot -> restore round-trip" ra_snapshot_verify
	run_stage "capacity saturation -> 429" ra_capacity_verify
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib.sh"
	resolve_osb
	require_live_stack
	[[ "$POOL_MODE" == "resource-aware" ]] \
		|| die "verify/resourceaware.sh requires POOL_MODE=resource-aware"
	trap 'on_error verify-resourceaware' ERR
	run_stage "OSEP-0026 ResourceAware verify" verify_resourceaware
fi
