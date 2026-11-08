# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Pure-function regression tests for Fast Sandbox network policy scopes."""

from copy import deepcopy

import pytest

from opensandbox_server.api.schema import NetworkPolicy, NetworkRule
from opensandbox_server.services.fast_sandbox.network_policy import (
    delete_policy_rules,
    merge_policy_rules,
    normalized_policy,
)


@pytest.mark.parametrize("target", [None, "", "  "])
def test_normalized_policy_preserves_wire_shape_and_port_order(target):
    policy = NetworkPolicy(
        defaultAction=" Allow ",
        egress=[
            NetworkRule(action=" Deny ", target=target, ports=[65535, 1, 443]),
            NetworkRule(action=" Allow ", target=" Example.COM "),
        ],
    )
    before = policy.model_dump()

    assert normalized_policy(policy) == {
        "defaultAction": "allow",
        "egress": [
            {"action": "deny", "target": "", "ports": [65535, 1, 443]},
            {"action": "allow", "target": "Example.COM"},
        ],
    }
    assert policy.model_dump() == before


@pytest.mark.parametrize("default_action", ["allow", "deny"])
@pytest.mark.parametrize(
    "target,ports", [("Example.COM", None), ("", [443, 80]), ("10.0.0.5", [443, 80])],
)
def test_merge_duplicate_base_scopes_first_incoming_wins(default_action, target, ports):
    base_rule: dict = {"action": "deny", "target": target}
    if ports:
        base_rule["ports"] = ports
    duplicate = deepcopy(base_rule)
    duplicate["action"] = "allow"
    duplicate["target"] = target.lower()
    if ports:
        duplicate["ports"] = list(reversed(ports))
    policy = {"defaultAction": default_action, "egress": [base_rule, duplicate]}
    before = deepcopy(policy)
    incoming_ports = list(reversed(ports)) if ports else None
    first = NetworkRule(action=" Allow ", target=target.upper(), ports=incoming_ports)
    second = NetworkRule(action="deny", target=target.lower(), ports=ports)
    rules = [first, second]
    rules_before = [r.model_dump() for r in rules]
    expected: dict = {"action": "allow", "target": target.upper()}
    if incoming_ports:
        expected["ports"] = incoming_ports

    assert merge_policy_rules(policy, rules) == {
        "defaultAction": default_action, "egress": [expected],
    }
    assert policy == before
    assert [r.model_dump() for r in rules] == rules_before


@pytest.mark.parametrize("default_action", ["allow", "deny"])
def test_merge_empty_patch_preserves_base(default_action):
    policy = {
        "defaultAction": default_action,
        "egress": [
            {"action": "deny", "target": "", "ports": [443, 80]},
            {"action": "allow", "target": "", "ports": [80, 443]},
        ],
    }
    before = deepcopy(policy)
    assert merge_policy_rules(policy, []) == before
    assert policy == before


def test_merge_keeps_unmatched_scopes_and_first_base_duplicate():
    policy = {
        "defaultAction": "allow",
        "egress": [
            {"action": "allow", "target": "Example.COM"},
            {"action": "deny", "target": "EXAMPLE.com"},
            {"action": "deny", "target": "10.0.0.5"},
            {"action": "deny", "target": "10.0.0.5", "ports": [443, 80]},
            {"action": "deny", "target": "", "ports": [443, 80]},
        ],
    }
    additions = [
        NetworkRule(action="allow", target="10.0.0.5", ports=[22]),
        NetworkRule(action="allow", ports=[22]),
        NetworkRule(action="deny", target="", ports=[22]),
    ]

    assert merge_policy_rules(policy, additions) == {
        "defaultAction": "allow",
        "egress": [
            policy["egress"][0],
            *policy["egress"][2:],
            {"action": "allow", "target": "10.0.0.5", "ports": [22]},
            {"action": "allow", "target": "", "ports": [22]},
        ],
    }


@pytest.mark.parametrize("default_action", ["allow", "deny"])
@pytest.mark.parametrize("empty_target", ["", "  "])
def test_delete_empty_target_removes_only_port_only_rules(default_action, empty_target):
    ip_rules = [
        {"action": "deny", "target": "10.0.0.5"},
        {"action": "allow", "target": "10.0.0.5", "ports": [443, 80]},
        {"action": "deny", "target": "2001:DB8::/64", "ports": [25]},
    ]
    policy = {
        "defaultAction": default_action,
        "egress": [
            {"action": "deny", "target": "", "ports": [25]},
            {"action": "allow", "target": "", "ports": [443, 80]},
            {"action": "allow", "target": "Example.COM"},
            *ip_rules,
        ],
    }
    before = deepcopy(policy)
    assert delete_policy_rules(policy, []) == policy
    assert delete_policy_rules(policy, ["missing.com"]) == policy
    kept = delete_policy_rules(policy, [empty_target, " EXAMPLE.com "])
    assert kept == {"defaultAction": default_action, "egress": ip_rules}
    assert delete_policy_rules(kept, [empty_target, "example.com"]) == kept
    assert delete_policy_rules(kept, ["10.0.0.5", "2001:db8::/64"]) == {
        "defaultAction": default_action, "egress": [],
    }
    assert policy == before
