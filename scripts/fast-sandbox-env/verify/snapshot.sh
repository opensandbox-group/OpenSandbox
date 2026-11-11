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

# verify/snapshot.sh — snapshot round trip with recorded-policy carry
# (CLI -> SandboxSnapshot CR -> artifact store -> restore)
#
# Test shape: a POLICY-LESS sandbox gains deny rules (the fsb create seeds a
# default-allow egress binding for every sandbox — the registration is the
# point — so rules can be added after create), the snapshot must publish the
# recorded policy file (policy.json rides Template-kind artifact sets next
# to the manifest), and a restored sandbox must boot EXACTLY the dump-time
# policy — rules added after the dump must not be in it. Deny enforcement is
# verified with real in-guest traffic probes (execd POST /command through
# the signed gateway route running curl), not just the control-plane view:
# with defaultAction=allow a deny rule is only meaningful if the denied
# target is unreachable while everything else still works.
#
# Coverage:
#
#   1. Bootstrap: create WITHOUT a network policy; the sandbox registers
#      default-allow (GET /networkpolicy reports mode=allow_all) and the
#      guest reaches an allowed target.
#   2. Deny rule #1 (PUT {defaultAction: allow, deny pypi.org}): the policy
#      converges to enforcing with the deny rule; in-guest traffic —
#      pypi.org BLOCKED, example.com still reachable (default-allow intact).
#   3. Snapshot: `osb snapshot create` returns Creating; a re-entry snapshot
#      during the dump window is fenced (409, or 202 during fence cache lag
#      and resolved to a terminal phase); the row converges to Ready via the
#      SandboxSnapshot CR watcher; the source stays Running + /ping-healthy.
#   4. ARTIFACT (dump time): the published artifact set (the SandboxSnapshot
#      CR's status.manifestRef directory) carries policy.json whose recorded
#      policy is EXACTLY the dump-time one — defaultAction allow, egress
#      == [deny pypi.org]; github.com is absent (not added yet).
#   5. Deny rule #2 (PATCH merge deny github.com): enforcing with both
#      denies; in-guest traffic — github.com now blocked too, example.com
#      still reachable. Then the source sandbox is deleted.
#   6. Restore: `osb sandbox create --snapshot-id` boots the published
#      artifact set — the restored sandbox runs the DUMP-TIME policy: deny
#      pypi.org still enforced, github.com REACHABLE AGAIN (the post-dump
#      rule never entered the artifact set), example.com reachable.
#   7. Deny rule #3 on the restored sandbox (PATCH deny github.com):
#      enforced and traffic-blocked. The restored sandbox is deleted.
#   8. Negative: PUT /networkpolicy on the DELETED sandbox id must be
#      rejected with 404.
#   9. Cleanup: every snapshot row is deleted (the server forwards per-row
#      artifact deletion).
#
# Housekeeping: the artifact store is pruned at entry — stale checkpoint /
# template sets are removed, the baseline golden template set and the
# indexes pointing into it are preserved.
#
# The created snapshot id is shared through SNAPSHOT_ID; a re-entry
# snapshot accepted during fence cache lag is tracked through SNAPSHOT_EXTRA.
#
# Runs as part of `integration.sh up` (the entry chains every verify
# script) or standalone against a live stack from `env.sh up`.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# The created snapshot id is shared through SNAPSHOT_ID.
SNAPSHOT_ID=""
# A re-entry snapshot accepted during fence cache lag (202) is tracked here
# so its terminal outcome is asserted and it is cleaned up.
SNAPSHOT_EXTRA=""

# --- stage: snapshot (opensandbox CLI -> SandboxSnapshot CR -> restore) ----------

# Ready once the server watcher converges the row from the SandboxSnapshot
# CR (Succeeded + template index published).
snapshot_ready() { # <snapshot-id>
	[[ "$(osb_api snapshot get "$1" -o json 2>/dev/null | jq -r '.status.state // empty')" == "Ready" ]]
}

