#
# Copyright 2025 Alibaba Group Holding Ltd.
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
from types import SimpleNamespace

import pytest

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.filesystem import SearchEntry
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync


@pytest.mark.asyncio
async def test_async_search_unexpected_response_without_headers_still_raises_api_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    async def _fake_asyncio_detailed(**_: object) -> SimpleNamespace:
        return SimpleNamespace(status_code=200, parsed=object())

    from opensandbox.api.execd.api.filesystem import search_files

    monkeypatch.setattr(search_files, "asyncio_detailed", _fake_asyncio_detailed)

    cfg = ConnectionConfig(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = FilesystemAdapter(cfg, endpoint)
    async def _fake_get_client() -> object:
        return object()

    monkeypatch.setattr(adapter, "_get_client", _fake_get_client)

    with pytest.raises(SandboxApiException) as ei:
        await adapter.search(SearchEntry(path="/tmp", pattern="*.log"))

    assert "unexpected response type" in str(ei.value)
    assert ei.value.request_id is None


def test_sync_search_unexpected_response_without_headers_still_raises_api_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def _fake_sync_detailed(**_: object) -> SimpleNamespace:
        return SimpleNamespace(status_code=200, parsed=object())

    from opensandbox.api.execd.api.filesystem import search_files

    monkeypatch.setattr(search_files, "sync_detailed", _fake_sync_detailed)

    cfg = ConnectionConfigSync(protocol="http")
    endpoint = SandboxEndpoint(endpoint="localhost:44772", port=44772)
    adapter = FilesystemAdapterSync(cfg, endpoint)

    with pytest.raises(SandboxApiException) as ei:
        adapter.search(SearchEntry(path="/tmp", pattern="*.log"))

    assert "unexpected response type" in str(ei.value)
    assert ei.value.request_id is None
