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
from typing import Any

import httpx
import pytest

from opensandbox.adapters.diagnostics_adapter import DiagnosticsAdapter
from opensandbox.adapters.network_policy_adapter import NetworkPolicyAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.internal import lifecycle_metrics
from opensandbox.internal.lifecycle_headers import (
    LIFECYCLE_API_KEY_HEADER,
    apply_default_lifecycle_api_key,
    has_header,
)
from opensandbox.models.sandboxes import SandboxFilter
from opensandbox.sync.adapters.diagnostics_adapter import DiagnosticsAdapterSync
from opensandbox.sync.adapters.network_policy_adapter import NetworkPolicyAdapterSync
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync

_EXPLICIT_HEADER_VARIANTS = [
    None,
    "OPEN-SANDBOX-API-KEY",
    "open-sandbox-api-key",
    "Open-Sandbox-Api-Key",
]


def _expected_api_key_values(
    key_source: str,
    explicit_header: str | None,
    explicit_value: str,
) -> list[str]:
    if explicit_header is not None:
        return [explicit_value]
    if key_source == "none":
        return []
    return [f"{key_source}-key"]


def test_lifecycle_header_helpers() -> None:
    original = {"X-Request-ID": "req-1"}
    with_default = apply_default_lifecycle_api_key(original, "default-key")
    assert with_default == {
        "X-Request-ID": "req-1",
        LIFECYCLE_API_KEY_HEADER: "default-key",
    }
    assert original == {"X-Request-ID": "req-1"}

    assert apply_default_lifecycle_api_key(original, "") == original
    assert apply_default_lifecycle_api_key(original, None) == original

    for header_name in (
        "OPEN-SANDBOX-API-KEY",
        "open-sandbox-api-key",
        "Open-Sandbox-Api-Key",
    ):
        explicit = {"X-Request-ID": "req-1", header_name: ""}
        assert has_header(explicit, LIFECYCLE_API_KEY_HEADER)
        resolved = apply_default_lifecycle_api_key(explicit, "default-key")
        assert resolved == explicit
        assert httpx.Headers(resolved).get_list(LIFECYCLE_API_KEY_HEADER) == [""]


@pytest.mark.parametrize(
    "target",
    ["sandboxes", "diagnostics", "network_policy", "credential_vault"],
)
@pytest.mark.parametrize("synchronous", [False, True], ids=["async", "sync"])
@pytest.mark.parametrize("key_source", ["configured", "environment", "none"])
@pytest.mark.parametrize("explicit_header", _EXPLICIT_HEADER_VARIANTS)
@pytest.mark.parametrize("explicit_value", ["explicit-key", ""])
async def test_lifecycle_api_key_header_precedence(
    monkeypatch: pytest.MonkeyPatch,
    target: str,
    synchronous: bool,
    key_source: str,
    explicit_header: str | None,
    explicit_value: str,
) -> None:
    monkeypatch.delenv("OPEN_SANDBOX_API_KEY", raising=False)
    if key_source == "environment":
        monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "environment-key")
    headers = {"X-Request-ID": "test-request", "OPEN-SANDBOX-CLIENT-IP": "127.0.0.1"}
    if explicit_header is not None:
        headers[explicit_header] = explicit_value
    captured: list[httpx.Request] = []

    def respond(request: httpx.Request) -> httpx.Response:
        captured.append(request)
        if target == "sandboxes":
            return httpx.Response(
                200,
                json={
                    "items": [],
                    "pagination": {
                        "page": 1,
                        "pageSize": 20,
                        "totalItems": 0,
                        "totalPages": 0,
                        "hasNextPage": False,
                    },
                },
            )
        if target == "diagnostics":
            return httpx.Response(
                200,
                json={
                    "sandboxId": "sbx-1",
                    "backendType": "kubernetes",
                    "backendIdentifiers": {
                        "namespace": "tenant-ns",
                        "podName": "pod-1",
                    },
                },
            )
        if target == "network_policy":
            return httpx.Response(
                200,
                json={
                    "status": "ok",
                    "policy": {"defaultAction": "deny", "egress": []},
                },
            )
        return httpx.Response(
            200,
            json={"revision": 1, "credentials": [], "bindings": []},
        )

    config_type = ConnectionConfigSync if synchronous else ConnectionConfig
    config = config_type(
        domain="lifecycle.example.test",
        api_key="configured-key" if key_source == "configured" else None,
        headers=headers,
        transport=httpx.MockTransport(respond),
    )
    original_headers = dict(config.headers)

    if synchronous:
        assert isinstance(config, ConnectionConfigSync)
        if target == "sandboxes":
            adapter = SandboxesAdapterSync(config)
            try:
                adapter.list_sandboxes(SandboxFilter())
            finally:
                adapter._httpx_client.close()
            expected_path = "/v1/sandboxes"
        elif target == "diagnostics":
            adapter = DiagnosticsAdapterSync(config)
            try:
                adapter.get_context("sbx-1")
            finally:
                adapter._httpx_client.close()
            expected_path = "/v1/sandboxes/sbx-1/diagnostics/context"
        else:
            adapter = NetworkPolicyAdapterSync(config, "sbx-1")
            try:
                if target == "network_policy":
                    adapter.get_policy()
                    expected_path = "/v1/sandboxes/sbx-1/networkpolicy"
                else:
                    adapter.get()
                    expected_path = "/v1/sandboxes/sbx-1/credential-vault"
            finally:
                adapter._httpx_client.close()
    else:
        assert isinstance(config, ConnectionConfig)
        if target == "sandboxes":
            adapter = SandboxesAdapter(config)
            try:
                await adapter.list_sandboxes(SandboxFilter())
            finally:
                await adapter._httpx_client.aclose()
            expected_path = "/v1/sandboxes"
        elif target == "diagnostics":
            adapter = DiagnosticsAdapter(config)
            try:
                await adapter.get_context("sbx-1")
            finally:
                await adapter._httpx_client.aclose()
            expected_path = "/v1/sandboxes/sbx-1/diagnostics/context"
        else:
            adapter = NetworkPolicyAdapter(config, "sbx-1")
            try:
                if target == "network_policy":
                    await adapter.get_policy()
                    expected_path = "/v1/sandboxes/sbx-1/networkpolicy"
                else:
                    await adapter.get()
                    expected_path = "/v1/sandboxes/sbx-1/credential-vault"
            finally:
                await adapter._httpx_client.aclose()

    assert len(captured) == 1
    assert captured[0].url.path == expected_path
    expected = _expected_api_key_values(key_source, explicit_header, explicit_value)
    assert captured[0].headers.get_list("OPEN-SANDBOX-API-KEY") == expected
    assert captured[0].headers["X-Request-ID"] == "test-request"
    assert config.headers == original_headers