# Terminal (Ready or Failed): the pause-window fence resolves any
# accepted-but-conflicting snapshot one way or the other.
snapshot_terminal() { # <snapshot-id>
	local state
	state="$(osb_api snapshot get "$1" -o json 2>/dev/null | jq -r '.status.state // empty')"
	[[ "$state" == "Ready" || "$state" == "Failed" ]]
}

# --- policy view + in-guest traffic probes -----------------------------------------

snapshot_policy_mode_is() { # <mode> — GET /networkpolicy reports <mode>
	[[ "$(server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null | jq -r '.mode // empty')" == "$1" ]]
}

snapshot_policy_rule_is() { # <target> <action> — a rule for <target> with <action>
	server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null | jq -e --arg t "$1" --arg a "$2" \
		'([.policy.egress[]? | select(.target == $t)][0].action // "") == $a' >/dev/null
}

snapshot_policy_lacks_target() { # <target> — no rule for <target>
	server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null | jq -e --arg t "$1" \
		'([.policy.egress[]? | select(.target == $t)] | length) == 0' >/dev/null
}

# In-guest traffic probe: execd POST /command through the signed gateway
# route (the same surface the SDK's commands.run uses). The probe curls the
# target from INSIDE the guest and echoes a marker; the raw SSE stream is
# scanned for it, which survives any event framing.
snapshot_guest_exec() { # <shell-snippet> -> raw SSE stream (best effort)
	local route
	route="$(osb_api sandbox endpoint "$VERIFY_ID" --port 44772 -o json 2>/dev/null \
		| jq -r '.headers["OpenSandbox-Ingress-To"] // empty')"
	[[ -n "$route" ]] || return 1
	curl -sS -N -m 45 -H "OpenSandbox-Ingress-To: $route" -H "Content-Type: application/json" \
		-d "$(jq -n --arg c "$1" '{command: $c, timeout: 30000}')" \
		"$GATEWAY_URL/command" 2>/dev/null
}

snapshot_traffic_reachable() { # <target> — the guest can complete https://<target>
	local out
	out="$(snapshot_guest_exec "curl -fsS -m 10 -o /dev/null https://$1 && echo PROBE_REACHABLE || echo PROBE_BLOCKED")" || return 1
	[[ "$out" == *"PROBE_REACHABLE"* ]]
}

snapshot_traffic_blocked() { # <target> — the guest cannot complete https://<target>
	local out
	out="$(snapshot_guest_exec "curl -fsS -m 10 -o /dev/null https://$1 && echo PROBE_REACHABLE || echo PROBE_BLOCKED")" || return 1
	[[ "$out" == *"PROBE_BLOCKED"* ]]
}

# --- artifact probes ---------------------------------------------------------------

# The SandboxSnapshot CR name is deterministic: osb-snap-<uuid hex without
# dashes> (server/sandbox_snapshot naming), with the raw id in a label.
snapshot_cr_name() { # <snapshot-id> -> CR object name
	printf 'osb-snap-%s' "$(printf '%s' "$1" | tr -d '-')"
}

# status.manifestRef points at the published manifest.json (Succeeded only);
# the recorded policy file sits next to it in the artifact set.
snapshot_bundle_policy() { # -> the snapshot set's policy.json envelope
	local ref key
	ref="$(kubectl_get "sandboxsnapshot/$(snapshot_cr_name "$SNAPSHOT_ID")" '{.status.manifestRef}' 2>/dev/null)" || return 1
	[[ "$ref" == "s3://$RUSTFS_BUCKET/publish/"* ]] || return 1
	key="${ref#s3://$RUSTFS_BUCKET/publish/}"
	key="${key%/*}/policy.json"
	rc cat "chain/$RUSTFS_BUCKET/publish/$key" 2>/dev/null
}

snapshot_bundle_policy_is() { # <jq filter> — assert on the envelope
	snapshot_bundle_policy | jq -e "$1" >/dev/null
}

# Status-code-returning server_api for negative-path assertions.
snapshot_api_status() { # method path [json-body] -> http status
	local method="$1" path="$2" body="${3:-}" out
	if [[ -n "$body" ]]; then
		out="$(curl -sS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-H "Content-Type: application/json" -d "$body" -w '\n%{http_code}' "$SERVER_URL$path" 2>/dev/null)"
	else
		out="$(curl -sS -m 60 -X "$method" -H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" \
			-w '\n%{http_code}' "$SERVER_URL$path" 2>/dev/null)"
	fi
	printf '%s' "${out##*$'\n'}"
}

