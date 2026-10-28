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

# verify/pause-resume.sh — pause/resume round trip with recorded-policy carry
# (CLI -> FastPath checkpoint -> artifact store)
#
# Test shape: the sandbox is created WITH an egress policy (default deny;
# allow example.com + *.opensandbox.ai), so every pause checkpoint must
# carry the subject's recorded policy file — policy.json rides the bundle
# as an opaque artifact, uploaded next to the checkpoint manifest and
# re-delivered to egress (POLICY_IMPORT) before the data plane is ready on
# resume. The checkpoint bundle is addressed through the Sandbox CR
# (status.runtime.checkpoint.manifestRef) and read from the artifact store
# with the rc CLI; the recorded policy.json is plain JSON (revision +
# embedded policy body), so its rules are asserted directly.
#
# Coverage:
#
#   1. Baseline: create via `osb sandbox create` carrying the policy file,
#      poll to Running, confirm execd /ping 200 through a signed gateway
#      route, and confirm the policy is ENFORCING pre-pause
#      (networkPolicy -> egress action binding -> nft convergence) — the
#      recorded policy file must exist before the checkpoint can carry it.
#   2. Pause semantics (durable-first): POST pause, poll GET until
#      state=Paused (checkpoint durable in the artifact store + fastlet
#      capacity released); pause -> Paused latency logged.
#   3. ARTIFACT (first pause): the egress logs "policy exported to
#      snapshot bundle" for the subject, and the published bundle under
#      the checkpoint manifestRef contains policy.json whose recorded
#      policy still allows example.com + *.opensandbox.ai (revision >= 1).
#   4. Route release (negative assertion): after Paused, the OLD signed
#      route must no longer serve /ping — a still-serving paused sandbox
#      fails the verify.
#   5. Resume: POST resume, poll back to Running (checkpoint restored),
#      /ping 200 through a FRESH route (resume advances the route
#      generation — the old route staying valid would be a bug), the
#      egress logs the policy-bundle delivery for the subject (normally
#      "bundle landed for the next registration" — the delivery precedes
#      the SET_BINDING replay; "imported from snapshot bundle" when it
#      lands registered), and the policy is STILL ENFORCING with the
#      original rules.
#   6. Policy update: PUT /networkpolicy replaces the rule set with a
#      single allow github.com rule; the policy converges to enforcing
#      with the new target.
#   7. ARTIFACT (second pause): pause again; the NEW checkpoint bundle's
#      policy.json must carry the UPDATED policy — github.com present,
#      example.com replaced (absent), and a revision strictly greater
#      than the first bundle's.
#   8. Cleanup: kill the sandbox and wait until GET 404s.
#
# Housekeeping: the artifact store is pruned at entry — stale checkpoint /
# template sets are removed, the baseline golden template set and the
# indexes pointing into it are preserved.
#
# Runs as part of `integration.sh up` (the entry chains every verify
# script) or standalone against a live stack from `env.sh up`.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# --- recorded-policy probes -----------------------------------------------------
# The checkpoint bundle is located through the Sandbox CR (status.runtime.
# checkpoint.manifestRef) and read from the artifact store via the rc CLI;
# policy.json is the egress policy-file envelope: a plain-JSON document
# carrying the revision and the verbatim policy body.

checkpoint_policy_bundle() { # -> policy.json envelope of the CURRENT pause checkpoint
	local ref key
	ref="$(kubectl_get "sandbox/$VERIFY_ID" '{.status.runtime.checkpoint.manifestRef}' 2>/dev/null)" || return 1
	[[ "$ref" == "s3://$RUSTFS_BUCKET/"* ]] || return 1
	key="${ref#s3://$RUSTFS_BUCKET/}"
	key="${key%/*}/policy.json"
	rc cat "chain/$RUSTFS_BUCKET/$key" 2>/dev/null
}

pause_bundle_allows() { # <target> — the bundle's recorded policy allows <target>
	checkpoint_policy_bundle | jq -e --arg t "$1" '[.policy.egress[]?.target] | index($t) != null' >/dev/null
}

