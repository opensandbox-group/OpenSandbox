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

import json

import httpx
import pytest

from opensandbox.adapters.egress_adapter import EgressAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.egress_adapter import EgressAdapterSync


class _CredentialVaultAsyncTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.requests: list[httpx.Request] = []

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if request.method == "POST" and request.url.path == "/credential-vault":
            return httpx.Response(
                201,
                json={
                    "revision": 1,
                    "credentials": [
                        {"name": "gitlab-token", "sourceType": "inline", "revision": 1}
                    ],
                    "bindings": [
                        {
                            "name": "gitlab-api",
                            "revision": 1,
                            "auth": {"type": "apiKey", "name": "PRIVATE-TOKEN"},
                            "match": {"hosts": ["code.example.com"]},
                        }
                    ],
                },
                request=request,
            )
        if request.method == "PATCH" and request.url.path == "/credential-vault":
            return httpx.Response(
                200,
                json={"revision": 2, "credentials": [], "bindings": []},
                request=request,
            )
        if request.method == "GET" and request.url.path == "/credential-vault/bindings":
            return httpx.Response(
                200,
                json={
                    "revision": 2,
                    "bindings": [{"name": "gitlab-api", "revision": 2}],
                },
                request=request,
            )
        if request.method == "GET" and request.url.path == "/credential-vault/credentials/missing":
            return httpx.Response(404, text="credential not found", request=request)
        return httpx.Response(204, request=request)


class _CredentialVaultSyncTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.requests: list[httpx.Request] = []

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        if request.method == "GET" and request.url.path == "/credential-vault":
            return httpx.Response(
                200,
                json={"revision": 7, "credentials": [], "bindings": []},
                request=request,
            )
        if request.method == "GET" and request.url.path == "/credential-vault/credentials/missing":
            return httpx.Response(404, text="credential not found", request=request)
        return httpx.Response(204, request=request)


@pytest.mark.asyncio
async def test_async_credential_vault_create_patch_and_list_bindings() -> None:
    transport = _CredentialVaultAsyncTransport()
    adapter = EgressAdapter(
        ConnectionConfig(transport=transport),
        SandboxEndpoint(endpoint="sandbox.internal:18080", headers={"X-Egress": "1"}),
    )

    state = await adapter.create(
        credentials=[
            {
                "name": "gitlab-token",
                "source": {"value": "secret-token"},
            }
        ],
        bindings=[
            {
                "name": "gitlab-api",
                "match": {"hosts": ["code.example.com"]},
                "auth": {
                    "type": "apiKey",
                    "name": "PRIVATE-TOKEN",
                    "credential": "gitlab-token",
                },
            }
        ],
    )
    assert state.revision == 1
    assert state.credentials[0].source_type == "inline"

    post_body = json.loads(transport.requests[0].content)
    assert post_body["credentials"][0]["source"] == {
        "type": "inline",
        "value": "secret-token",
    }
    assert post_body["bindings"][0]["auth"]["type"] == "apiKey"
    assert transport.requests[0].headers["X-Egress"] == "1"

    patched = await adapter.patch(
        expected_revision=1,
        credentials={"delete": ["gitlab-token"]},
        bindings={"delete": ["gitlab-api"]},
    )
    assert patched.revision == 2
    patch_body = json.loads(transport.requests[1].content)
    assert patch_body == {
        "expectedRevision": 1,
        "credentials": {"delete": ["gitlab-token"]},
        "bindings": {"delete": ["gitlab-api"]},
    }

    bindings = await adapter.list_bindings()
    assert bindings[0].name == "gitlab-api"

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.get_credential("missing")
    assert exc_info.value.status_code == 404


def test_sync_credential_vault_get_and_delete() -> None:
    transport = _CredentialVaultSyncTransport()
    adapter = EgressAdapterSync(
        ConnectionConfigSync(transport=transport),
        SandboxEndpoint(endpoint="sandbox.internal:18080"),
    )

    state = adapter.get()
    assert state.revision == 7

    adapter.delete()
    assert [request.method for request in transport.requests] == ["GET", "DELETE"]

    with pytest.raises(SandboxApiException) as exc_info:
        adapter.get_credential("missing")
    assert exc_info.value.status_code == 404