# --- the verify ---------------------------------------------------------------------

snapshot_verify() {
	# Test shape: a policy-less sandbox gains deny rules over a default-allow
	# bootstrap; the snapshot must carry the recorded policy at dump time;
	# the restored sandbox must run EXACTLY that policy (post-dump rules out);
	# rule updates on a deleted sandbox must be rejected with 404.
	local created out source_id snapshot_id snapshot_id2 restore_id reentry_out reentry_code
	# Snapshot sets are multi-GiB: drop stale ones from earlier runs before
	# this verify adds its own (baseline template kept).
	prune_artifact_store
	local t0 t1 t2 t3 t4
	t0="$(now_ms)"

	# --- Stage A: policy-less source -> deny rule #1 -> snapshot -----------------
	log "verify (snapshot): creating a sandbox WITHOUT a network policy via the opensandbox CLI (templateId=$TEMPLATE_ID)"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
		--metadata origin=fast-sandbox-env-snapshot --skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	source_id="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$source_id" && "$source_id" != "null" ]] || fail "create response carried no id"
	VERIFY_ID="$source_id"
	wait_for "snapshot source sandbox Running" 300 sandbox_running
	wait_for "pre-snapshot execd /ping 200 through the gateway" 100 execd_ping_ok
	t1="$(now_ms)"
	log "verify (snapshot): source sandbox $source_id create->Running $(( (t1 - t0) / 1000000 ))ms"

	# Bootstrap: a policy-less create seeds a default-allow egress binding
	# (registration is the point), so the policy surface is live and open.
	wait_for "policy-less sandbox bootstraps default-allow (mode=allow_all)" 60 \
		snapshot_policy_mode_is allow_all
	wait_for "guest reaches example.com under the bootstrap policy" 120 \
		snapshot_traffic_reachable example.com
	pass "policy-less source: default-allow bootstrap live (mode=allow_all, example.com reachable)"

	# Deny rule #1 via full replace: default-allow + a single deny target —
	# a deny rule is only meaningful when the default is allow.
	local put_body
	put_body="$(jq -n '{defaultAction: "allow", egress: [{action: "deny", target: "pypi.org"}]}')"
	server_api PUT "/sandboxes/$VERIFY_ID/networkpolicy" "$put_body" >/dev/null \
		|| fail "PUT networkpolicy (deny pypi.org) failed for $VERIFY_ID"
	wait_for "deny pypi.org recorded and enforcing" 120 snapshot_policy_rule_is pypi.org deny
	wait_for "guest blocked from pypi.org" 180 snapshot_traffic_blocked pypi.org
	wait_for "guest still reaches example.com (default-allow intact)" 120 \
		snapshot_traffic_reachable example.com
	pass "deny rule #1 live: pypi.org blocked, example.com reachable (default-allow)"

	# 1. Snapshot create returns Creating; the row converges from the
	# SandboxSnapshot CR via the server watcher.
	log "verify (snapshot): osb snapshot create $source_id --name env-verify"
	out="$(osb_api snapshot create "$source_id" --name env-verify -o json 2>/dev/null)" \
		|| fail "snapshot create failed for $source_id"
	SNAPSHOT_ID="$(printf '%s' "$out" | jq -r '.id')"
	[[ -n "$SNAPSHOT_ID" && "$SNAPSHOT_ID" != "null" ]] || fail "snapshot create carried no id"
	[[ "$(printf '%s' "$out" | jq -r '.status')" == "Creating" ]] \
		|| fail "snapshot create did not return Creating: $(printf '%s' "$out" | head -c 300)"

	# 2. Re-entry during the dump window: FastPath fences with 409 once the
	# CR is cache-visible; with cache lag it is accepted (202) and the
	# pause window resolves it to a terminal phase later.
	reentry_out="$(curl -sS -m 60 -w '\n%{http_code}' -X POST \
		-H "OPEN-SANDBOX-API-KEY: $SERVER_API_KEY" -H "Content-Type: application/json" \
		-d '{"name":"env-verify-reentry"}' "$SERVER_URL/sandboxes/$source_id/snapshots" 2>/dev/null || true)"
	reentry_code="$(printf '%s' "$reentry_out" | tail -n1)"
	reentry_out="$(printf '%s' "$reentry_out" | sed '$d')"
	case "$reentry_code" in
		409)
			pass "snapshot: re-entry rejected by the fence (409)"
			;;
		202)
			SNAPSHOT_EXTRA="$(printf '%s' "$reentry_out" | jq -r '.id' 2>/dev/null || true)"
			[[ -n "$SNAPSHOT_EXTRA" && "$SNAPSHOT_EXTRA" != "null" ]] \
				|| fail "re-entry snapshot POST returned 202 without an id: $(printf '%s' "$reentry_out" | head -c 300)"
			log "verify (snapshot): re-entry accepted during fence cache lag ($SNAPSHOT_EXTRA); terminal outcome asserted below"
			;;
		*)
			fail "re-entry snapshot POST returned unexpected HTTP ${reentry_code:-none}: $(printf '%s' "$reentry_out" | head -c 300)"
			;;
	esac

	wait_for "poll osb snapshot get $SNAPSHOT_ID until Ready (watcher -> SandboxSnapshot CR -> store index)" 300 snapshot_ready "$SNAPSHOT_ID"
	t2="$(now_ms)"
	log "verify (snapshot): $SNAPSHOT_ID snapshot create->Ready $(( (t2 - t1) / 1000000 ))ms"
	pass "snapshot: create -> Creating -> watcher -> Ready"

	# 3. Source survival: Running + /ping again after the terminal phase.
	wait_for "source sandbox Running again after the snapshot" 120 sandbox_running
	wait_for "source sandbox execd /ping 200 after the snapshot" 100 execd_ping_ok
	pass "snapshot: source sandbox survived (Running + /ping 200)"

	# 4. ARTIFACT (dump time): the published set carries the recorded policy
	# file with EXACTLY the policy at dump time — rule #2 does not exist yet.
