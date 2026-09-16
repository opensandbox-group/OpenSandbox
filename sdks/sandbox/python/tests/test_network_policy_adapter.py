#
# Copyright 2026 Alibaba Group Holding Ltd.
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
#
from __future__ import annotations

import pytest

from opensandbox.adapters.network_policy_adapter import (
    NetworkPolicyAdapter,
    delete_network_policy_rules,
    merge_network_policy_rules,
)
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException, SandboxException
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule


class _Resp:
    def __init__(self, *, status_code: int, parsed) -> None:
        self.status_code = status_code
        self.parsed = parsed


def _api_policy_status(
    default_action: str = "deny",
    rules: list[tuple[str, str]] | None = None,
):
    from opensandbox.api.lifecycle.models.network_policy import NetworkPolicy
    from opensandbox.api.lifecycle.models.network_policy_default_action import (
        NetworkPolicyDefaultAction,
    )
    from opensandbox.api.lifecycle.models.network_rule import NetworkRule
    from opensandbox.api.lifecycle.models.network_rule_action import (
        NetworkRuleAction,
    )
    from opensandbox.api.lifecycle.models.policy_status_response import (
        PolicyStatusResponse,
    )

    return PolicyStatusResponse(
        status="ok",
        policy=NetworkPolicy(
            default_action=NetworkPolicyDefaultAction(default_action),
            egress=[
                NetworkRule(action=NetworkRuleAction(action), target=target)
                for action, target in (rules or [])
            ],
        ),
    )


def test_merge_rules_first_wins_and_preserves_default_action() -> None:
    current = NetworkPolicy.model_validate(
        {
            "defaultAction": "allow",
            "egress": [
                {"action": "deny", "target": "a.com"},
                {"action": "allow", "target": "b.com"},
            ],
        }
    )
    merged = merge_network_policy_rules(
        current,
        [
            NetworkRule(action="allow", target="a.com"),
            NetworkRule(action="deny", target="c.com"),
            NetworkRule(action="deny", target="c.com"),
        ],
    )

    assert merged.default_action == "allow"
    assert [(r.action, r.target) for r in merged.egress or []] == [
        ("allow", "a.com"),
        ("allow", "b.com"),
        ("deny", "c.com"),
    ]


def test_delete_rules_is_idempotent() -> None:
    current = NetworkPolicy.model_validate(
        {
            "defaultAction": "deny",
            "egress": [{"action": "allow", "target": "a.com"}],
        }
    )
    deleted = delete_network_policy_rules(current, ["a.com", "missing.com"])
    assert deleted.default_action == "deny"
    assert deleted.egress == []


@pytest.mark.asyncio
async def test_get_policy_routes_to_lifecycle_networkpolicy(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, sandbox_id):
        captured["sandbox_id"] = sandbox_id
        return _Resp(
            status_code=200,
            parsed=_api_policy_status(rules=[("allow", "pypi.org")]),
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandbox_network_policy.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(domain="example.com:8080"), "sbx-1")
    policy = await adapter.get_policy()

    assert captured["sandbox_id"] == "sbx-1"
    assert policy.default_action == "deny"
    assert [(r.action, r.target) for r in policy.egress or []] == [
        ("allow", "pypi.org")
    ]


@pytest.mark.asyncio
async def test_patch_rules_reads_then_replaces(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_get(*, client, sandbox_id):
        return _Resp(
            status_code=200,
            parsed=_api_policy_status(
                default_action="allow",
                rules=[("deny", "a.com"), ("allow", "b.com")],
            ),
        )

    async def _fake_put(*, client, sandbox_id, body):
        captured["put_body"] = body.to_dict()
        return _Resp(status_code=200, parsed=_api_policy_status())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandbox_network_policy.asyncio_detailed",
        _fake_get,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.replace_sandbox_network_policy.asyncio_detailed",
        _fake_put,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-1")
    await adapter.patch_rules(
        [
            NetworkRule(action="allow", target="a.com"),
            NetworkRule(action="deny", target="c.com"),
        ]
    )

    assert captured["put_body"] == {
        "defaultAction": "allow",
        "egress": [
            {"action": "allow", "target": "a.com"},
            {"action": "allow", "target": "b.com"},
            {"action": "deny", "target": "c.com"},
        ],
    }


@pytest.mark.asyncio
async def test_delete_rules_reads_then_replaces(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_get(*, client, sandbox_id):
        return _Resp(
            status_code=200,
            parsed=_api_policy_status(rules=[("allow", "a.com"), ("allow", "b.com")]),
        )

    async def _fake_put(*, client, sandbox_id, body):
        captured["put_body"] = body.to_dict()
        return _Resp(status_code=200, parsed=_api_policy_status())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandbox_network_policy.asyncio_detailed",
        _fake_get,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.replace_sandbox_network_policy.asyncio_detailed",
        _fake_put,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-1")
    await adapter.delete_rules(["a.com"])

    assert captured["put_body"] == {
        "defaultAction": "deny",
        "egress": [{"action": "allow", "target": "b.com"}],
    }


@pytest.mark.asyncio
async def test_policy_read_error_maps_to_sandbox_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.models.error_response import ErrorResponse

    async def _fake_get(*, client, sandbox_id):
        return _Resp(
            status_code=404,
            parsed=ErrorResponse(code="NOT_FOUND", message="sandbox not found"),
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandbox_network_policy.asyncio_detailed",
        _fake_get,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-missing")
    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.get_policy()

    assert exc_info.value.status_code == 404


@pytest.mark.asyncio
async def test_credential_vault_unsupported_for_template_sandboxes() -> None:
    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-1")
    with pytest.raises(SandboxException, match="Credential Vault"):
        await adapter.create(credentials=[], bindings=[])
    with pytest.raises(SandboxException, match="Credential Vault"):
        await adapter.get()
    with pytest.raises(SandboxException, match="Credential Vault"):
        await adapter.list_credentials()
