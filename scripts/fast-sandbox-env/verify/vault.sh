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

# verify/vault.sh — credential-vault lifecycle + MITM CA chain (OSEP-0022)
#
# Stage 1: vault lifecycle over the server relay. Coverage (all assertions
# on the live egress sidecar via the fastlet Actions channel; log markers
# are read from the pool's egress containers):
#
#   1. Subject registration: the sandbox is created with a create-carried
#      default-deny egress policy (allow ext.test) — a policy-less create
#      produces no egress binding at all, so the subject would never
#      register. The verify waits for the egress log marker
#      "subject s-<uid>: data-plane-ready, policy active".
#   2. Vault create over the server relay: POST
#      /sandboxes/{id}/credential-vault (inline credential `tok` + an
#      apiKey binding for host ext.test) must ack with a numeric revision.
#   3. Create-only invariant: a duplicate POST must be rejected with 409.
#   4. Pause/resume vault transfer: pause exports the vault into the
#      checkpoint bundle ("vault exported to snapshot bundle" marker);
#      the pause's REMOVE_BINDING purges the subject, so on resume the
#      import lands through the unregistered branch and the file reloads
#      at the SET_BINDING replay ("vault reloaded from file" marker). The
#      relay GET must report the SAME revision across the round trip.
#   5. Egress crash recovery: the egress process is pkilled in every pool
#      pod; when the containers come back (fresh instanceId -> SET_BINDING
#      replay -> vault file reload by incarnation) the relay must still
#      report the original revision. Skipped on pods without pkill.
#   6. Delete/re-create: DELETE -> GET 404; a fresh POST starts again at
#      revision 1.
#   7. Cleanup: kill the sandbox and wait until GET 404s.
#
# Stage 2: MITM CA chain verify (OSEP-0022 trust anchor) — proves the chain
# `up` provisions: pinned CA Secret -> template bake -> egress mitmdump:
#
#   1. Three-way fingerprint pinning on the S3 publication path: the
#      published artifact manifest's platform.egressMitmCA.certSha256 ==
#      the egress /healthz mitmCaFingerprint == the pinned Secret
#      fingerprint (content, not name). An oci:// manifestRef lives in the
#      in-cluster registry, out of rc's reach — there the assertion is the
#      /healthz leg plus the behavioral chain below (a guest accepts the
#      MITM certificate only because it chains to the template-baked CA).
#   2. Negative control before the vault exists: a guest HTTPS request to
#      the CA-signed TLS echo endpoint passes through the shared mitmdump
#      opaquely, TLS verified against the template-baked CA (no -k), and
#      no X-Api-Key appears.
#   3. Positive: after binding a vault credential to the SNI, the same
#      guest-originated request reflects the injected X-Api-Key —
#      observed by the echo endpoint, outside the sandbox.
#   4. Pause/resume: the request keeps passing after checkpoint/restore —
#      trust (baked CA in the rootfs) and vault travel with the snapshot.
#
# The CA/echo material is provisioned by `up` (mitm_ca_up / mitm_echo_up):
# this file reads the pinned fingerprint from $WORK/mitm-ca/fingerprint and
# re-derives the echo endpoint's kind-network IP from the live container, so
# both stages run against a live stack from a prior `up`.
#
# NOTE: integration-env.sh (the monolith CI drives today) carries working
# copies of these stages; keep the two in sync until the env.sh split is
# completed and this file becomes the single home.
#
# Housekeeping: the artifact store is pruned at entry — stale checkpoint /
# template sets are removed, the baseline golden template set and the
# indexes pointing into it are preserved.
#
# Runs as part of `integration.sh up` (the entry chains every verify
# script) or standalone against a live stack from `env.sh up`.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

# MITM chain material config (mirrors integration-env.sh; lib.sh does not
# carry env-side MITM state).
MITM_CA_DIR="$WORK/mitm-ca"
MITM_CA_FINGERPRINT_FILE="$MITM_CA_DIR/fingerprint"
MITM_CA_FINGERPRINT=""
MITM_ECHO_CONTAINER="${MITM_ECHO_CONTAINER:-fast-sandbox-env-mitm-echo}"
MITM_ECHO_DOMAIN="${MITM_ECHO_DOMAIN:-echo.test}"
MITM_ECHO_PORT=443
MITM_ECHO_IP=""

# --- stage: vault lifecycle (OSEP-0022: egress-held vault over the relay) --------

