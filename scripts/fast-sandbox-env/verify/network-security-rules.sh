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
# The pool manifests mount the operator rule files (the optional
# egress-network-security-rules ConfigMap: deny.always = 10.0.0.0/8 +
# 172.30.0.0/24, allow.always = example.com, reachable.ports.cap = TCP
# 53/80/443) into every fastlet egress container at /var/egress/rules.
# Proven: kernel-side (mount, startup logs, port-cap complement in the
# dispatch chain, subject deny set), then live — a default-deny sandbox
# resolves example.com but not github.com (allow.always), and a
# default-allow sandbox reaches the TLS echo endpoint while the Kubernetes
# API ClusterIP and a non-whitelisted node port stay dropped (deny.always
# and the port cap beat the user policy).
#
# Runs as part of `integration.sh up` (the entry chains every verify script)
# or standalone against a live stack from `env.sh up`.

source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/lib.sh"

MITM_ECHO_DOMAIN="${MITM_ECHO_DOMAIN:-echo.test}"
MITM_ECHO_CONTAINER="${MITM_ECHO_CONTAINER:-fast-sandbox-env-mitm-echo}"

network_security_rules_files_mounted() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$1" -c egress -- ls /var/egress/rules 2>/dev/null)" || return 1
	[[ "$out" == *"deny.always"* && "$out" == *"allow.always"* && "$out" == *"reachable.ports.cap"* ]]
}

network_security_rules_startup_logged() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" logs "pod/$1" -c egress 2>/dev/null)" || return 1
	[[ "$out" == *"always-deny rule(s) from /var/egress/rules/deny.always"* \
		&& "$out" == *"always-allow rule(s) from /var/egress/rules/allow.always"* \
		&& "$out" == *"loaded port floor from /var/egress/rules/reachable.ports.cap"* ]]
}

# reachable.ports.cap whitelists TCP 53/80/443; the complement must show up
# as one drop rule in the master dispatch chain.
network_security_rules_floor_chain() { # <pod>
	local out
	out="$(kubectl -n "$RESOURCE_NS" exec "pod/$1" -c egress -- \
		nft list chain inet opensandbox-fast-sandbox dispatch 2>/dev/null)" || return 1
	[[ "$out" == *"tcp dport {"* && "$out" == *"444-65535"* ]]
}

network_security_rules_subject_deny_set() { # <sandbox-id>
	local out subj pod
	subj="subj_s_$(printf '%s' "$1" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9_' '_')_deny_v4"
	# POOL_MIN=2: the sandbox may live on any Fastlet — scan them all.
	for pod in $(kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" -o name 2>/dev/null); do
		out="$(kubectl -n "$RESOURCE_NS" exec "${pod#pod/}" -c egress -- \
			nft list set inet opensandbox-fast-sandbox "$subj" 2>/dev/null)" || continue
		[[ "$out" == *"10.0.0.0/8"* && "$out" == *"172.30.0.0/24"* ]] && return 0
	done
	return 1
}

# network_security_rules_guest_dns <sandbox-id> <must-resolve>: guest curl
# through the default-deny sandbox; the exit code carries the verdict.
network_security_rules_guest_dns() { # <sandbox-id> <must-resolve:yes|no>
	local host; [[ "$2" == "yes" ]] && host="example.com" || host="github.com"
	if [[ "$2" == "yes" ]]; then
		osb_api command run "$1" -- curl -fsS -m 15 -o /dev/null "https://$host" >/dev/null 2>&1
	else
		! osb_api command run "$1" -- curl -fsS -m 15 -o /dev/null "https://$host" >/dev/null 2>&1
	fi
}

# network_security_rules_guest_dial <sandbox-id> <must-fail> <url> <resolve>:
# a deny/port-cap drop surfaces as a curl timeout (exit 28); any fast answer
# means the packet got through.
network_security_rules_guest_dial() { # <sandbox-id> <must-fail> <url> <resolve>
	local id="$1" must_fail="$2" url="$3" resolve="$4"
	local probe=(curl -k -sS -m 3 -o /dev/null)
	[[ -n "$resolve" ]] && probe+=(--resolve "$resolve")
	if [[ "$must_fail" == "yes" ]]; then
		! osb_api command run "$id" -- "${probe[@]}" "$url" >/dev/null 2>&1
	else
		osb_api command run "$id" -- "${probe[@]}" "$url" >/dev/null 2>&1
	fi
}

network_security_rules_verify() {
	local pod id payload response echo_ip cluster_ip node_ip
	pod="$(kubectl -n "$RESOURCE_NS" get pods \
		-l "app=sandbox-fastlet,fast-sandbox.io/pool=$POOL_NAME" \
		-o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	[[ -n "$pod" ]] || fail "no fastlet pod found (pool not up)"
	wait_for "rule files mounted into the egress container" 30 network_security_rules_files_mounted "$pod"
	wait_for "rule files parsed at egress startup" 30 network_security_rules_startup_logged "$pod"
	wait_for "port-cap complement in the master dispatch chain" 30 network_security_rules_floor_chain "$pod"

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
	wait_for "example.com answers through allow.always (default-deny sandbox)" 20 \
		network_security_rules_guest_dns "$id" yes
	wait_for "github.com NXDOMAIN under the default-deny sandbox" 20 \
		network_security_rules_guest_dns "$id" no
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
		network_security_rules_guest_dial "$id" no "https://$MITM_ECHO_DOMAIN/echo" "$MITM_ECHO_DOMAIN:443:$echo_ip"
	wait_for "kubernetes ClusterIP $cluster_ip:443 dropped despite default-allow (deny.always wins)" 20 \
		network_security_rules_guest_dial "$id" yes "https://$cluster_ip/"
	wait_for "kind node $node_ip:10250 dropped (port not in reachable.ports.cap)" 20 \
		network_security_rules_guest_dial "$id" yes "https://$node_ip:10250/"
	pass "live traffic: deny.always beats default-allow; port cap holds on the real data path"
	osb_api sandbox kill "$id" >/dev/null 2>&1 || fail "sandbox $id kill failed"
	wait_for "default-allow sandbox deleted" 120 verify_sandbox_gone
}

# --- standalone entry ----------------------------------------------------------------

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	resolve_osb
	require_template_id
	trap 'on_error verify-network-security-rules' ERR
	run_stage "egress network-security-rules verify (configmap -> kernel -> live traffic)" network_security_rules_verify
fi
