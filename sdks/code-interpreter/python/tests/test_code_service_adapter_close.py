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
"""Regression tests for interpreter resource cleanup and language constants.

Audit finding: CodesAdapter owned three HTTP clients that were never closed,
so every ``CodeInterpreter.create()`` leaked sockets for the process lifetime.
``SupportedLanguageSync`` was also missing ``JAVASCRIPT`` despite claiming
value parity with ``SupportedLanguage``.
"""

from __future__ import annotations

import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.models.code import SupportedLanguage
from code_interpreter.models.code_sync import SupportedLanguageSync
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync

ENDPOINT = SandboxEndpoint(endpoint="localhost:44772", port=44772)


@pytest.mark.asyncio
async def test_async_adapter_aclose_closes_owned_clients() -> None:
    adapter = CodesAdapter(ENDPOINT, ConnectionConfig(protocol="http"))
    httpx_client = await adapter._get_client()
    httpx_client = httpx_client.get_async_httpx_client()

    await adapter.aclose()

    assert httpx_client.is_closed
    assert adapter._sse_client.is_closed


def test_sync_adapter_close_closes_owned_clients() -> None:
    adapter = CodesAdapterSync(ENDPOINT, ConnectionConfigSync(protocol="http"))
    httpx_client = adapter._client.get_httpx_client()

    adapter.close()

    assert httpx_client.is_closed
    assert adapter._sse_client.is_closed


@pytest.mark.asyncio
async def test_async_adapter_aclose_is_idempotent() -> None:
    adapter = CodesAdapter(ENDPOINT, ConnectionConfig(protocol="http"))

    await adapter.aclose()
    await adapter.aclose()


def test_supported_language_sync_matches_async_values() -> None:
    async_values = {
        name: value
        for name, value in vars(SupportedLanguage).items()
        if not name.startswith("_") and isinstance(value, str)
    }
    sync_values = {
        name: value
        for name, value in vars(SupportedLanguageSync).items()
        if not name.startswith("_") and isinstance(value, str)
    }
    assert sync_values == async_values
    assert SupportedLanguageSync.JAVASCRIPT == "javascript"
