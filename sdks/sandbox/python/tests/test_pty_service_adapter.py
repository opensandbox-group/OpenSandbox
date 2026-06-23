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

from opensandbox.adapters.pty_adapter import PtyAdapter
from opensandbox.api.execd.models import (
    CreatePtySessionResponse,
    PtySessionStatusResponse,
)
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint


class _Resp:
    def __init__(self, *, status_code: int, parsed=None) -> None:
        self.status_code = status_code
        self.parsed = parsed
        self.headers = {}


def _adapter() -> PtyAdapter:
    return PtyAdapter(
        ConnectionConfig(domain="example.com:8080", api_key="k"),
        SandboxEndpoint(endpoint="example.com:8080"),
    )


@pytest.mark.asyncio
async def test_create_session_maps_request_and_response(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    called = {}

    async def _fake(*, client, body):
        called["cwd"] = body.cwd
        called["command"] = body.command
        return _Resp(status_code=201, parsed=CreatePtySessionResponse(session_id="sess-123"))

    monkeypatch.setattr(
        "opensandbox.api.execd.api.pty.create_pty_session.asyncio_detailed", _fake
    )

    session = await _adapter().create_session(cwd="/tmp", command="bash")

    assert session.session_id == "sess-123"
    assert called["cwd"] == "/tmp"
    assert called["command"] == "bash"


@pytest.mark.asyncio
async def test_get_session_maps_status(monkeypatch: pytest.MonkeyPatch) -> None:
    async def _fake(session_id, *, client):
        assert session_id == "sess-123"
        return _Resp(
            status_code=200,
            parsed=PtySessionStatusResponse(
                session_id="sess-123", running=True, output_offset=4096
            ),
        )

    monkeypatch.setattr(
        "opensandbox.api.execd.api.pty.get_pty_session.asyncio_detailed", _fake
    )

    status = await _adapter().get_session("sess-123")

    assert status.session_id == "sess-123"
    assert status.running is True
    assert status.output_offset == 4096


@pytest.mark.asyncio
async def test_delete_session_calls_api(monkeypatch: pytest.MonkeyPatch) -> None:
    called = {}

    async def _fake(session_id, *, client):
        called["session_id"] = session_id
        return _Resp(status_code=200)

    monkeypatch.setattr(
        "opensandbox.api.execd.api.pty.delete_pty_session.asyncio_detailed", _fake
    )

    await _adapter().delete_session("sess-123")

    assert called["session_id"] == "sess-123"
