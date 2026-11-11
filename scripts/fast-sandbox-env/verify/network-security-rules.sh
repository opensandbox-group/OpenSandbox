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

# verify/network-security-rules.sh — operator network security rules verify
# (configmap -> hot load -> kernel -> live traffic)
#
# The pool manifests mount /var/egress/rules from the optional
# egress-network-security-rules ConfigMap. This verify owns the baseline
# lifecycle: it applies the ConfigMap (deny.always = 10.0.0.0/8 +
# 172.30.0.0/24, allow.always = example.com, reachable.ports.cap = TCP
# 53/80/443), proves enforcement, then removes it and waits for the reload
# to unload the rules — so the restrictive baseline never leaks into the
# other verify stages or the SDK e2e suites.
#
# Proven while the baseline is active: kernel-side (mount, port-cap
# complement in the dispatch chain, subject deny set), then live — a
# default-deny sandbox resolves example.com but not github.com
# (allow.always), and a default-allow sandbox reaches the TLS echo endpoint
# while the Kubernetes API ClusterIP and a non-whitelisted node port stay
# dropped (deny.always and the port cap beat the user policy).
#
# Runs as part of `integration.sh up` (the entry chains every verify script)
# or standalone against a live stack from `env.sh up`.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

MITM_ECHO_DOMAIN="${MITM_ECHO_DOMAIN:-echo.test}"
MITM_ECHO_CONTAINER="${MITM_ECHO_CONTAINER:-fast-sandbox-env-mitm-echo}"
RULES_CONFIGMAP="egress-network-security-rules"

network_security_rules_files_mounted() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$1" -c egress -- ls /var/egress/rules 2>/dev/null)" || return 1
	[[ "$out" == *"deny.always"* && "$out" == *"allow.always"* && "$out" == *"reachable.ports.cap"* ]]
}

network_security_rules_floor_chain() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$1" -c egress -- \
		nft list chain inet opensandbox-fast-sandbox dispatch 2>/dev/null)" || return 1
	[[ "$out" == *"tcp dport {"* && "$out" == *"444-65535"* ]]
}

network_security_rules_floor_gone() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$1" -c egress -- \
		nft list chain inet opensandbox-fast-sandbox dispatch 2>/dev/null)" || return 1
	[[ "$out" != *"tcp dport {"* ]]
}

network_security_rules_subject_deny_set() { # <sandbox-id>
	local uid out subj pod
	uid="$(kubectl -n "$RESOURCE_NS" get sandbox "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null)" || return 1
	[[ -n "$uid" ]] || return 1
	# the egress derives the nft subject from the Sandbox UID, not the API id
	subj="subj_s_$(printf '%s' "$uid" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_' '_')_deny_v4"
	# POOL_MIN=2: the sandbox may live on any Fastlet — scan them all.
	for pod in $(kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o name 2>/dev/null); do
		out="$(kubectl -n "$RESOURCE_NS" exec "${pod#pod/}" -c egress -- \
			nft list set inet opensandbox-fast-sandbox "$subj" 2>/dev/null)" || continue
		[[ "$out" == *"10.0.0.0/8"* && "$out" == *"172.30.0.0/24"* ]] && return 0
	done
	return 1
}

# network_security_rules_guest_curl <sandbox-id> <expect-exit> <curl args...>:
# run curl in the guest and assert its exact exit code — the CLI/execd
# transport failing produces no CURL_EXIT line (probe fails), a
# deny/port-cap drop surfaces as curl timeout (28), an NXDOMAIN as 6, and
# only a real answer as 0.
network_security_rules_guest_curl() { # <sandbox-id> <expect-exit> <curl args...>
	local id="$1" expect="$2"
	shift 2
	local out
	out="$(osb_api command run "$id" -- \
		sh -c 'curl "$@" 2>/dev/null; printf "CURL_EXIT=%s" "$?"' sh "$@" 2>/dev/null)" || return 1
	[[ "$out" == *"CURL_EXIT=$expect" ]]
}

