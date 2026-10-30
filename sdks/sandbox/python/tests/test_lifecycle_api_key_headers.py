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
import httpx
import pytest

from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig, ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxFilter
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync


@pytest.mark.parametrize("synchronous", [False, True], ids=["async", "sync"])
@pytest.mark.parametrize("key_source", ["configured", "environment", "none"])
@pytest.mark.parametrize(
    "explicit_header",
    [None, "OPEN-SANDBOX-API-KEY", "open-sandbox-api-key", "Open-Sandbox-Api-Key"],
)
@pytest.mark.parametrize("explicit_value", ["explicit-key", ""])
async def test_lifecycle_api_key_header_precedence(
    monkeypatch: pytest.MonkeyPatch,
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

    config_type = ConnectionConfigSync if synchronous else ConnectionConfig
    config = config_type(
        domain="lifecycle.example.test",
        api_key="configured-key" if key_source == "configured" else None,
        headers=headers,
        transport=httpx.MockTransport(respond),
    )
    original_headers = dict(config.headers)
    if synchronous:
        adapter = SandboxesAdapterSync(config)
        try:
            adapter.list_sandboxes(SandboxFilter())
        finally:
            adapter._httpx_client.close()
    else:
        adapter = SandboxesAdapter(config)
        try:
            await adapter.list_sandboxes(SandboxFilter())
        finally:
            await adapter._httpx_client.aclose()

    assert len(captured) == 1
    assert captured[0].url.path == "/v1/sandboxes"
    if explicit_header is not None:
        expected = [explicit_value]
    elif key_source == "none":
        expected = []
    else:
        expected = [f"{key_source}-key"]
    assert captured[0].headers.get_list("OPEN-SANDBOX-API-KEY") == expected
    assert captured[0].headers["X-Request-ID"] == "test-request"
    assert config.headers == original_headers
