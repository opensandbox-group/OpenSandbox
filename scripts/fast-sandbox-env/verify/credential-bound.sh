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

# verify/credential-bound.sh — OSEP-0023 credential-bound interception verify
# (EGRESS_INTERCEPTION_MODE=credential-bound only).
#
# Real stack, no simulation: two sandboxes reach a persistent HTTP/1.1 TLS
# origin (verify/credential-bound-origin.py, a container on the kind network
# serving the CA-signed echo.test leaf) through the pool egress, using a
# guest-side client (verify/credential-bound-client.py) that verifies TLS
# against the template-baked trust store, never reconnects, and reports the
# presented leaf's DER SHA-256. Origin and MITM leaves chain to the same CA,
# so passthrough is decided by comparing that digest with the origin leaf's
# own digest — never by issuer or chain trust. The origin reports only
# digests of the credential header it received.
#
#   AC2  a sandbox with no vault: origin leaf, no credential header
#   AC8  a network deny still blocks the passthrough flow (origin proven
#        alive in the same probe), and the restored policy reopens it
#   AC3  A bound / B unbound, both alive on the same Fastlet Pod (same Pod
#        UID, so the same egress registry; B is re-created a bounded number
#        of times until it lands there): A MITM leaf + origin sees A's
#        value; B origin leaf + nothing injected
#   AC7  mitmdump, then the egress process, killed in the Pod hosting A
#        and B: a new PID / a new Actions instanceId is proven, then AC3
#        is re-established
#   AC4  B holds a passthrough connection, gets a binding: a fresh
#        connection is injected; the held socket (same socket, no
#        reconnect) never carries a credential, or is closed with evidence
#   AC5  credential-only rotation on A: after the ACK no request ever
#        observes the old value; deleting A's last binding: a socket held
#        across the removal ACK (proven injected with the rotated value) is
#        fenced — its next request is rejected without reaching the origin —
#        and the first new connection after the ACK presents the origin leaf
#   AC6  B (still bound, injection re-proven just before) killed; bounded
#        create/kill attempts look for a new identity on the same Fastlet
#        Pod UID with the same guest address: it carries no old credential;
#        no such reuse observed -> NOT_RUN in the stage summary (never PASS)
#
# A retried probe only retries failures that carry no traffic evidence: a
# credential the probe must not see, or a MITM leaf where passthrough is
# expected, fails the stage at once and is never overwritten by a later
# clean probe.
#   AC9  the current and previous egress container logs (and this env's
#        own logs) are collected in full, then scanned for the fake
#        credential values; a collection error is a failure, never clean;
#        only the verdict is printed
#
# Every sandbox, the origin container and the generated files are
# registered and removed on success and failure paths (EXIT trap).
#
# Loaded (sourced) by integration-env.sh, which owns the globals and helpers;
# standalone runs source verify/vault.sh (lib.sh + MITM helpers) instead.

CB_SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	# shellcheck source=vault.sh
	source "$CB_SELF_DIR/vault.sh"
fi

CB_CLIENT_SRC="$CB_SELF_DIR/credential-bound-client.py"
CB_ORIGIN_SRC="$CB_SELF_DIR/credential-bound-origin.py"
CB_FIXTURE_LABEL="opensandbox.io/fast-sandbox-env-fixture=credential-bound-origin"
CB_GUEST_DIR="/tmp/osep23-cbv"
CB_GUEST_CAFILE="/etc/ssl/certs/ca-certificates.crt"
CB_REUSE_ATTEMPTS="${CB_REUSE_ATTEMPTS:-4}"
CB_COLOCATE_ATTEMPTS="${CB_COLOCATE_ATTEMPTS:-6}"
CB_CLOSED_KINDS="eof reset broken_pipe tls_closed"

CB_SANDBOXES=()
CB_ORIGIN_CONTAINER=""
CB_ORIGIN_IP=""
CB_ORIGIN_LEAF=""
CB_NET=""
CB_WORK_DIR=""
CB_PREV_EXIT_TRAP=""
CB_RESULT=""
CB_RC=0
CB_LAST_ID=""
CB_ACK_REV=""
CB_OLD_PIDS=""
CB_TEMPLATE_ID=""
CB_POD_UID=""
CB_POD_NAME=""

# --- fixture bookkeeping

cb_cleanup() {
	local id
	for id in ${CB_SANDBOXES[@]+"${CB_SANDBOXES[@]}"}; do
		osb_api sandbox kill "$id" >/dev/null 2>&1 || true
	done
	CB_SANDBOXES=()
	if [[ -n "$CB_ORIGIN_CONTAINER" ]]; then
		docker rm -f "$CB_ORIGIN_CONTAINER" >/dev/null 2>&1 || true
		CB_ORIGIN_CONTAINER=""
	fi
	if [[ -n "$CB_WORK_DIR" ]]; then
		rm -rf "$CB_WORK_DIR"
		CB_WORK_DIR=""
	fi
}

cb_forget_sandbox() { # <id>
	local id kept=()
	for id in ${CB_SANDBOXES[@]+"${CB_SANDBOXES[@]}"}; do
		[[ "$id" == "$1" ]] || kept+=("$id")
	done
	CB_SANDBOXES=(${kept[@]+"${kept[@]}"})
}

cb_sandbox_state_is() { # <id> <state>
	[[ "$(osb_api sandbox get "$1" -o json 2>/dev/null | jq -r '.status.state // empty')" == "$2" ]]
}

cb_sandbox_gone() { # <id>
	! osb_api sandbox get "$1" -o json >/dev/null 2>&1
}

cb_sandbox_uid() { # <id>
	kubectl -n "$RESOURCE_NS" get sandbox "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null
}

