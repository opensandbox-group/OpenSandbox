#
# Copyright 2025 The OpenSandbox Authors
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
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter


class _Resp:
    def __init__(self, *, status_code: int, parsed) -> None:
        self.status_code = status_code
        self.parsed = parsed


@pytest.mark.asyncio
async def test_create_context_uses_openapi_and_converts(monkeypatch: pytest.MonkeyPatch) -> None:
    from opensandbox.api.execd.models.code_context import CodeContext as ApiCodeContext

    async def _fake_asyncio_detailed(*, client, body):
        assert body.language == "python"
        return _Resp(status_code=200, parsed=ApiCodeContext(language="python", id="ctx-1"))

    monkeypatch.setattr(
        "opensandbox.api.execd.api.code_interpreting.create_code_context.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = CodesAdapter(
        SandboxEndpoint(endpoint="localhost:44772", port=44772),
        ConnectionConfig(protocol="http"),
    )
    ctx = await adapter.create_context("python")
    assert ctx.id == "ctx-1"
    assert ctx.language == "python"


@pytest.mark.asyncio
async def test_interrupt_calls_openapi(monkeypatch: pytest.MonkeyPatch) -> None:
    called = {"id": None}

    async def _fake_asyncio_detailed(*, client, id):
        called["id"] = id
        return _Resp(status_code=204, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.execd.api.code_interpreting.interrupt_code.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = CodesAdapter(
        SandboxEndpoint(endpoint="localhost:44772", port=44772),
        ConnectionConfig(protocol="http"),
    )
    await adapter.interrupt("exec-1")
    assert called["id"] == "exec-1"
