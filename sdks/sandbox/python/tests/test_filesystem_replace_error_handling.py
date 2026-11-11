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

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException, SandboxException
from opensandbox.models.filesystem import ContentReplaceEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync


def replacement_entries() -> list[ContentReplaceEntry]:
    return [
        ContentReplaceEntry(
            path="/workspace/report.txt", old_content="old", new_content="new"
        )
    ]


@pytest.mark.parametrize("scoped", [False, True])
@pytest.mark.parametrize("status", [400, 500])
@pytest.mark.parametrize("body", [b"", b"upstream failed"])
@pytest.mark.asyncio
async def test_async_replace_rejects_non_json_error_response(
    scoped: bool, status: int, body: bytes
) -> None:
    original = FilesystemAdapter(
        ConnectionConfig(
            transport=httpx.MockTransport(
                lambda _: httpx.Response(status, content=body)
            )
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    adapter = original.with_identity(1001, 2000) if scoped else original
    try:
        with pytest.raises(SandboxException):
            await adapter.replace_contents(replacement_entries())
    finally:
        await adapter._httpx_client.aclose()
        if scoped:
            await original._httpx_client.aclose()


@pytest.mark.parametrize("scoped", [False, True])
@pytest.mark.parametrize("status", [400, 500])
@pytest.mark.parametrize("body", [b"", b"upstream failed"])
def test_sync_replace_rejects_non_json_error_response(
    scoped: bool, status: int, body: bytes
) -> None:
    original = FilesystemAdapterSync(
        ConnectionConfigSync(
            transport=httpx.MockTransport(
                lambda _: httpx.Response(status, content=body)
            )
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    adapter = original.with_identity(1001, 2000) if scoped else original
    try:
        with pytest.raises(SandboxException):
            adapter.replace_contents(replacement_entries())
    finally:
        adapter._httpx_client.close()
        if scoped:
            original._httpx_client.close()


@pytest.mark.parametrize("status", [400, 500])
@pytest.mark.asyncio
async def test_async_replace_preserves_json_api_error(status: int) -> None:
    adapter = FilesystemAdapter(
        ConnectionConfig(
            transport=httpx.MockTransport(
                lambda _: httpx.Response(
                    status, json={"code": "REPLACE_FAILED", "message": "replace failed"}
                )
            )
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    try:
        with pytest.raises(SandboxApiException) as exc:
            await adapter.replace_contents(replacement_entries())
        assert exc.value.status_code == status
        assert exc.value.error.code == "REPLACE_FAILED"
    finally:
        await adapter._httpx_client.aclose()


@pytest.mark.parametrize("status", [400, 500])
def test_sync_replace_preserves_json_api_error(status: int) -> None:
    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(
            transport=httpx.MockTransport(
                lambda _: httpx.Response(
                    status, json={"code": "REPLACE_FAILED", "message": "replace failed"}
                )
            )
        ),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    try:
        with pytest.raises(SandboxApiException) as exc:
            adapter.replace_contents(replacement_entries())
        assert exc.value.status_code == status
        assert exc.value.error.code == "REPLACE_FAILED"
    finally:
        adapter._httpx_client.close()