# Placement: the Fastlet Pod UID recorded on the Sandbox, which must be a
# live Pod of this run's pool (a rebuilt Pod has a new UID, so a stale UID or
# a reused Pod name never matches). Sets CB_POD_UID / CB_POD_NAME (empty and
# rc 1 when unknown).
cb_placement() { # <id>
	local pod_uid name
	CB_POD_UID=""; CB_POD_NAME=""
	pod_uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$1" -o jsonpath='{.status.placement.fastletPodUID}' 2>/dev/null)" || return 1
	[[ -n "$pod_uid" ]] || return 1
	name="$(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o json 2>/dev/null | jq -r --arg uid "$pod_uid" '.items[] | select(.metadata.uid == $uid) | .metadata.name')" || return 1
	[[ -n "$name" && "$name" != *$'\n'* ]] || return 1
	CB_POD_UID="$pod_uid"; CB_POD_NAME="$name"
}

cb_on_pod() { # <id> <fastlet-pod-uid>: placed on that live Pod right now
	cb_placement "$1" && [[ "$CB_POD_UID" == "$2" ]]
}

# cb_create_colocated <label> <policy> <fastlet-pod-uid> -> CB_LAST_ID on
# that Pod. Off-Pod candidates stay alive while searching (so a spreading
# placement fills the other Pods), then are killed; none on the Pod after
# CB_COLOCATE_ATTEMPTS creates is a failure, never a pass.
cb_create_colocated() {
	local attempt id found="" misplaced=()
	for attempt in $(seq 1 "$CB_COLOCATE_ATTEMPTS"); do
		cb_create_sandbox "$1-$attempt" "$2"; id="$CB_LAST_ID"
		if cb_on_pod "$id" "$3"; then found="$id"; break; fi
		log "credential-bound: $1 candidate $id placed on ${CB_POD_NAME:-an unknown Pod}, not the target Pod; retrying"
		misplaced+=("$id")
	done
	for id in ${misplaced[@]+"${misplaced[@]}"}; do cb_kill_sandbox "$id"; done
	[[ -n "$found" ]] || fail "credential-bound: $1 never landed on the same Fastlet Pod in $CB_COLOCATE_ATTEMPTS creates (shared-egress isolation unprovable)"
	CB_LAST_ID="$found"
}

cb_kill_sandbox() { # <id>
	osb_api sandbox kill "$1" >/dev/null 2>&1 || fail "credential-bound: kill of $1 failed"
	wait_for "credential-bound: sandbox $1 deleted" 120 cb_sandbox_gone "$1"
	cb_forget_sandbox "$1"
}

# cb_create_sandbox <label> <policy-file> -> CB_LAST_ID (registered for cleanup)
cb_create_sandbox() {
	local label="$1" policy="$2" created id uid version
	created="$(osb_api sandbox create --template "$CB_TEMPLATE_ID" --timeout 1h \
		--metadata "origin=fast-sandbox-env-credential-bound-$label" \
		--network-policy-file "$policy" --skip-health-check -o json 2>/dev/null)" \
		|| fail "credential-bound: sandbox $label create failed"
	id="$(printf '%s' "$created" | jq -r '.id // empty')"
	[[ -n "$id" ]] || fail "credential-bound: sandbox $label create response carried no id"
	CB_SANDBOXES+=("$id")
	wait_for "credential-bound: sandbox $label ($id) Running" 300 cb_sandbox_state_is "$id" Running
	uid="$(cb_sandbox_uid "$id")" || true
	[[ -n "$uid" ]] || fail "credential-bound: sandbox $label CR carried no uid"
	wait_for "credential-bound: sandbox $label subject registered on egress" 120 \
		vault_log_contains "subject s-$uid: data-plane-ready, policy active"
	osb_api command run "$id" -- mkdir -p "$CB_GUEST_DIR" >/dev/null 2>>"$LOGS_DIR/cb-guest.err" \
		|| fail "credential-bound: guest mkdir failed in $label"
	osb_api file upload "$id" "$CB_CLIENT_SRC" "$CB_GUEST_DIR/client.py" >/dev/null 2>>"$LOGS_DIR/cb-guest.err" \
		|| fail "credential-bound: client upload failed in $label"
	version="$(osb_api command run "$id" -- python3 "$CB_GUEST_DIR/client.py" version 2>>"$LOGS_DIR/cb-guest.err")" \
		|| fail "credential-bound: guest python3 client not runnable in $label"
	[[ "$version" == *"CBV-CLIENT 1"* ]] || fail "credential-bound: unexpected guest client version in $label"
	CB_LAST_ID="$id"
}

# --- origin fixture

cb_digest() { # <value> -> origin credential code
	printf 'sha256:%s' "$(printf '%s' "$1" | openssl dgst -sha256 | awk '{print $NF}')"
}

cb_fake_value() { # <role> -> an obviously fake credential value
	printf 'fake-osep23-%s-%s' "$1" "$(openssl rand -hex 8)"
}