# Status-code-returning variant of server_api for negative-path assertions
# (curl -f hides the status).
server_api_status() { # method path [json-body]
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

# The vault stage's sandbox must ride the subject-scoped egress log markers
# (export/import/registration), so every log assertion goes through this.
vault_log_contains() { # <fixed-substring>
	kubectl -n "$RESOURCE_NS" logs -l app=sandbox-fastlet -c egress --tail=800 2>/dev/null \
		| grep -qF "$1"
}

vault_revision_is() { # <expected-revision>
	local out
	out="$(server_api GET "/sandboxes/$VERIFY_ID/credential-vault" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.revision // empty')" == "$1" ]]
}

vault_lifecycle_verify() {
	local created vault_body ack rev sandbox_uid
	# The pause here publishes a checkpoint set (vault + policy bundles):
	# drop stale ones from earlier runs before this verify adds its own.
	prune_artifact_store
	log "verify (vault): creating a sandbox via the opensandbox CLI (templateId=$TEMPLATE_ID)"
	# The policy rides the create: a policy-less create produces no egress
	# binding at all, so the subject would never register.
	jq -n '{defaultAction: "deny", egress: [{action: "allow", target: "ext.test"}]}' \
		> "$GEN_DIR/vault-policy.json"
	created="$(osb_api sandbox create --template "$TEMPLATE_ID" --timeout 1h \
		--metadata origin=fast-sandbox-env-vault \
		--network-policy-file "$GEN_DIR/vault-policy.json" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "vault verify: sandbox create failed"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	wait_for "vault sandbox Running" 150 sandbox_running
	sandbox_uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$sandbox_uid" ]] || fail "vault verify: sandbox CR carried no uid"
	# Registered and activated before the vault POST, else the relay 404s.
	# (The "(state=active)" re-delivery only follows a policy UPDATE; a
	# create-carried policy activates without it.)
	wait_for "vault subject registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: data-plane-ready, policy active"

	# 1) create-only: ack carries the revision; duplicate POST is 409.
	vault_body="$(jq -n '{credentials: [{name: "tok", source: {type: "inline", value: "sekrit-vault-e2e"}}], bindings: [{name: "ext", match: {hosts: ["ext.test"]}, auth: {type: "apiKey", name: "X-Api-Key", credential: "tok"}}]}')"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "vault verify: POST credential-vault failed"
	rev="$(printf '%s' "$ack" | jq -r '.revision // empty')"
	[[ "$rev" =~ ^[0-9]+$ ]] || fail "vault verify: create ack carried no revision: $(printf '%s' "$ack" | head -c 200)"
	pass "vault created over the relay (ack revision $rev)"
	if server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body" >/dev/null 2>&1; then
		fail "vault verify: duplicate POST succeeded (create-only must 409)"
	fi
	[[ "$(server_api_status POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" == "409" ]] \
		|| fail "vault verify: duplicate POST was not 409"
	pass "duplicate POST rejected (create-only 409)"

	# 2) pause/resume carries the real vault: export -> bundle -> import,
	#    with the relay GET reporting the same revision.
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 || fail "vault verify: pause failed"
	wait_for "vault sandbox Paused (checkpoint durable)" 240 sandbox_paused
	wait_for "vault exported into the checkpoint bundle" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault exported to snapshot bundle"
	pass "pause exported the vault bundle (subject sealed)"
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 || fail "vault verify: resume failed"
	wait_for "vault sandbox Running (restored)" 240 sandbox_running
	# The pause's REMOVE_BINDING purges the subject, so the restore import
	# lands through the unregistered branch and the file reloads at the
	# SET_BINDING replay (the reload line carries the restored revision).
	wait_for "vault bundle reloaded at re-registration" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault reloaded from file"
	wait_for "vault revision preserved across pause/resume" 120 vault_revision_is "$rev"
	pass "vault survived pause/resume (revision $rev preserved)"

	# 3) egress-restart recovery: crash the worker (fresh instanceId ->
	#    SET_BINDING replay -> vault file reload by incarnation).
	local pod restarted=0
	for pod in $(pool_pods); do
		if kubectl -n "$RESOURCE_NS" exec "$pod" -c egress -- sh -c 'command -v pkill >/dev/null' 2>/dev/null; then
			kubectl -n "$RESOURCE_NS" exec "$pod" -c egress -- pkill -f '/opt/opensandbox-egress/egress' >/dev/null 2>&1 || true
			restarted=1
		else
			log "verify (vault): egress restart skipped on ${pod##*/} (pkill unavailable)"
		fi
	done
	if [[ "$restarted" == 1 ]]; then
		wait_for "pool egress containers back up" 180 egress_containers_ready
		wait_for "vault reloaded after egress restart" 240 vault_revision_is "$rev"
		pass "vault reloaded after egress restart (revision $rev preserved)"
	fi

	# 4) delete + re-create.
	server_api DELETE "/sandboxes/$VERIFY_ID/credential-vault" >/dev/null \
		|| fail "vault verify: DELETE failed"
	[[ "$(server_api_status GET "/sandboxes/$VERIFY_ID/credential-vault")" == "404" ]] \
		|| fail "vault verify: GET after DELETE was not 404"
	pass "vault deleted (GET 404)"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "vault verify: re-create after DELETE failed"
	rev="$(printf '%s' "$ack" | jq -r '.revision // empty')"
	[[ "$rev" == "1" ]] || fail "vault verify: fresh vault revision is $rev, expected 1"
	pass "vault re-created (fresh revision $rev)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "vault sandbox deleted" 120 verify_sandbox_gone
}

