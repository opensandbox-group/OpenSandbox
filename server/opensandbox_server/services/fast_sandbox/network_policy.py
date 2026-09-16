# Copyright 2026 Alibaba Group Holding Ltd.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Translate policy intent to the Fastlet Actions handler's JSON input."""

from fastapi import HTTPException

from opensandbox_server.api.schema import NetworkPolicy, NetworkRule


def normalized_policy(policy: NetworkPolicy) -> dict:
    default_action = (policy.default_action or "deny").strip().lower() or "deny"
    if default_action not in ("allow", "deny"):
        raise HTTPException(400, detail="networkPolicy.defaultAction must be allow or deny.")
    rules = []
    for rule in policy.egress:
        target = rule.target.strip()
        action = rule.action.strip().lower() or "deny"
        if action not in ("allow", "deny"):
            raise HTTPException(400, detail="networkPolicy rule action must be allow or deny.")
        if not target:
            raise HTTPException(400, detail="networkPolicy rule target cannot be empty.")
        rules.append({"action": action, "target": target})
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


def merge_policy_rules(policy: dict, rules: list[NetworkRule]) -> dict:
    """Apply sidecar PATCH merge semantics to a normalized policy dict.

    - Incoming rules take priority over existing rules with the same target
      and replace them in place.
    - Within one patch payload, the first rule for a target wins.
    - Existing rules for other targets remain; the current defaultAction is
      preserved.
    """
    incoming: dict[str, dict] = {}
    for rule in rules:
        target = rule.target.strip()
        if target and target not in incoming:
            incoming[target] = {"action": rule.action, "target": target}
    merged: list[dict] = []
    for rule in policy.get("egress") or []:
        merged.append(incoming.pop(rule["target"]) if rule["target"] in incoming else rule)
    merged.extend(incoming.values())
    return {"defaultAction": policy.get("defaultAction") or "deny", "egress": merged}


def delete_policy_rules(policy: dict, targets: list[str]) -> dict:
    """Drop rules by target (idempotent), preserving the current defaultAction."""
    removed = {t.strip() for t in targets}
    kept = [rule for rule in policy.get("egress") or [] if rule["target"] not in removed]
    return {"defaultAction": policy.get("defaultAction") or "deny", "egress": kept}