# Host-side probe from a container on the kind network (no egress on the
# path): TLS verified against the pinned CA; two requests on one connection
# prove the origin really keeps HTTP/1.1 connections open.
cb_host_probe() {
	local out rc=0
	out="$(docker run --rm --network "$CB_NET" --entrypoint python3 \
		-v "$CB_CLIENT_SRC":/srv/cbv-client.py:ro \
		-v "$MITM_CA_DIR/tls.crt":/probe/ca.crt:ro \
		"$IMG_EGRESS" /srv/cbv-client.py once --host "$MITM_ECHO_DOMAIN" --ip "$CB_ORIGIN_IP" \
		--port "$MITM_ECHO_PORT" --cafile /probe/ca.crt --tag host-probe --count 2 \
		2>"$LOGS_DIR/cb-host-probe.err")" || rc=$?
	CB_RESULT="$(cb_extract "$out")"
	CB_RC=$rc
	[[ "$rc" == 0 && -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e '.ok and .verified and .connects == 1
		and (.responses | length) == 2
		and .responses[0].origin.conn == .responses[1].origin.conn
		and .responses[1].origin.req == 2
		and .last.origin.proto == "HTTP/1.1"
		and .last.origin.cred == "absent"' >/dev/null
}

cb_origin_up() {
	local leaf_file_digest
	CB_ORIGIN_CONTAINER="fast-sandbox-env-cb-origin-$(openssl rand -hex 4)"
	docker run -d --name "$CB_ORIGIN_CONTAINER" --label "$CB_FIXTURE_LABEL" \
		--label "opensandbox.io/fast-sandbox-env-cluster=$KIND_CLUSTER" --network "$CB_NET" \
		--entrypoint python3 \
		-v "$CB_ORIGIN_SRC":/srv/cbv-origin.py:ro \
		-v "$MITM_CA_DIR/echo.crt":/etc/cbv-origin/tls.crt:ro \
		-v "$MITM_CA_DIR/echo.key":/etc/cbv-origin/tls.key:ro \
		"$IMG_EGRESS" /srv/cbv-origin.py --port "$MITM_ECHO_PORT" \
		--cert /etc/cbv-origin/tls.crt --key /etc/cbv-origin/tls.key >/dev/null \
		|| fail "credential-bound: origin container failed to start"
	CB_ORIGIN_IP="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' "$CB_ORIGIN_CONTAINER")" || true
	[[ -n "$CB_ORIGIN_IP" ]] || fail "credential-bound: origin container has no IP on $CB_NET"
	wait_for "credential-bound: origin keeps HTTP/1.1 connections open ($CB_ORIGIN_IP:$MITM_ECHO_PORT, pinned-CA verified)" 30 cb_host_probe
	CB_ORIGIN_LEAF="$(printf '%s' "$CB_RESULT" | jq -r '.leaf_sha256')"
	leaf_file_digest="$(openssl x509 -in "$MITM_CA_DIR/echo.crt" -outform DER | openssl dgst -sha256 | awk '{print $NF}')" || true
	[[ "$CB_ORIGIN_LEAF" =~ ^[0-9a-f]{64}$ && "$CB_ORIGIN_LEAF" == "$leaf_file_digest" ]] \
		|| fail "credential-bound: origin presented leaf ${CB_ORIGIN_LEAF:0:16}… != echo.crt ${leaf_file_digest:0:16}…"
	pass "credential-bound origin up (leaf ${CB_ORIGIN_LEAF:0:16}…)"
}

# --- guest probes

cb_extract() { # <cli-output> -> the last CBV-RESULT JSON (empty when none)
	printf '%s\n' "$1" | sed -n 's/^.*CBV-RESULT //p' | tail -n 1
}

# One fresh connection from the guest. The CLI exit status is kept (a failed
# guest command is never swallowed); CB_RESULT is empty when the client
# printed no result (CLI/transport failure, not a network verdict).
cb_guest_once() { # <sandbox-id> <tag>
	local out rc=0
	out="$(osb_api command run "$1" -- python3 "$CB_GUEST_DIR/client.py" once \
		--host "$MITM_ECHO_DOMAIN" --ip "$CB_ORIGIN_IP" --port "$MITM_ECHO_PORT" \
		--cafile "$CB_GUEST_CAFILE" --tag "$2" 2>>"$LOGS_DIR/cb-guest.err")" || rc=$?
	CB_RESULT="$(cb_extract "$out")"
	CB_RC=$rc
}

# Verdicts on CB_RESULT: passthrough = origin leaf + nothing injected;
# injected <code> = a different (MITM) leaf + the origin saw that value.
cb_result_passthrough() {
	[[ "$CB_RC" == 0 && -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg leaf "$CB_ORIGIN_LEAF" '.ok and .verified
		and .leaf_sha256 == $leaf and .last.status == 200
		and .last.origin.cred == "absent" and (.last.origin.credential_headers | length) == 0' >/dev/null
}

cb_result_injected() { # <code>
	[[ "$CB_RC" == 0 && -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg leaf "$CB_ORIGIN_LEAF" --arg code "$1" '.ok and .verified
		and .leaf_sha256 != $leaf and .last.status == 200 and .last.origin.cred == $code' >/dev/null
}

cb_result_bound_leaf() { # the response came over a MITM (non-origin) leaf
	[[ -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg leaf "$CB_ORIGIN_LEAF" '(.leaf_sha256 // "") != "" and .leaf_sha256 != $leaf' >/dev/null
}

cb_result_cred_is() { # <code>
	[[ -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg code "$1" '.last.origin.cred == $code' >/dev/null
}

cb_probe_passthrough() { cb_guest_once "$1" "$2"; cb_result_passthrough; }
cb_probe_injected() { cb_guest_once "$1" "$2"; cb_result_injected "$3"; }

# Some response in CB_RESULT carried a credential other than <code>, or any
# credential-shaped header when <code> is "absent".
cb_result_other_credential() { # <code|absent>
	[[ -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg want "$1" '
		def leaked: ((.cred // "absent") as $c | $c != "absent" and $c != $want)
			or ($want == "absent" and ((.credential_headers // []) | length) > 0);
		([.responses[]?.origin | objects | select(leaked)] | length) > 0' >/dev/null
}

# CB_RESULT contradicts the expectation (<code> = injected with that value,
# "absent" = passthrough): another credential was seen, or an expected
# passthrough presented a non-origin (MITM) leaf. Evidence-free failures
# (connect errors, timeouts, no result) do not match.
cb_result_contradicts() { # <code|absent>
	cb_result_other_credential "$1" && return 0
	[[ "$1" == absent && -n "$CB_RESULT" ]] || return 1
	printf '%s' "$CB_RESULT" | jq -e --arg leaf "$CB_ORIGIN_LEAF" \
		'(.leaf_sha256 // "") != "" and .leaf_sha256 != $leaf' >/dev/null
}

# Bounded fresh-connection retries (tags <tag>-<n>) until the expectation
# holds; contradicting evidence fails at once instead of being retried away.
cb_until() { # <description> <attempts> <sandbox-id> <tag> <code|absent>
	local attempt
	for attempt in $(seq 1 "$2"); do
		cb_guest_once "$3" "$4-$attempt"
		! cb_result_contradicts "$5" \
			|| fail "$1: contradicting evidence on attempt $attempt, not retried ($(cb_result_summary))"
		if [[ "$5" == absent ]]; then cb_result_passthrough && break; else cb_result_injected "$5" && break; fi
		[[ "$attempt" -lt "$2" ]] || fail "$1 (after $2 attempts: $(cb_result_summary))"
		sleep 2
	done
	pass "$1"
}

cb_result_summary() { # closed vocabulary only — never credential material
	local r="${CB_RESULT:-}"
	[[ -n "$r" ]] || r='{}'
	printf '%s' "$r" | jq -c --arg leaf "$CB_ORIGIN_LEAF" '{ok, connects,
		leaf: (if .leaf_sha256 == null then null elif .leaf_sha256 == $leaf then "origin" else "other" end),
		status: .last.status, cred: (if .last.origin.cred == null then null elif .last.origin.cred == "absent" then "absent" else "present" end),
		error}' 2>/dev/null || printf 'unparseable'
}

# --- held connections (one socket across a binding/rotation change)

cb_hold_start() { # <sandbox-id> <name>
	local out
	out="$(osb_api command run "$1" --background -o json -- python3 "$CB_GUEST_DIR/client.py" hold \
		--host "$MITM_ECHO_DOMAIN" --ip "$CB_ORIGIN_IP" --port "$MITM_ECHO_PORT" \
		--cafile "$CB_GUEST_CAFILE" --tag "$2" --dir "$CB_GUEST_DIR/$2" 2>>"$LOGS_DIR/cb-guest.err")" \
		|| fail "credential-bound: background hold client $2 failed to start"
	[[ -n "$(printf '%s' "$out" | jq -r '.execution_id // empty' 2>/dev/null)" ]] \
		|| fail "credential-bound: hold client $2 start carried no execution id"
}

cb_hold_read() { # <sandbox-id> <name> <r1|r2> -> CB_RESULT
	local out
	out="$(osb_api command run "$1" -- cat "$CB_GUEST_DIR/$2/$3.json" 2>/dev/null)" || return 1
	CB_RESULT="$(printf '%s\n' "$out" | grep '^{' | tail -n 1)"
	CB_RC=0
	[[ -n "$CB_RESULT" ]] && printf '%s' "$CB_RESULT" | jq -e '.v == 1' >/dev/null 2>&1
}

cb_hold_release() { # <sandbox-id> <name>
	osb_api command run "$1" -- touch "$CB_GUEST_DIR/$2/go" >/dev/null 2>>"$LOGS_DIR/cb-guest.err" \
		|| fail "credential-bound: could not release hold client $2"
}

# Same-socket evidence: one connect() in the process, the socket token and
# local port unchanged between request 1 and request 2.
cb_hold_same_socket() { # <r1-json> <r2-json>
	jq -n -e --argjson a "$1" --argjson b "$2" '$a.connects == 1 and $b.connects == 1
		and $a.socket_id == $b.socket_id and $a.local_port == $b.local_port' >/dev/null
}

cb_hold_closed_with_evidence() { # <r2-json>
	local kind
	kind="$(printf '%s' "$1" | jq -r '.error.kind // empty')"
	[[ -n "$kind" && " $CB_CLOSED_KINDS " == *" $kind "* ]]
}

# OSEP-0023 R7 request fence on a socket decrypted before the last binding
# was removed: a request after the ACK is rejected without reaching the
# origin. The subject path terminates the transport (EOF/reset/TLS close);
# the sidecar path answers locally with 403 + Connection: close (no origin
# body). Prints the outcome; 1 = an origin-served response, a credential, or
# a failure without closure evidence (timeout is never closure).
cb_hold_fence_outcome() { # <r2-json>
	if cb_hold_closed_with_evidence "$1"; then
		printf 'closed by the peer (%s)' "$(printf '%s' "$1" | jq -r '.error.phase + "/" + .error.kind')"
		return 0
	fi
	printf '%s' "$1" | jq -e '.ok and .last.status == 403 and .last.origin == null
		and (.last.connection | ascii_downcase) == "close"' >/dev/null || return 1
	printf 'rejected locally (403, Connection: close, no origin response)'
}

# --- vault relay

cb_vault_body() { # <value> -> create body binding echo.test -> X-Api-Key
	jq -n --arg v "$1" --arg domain "$MITM_ECHO_DOMAIN" \
		'{credentials: [{name: "tok", source: {type: "inline", value: $v}}],
		  bindings: [{name: "cbv", match: {hosts: [$domain]}, auth: {type: "apiKey", name: "X-Api-Key", credential: "tok"}}]}'
}

cb_vault_revision_is() { # <id> <revision>
	local out
	out="$(server_api GET "/sandboxes/$1/credential-vault" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | jq -r '.revision // empty')" == "$2" ]]
}

# POST/PATCH over the relay; the ACK revision lands in CB_ACK_REV and the
# GET readback must converge on it before any traffic assertion.
cb_vault_commit() { # <method> <id> <body> <label>
	local ack
	ack="$(server_api "$1" "/sandboxes/$2/credential-vault" "$3")" \
		|| fail "credential-bound: $4 ($1 credential-vault) failed"
	CB_ACK_REV="$(printf '%s' "$ack" | jq -r '.revision // empty')"
	[[ "$CB_ACK_REV" =~ ^[0-9]+$ ]] || fail "credential-bound: $4 ACK carried no revision"
	wait_for "credential-bound: $4 committed (revision $CB_ACK_REV readback)" 60 cb_vault_revision_is "$2" "$CB_ACK_REV"
}

# --- process restarts (only the pool Pod hosting A and B)

# PIDs whose argv[0] (argv0 mode) or argv[0]/argv[1] basename (script mode)
# equals the target; the target rides the environment so the scanning shell
# never matches itself.
cb_pod_pids() { # <pod> <argv0|script> <target>
	kubectl -n "$RESOURCE_NS" exec "$1" -c egress -- env CBV_MATCH="$2" CBV_TARGET="$3" sh -c '
		for d in /proc/[0-9]*; do
			p=${d#/proc/}
			[ "$p" = "$$" ] && continue
			a=$(tr "\000" "\n" < "$d/cmdline" 2>/dev/null | head -n 2) || continue
			a0=$(printf "%s\n" "$a" | sed -n 1p)
			a1=$(printf "%s\n" "$a" | sed -n 2p)
			case "$CBV_MATCH" in
				argv0) [ "$a0" = "$CBV_TARGET" ] && echo "$p" ;;
				script) { [ "${a0##*/}" = "$CBV_TARGET" ] || [ "${a1##*/}" = "$CBV_TARGET" ]; } && echo "$p" ;;
			esac
		done; true' 2>/dev/null | tr '\n' ' '
}

cb_pids_replaced() { # <pod> <mode> <target> <old-pids>
	local now p
	now="$(cb_pod_pids "$1" "$2" "$3")"
	[[ -n "${now// /}" ]] || return 1
	for p in $now; do
		[[ " $4 " != *" $p "* ]] || return 1
	done
}

cb_instance_id() { # <pod>
	kubectl -n "$RESOURCE_NS" exec "$1" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/_fastlet/v1/actions/status" 2>/dev/null \
		| sed -n 's/.*"instanceId":"\([^"]*\)".*/\1/p'
}

cb_instance_changed() { # <pod> <old-instance-id>
	local now
	now="$(cb_instance_id "$1")"
	[[ -n "$now" && "$now" != "$2" ]]
}

cb_pod_mitm_pinned() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "$1" -c egress -- \
		curl -fsS -m 5 "http://127.0.0.1:18080/healthz" 2>/dev/null)" || return 1
	[[ "$(printf '%s' "$out" | sed -n 's/.*"mitmCaFingerprint":"\([0-9a-f]*\)".*/\1/p')" == "$MITM_CA_FINGERPRINT" ]]
}

cb_kill_in_pod() { # <pod> <mode> <target> <label>
	local pids
	pids="$(cb_pod_pids "$1" "$2" "$3")" || true
	[[ -n "${pids// /}" ]] || fail "credential-bound: no $4 process found in ${1} (cannot prove a restart)"
	# shellcheck disable=SC2086 # word-split PID list on purpose
	kubectl -n "$RESOURCE_NS" exec "$1" -c egress -- kill -TERM $pids >/dev/null 2>&1 \
		|| fail "credential-bound: could not signal $4 in $1"
	CB_OLD_PIDS="$pids"
}

# AC3 re-established: A injected with its value over a MITM leaf, then B
# pure passthrough; another subject's credential or a MITM leaf on B fails
# at once instead of being retried.
cb_until_ac3() { # <description> <attempts> <a-id> <a-code> <b-id> <tag>
	local attempt
	for attempt in $(seq 1 "$2"); do
		cb_guest_once "$3" "$6-a-$attempt"
		! cb_result_contradicts "$4" || fail "$1: A contradicting evidence on attempt $attempt ($(cb_result_summary))"
		if cb_result_injected "$4"; then
			cb_guest_once "$5" "$6-b-$attempt"
			! cb_result_contradicts absent || fail "$1: B contradicting evidence on attempt $attempt ($(cb_result_summary))"
			cb_result_passthrough && break
		fi
		[[ "$attempt" -lt "$2" ]] || fail "$1 (after $2 attempts: $(cb_result_summary))"
		sleep 2
	done
	pass "$1"
}

# AC6 verdict on a candidate whose passthrough probe is in CB_RESULT: 0 =
# the predecessor's registry slot was reused (same live Fastlet Pod UID, same
# guest address) by a new identity; 1 = not such a reuse (other Pod, other
# address, unknown placement). A reuse that kept the old identity fails.
cb_ac6_reused() { # <cand-id> <pred-uid> <pred-ip> <pred-fastlet-pod-uid>
	local ip uid
	cb_result_passthrough || return 1
	ip="$(printf '%s' "$CB_RESULT" | jq -r '.local_ip // empty')"
	[[ -n "$ip" && "$ip" == "$3" ]] || return 1
	cb_on_pod "$1" "$4" || return 1
	uid="$(cb_sandbox_uid "$1")" || true
	[[ -n "$uid" && "$uid" != "$2" ]] || fail "AC6: same Pod and guest address but the sandbox identity did not change"
}

# --- log scan

# Logs are first collected in full into a file outside the scanned and
# uploaded trees (CB_WORK_DIR, removed by cb_cleanup), and only then
# scanned: a matcher that exits on the first hit can never turn the
# collector's broken pipe into "clean", and a failed collection is an
# error, never a clean verdict.
cb_scan_file() { # <values-file> <file|dir> <where>: grep 0 = hit, 1 = clean, else error (cb_log_scan's lists)
	local rc=0
	grep -rqF -f "$1" "$2" || rc=$?
	case "$rc" in
		0) CB_SCAN_HITS+=" $3" ;;
		1) ;;
		*) CB_SCAN_ERRORS+=" $3(scan)" ;;
	esac
}

cb_log_scan() { # <values-file> <since-time>
	local pod pods buf="$CB_WORK_DIR/log-scan.buf" err="$CB_WORK_DIR/log-scan.err" CB_SCAN_HITS="" CB_SCAN_ERRORS=""
	pods="$(kubectl -n "$RESOURCE_NS" get pods -l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[*].metadata.name}' 2>/dev/null)" || fail "AC9: pool Pod list failed (logs not scanned)"
	[[ -n "${pods// /}" ]] || fail "AC9: no pool Pods found (logs not scanned)"
	for pod in $pods; do
		if kubectl -n "$RESOURCE_NS" logs "$pod" -c egress --since-time="$2" >"$buf" 2>"$err"; then
			cb_scan_file "$1" "$buf" "$pod"
		else
			CB_SCAN_ERRORS+=" $pod(read)"
		fi
		if kubectl -n "$RESOURCE_NS" logs "$pod" -c egress --previous >"$buf" 2>"$err"; then
			cb_scan_file "$1" "$buf" "$pod(previous)"
		elif ! grep -q 'previous terminated container .* not found' "$err"; then
			CB_SCAN_ERRORS+=" $pod(previous read)"
		fi
	done
	rm -f "$buf" "$err"
	# Both exist once the stage has logged; a missing one is a scan error.
	cb_scan_file "$1" "$WORK/run.log" env-logs
	cb_scan_file "$1" "$LOGS_DIR" env-logs
	[[ -z "$CB_SCAN_HITS" ]] || fail "credential-bound: a test credential value appears in logs:$CB_SCAN_HITS (lines withheld)"
	[[ -z "$CB_SCAN_ERRORS" ]] || fail "AC9: log collection/scan failed:$CB_SCAN_ERRORS (cannot report clean)"
}

# --- the stage

credential_bound_verify() {
	CB_TEMPLATE_ID="${1:-$TEMPLATE_ID}"
	[[ "${EGRESS_INTERCEPTION_MODE:-}" == "credential-bound" ]] \
		|| fail "credential-bound verify requires EGRESS_INTERCEPTION_MODE=credential-bound"
	# Credential values must never reach a trace.
	[[ "$-" != *x* ]] || fail "credential-bound verify refuses to run under xtrace"
	load_mitm_ca_fingerprint
	[[ -n "$MITM_CA_FINGERPRINT" ]] || die "no pinned MITM CA fingerprint (run up first)"
	CB_NET="$(kind_network)" || true
	[[ "$CB_REUSE_ATTEMPTS" =~ ^[1-9][0-9]?$ ]] || die "CB_REUSE_ATTEMPTS must be 1..99 (got: $CB_REUSE_ATTEMPTS)"
	[[ -n "$CB_NET" ]] || die "kind network not found (run up first)"
	local f
	for f in tls.crt echo.crt echo.key; do
		[[ -s "$MITM_CA_DIR/$f" ]] || die "missing $MITM_CA_DIR/$f (run up first)"
	done

	CB_SANDBOXES=()
	CB_PREV_EXIT_TRAP="$(trap -p EXIT)"
	trap 'cb_cleanup' EXIT
	mkdir -p "$GEN_DIR"
	CB_WORK_DIR="$(mktemp -d "$GEN_DIR/credential-bound.XXXXXX")"
	local since values cred_a cred_a2 cred_b code_a code_a2 code_b
	since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	values="$CB_WORK_DIR/values"
	cred_a="$(cb_fake_value a)"; cred_a2="$(cb_fake_value a-rotated)"; cred_b="$(cb_fake_value b)"
	( umask 077; printf '%s\n%s\n%s\n' "$cred_a" "$cred_a2" "$cred_b" > "$values" )
	code_a="$(cb_digest "$cred_a")"; code_a2="$(cb_digest "$cred_a2")"; code_b="$(cb_digest "$cred_b")"

	cb_origin_up
	local policy="$CB_WORK_DIR/policy.json" deny_policy="$CB_WORK_DIR/deny-policy.json"
	jq -n --arg ip "$CB_ORIGIN_IP" --arg domain "$MITM_ECHO_DOMAIN" \
		'{defaultAction: "deny", egress: [{action: "allow", target: $ip}, {action: "allow", target: $domain}]}' > "$policy"
	jq -n --arg ip "$CB_ORIGIN_IP" --arg domain "$MITM_ECHO_DOMAIN" \
		'{defaultAction: "deny", egress: [{action: "deny", target: $ip}, {action: "allow", target: $domain}]}' > "$deny_policy"

	local sbx_a sbx_b pod pod_uid
	cb_create_sandbox a "$policy"; sbx_a="$CB_LAST_ID"
	cb_placement "$sbx_a" || fail "credential-bound: A's placement (fastletPodUID) is not a live pool Pod"
	pod="$CB_POD_NAME"; pod_uid="$CB_POD_UID"
	cb_create_colocated b "$policy" "$pod_uid"; sbx_b="$CB_LAST_ID"
	log "credential-bound: A=$sbx_a and B=$sbx_b on $pod (Fastlet Pod UID $pod_uid)"

	# AC2 — never-vaulted sandboxes: origin leaf, no credential.
	cb_until "AC2: B (no vault) reaches the origin over the origin leaf, nothing injected" 60 "$sbx_b" ac2-b absent
	cb_until "AC2: A (no vault yet) likewise passes through" 60 "$sbx_a" ac2-a absent

	# AC8 — deny on the passthrough flow (B), origin proven alive per probe.
	server_api PUT "/sandboxes/$sbx_b/networkpolicy" "$(cat "$deny_policy")" >/dev/null \
		|| fail "AC8: networkpolicy deny PUT failed"
	local attempt denied=0
	for attempt in $(seq 1 30); do
		cb_guest_once "$sbx_b" "ac8-deny-$attempt"
		# The policy relay acks only after the deny is enforced, so a
		# completed request under the deny policy is counter-evidence: it
		# fails at once, never overwritten by a later denied probe.
		[[ "$CB_RC" != 0 ]] || fail "AC8: a request still reached the responder under the deny policy ($(cb_result_summary))"
		if [[ "$CB_RC" != 0 && -n "$CB_RESULT" ]] \
			&& printf '%s' "$CB_RESULT" | jq -e '.ok == false and (.error.phase == "connect" or .error.phase == "handshake")
				and .error.kind != "verify_failed"' >/dev/null; then
			cb_host_probe || fail "AC8: origin was not reachable during the denied probe (cannot attribute the denial)"
			cb_until "AC8: control sandbox A reaches the origin during the denied probe" 30 "$sbx_a" "ac8-control-$attempt" absent
			denied=1
			break
		fi
		sleep 2
	done
	[[ "$denied" == 1 ]] || fail "AC8: passthrough flow still reached the origin under the deny policy ($(cb_result_summary))"
	pass "AC8: deny blocks the passthrough flow while the origin stays reachable"
	server_api PUT "/sandboxes/$sbx_b/networkpolicy" "$(cat "$policy")" >/dev/null \
		|| fail "AC8: networkpolicy restore PUT failed"
	cb_until "AC8: restored policy reopens the passthrough flow" 60 "$sbx_b" ac8-restored absent

	# AC3 — A bound, B unbound, same host, both alive.
	cb_vault_commit POST "$sbx_a" "$(cb_vault_body "$cred_a")" "AC3 vault for A"
	local rev_a="$CB_ACK_REV"
	cb_until "AC3: A's fresh connection is MITM-terminated and the origin sees A's value" 120 "$sbx_a" ac3-a "$code_a"
	cb_sandbox_state_is "$sbx_b" Running || fail "AC3: B is not running alongside A"
	cb_until "AC3: unbound B is a pure passthrough alongside bound A" 30 "$sbx_b" ac3-b absent
	{ cb_on_pod "$sbx_a" "$pod_uid" && cb_on_pod "$sbx_b" "$pod_uid"; } || fail "AC3: A and B are no longer both on Fastlet Pod UID $pod_uid"
	pass "AC3: same Fastlet Pod — A injected over a MITM leaf, B origin leaf with no credential"

	# AC7 — mitmdump, then egress, restarted in the Pod hosting A and B.
	local old_instance
	cb_kill_in_pod "$pod" script mitmdump mitmdump
	wait_for "AC7: new mitmdump process in $pod" 90 cb_pids_replaced "$pod" script mitmdump "$CB_OLD_PIDS"
	wait_for "AC7: MITM addon pinned again in $pod" 120 cb_pod_mitm_pinned "$pod"
	cb_until_ac3 "AC7: AC3 re-established after the mitmdump restart" 120 "$sbx_a" "$code_a" "$sbx_b" ac7-mitm
	old_instance="$(cb_instance_id "$pod")" || true
	[[ -n "$old_instance" ]] || fail "AC7: no Actions instanceId on $pod before the egress restart"
	cb_kill_in_pod "$pod" argv0 /opt/opensandbox-egress/egress egress
	wait_for "AC7: egress restarted in $pod (new Actions instanceId)" 120 cb_instance_changed "$pod" "$old_instance"
	wait_for "AC7: MITM addon pinned after the egress restart in $pod" 180 cb_pod_mitm_pinned "$pod"
	cb_until_ac3 "AC7: AC3 re-established after the egress restart" 180 "$sbx_a" "$code_a" "$sbx_b" ac7-egress
	{ cb_on_pod "$sbx_a" "$pod_uid" && cb_on_pod "$sbx_b" "$pod_uid"; } || fail "AC7: A and B are no longer both on Fastlet Pod UID $pod_uid"
	pass "AC7: mitmdump and egress restarts proven and recovered (AC3 holds)"

	# AC4 — B holds a passthrough connection while it gets a binding.
	local r1 r2
	cb_hold_start "$sbx_b" hold-b
	wait_for "AC4: held connection on B established (request 1)" 60 cb_hold_read "$sbx_b" hold-b r1
	r1="$CB_RESULT"
	cb_result_passthrough || fail "AC4: held connection request 1 is not passthrough ($(cb_result_summary))"
	cb_vault_commit POST "$sbx_b" "$(cb_vault_body "$cred_b")" "AC4 vault for B"
	cb_until "AC4: B's new connection is injected with B's value" 120 "$sbx_b" ac4-new "$code_b"
	cb_hold_release "$sbx_b" hold-b
	wait_for "AC4: held connection request 2 completed" 60 cb_hold_read "$sbx_b" hold-b r2
	r2="$CB_RESULT"
	cb_hold_same_socket "$r1" "$r2" || fail "AC4: request 2 did not ride the held socket (reconnect or new socket)"
	if printf '%s' "$r2" | jq -e '.ok' >/dev/null; then
		printf '%s' "$r2" | jq -e --argjson a "$r1" '.last.origin.conn == $a.last.origin.conn
			and .last.origin.cred == "absent" and (.last.origin.credential_headers | length) == 0' >/dev/null \
			|| fail "AC4: the held connection inherited a credential or changed origin connection ($(cb_result_summary))"
		pass "AC4: held socket carried no credential after the binding; new connection injected"
	elif cb_hold_closed_with_evidence "$r2"; then
		pass "AC4: held socket closed by the peer ($(printf '%s' "$r2" | jq -r '.error.phase + "/" + .error.kind')) — never injected; new connection injected"
	else
		fail "AC4: held connection request 2 failed without closure evidence ($(printf '%s' "$r2" | jq -c '.error'))"
	fi

	# AC5 — rotation on A (bound), then removal of A's last binding.
	cb_hold_start "$sbx_a" hold-a
	wait_for "AC5: held connection on A established (request 1)" 60 cb_hold_read "$sbx_a" hold-a r1
	r1="$CB_RESULT"
	cb_result_injected "$code_a" || fail "AC5: A's held connection request 1 not injected with A's value"
	cb_vault_commit PATCH "$sbx_a" "$(jq -n --arg v "$cred_a2" --argjson rev "$rev_a" \
		'{expectedRevision: $rev, credentials: {replace: [{name: "tok", source: {type: "inline", value: $v}}]}}')" \
		"AC5 credential rotation on A"
	local rev_a2="$CB_ACK_REV" rotated=0
	for attempt in $(seq 1 30); do
		cb_guest_once "$sbx_a" "ac5-rotated-$attempt"
		cb_result_cred_is "$code_a" && fail "AC5: a new connection after the rotation ACK carried the old value"
		# Any other credential (another subject's value) is counter-evidence:
		# it fails at once, never overwritten by a later clean probe.
		! cb_result_contradicts "$code_a2" \
			|| fail "AC5: a new connection after the rotation ACK carried an unexpected credential ($(cb_result_summary))"
		if cb_result_injected "$code_a2"; then rotated=1; break; fi
		sleep 2
	done
	[[ "$rotated" == 1 ]] || fail "AC5: new connections never carried the rotated value ($(cb_result_summary))"
	cb_hold_release "$sbx_a" hold-a
	wait_for "AC5: A's held connection request 2 completed" 60 cb_hold_read "$sbx_a" hold-a r2
	r2="$CB_RESULT"
	cb_hold_same_socket "$r1" "$r2" || fail "AC5: held request 2 did not ride the held socket"
	if printf '%s' "$r2" | jq -e '.ok' >/dev/null; then
		cb_result_cred_is "$code_a" && fail "AC5: the held connection still carried the old value after the rotation ACK"
		cb_result_cred_is "$code_a2" || fail "AC5: the held connection carried neither the rotated value nor nothing expected"
		pass "AC5: rotation — new and held connections carry the new value, never the old one"
	elif cb_hold_closed_with_evidence "$r2"; then
		pass "AC5: rotation — new connections carry the new value; held socket closed by the peer ($(printf '%s' "$r2" | jq -r '.error.kind'))"
	else
		fail "AC5: held request 2 failed without closure evidence ($(printf '%s' "$r2" | jq -c '.error'))"
	fi
	# A bound socket held across the last-binding removal ACK: proven injected
	# with the rotated value first, then one request on the same socket after
	# the ACK must hit the request fence (never the origin, never a value).
	cb_hold_start "$sbx_a" hold-a2
	wait_for "AC5: held connection on A established before the removal (request 1)" 60 cb_hold_read "$sbx_a" hold-a2 r1
	r1="$CB_RESULT"
	cb_result_injected "$code_a2" || fail "AC5: A's held connection before the removal not injected with the rotated value ($(cb_result_summary))"
	cb_vault_commit PATCH "$sbx_a" "$(jq -n --argjson rev "$rev_a2" '{expectedRevision: $rev, bindings: {delete: ["cbv"]}}')" \
		"AC5 removal of A's last binding"
	cb_hold_release "$sbx_a" hold-a2
	wait_for "AC5: request 2 on the held decrypted socket completed" 60 cb_hold_read "$sbx_a" hold-a2 r2
	r2="$CB_RESULT"
	cb_hold_same_socket "$r1" "$r2" || fail "AC5: request 2 on the held decrypted socket did not ride that socket (reconnect or new socket)"
	cb_result_other_credential absent \
		&& fail "AC5: request 2 on the held decrypted socket carried a credential after the removal ACK"
	local fenced
	if fenced="$(cb_hold_fence_outcome "$r2")"; then
		pass "AC5: last-binding removal fenced the held decrypted socket — $fenced"
	elif printf '%s' "$r2" | jq -e '.last.origin | objects' >/dev/null; then
		fail "AC5: request 2 on the held decrypted socket reached the origin after the removal ACK ($(cb_result_summary))"
	else
		fail "AC5: request 2 on the held decrypted socket failed without closure evidence or a fence rejection ($(cb_result_summary))"
	fi
	local unbound=0
	for attempt in $(seq 1 15); do
		cb_guest_once "$sbx_a" "ac5-unbound-$attempt"
		cb_result_bound_leaf && fail "AC5: a new connection after the last-binding removal ACK was still MITM-terminated"
		if cb_result_passthrough; then unbound=1; break; fi
		sleep 2
	done
	[[ "$unbound" == 1 ]] || fail "AC5: A never reached the origin after the last binding was removed ($(cb_result_summary))"
	pass "AC5: last binding removed — the next new connection presents the origin leaf, nothing injected"

	# AC6 — B is still bound (AC4; A lost its last binding in AC5): re-prove
	# MITM + injection of B's value, record its identity, address and Pod,
	# kill it, then look for a new identity on the same Pod and address.
	local ip_p uid_p reused=0 sbx_c
	cb_until "AC6: predecessor B still bound — MITM leaf, origin sees B's value" 60 "$sbx_b" ac6-pred "$code_b"
	ip_p="$(printf '%s' "$CB_RESULT" | jq -r '.local_ip // empty')"
	uid_p="$(cb_sandbox_uid "$sbx_b")" || true
	[[ -n "$ip_p" && -n "$uid_p" ]] || fail "AC6: could not capture B's guest address/uid"
	cb_on_pod "$sbx_b" "$pod_uid" || fail "AC6: predecessor B is no longer on Fastlet Pod UID $pod_uid"
	log "credential-bound: AC6 predecessor B guest address $ip_p, uid $uid_p, Fastlet Pod UID $pod_uid ($pod)"
	cb_kill_sandbox "$sbx_b"
	for attempt in $(seq 1 "$CB_REUSE_ATTEMPTS"); do
		cb_create_sandbox "c$attempt" "$policy"; sbx_c="$CB_LAST_ID"
		cb_until "AC6: candidate $sbx_c passes through (no vault)" 60 "$sbx_c" "ac6-$attempt" absent
		cb_placement "$sbx_c" || true
		log "credential-bound: AC6 attempt $attempt: $sbx_c guest address $(printf '%s' "$CB_RESULT" | jq -r '.local_ip // "unknown"') on Fastlet Pod UID ${CB_POD_UID:-unknown}"
		if cb_ac6_reused "$sbx_c" "$uid_p" "$ip_p" "$pod_uid"; then
			pass "AC6: guest address $ip_p on Fastlet Pod UID $pod_uid reused by a new identity ($sbx_c) — origin leaf, no old credential"
			reused=1
			cb_kill_sandbox "$sbx_c"
			break
		fi
		cb_kill_sandbox "$sbx_c"
	done
	if [[ "$reused" == 0 ]]; then
		stage_unverified NOT_RUN "AC6 source-IP reuse on the same Fastlet Pod not observed in $CB_REUSE_ATTEMPTS create/kill attempts (B guest address $ip_p)"
	fi

	# AC9 — no fake credential value in egress or env logs.
	cb_log_scan "$values" "$since"
	pass "AC9: no test credential value in the egress logs or the env logs"

	local id
	for id in ${CB_SANDBOXES[@]+"${CB_SANDBOXES[@]}"}; do
		cb_kill_sandbox "$id"
	done
	cb_cleanup
	if [[ -n "$CB_PREV_EXIT_TRAP" ]]; then eval "$CB_PREV_EXIT_TRAP"; else trap - EXIT; fi
}

# --- standalone entry

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error credential-bound-verify' ERR
	run_stage "credential-bound interception verify (OSEP-0023)" credential_bound_verify
	stage_summary
fi