# --- stage: MITM CA chain verify (OSEP-0022: baked CA + injection + pinning) ----

# first_pool_pod -> one fastlet pod of the pool (the egress /healthz probe
# binds Pod loopback, so it must exec into a specific pod).
first_pool_pod() {
	kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# Cross-invocation state (up and the verify actions are separate processes;
# same pattern as require_template_id): the fingerprint pin lives in a file,
# the echo endpoint is re-derived from the live container.
load_mitm_ca_fingerprint() {
	if [[ -z "$MITM_CA_FINGERPRINT" && -s "$MITM_CA_FINGERPRINT_FILE" ]]; then
		MITM_CA_FINGERPRINT="$(cat "$MITM_CA_FINGERPRINT_FILE")"
	fi
	[[ -n "$MITM_CA_FINGERPRINT" ]] \
		|| die "no pinned MITM CA fingerprint at $MITM_CA_FINGERPRINT_FILE (run up first; mitm_ca_up stores it there)"
}

load_mitm_echo_endpoint() {
	MITM_ECHO_IP="$(docker inspect -f \
		'{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' \
		"$MITM_ECHO_CONTAINER" 2>/dev/null || true)"
	[[ -n "$MITM_ECHO_IP" ]] \
		|| die "TLS echo container $MITM_ECHO_CONTAINER is not running (run up first; mitm_echo_up starts it)"
}

# egress_mitm_ca_fingerprint -> the pinned CA fingerprint the egress exposes
# on /healthz (empty while the MITM addon is down/pending or not yet up).
egress_mitm_ca_fingerprint() {
	local pod out
	pod="$(first_pool_pod)"
	[[ -n "$pod" ]] || return 1
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$pod" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/healthz" 2>/dev/null)" || return 1
	printf '%s' "$out" | sed -n 's/.*"mitmCaFingerprint":"\([0-9a-f]*\)".*/\1/p'
}

# template_mitm_lineage_fingerprint [template-id] -> the certSha256 the
# builder stamped under platform.egressMitmCA in the published artifact-set
# manifest (empty when the manifest or the entry is missing).
template_mitm_lineage_fingerprint() {
	local tid manifest_ref manifest_json
	tid="${1:-$TEMPLATE_ID}"
	manifest_ref="$(osb_api template get "$tid" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')"
	[[ -n "$manifest_ref" ]] || return 0
	manifest_json="$(rc cat "chain/$RUSTFS_BUCKET/$(dirname "${manifest_ref#s3://$RUSTFS_BUCKET/}")/manifest.json" 2>/dev/null)" || return 0
	printf '%s' "$manifest_json" | jq -r '.platform.egressMitmCA.certificates[0].certSha256 // empty' 2>/dev/null
}

# Guest-side HTTPS request to the echo endpoint via `osb command run` (the
# only sandbox-reachable surface). The command must ride as separate words —
# the CLI joins argv with shlex.quote into one shell line, so a pre-quoted
# string would execute as a single program name (exit 127). `curl --resolve`
# pins the SNI to the echo domain — deliberately unresolvable DNS, the
# policy carries the IP-allow rule the resolved flow needs; the egress DNATs
# the allowed flow to the shared mitmdump, which matches the vault binding
# by SNI. TLS verification runs against the template-baked CA (no -k
# anywhere). The guest-side stderr (the first diagnostic for a broken TLS
# chain) is kept per attempt at $LOGS_DIR/mitm-guest-curl.err.
mitm_ca_guest_echo() {
	osb_api command run "$VERIFY_ID" -- \
		curl -fsS -m 20 --resolve "$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT:$MITM_ECHO_IP" \
		"https://$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT/echo" \
		2>"$LOGS_DIR/mitm-guest-curl.err"
}

