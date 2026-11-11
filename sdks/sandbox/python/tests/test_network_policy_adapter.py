#
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
#
from __future__ import annotations

import json

import httpx
import pytest

from opensandbox.adapters.network_policy_adapter import NetworkPolicyAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import NetworkRule


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
async def test_patch_rules_delegates_to_lifecycle_patch(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls = []

    async def _fake_patch(*, client, sandbox_id, body):
        calls.append(("patch", sandbox_id, [r.to_dict() for r in body]))
        return _Resp(status_code=200, parsed=_api_policy_status())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.patch_sandbox_network_policy.asyncio_detailed",
        _fake_patch,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-1")
    await adapter.patch_rules(
        [
            NetworkRule(action="allow", target="a.com"),
            NetworkRule(action="deny", target="c.com"),
        ]
    )

    assert calls == [
        (
            "patch",
            "sbx-1",
            [
                {"action": "allow", "target": "a.com"},
                {"action": "deny", "target": "c.com"},
            ],
        )
    ]


@pytest.mark.asyncio
async def test_delete_rules_delegates_to_lifecycle_delete(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls = []

    async def _fake_delete(*, client, sandbox_id, body):
        calls.append(("delete", sandbox_id, list(body)))
        return _Resp(status_code=200, parsed=_api_policy_status())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.delete_sandbox_network_policy_rules.asyncio_detailed",
        _fake_delete,
    )

    adapter = NetworkPolicyAdapter(ConnectionConfig(), "sbx-1")
    await adapter.delete_rules(["a.com"])

    assert calls == [("delete", "sbx-1", ["a.com"])]


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
async def test_credential_vault_create_and_get_routes_to_lifecycle() -> None:
    requests: list[httpx.Request] = []

    def _handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.method == "POST":
            return httpx.Response(
                201,
                json={
                    "revision": 1,
                    "credentials": [
                        {"name": "tok", "sourceType": "inline", "revision": 1}
                    ],
                    "bindings": [
                        {
                            "name": "b",
                            "revision": 1,
                            "match": {"hosts": ["api.test"]},
                            "auth": {"type": "bearer"},
                        }
                    ],
                },
            )
        return httpx.Response(
            200,
            json={
                "revision": 1,
                "credentials": [
                    {"name": "tok", "sourceType": "inline", "revision": 1}
                ],
                "bindings": [
                    {
                        "name": "b",
                        "revision": 1,
                        "match": {"hosts": ["api.test"]},
                        "auth": {"type": "apiKey", "name": "X-API-Key"},
                    }
                ],
            },
        )

    config = ConnectionConfig(
        domain="example.com:8080",
        transport=httpx.MockTransport(_handler),
    )
    adapter = NetworkPolicyAdapter(config, "sbx-1")

    state = await adapter.create(
        credentials=[
            {"name": "tok", "source": {"type": "inline", "value": "sekrit"}}
        ],
        bindings=[
            {
                "name": "b",
                "match": {"hosts": ["api.test"]},
                "auth": {"type": "bearer", "credential": "tok"},
            }
        ],
    )

    assert [r.method for r in requests] == ["POST"]
    assert requests[0].url.path == "/v1/sandboxes/sbx-1/credential-vault"
    body = json.loads(requests[0].content)
    assert body["credentials"][0]["source"] == {"type": "inline", "value": "sekrit"}
    assert state.revision == 1
    assert state.credentials[0].source_type == "inline"

    got = await adapter.get()
    assert got.bindings[0].auth is not None
    assert got.bindings[0].auth.type == "apiKey"
    assert got.bindings[0].auth.name == "X-API-Key"


@pytest.mark.asyncio
async def test_credential_vault_patch_sends_expected_revision_and_maps_412() -> None:
    requests: list[httpx.Request] = []

    def _handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return httpx.Response(
            412,
            json={
                "code": "FSB::API_ERROR",
                "message": "credential vault revision conflict",
            },
        )

    config = ConnectionConfig(
        domain="example.com:8080",
        transport=httpx.MockTransport(_handler),
    )
    adapter = NetworkPolicyAdapter(config, "sbx-1")

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.patch(
            expected_revision=3,
            credentials={"delete": ["tok"]},
        )

    assert exc_info.value.status_code == 412
    assert "revision conflict" in str(exc_info.value)
    body = json.loads(requests[0].content)
    assert body["expectedRevision"] == 3
    assert body["credentials"] == {"delete": ["tok"]}
    assert "bindings" not in body


@pytest.mark.asyncio
async def test_credential_vault_metadata_reads_route_to_subresources() -> None:
    requests: list[httpx.Request] = []

    def _handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        path = request.url.path
        if path.endswith("/credentials/api key"):
            return httpx.Response(
                200,
                json={"name": "api key", "sourceType": "inline", "revision": 2},
            )
        if path.endswith("/credentials"):
            return httpx.Response(
                200,
                json={
                    "revision": 2,
                    "credentials": [
                        {"name": "api key", "sourceType": "inline", "revision": 2}
                    ],
                },
            )
        if path.endswith("/bindings/ext"):
            return httpx.Response(
                200,
                json={
                    "name": "ext",
                    "revision": 2,
                    "match": {"hosts": ["ext.test"]},
                    "auth": {"type": "basic"},
                },
            )
        return httpx.Response(
            200,
            json={
                "revision": 2,
                "bindings": [
                    {
                        "name": "ext",
                        "revision": 2,
                        "match": {"hosts": ["ext.test"]},
                        "auth": {"type": "basic"},
                    }
                ],
            },
        )

    config = ConnectionConfig(
        domain="example.com:8080",
        transport=httpx.MockTransport(_handler),
    )
    adapter = NetworkPolicyAdapter(config, "sbx-1")

    listing = await adapter.list_credentials()
    one = await adapter.get_credential("api key")
    binding = await adapter.get_binding("ext")

    assert [r.url.path for r in requests] == [
        "/v1/sandboxes/sbx-1/credential-vault/credentials",
        "/v1/sandboxes/sbx-1/credential-vault/credentials/api key",
        "/v1/sandboxes/sbx-1/credential-vault/bindings/ext",
    ]
    assert listing[0].name == "api key"
    assert one.revision == 2
    assert binding.match is not None
    assert binding.match.hosts == ["ext.test"]


def test_sync_credential_vault_round_trip() -> None:
    from opensandbox.config import ConnectionConfigSync
    from opensandbox.sync.adapters.network_policy_adapter import (
        NetworkPolicyAdapterSync,
    )

    requests: list[httpx.Request] = []

    def _handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        if request.method == "DELETE":
            return httpx.Response(204)
        return httpx.Response(
            200,
            json={
                "revision": 7,
                "credentials": [],
                "bindings": [],
            },
        )

    adapter = NetworkPolicyAdapterSync(
        ConnectionConfigSync(
            domain="example.com:8080",
            transport=httpx.MockTransport(_handler),
        ),
        "sbx-1",
    )

    state = adapter.patch(
        expected_revision=6,
        bindings={"add": [{"name": "n", "match": {"hosts": ["h.test"]}, "auth": {"type": "passthrough"}}]},
    )
    adapter.delete()

    assert state.revision == 7
    assert [r.method for r in requests] == ["PATCH", "DELETE"]
    assert requests[0].url.path == "/v1/sandboxes/sbx-1/credential-vault"
    assert json.loads(requests[0].content)["expectedRevision"] == 6
    assert requests[1].url.path == "/v1/sandboxes/sbx-1/credential-vault"
