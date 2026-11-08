# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Translate policy intent to the Fastlet Actions handler's JSON input."""

from fastapi import HTTPException

from opensandbox_server.api.schema import NetworkPolicy, NetworkRule
from opensandbox_server.services.validators import normalize_network_policy_action


def normalized_policy(policy: NetworkPolicy) -> dict:
    default_action = normalize_network_policy_action(policy.default_action, "networkPolicy.defaultAction")
    rules = []
    for rule in policy.egress:
        target = (rule.target or "").strip()
        action = normalize_network_policy_action(rule.action, "networkPolicy rule action")
        if not target and not rule.ports:
            raise HTTPException(400, detail="networkPolicy rule requires target or ports.")
        normalized: dict = {"action": action, "target": target}
        if rule.ports:
            normalized["ports"] = list(rule.ports)
        rules.append(normalized)
    return {"defaultAction": default_action, "egress": rules}


def policy_status(policy: dict | None) -> dict:
    # The Actions handler resets a removed/empty binding to deny-first.
    policy = policy or {"defaultAction": "deny", "egress": []}
    mode = (
        "enforcing"
        if policy.get("egress")
        else ("allow_all" if policy.get("defaultAction") == "allow" else "deny_all")
    )
    return {"status": "ok", "mode": mode, "policy": policy}


def _rule_key(rule: dict) -> tuple[str, tuple[int, ...]]:
    return (rule.get("target") or "").lower(), tuple(sorted(rule.get("ports") or []))


def merge_policy_rules(policy: dict, rules: list[NetworkRule]) -> dict:
    """Apply sidecar PATCH merge semantics to a normalized policy dict.

    - Incoming rules replace every existing rule with the same
      (case-insensitive target, sorted ports) scope, at the first one's position.
    - Within one patch payload, the first rule for a scope wins.
    - Existing rules for other scopes remain untouched, duplicates included;
      the current defaultAction is preserved.
    """
    incoming: dict[tuple[str, tuple[int, ...]], dict] = {}
    for rule in normalized_policy(NetworkPolicy(egress=rules))["egress"]:
        incoming.setdefault(_rule_key(rule), rule)
    merged: list[dict] = []
    replaced: set[tuple[str, tuple[int, ...]]] = set()
    for rule in policy.get("egress") or []:
        key = _rule_key(rule)
        if key not in incoming:
            merged.append(rule)
        elif key not in replaced:
            merged.append(incoming[key])
            replaced.add(key)
    merged.extend(rule for key, rule in incoming.items() if key not in replaced)
    return {"defaultAction": policy.get("defaultAction") or "deny", "egress": merged}


def delete_policy_rules(policy: dict, targets: list[str]) -> dict:
    """Drop targets case-insensitively; an empty target drops all port-only rules."""
    removed = {t.strip().lower() for t in targets}
    kept = [
        rule for rule in policy.get("egress") or []
        if (rule.get("target") or "").lower() not in removed
    ]
    return {"defaultAction": policy.get("defaultAction") or "deny", "egress": kept}