network_security_rules_verify() {
	local pod id payload response echo_ip cluster_ip node_ip
	pod="$(kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	[[ -n "$pod" ]] || fail "no fastlet pod found (pool not up)"
	kubectl apply -f "$SCRIPT_DIR/manifests/pool/egress-network-security-rules-configmap.yaml" >/dev/null \
		|| fail "failed to apply the network-security-rules baseline"
	# Best-effort cleanup on any exit: a failed run must not leave the
	# restrictive baseline active for the later verify stages / SDK suites.
	trap 'kubectl -n "$RESOURCE_NS" delete configmap "$RULES_CONFIGMAP" --ignore-not-found >/dev/null 2>&1 || true' EXIT

	# The ConfigMap lands on already-running pods: kubelet syncs the files
	# within its sync period and the next reload tick (≤60s) picks them up —
	# no egress restart, hence no startup-load logs; the kernel probes below
	# are the parsed proof.
	wait_for "rule files mounted into the egress container" 60 network_security_rules_files_mounted "$pod"
	wait_for "port-cap complement in the master dispatch chain" 60 network_security_rules_floor_chain "$pod"

	# Live probes need the TLS echo endpoint (positive control) and the
	# cluster endpoints the deny checks target.
	echo_ip="$(docker inspect -f \
		'{{range $k,$v := .NetworkSettings.Networks}}{{$v.IPAddress}}{{end}}' \
		"$MITM_ECHO_CONTAINER" 2>/dev/null || true)"
	[[ -n "$echo_ip" ]] || fail "TLS echo container not running (run up first)"
	cluster_ip="$(kubectl -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
	[[ -n "$cluster_ip" ]] || fail "could not read the kubernetes default ClusterIP"
	node_ip="$(kubectl get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null)"
	[[ -n "$node_ip" ]] || fail "could not read a kind node InternalIP"

	# Sandbox 1 — default-deny: the overlay is the only opener.
	payload="$(jq -n --arg tpl "$TEMPLATE_ID" \
		'{templateId: $tpl, timeout: 3600, metadata: {origin: "fast-sandbox-env-verify-nsr"}, networkPolicy: {defaultAction: "deny"}}')"
	response="$(server_api POST "/sandboxes" "$payload")" \
		|| fail "default-deny sandbox create failed: $(printf '%s' "$payload" | head -c 200)"
	id="$(printf '%s' "$response" | jq -r '.id')"
	[[ -n "$id" && "$id" != "null" ]] || fail "default-deny sandbox create carried no id"
	VERIFY_ID="$id"
	wait_for "default-deny sandbox Running" 150 sandbox_running
	wait_for "deny CIDRs in the subject deny set" 60 network_security_rules_subject_deny_set "$id"
	wait_for "example.com answers through allow.always (CURL_EXIT=0)" 20 \
		network_security_rules_guest_curl "$id" 0 -fsS -m 15 -o /dev/null https://example.com
	wait_for "github.com NXDOMAIN under the default-deny policy (CURL_EXIT=6)" 20 \
		network_security_rules_guest_curl "$id" 6 -fsS -m 15 -o /dev/null https://github.com
	pass "live traffic: allow.always opens exactly example.com (DNS layer)"
	osb_api sandbox kill "$id" >/dev/null 2>&1 || fail "sandbox $id kill failed"
	wait_for "default-deny sandbox deleted" 120 verify_sandbox_gone

	# Sandbox 2 — default-allow: any drop below is the operator overlay.
	payload="$(jq -n --arg tpl "$TEMPLATE_ID" \
		'{templateId: $tpl, timeout: 3600, metadata: {origin: "fast-sandbox-env-verify-nsr"}, networkPolicy: {defaultAction: "allow"}}')"
	response="$(server_api POST "/sandboxes" "$payload")" \
		|| fail "default-allow sandbox create failed: $(printf '%s' "$payload" | head -c 200)"
	id="$(printf '%s' "$response" | jq -r '.id')"
	[[ -n "$id" && "$id" != "null" ]] || fail "default-allow sandbox create carried no id"
	VERIFY_ID="$id"
	wait_for "default-allow sandbox Running" 150 sandbox_running
	wait_for "TLS echo endpoint answers (443 whitelisted, outside deny CIDRs)" 20 \
		network_security_rules_guest_curl "$id" 0 -fsS -m 8 --resolve "$MITM_ECHO_DOMAIN:443:$echo_ip" "https://$MITM_ECHO_DOMAIN/echo"
	wait_for "kubernetes ClusterIP $cluster_ip:443 dropped despite default-allow (CURL_EXIT=28)" 20 \
		network_security_rules_guest_curl "$id" 28 -k -sS -m 3 -o /dev/null "https://$cluster_ip/"
	wait_for "kind node $node_ip:10250 dropped (port not in reachable.ports.cap, CURL_EXIT=28)" 20 \
		network_security_rules_guest_curl "$id" 28 -k -sS -m 3 -o /dev/null "https://$node_ip:10250/"
	pass "live traffic: deny.always beats default-allow; port cap holds on the real data path"
	osb_api sandbox kill "$id" >/dev/null 2>&1 || fail "sandbox $id kill failed"
	wait_for "default-allow sandbox deleted" 120 verify_sandbox_gone

	# Restore the pre-verify environment: remove the baseline and wait for
	# the hot reload to unload it, so later stages see no platform rules.
	kubectl -n "$RESOURCE_NS" delete configmap "$RULES_CONFIGMAP" --ignore-not-found >/dev/null \
		|| fail "baseline configmap delete failed"
	wait_for "baseline unloaded after verify (port-cap complement gone)" 45 network_security_rules_floor_gone "$pod"
	pass "baseline removed; environment restored"
	trap - EXIT
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error verify-network-security-rules' ERR
	run_stage "egress network-security-rules verify (configmap -> kernel -> live traffic)" network_security_rules_verify
fi