pause_bundle_lacks() { # <target> — the bundle's recorded policy no longer has <target>
	checkpoint_policy_bundle | jq -e --arg t "$1" '[.policy.egress[]?.target] | index($t) == null' >/dev/null
}

pause_bundle_revision() { # -> the bundle's recorded revision (0 when unreadable)
	local bundle
	bundle="$(checkpoint_policy_bundle 2>/dev/null)" || { printf '0'; return 0; }
	printf '%s' "$bundle" | jq '.revision // 0' 2>/dev/null || printf '0'
}

pause_policy_enforced() { # <target> — GET /networkpolicy enforcing with <target>
	local out
	out="$(server_api GET "/sandboxes/$VERIFY_ID/networkpolicy" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.mode // empty')" == "enforcing" ]] || return 1
	printf '%s' "$out" | jq -e --arg t "$1" '[.policy.egress[]?.target] | index($t) != null' >/dev/null
}

pause_egress_log_contains() { # <extended-regex> — marker in the pool egress logs
	kubectl -n "$RESOURCE_NS" logs -l app=sandbox-fastlet -c egress --tail=800 2>/dev/null | grep -qE "$1"
}

# --- the verify -------------------------------------------------------------------

pause_resume_verify() {
	# Test shape: create WITH an egress policy -> pause -> the checkpoint
	# bundle in the artifact store must carry the recorded policy file ->
	# resume -> the sandbox serves again AND the policy is still enforced ->
	# PUT a new rule set -> pause again -> the NEW checkpoint bundle must
	# carry the UPDATED policy (new rule present, replaced rule gone).
	local created t0 t1 t2 t3 t4 rev1 rev2 sandbox_uid policy_file
	# Every pause publishes a multi-GiB checkpoint set: drop stale ones from
	# earlier runs before this verify adds its own (baseline template kept).
	prune_artifact_store
	policy_file="$GEN_DIR/pause-policy.json"
	jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "example.com"}, {action: "allow", target: "*.opensandbox.ai"}]}' \
		> "$policy_file"

	log "verify (pause): creating a sandbox via the opensandbox CLI (templateId=$TEMPLATE_ID, with an egress policy)"
	t0="$(now_ms)"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
		--metadata origin=fast-sandbox-env-pause \
		--network-policy-file "$policy_file" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "osb sandbox create failed against $SERVER_URL"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "create response carried no id"
	wait_for "pause target Running" 150 sandbox_running
	t1="$(now_ms)"
	log "verify (pause): $VERIFY_ID create->Running $(( (t1 - t0) / 1000000 ))ms"

	sandbox_uid="$(kubectl_get "sandbox/$VERIFY_ID" '{.metadata.uid}' 2>/dev/null)"
	[[ -n "$sandbox_uid" ]] || fail "sandbox CR carried no uid"

	wait_for "pre-pause execd /ping 200 through the gateway" 100 execd_ping_ok
	# The recorded policy file must exist before the checkpoint can carry it.
	wait_for "egress policy enforcing pre-pause (networkPolicy -> binding -> nft)" 120 \
		pause_policy_enforced example.com

	log "verify (pause): osb sandbox pause $VERIFY_ID"
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 \
		|| fail "pause failed for $VERIFY_ID"
	# Paused is durable-first: the checkpoint must be complete in the artifact
	# store before the state is reported; the dump itself keeps serving.
	wait_for "poll GET until Paused (checkpoint durable, capacity released)" 240 sandbox_paused
	t2="$(now_ms)"
	log "verify (pause): $VERIFY_ID pause->Paused (checkpoint durable) $(( (t2 - t1) / 1000000 ))ms"
	if execd_ping_ok; then
		fail "paused sandbox still serves /ping through the gateway"
	fi
	pass "paused: execd /ping no longer served (runtime released, signed route rejected at the gateway)"

	# Artifact check (first pause): the published checkpoint set must carry
	# the recorded policy file with the pre-pause rules.
	wait_for "egress exported the recorded policy into the checkpoint bundle" 120 \
		pause_egress_log_contains "subject s-$sandbox_uid: policy exported to snapshot bundle"
	wait_for "checkpoint bundle policy.json allows example.com" 60 pause_bundle_allows example.com
	wait_for "checkpoint bundle policy.json allows *.opensandbox.ai" 60 pause_bundle_allows '*.opensandbox.ai'
	rev1="$(pause_bundle_revision)"
	[[ "${rev1:-0}" -ge 1 ]] || fail "checkpoint policy.json carries no positive revision (got ${rev1:-0})"
	pass "checkpoint artifact carries the recorded policy (policy.json, revision $rev1, pre-pause rules intact)"

	log "verify (pause): osb sandbox resume $VERIFY_ID"
	t4="$(now_ms)"
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 \
		|| fail "resume failed for $VERIFY_ID"
	wait_for "poll GET until Running (checkpoint restored)" 240 sandbox_running
	t3="$(now_ms)"
	log "verify (pause): $VERIFY_ID resume->Running (checkpoint restored) $(( (t3 - t4) / 1000000 ))ms"
	wait_for "post-resume execd /ping 200 through a fresh route" 100 execd_ping_ok
	# The bundle is delivered to egress (POLICY_IMPORT) before the data plane
	# is ready, so the restored subject keeps the checkpoint's policy. The
	# delivery lands before SET_BINDING (the pause's REMOVE_BINDING purged
	# the registration), so the marker is the unregistered branch's "bundle
	# landed for the next registration" — accept the registered branch's
	# wording too, in case the delivery ever races ahead of the purge.
	wait_for "egress imported the policy from the checkpoint bundle" 120 \
		pause_egress_log_contains "subject s-$sandbox_uid: policy (imported from snapshot bundle|bundle landed for the next registration)"
	wait_for "policy still enforcing after resume (example.com)" 120 pause_policy_enforced example.com
	pass "policy survived the resume (imported from the bundle, still enforcing)"
	pass "resume->Running $(( (t3 - t4) / 1000000 ))ms (checkpoint restored from the artifact store)"

	# Policy update (raw PUT: the CLI only has merge-style patches), then a
	# second pause whose checkpoint must publish the UPDATED policy.
	local put_body
	put_body="$(jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "github.com"}]}')"
	server_api PUT "/sandboxes/$VERIFY_ID/networkpolicy" "$put_body" >/dev/null \
		|| fail "PUT networkpolicy failed for $VERIFY_ID"
	wait_for "policy update converged (PUT -> ReplaceActionBindings -> nft)" 120 \
		pause_policy_enforced github.com

	log "verify (pause): osb sandbox pause $VERIFY_ID (second round, after the policy update)"
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 \
		|| fail "second pause failed for $VERIFY_ID"
	wait_for "poll GET until Paused (second checkpoint durable)" 240 sandbox_paused

	# Artifact check (second pause): the NEW checkpoint bundle must carry the
	# UPDATED policy — the new rule present, the replaced rule gone, and a
	# strictly higher revision than the first bundle.
	wait_for "updated checkpoint bundle policy.json allows github.com" 60 pause_bundle_allows github.com
	wait_for "updated checkpoint bundle policy.json dropped example.com" 60 pause_bundle_lacks example.com
	rev2="$(pause_bundle_revision)"
	[[ "${rev2:-0}" -gt "${rev1:-0}" ]] \
		|| fail "checkpoint policy.json revision did not advance across the policy update ($rev1 -> $rev2)"
	pass "checkpoint artifact updated: policy.json revision $rev1 -> $rev2, github.com added, example.com replaced"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "pause/resume sandbox deleted" 120 verify_sandbox_gone
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error pause-resume' ERR
	run_stage "pause/resume verify (CLI -> FastPath checkpoint)" pause_resume_verify
fi