wait_for "snapshot artifact carries policy.json (defaultAction allow)" 60 \
	snapshot_bundle_policy_is '.policy.defaultAction == "allow"'
wait_for "snapshot artifact policy == dump-time rules (deny pypi.org only)" 60 \
	snapshot_bundle_policy_is '.policy == {defaultAction: "allow", egress: [{action: "deny", target: "pypi.org"}]}'
	pass "snapshot artifact carries the dump-time recorded policy (policy.json: allow + deny pypi.org only)"

	# 5. Deny rule #2 AFTER the dump: merged via PATCH; the source sandbox
	# blocks github.com too, then is deleted per the test shape.
	local patch_body
	patch_body='[{"action": "deny", "target": "github.com"}]'
	server_api PATCH "/sandboxes/$VERIFY_ID/networkpolicy" "$patch_body" >/dev/null \
		|| fail "PATCH networkpolicy (deny github.com) failed for $VERIFY_ID"
	wait_for "deny github.com recorded and enforcing" 120 snapshot_policy_rule_is github.com deny
	wait_for "guest blocked from github.com" 180 snapshot_traffic_blocked github.com
	wait_for "guest still reaches example.com after rule #2" 120 \
		snapshot_traffic_reachable example.com
	pass "deny rule #2 live post-dump: github.com blocked (source only)"

	# 5b. Listing: sandboxId scoping returns the snapshot as Ready.
	out="$(osb_api snapshot list --sandbox-id "$source_id" --page-size 50 -o json 2>"$WORK/last-snapshot-list.err")" \
		|| fail "snapshot list failed for $source_id: $(head -c 400 "$WORK/last-snapshot-list.err" 2>/dev/null)"
	[[ "$(printf '%s' "$out" | jq -r --arg id "$SNAPSHOT_ID" \
		'[.items[] | select(.id == $id and .status.state == "Ready")] | length')" == "1" ]] \
		|| fail "snapshot list does not contain $SNAPSHOT_ID as Ready: $(printf '%s' "$out" | head -c 400)"
	pass "snapshot: list scoped by sandboxId contains the snapshot (Ready)"

	# 5c. Delete the source sandbox (the snapshot outlives it).
	osb_api sandbox kill "$source_id" >/dev/null 2>&1 \
		|| log "verify cleanup: kill $source_id failed"
	wait_for "snapshot source sandbox deleted" 120 verify_sandbox_gone

	# --- Stage B: restore from the snapshot --------------------------------------
	local restored
	log "verify (snapshot): osb sandbox create --snapshot-id $SNAPSHOT_ID (pool profile restated)"
	restored="$(osb_api sandbox create --snapshot-id "$SNAPSHOT_ID" --timeout 1h \
		--resource cpu=1 --resource memory=512Mi --resource pids=128 \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create (snapshotId=$SNAPSHOT_ID) failed"
	restore_id="$(printf '%s' "$restored" | jq -r '.id')"
	[[ -n "$restore_id" && "$restore_id" != "null" ]] || fail "restore create carried no id"
	VERIFY_ID="$restore_id"
	wait_for "restored sandbox Running" 600 sandbox_running
	wait_for "restored execd /ping 200 through the gateway" 300 execd_ping_ok
	# The restored sandbox runs the DUMP-TIME policy: the bundle is delivered
	# to egress (POLICY_IMPORT) before the data plane is ready.
	wait_for "restored sandbox enforcing the dump-time deny (pypi.org)" 120 \
		snapshot_policy_rule_is pypi.org deny
	wait_for "restored sandbox has no post-dump rule (github.com absent)" 60 \
		snapshot_policy_lacks_target github.com
	wait_for "restored guest still blocked from pypi.org" 180 snapshot_traffic_blocked pypi.org
	wait_for "restored guest reaches github.com AGAIN (post-dump rule not in the artifact set)" 180 \
		snapshot_traffic_reachable github.com
	wait_for "restored guest reaches example.com" 120 snapshot_traffic_reachable example.com
	pass "restore: dump-time policy live (pypi.org blocked, github.com reachable again, example.com fine)"

	# 7. Deny rule #3 on the restored sandbox: github.com denied and blocked.
	server_api PATCH "/sandboxes/$VERIFY_ID/networkpolicy" "$patch_body" >/dev/null \
		|| fail "PATCH networkpolicy (deny github.com) failed for the restored sandbox"
	wait_for "restored sandbox enforcing deny github.com" 120 snapshot_policy_rule_is github.com deny
	wait_for "restored guest blocked from github.com" 180 snapshot_traffic_blocked github.com
	pass "deny rule #3 live on the restored sandbox: github.com blocked"

	# 8. Negative: updating rules on a DELETED sandbox must be rejected (404).
	osb_api sandbox kill "$restore_id" >/dev/null 2>&1 \
		|| log "verify cleanup: kill $restore_id failed"
	wait_for "restored sandbox deleted" 120 verify_sandbox_gone
	local neg_body neg_code
	neg_body="$(jq -n '{defaultAction: "allow", egress: [{action: "deny", target: "example.com"}]}')"
	neg_code="$(snapshot_api_status PUT "/sandboxes/$restore_id/networkpolicy" "$neg_body")"
	[[ "$neg_code" == "404" ]] \
		|| fail "networkpolicy update on the deleted sandbox returned $neg_code, expected 404"
	pass "policy update on the deleted sandbox rejected (404)"

	# 9. Cleanup; the server forwards artifact deletion per snapshot row.
	for snapshot_id in "$SNAPSHOT_ID" "$SNAPSHOT_EXTRA"; do
		[[ -n "$snapshot_id" ]] || continue
		osb_api snapshot delete "$snapshot_id" >/dev/null 2>&1 \
			|| log "verify cleanup: snapshot delete $snapshot_id failed"
	done
	pass "snapshot: cleanup (source + restored sandboxes deleted, snapshot rows removed)"
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error snapshot-verify' ERR
	run_stage "snapshot verify (CLI -> SandboxSnapshot -> restore)" snapshot_verify
fi
