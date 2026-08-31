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

"""Guards the X-Request-Id propagation contract of the generated run_command
client: the header must be forwarded by every wrapper (sync, asyncio_detailed,
and the asyncio() convenience wrapper) and must stay absent when the parameter
is unset. A regeneration that drops the parameter fails these tests loudly."""

from __future__ import annotations

import asyncio
import httpx

from opensandbox.api.execd.api.command import run_command
from opensandbox.api.execd.client import Client
from opensandbox.api.execd.models.run_command_request import RunCommandRequest
from opensandbox.api.execd.types import UNSET


def _client_with_capture(captured: dict[str, str]) -> Client:
    def _handler(request: httpx.Request) -> httpx.Response:
        captured.update({key.lower(): value for key, value in request.headers.items()})
        # A 400 with a parseable ErrorResponse keeps the generated _parse_response
        # happy (the 200 branch parses SSE text, which a MockTransport body is not).
        return httpx.Response(400, json={"code": "INVALID_REQUEST_BODY", "message": "test"})

    client = Client(base_url="http://test")
    client.set_httpx_client(httpx.Client(base_url="http://test", transport=httpx.MockTransport(_handler)))
    client.set_async_httpx_client(httpx.AsyncClient(base_url="http://test", transport=httpx.MockTransport(_handler)))
    return client


def _body() -> RunCommandRequest:
    return RunCommandRequest(command="echo hi")


def test_sync_detailed_forwards_request_id_header() -> None:
    captured: dict[str, str] = {}
    client = _client_with_capture(captured)

    response = run_command.sync_detailed(
        client=client,
        body=_body(),
        x_request_id="req-sync-1",
    )

    assert response.status_code == 400
    assert captured.get("x-request-id") == "req-sync-1"


def test_sync_wrapper_forwards_request_id_header() -> None:
    captured: dict[str, str] = {}
    client = _client_with_capture(captured)

    parsed = run_command.sync(client=client, body=_body(), x_request_id="req-sync-2")

    assert parsed is not None and parsed.code == "INVALID_REQUEST_BODY"
    assert captured.get("x-request-id") == "req-sync-2"


def test_asyncio_wrapper_forwards_request_id_header() -> None:
    # Regression guard: the asyncio() convenience wrapper must forward the
    # parameter to asyncio_detailed, mirroring the sync path.
    async def _run() -> None:
        captured: dict[str, str] = {}
        client = _client_with_capture(captured)

        parsed = await run_command.asyncio(client=client, body=_body(), x_request_id="req-async-1")

        assert parsed is not None and parsed.code == "INVALID_REQUEST_BODY"
        assert captured.get("x-request-id") == "req-async-1"

    asyncio.run(_run())


def test_unset_request_id_sends_no_header() -> None:
    async def _run() -> None:
        captured: dict[str, str] = {}
        client = _client_with_capture(captured)

        await run_command.asyncio(client=client, body=_body(), x_request_id=UNSET)

        # Capture lowercases header names: assert the normalized key so a
        # regeneration that emits the header cannot slip past a mixed-case check.
        assert "x-request-id" not in captured

    asyncio.run(_run())
