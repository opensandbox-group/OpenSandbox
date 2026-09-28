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

Follow-up finding: the adapter clients originally wrapped the *shared*
``connection_config.transport``; httpx closes a client's transport on
close(), so ``aclose()`` tore down the sandbox's own connection pool and
broke the documented ``interpreter.aclose() -> sandbox.kill()`` sequence.
The adapter clients now run on adapter-owned transports
(``ConnectionConfig.new_owned_transport()``).
"""

from __future__ import annotations

import uuid

import httpx
import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxReadyTimeoutException
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.models.code import SupportedLanguage
from code_interpreter.models.code_sync import SupportedLanguageSync
from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync
from code_interpreter.sync.adapters.factory import AdapterFactorySync
from code_interpreter.sync.code_interpreter import CodeInterpreterSync

ENDPOINT = SandboxEndpoint(endpoint="localhost:44772", port=44772)


class _TrackingAsyncTransport(httpx.AsyncBaseTransport):
    def __init__(self) -> None:
        self.aclose_calls = 0

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        return httpx.Response(200)

    async def aclose(self) -> None:
        self.aclose_calls += 1


class _TrackingSyncTransport(httpx.BaseTransport):
    def __init__(self) -> None:
        self.close_calls = 0

    def handle_request(self, request: httpx.Request) -> httpx.Response:
        return httpx.Response(200)

    def close(self) -> None:
        self.close_calls += 1


class _FakeSandboxSync:
    def __init__(self) -> None:
        self._id = str(uuid.uuid4())
        self.connection_config = ConnectionConfigSync(protocol="http")

    @property
    def id(self) -> str:
        return self._id

    def get_endpoint(self, port: int) -> SandboxEndpoint:
        return SandboxEndpoint(endpoint="localhost:44772", port=port)


@pytest.mark.asyncio
async def test_async_adapter_aclose_closes_owned_clients() -> None:
    adapter = CodesAdapter(ENDPOINT, ConnectionConfig(protocol="http"))
    httpx_client = await adapter._get_client()
    httpx_client = httpx_client.get_async_httpx_client()

    await adapter.aclose()

    assert httpx_client.is_closed
    assert adapter._sse_client.is_closed


@pytest.mark.asyncio
async def test_async_adapter_aclose_keeps_shared_transport_open() -> None:
    # Production shape: the sandbox built the config's shared transport via
    # with_transport_if_missing(); the adapter must not tear it down. A
    # closed pool raises RuntimeError; ConnectError proves it is still open.
    config = ConnectionConfig(protocol="http").with_transport_if_missing()
    shared = config.transport
    adapter = CodesAdapter(ENDPOINT, config)

    await adapter.aclose()

    with pytest.raises(httpx.ConnectError):
        await shared.handle_async_request(
            httpx.Request("GET", "http://127.0.0.1:1/ping")
        )

    # User-supplied transports are wrapped but never closed either.
    user = _TrackingAsyncTransport()
    adapter = CodesAdapter(
        ENDPOINT, ConnectionConfig(protocol="http", transport=user)
    )
    await adapter.aclose()

    assert user.aclose_calls == 0
    assert not adapter._httpx_client.is_closed
    assert not adapter._sse_client.is_closed


def test_sync_adapter_close_closes_owned_clients() -> None:
    adapter = CodesAdapterSync(ENDPOINT, ConnectionConfigSync(protocol="http"))
    httpx_client = adapter._client.get_httpx_client()

    adapter.close()

    assert httpx_client.is_closed
    assert adapter._sse_client.is_closed


def test_sync_adapter_close_keeps_shared_transport_open() -> None:
    # Production shape: the sandbox built the config's shared transport via
    # with_transport_if_missing(); the adapter must not tear it down. A
    # closed pool raises RuntimeError; ConnectError proves it is still open.
    config = ConnectionConfigSync(protocol="http").with_transport_if_missing()
    shared = config.transport
    adapter = CodesAdapterSync(ENDPOINT, config)

    adapter.close()

    with pytest.raises(httpx.ConnectError):
        shared.handle_request(httpx.Request("GET", "http://127.0.0.1:1/ping"))

    # User-supplied transports are wrapped but never closed either.
    user = _TrackingSyncTransport()
    adapter = CodesAdapterSync(
        ENDPOINT, ConnectionConfigSync(protocol="http", transport=user)
    )
    adapter.close()

    assert user.close_calls == 0
    assert not adapter._httpx_client.is_closed
    assert not adapter._sse_client.is_closed


def test_sync_create_failure_releases_service_clients(monkeypatch) -> None:
    def failing_check(self, timeout, polling_interval) -> None:
        raise SandboxReadyTimeoutException("health check timed out")

    monkeypatch.setattr(CodeInterpreterSync, "check_ready", failing_check)

    created: list[CodesAdapterSync] = []
    original = AdapterFactorySync.create_code_execution_service

    def capture(self, endpoint):
        service = original(self, endpoint)
        created.append(service)
        return service

    monkeypatch.setattr(AdapterFactorySync, "create_code_execution_service", capture)

    sbx = _FakeSandboxSync()
    with pytest.raises(SandboxReadyTimeoutException):
        CodeInterpreterSync.create(sandbox=sbx)

    assert created and created[0]._httpx_client.is_closed
    assert created[0]._sse_client.is_closed


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
