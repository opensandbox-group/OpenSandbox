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
"""Error responses whose body is not an ErrorResponse keep their status code.

Gateways and proxies in front of the server or execd can answer with an HTML,
plain-text or empty body.
"""

import runpy
from pathlib import Path

import httpx
import pytest

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync

NON_JSON_BODIES = [b"", b"<html><body>Not Found</body></html>", b'"not an object"']


def _transport(status: int, body: bytes) -> httpx.MockTransport:
    return httpx.MockTransport(lambda _: httpx.Response(status, content=body))


@pytest.mark.parametrize("body", NON_JSON_BODIES)
@pytest.mark.asyncio
async def test_async_execd_error_keeps_status(body: bytes) -> None:
    adapter = CommandsAdapter(
        ConnectionConfig(transport=_transport(404, body)),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.get_command_status("cmd-1")
    assert exc_info.value.status_code == 404


@pytest.mark.parametrize("body", NON_JSON_BODIES)
def test_sync_execd_error_keeps_status(body: bytes) -> None:
    adapter = CommandsAdapterSync(
        ConnectionConfigSync(transport=_transport(404, body)),
        SandboxEndpoint(endpoint="localhost:44772"),
    )
    with pytest.raises(SandboxApiException) as exc_info:
        adapter.get_command_status("cmd-1")
    assert exc_info.value.status_code == 404


@pytest.mark.parametrize("body", NON_JSON_BODIES)
@pytest.mark.asyncio
async def test_async_lifecycle_error_keeps_status(body: bytes) -> None:
    adapter = SandboxesAdapter(ConnectionConfig(transport=_transport(500, body)))
    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.get_sandbox_info("sbx-1")
    assert exc_info.value.status_code == 500


@pytest.mark.parametrize("body", NON_JSON_BODIES)
def test_sync_lifecycle_error_keeps_status(body: bytes) -> None:
    adapter = SandboxesAdapterSync(ConnectionConfigSync(transport=_transport(500, body)))
    with pytest.raises(SandboxApiException) as exc_info:
        adapter.get_sandbox_info("sbx-1")
    assert exc_info.value.status_code == 500


@pytest.mark.asyncio
async def test_async_error_response_body_is_still_parsed() -> None:
    body = b'{"code": "SANDBOX_NOT_FOUND", "message": "sandbox sbx-1 not found"}'
    adapter = SandboxesAdapter(ConnectionConfig(transport=_transport(404, body)))
    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.get_sandbox_info("sbx-1")
    assert exc_info.value.status_code == 404
    assert "sandbox sbx-1 not found" in str(exc_info.value)


def test_generation_leaves_unparsable_error_bodies_unparsed(tmp_path: Path) -> None:
    script = Path(__file__).resolve().parents[1] / "scripts/generate_api.py"
    post_process = runpy.run_path(str(script))["tolerate_unparsable_error_bodies"]
    operation = tmp_path / "api/sandboxes/get_sandbox.py"
    operation.parent.mkdir(parents=True)
    operation.write_text(
        "def _parse_response(*, client, response):\n"
        "    if response.status_code == 200:\n"
        "        response_200 = Sandbox.from_dict(response.json())\n"
        "\n"
        "        return response_200\n"
        "\n"
        "    if response.status_code == 404:\n"
        "        response_404 = ErrorResponse.from_dict(response.json())\n"
        "\n"
        "        return response_404\n",
        encoding="utf-8",
    )

    post_process(tmp_path)
    first_pass = operation.read_text(encoding="utf-8")
    assert "        response_200 = Sandbox.from_dict(response.json())\n" in first_pass
    assert (
        "        try:\n"
        "            response_404 = ErrorResponse.from_dict(response.json())\n"
        "        except (ValueError, TypeError, KeyError):\n"
        "            return None\n"
    ) in first_pass

    post_process(tmp_path)
    assert operation.read_text(encoding="utf-8") == first_pass
