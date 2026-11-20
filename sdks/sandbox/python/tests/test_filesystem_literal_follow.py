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
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync


def _endpoint() -> SandboxEndpoint:
    return SandboxEndpoint(endpoint="localhost:44772")


@pytest.mark.asyncio
async def test_download_literal_maps_to_query_param() -> None:
    seen: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.clear()
        seen.update(dict(request.url.params))
        return httpx.Response(200, request=request, content=b"payload")

    adapter = FilesystemAdapter(
        ConnectionConfig(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )
    await adapter.read_bytes_detailed("/data-$V.txt", literal=True)
    assert seen["literal"] == "true"
    assert seen["path"] == "/data-$V.txt"

    # Omitted by default so the server keeps its expansion behavior.
    await adapter.read_bytes_detailed("/data-$V.txt")
    assert "literal" not in seen

    await adapter.read_bytes_detailed("/data-$V.txt", literal=False)
    assert seen["literal"] == "false"


def test_download_literal_maps_to_query_param_sync() -> None:
    seen: dict[str, str] = {}

    def handler(request: httpx.Request) -> httpx.Response:
        seen.clear()
        seen.update(dict(request.url.params))
        return httpx.Response(200, request=request, content=b"payload")

    adapter = FilesystemAdapterSync(
        ConnectionConfigSync(protocol="http", transport=httpx.MockTransport(handler)),
        _endpoint(),
    )
    adapter.read_bytes_detailed("/data-$V.txt", literal=True)
    assert seen["literal"] == "true"


def test_get_files_info_kwargs_carry_literal_and_follow() -> None:
    from opensandbox.api.execd.api.filesystem.get_files_info import _get_kwargs

    kwargs = _get_kwargs(path=["/a.txt"], literal=True, follow=False)
    assert kwargs["params"]["literal"] is True
    assert kwargs["params"]["follow"] is False

    # Unset defaults must stay out of the request entirely.
    kwargs = _get_kwargs(path=["/a.txt"])
    assert "literal" not in kwargs["params"]
    assert "follow" not in kwargs["params"]