# osb_command_succeeds <command words...>: wait_for probe variant — the
# guest command must exit 0.
osb_command_succeeds() {
	osb_api command run "$VERIFY_ID" -- "$@" >/dev/null 2>"$LOGS_DIR/mitm-guest-curl.err"
}

# mitm_ca_guest_header_is <key> <value>: the echo reflected the exact header.
# Scans line-by-line: the CLI stream may carry non-JSON framing.
mitm_ca_guest_header_is() { # <lowercase-key> <expected-value>
	local out line
	out="$(mitm_ca_guest_echo)" || return 1
	while IFS= read -r line; do
		[[ -n "$line" ]] || continue
		printf '%s' "$line" | jq -e --arg k "$1" --arg v "$2" '.[$k] == $v' >/dev/null 2>&1 && return 0
	done <<EOF
$out
EOF
	return 1
}

mitm_ca_chain_verify() {
	# Optional template override (the overlaybd second pass); default native.
	local template_id="${1:-$TEMPLATE_ID}"
	load_mitm_ca_fingerprint
	load_mitm_echo_endpoint
	local created sandbox_uid ack echo_secret
	log "verify (mitm-ca): fingerprint pinning (pinned Secret == egress /healthz, plus the manifest lineage on the S3 publication path)"
	local healthz_fp lineage_fp manifest_ref
	# Guarded so a failed probe lands on the crafted mismatch message (and
	# its diagnostics) instead of an ERR-trap abort.
	healthz_fp="$(egress_mitm_ca_fingerprint)" || true
	[[ "$healthz_fp" == "$MITM_CA_FINGERPRINT" ]] \
		|| fail "mitm-ca verify: egress /healthz fingerprint is '${healthz_fp:-empty}', want pinned $MITM_CA_FINGERPRINT"
	# The manifest lineage leg is only readable on the S3 publication path:
	# an oci:// manifestRef lives in the in-cluster registry, out of rc's
	# reach. There the pool-side pinning plus the behavioral chain below
	# (the guest accepts the MITM certificate only because it chains to the
	# template-baked CA) carry the assertion.
	manifest_ref="$(osb_api template get "$template_id" -o json 2>/dev/null | jq -r '.status.manifest_ref // empty')" || true
	if [[ "$manifest_ref" == s3://* ]]; then
		lineage_fp="$(template_mitm_lineage_fingerprint "$template_id")"
		[[ "$lineage_fp" == "$MITM_CA_FINGERPRINT" ]] \
			|| fail "mitm-ca verify: template manifest platform.egressMitmCA fingerprint is '${lineage_fp:-empty}', want pinned $MITM_CA_FINGERPRINT"
		pass "template↔pool CA pinning: manifest platform.egressMitmCA == egress /healthz == pinned (${MITM_CA_FINGERPRINT:0:16}…)"
	else
		pass "template↔pool CA pinning: egress /healthz == pinned (${MITM_CA_FINGERPRINT:0:16}…); ${manifest_ref:-no manifest_ref} — guest-side chain proven behaviorally below"
	fi

	# Sandbox policy: the domain allow covers the vault binding (binding
	# validation requires the host be policy-allowed), the IP allow covers
	# the --resolve flow at the nft layer (no DNS query ever happens).
	jq -n --arg ip "$MITM_ECHO_IP" --arg domain "$MITM_ECHO_DOMAIN" \
		'{defaultAction: "deny", egress: [{action: "allow", target: $ip}, {action: "allow", target: $domain}]}' \
		> "$GEN_DIR/mitm-ca-policy.json"
	log "verify (mitm-ca): creating a sandbox via the opensandbox CLI (templateId=$template_id, echo=$MITM_ECHO_IP)"
	created="$(osb_api sandbox create --template "$template_id" --timeout 1h \
		--metadata origin=fast-sandbox-env-mitm-ca \
		--network-policy-file "$GEN_DIR/mitm-ca-policy.json" \
		--skip-health-check -o json 2>/dev/null)" \
		|| fail "mitm-ca verify: sandbox create failed"
	VERIFY_ID="$(printf '%s' "$created" | jq -r '.id')"
	[[ -n "$VERIFY_ID" && "$VERIFY_ID" != "null" ]] || fail "mitm-ca verify: create response carried no id"
	wait_for "mitm-ca sandbox Running" 300 sandbox_running
	sandbox_uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$VERIFY_ID" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
	[[ -n "$sandbox_uid" ]] || fail "mitm-ca verify: sandbox CR carried no uid"
	wait_for "mitm-ca subject registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: data-plane-ready, policy active"

	# Negative control BEFORE the vault exists: the flow is allowed and
	# passes through the shared mitmdump opaquely (unbound SNI), TLS is
	# verified end-to-end against the template-baked CA — and no credential
	# header appears. Proves the header in the positive case comes from the
	# vault, not from anything ambient.
	wait_for "pre-vault guest HTTPS reaches the echo (passthrough, baked-CA verified)" 120 \
		osb_command_succeeds curl -fsS -m 20 \
		--resolve "$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT:$MITM_ECHO_IP" \
		"https://$MITM_ECHO_DOMAIN:$MITM_ECHO_PORT/echo"
	local pre_vault_out
	pre_vault_out="$(mitm_ca_guest_echo)" \
		|| fail "mitm-ca verify: pre-vault guest HTTPS request failed (cannot assert header absence)"
	if printf '%s' "$pre_vault_out" | grep -qi 'x-api-key'; then
		fail "mitm-ca verify: x-api-key present before any vault binding existed"
	fi
	pass "pre-vault: guest HTTPS passthrough works, no credential injected"

	# Per-subject vault binding: SNI echo.test -> X-Api-Key.
	echo_secret="mitm-ca-e2e-$(openssl rand -hex 8)"
	local vault_body
	vault_body="$(jq -n --arg v "$echo_secret" --arg domain "$MITM_ECHO_DOMAIN" \
		'{credentials: [{name: "tok", source: {type: "inline", value: $v}}],
		  bindings: [{name: "echo", match: {hosts: [$domain]}, auth: {type: "apiKey", name: "X-Api-Key", credential: "tok"}}]}')"
	ack="$(server_api POST "/sandboxes/$VERIFY_ID/credential-vault" "$vault_body")" \
		|| fail "mitm-ca verify: POST credential-vault failed"
	[[ "$(printf '%s' "$ack" | jq -r '.revision // empty')" =~ ^[0-9]+$ ]] \
		|| fail "mitm-ca verify: vault create ack carried no revision: $(printf '%s' "$ack" | head -c 200)"
	pass "vault bound to SNI $MITM_ECHO_DOMAIN (X-Api-Key) over the relay"

	wait_for "guest HTTPS reflects the injected X-Api-Key (MITM decrypt + vault by SNI)" 120 \
		mitm_ca_guest_header_is "x-api-key" "$echo_secret"
	pass "credential injection observed by the echo endpoint on a guest-originated HTTPS request"

	# Pause/resume: trust travels with the snapshot (the baked CA is part of
	# the rootfs) and the vault is carried in the bundle — the same request
	# must keep working end to end after restore.
	osb_api sandbox pause "$VERIFY_ID" >/dev/null 2>&1 || fail "mitm-ca verify: pause failed"
	wait_for "mitm-ca sandbox Paused (checkpoint durable)" 240 sandbox_paused
	osb_api sandbox resume "$VERIFY_ID" --skip-health-check >/dev/null 2>&1 || fail "mitm-ca verify: resume failed"
	wait_for "mitm-ca sandbox Running (restored)" 240 sandbox_running
	wait_for "restored subject re-registered on egress" 120 \
		vault_log_contains "subject s-$sandbox_uid: vault reloaded from file"
	wait_for "post-resume guest HTTPS still MITM-injected (baked CA + vault survive the snapshot)" 180 \
		mitm_ca_guest_header_is "x-api-key" "$echo_secret"
	pass "paused/restored sandbox keeps passing TLS verification against the baked CA (injection intact)"

	osb_api sandbox kill "$VERIFY_ID" >/dev/null 2>&1 \
		|| log "verify cleanup: kill failed; remove $VERIFY_ID manually"
	wait_for "mitm-ca sandbox deleted" 120 verify_sandbox_gone
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error vault-verify' ERR
	run_stage "vault lifecycle verify (CLI -> relay -> egress-held vault)" vault_lifecycle_verify
	run_stage "MITM CA chain verify (baked CA -> injection -> pinning)" mitm_ca_chain_verify
fi