@pytest.mark.parametrize("synchronous", [False, True], ids=["async", "sync"])
@pytest.mark.parametrize("key_source", ["configured", "environment", "none"])
@pytest.mark.parametrize("explicit_header", _EXPLICIT_HEADER_VARIANTS)
@pytest.mark.parametrize("explicit_value", ["explicit-key", ""])
async def test_lifecycle_metrics_api_key_header_precedence(
    monkeypatch: pytest.MonkeyPatch,
    synchronous: bool,
    key_source: str,
    explicit_header: str | None,
    explicit_value: str,
) -> None:
    monkeypatch.delenv("OPEN_SANDBOX_API_KEY", raising=False)
    if key_source == "environment":
        monkeypatch.setenv("OPEN_SANDBOX_API_KEY", "environment-key")
    headers = {"X-Request-ID": "test-request", "User-agent": "custom-metrics-ua"}
    if explicit_header is not None:
        headers[explicit_header] = explicit_value
    captured: list[httpx.Request] = []

    def respond(request: httpx.Request) -> httpx.Response:
        captured.append(request)
        return httpx.Response(204)

    real_client = httpx.Client
    real_async_client = httpx.AsyncClient
    transport = httpx.MockTransport(respond)

    if synchronous:
        monkeypatch.setattr(
            lifecycle_metrics.httpx,
            "Client",
            lambda **kwargs: real_client(transport=transport, **kwargs),
        )
        config = ConnectionConfigSync(
            domain="lifecycle.example.test",
            api_key="configured-key" if key_source == "configured" else None,
            headers=headers,
        )
        original_headers = dict(config.headers)
        payload: dict[str, Any] = {"eventType": "sandbox.create", "success": True}
        lifecycle_metrics._post_sync(config, payload)
    else:
        monkeypatch.setattr(
            lifecycle_metrics.httpx,
            "AsyncClient",
            lambda **kwargs: real_async_client(transport=transport, **kwargs),
        )
        config = ConnectionConfig(
            domain="lifecycle.example.test",
            api_key="configured-key" if key_source == "configured" else None,
            headers=headers,
        )
        original_headers = dict(config.headers)
        payload = {"eventType": "sandbox.create", "success": True}
        await lifecycle_metrics._post_async(config, payload)

    assert len(captured) == 1
    assert captured[0].url.path == "/v1/metrics/events"
    expected = _expected_api_key_values(key_source, explicit_header, explicit_value)
    assert captured[0].headers.get_list("OPEN-SANDBOX-API-KEY") == expected
    assert captured[0].headers["X-Request-ID"] == "test-request"
    assert captured[0].headers.get_list("User-Agent") == ["custom-metrics-ua"]
    assert config.headers == original_headers
