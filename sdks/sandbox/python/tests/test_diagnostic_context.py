# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import httpx
import pytest

from opensandbox.adapters.diagnostics_adapter import DiagnosticsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.manager import SandboxManager
from opensandbox.models.diagnostics import DiagnosticContext
from opensandbox.sandbox import Sandbox
from opensandbox.sync.adapters.diagnostics_adapter import DiagnosticsAdapterSync
from opensandbox.sync.manager import SandboxManagerSync
from opensandbox.sync.sandbox import SandboxSync


def context_response(request):
    assert request.method == "GET"
    assert request.url.path == "/v1/sandboxes/sbx-1/diagnostics/context"
    assert request.headers["OPEN-SANDBOX-API-KEY"] == "key"
    assert request.headers["x-custom"] == "custom"
    return httpx.Response(
        200,
        json={
            "sandboxId": "sbx-1",
            "backendType": "kubernetes",
            "backendIdentifiers": {"namespace": "tenant-ns", "podName": "pod-1"},
        },
    )


@pytest.mark.asyncio
async def test_async_context_transport_and_public_facades():
    adapter = DiagnosticsAdapter(
        ConnectionConfig(
            domain="localhost:8080",
            api_key="key",
            headers={"x-custom": "custom"},
            transport=httpx.MockTransport(context_response),
        )
    )
    try:
        for facade, method, args in [
            (SandboxManager, SandboxManager.get_diagnostic_context, ("sbx-1",)),
            (Sandbox, Sandbox.get_diagnostic_context, ()),
        ]:
            obj = object.__new__(facade)
            obj._diagnostics_service = adapter
            if facade is Sandbox:
                obj.id = "sbx-1"
            result = await method(obj, *args)
            assert isinstance(result, DiagnosticContext)
            assert result.state is None
            assert result.backend_identifiers["podName"] == "pod-1"
    finally:
        await adapter._httpx_client.aclose()


def test_sync_context_transport_and_public_facades():
    adapter = DiagnosticsAdapterSync(
        ConnectionConfigSync(
            domain="localhost:8080",
            api_key="key",
            headers={"x-custom": "custom"},
            transport=httpx.MockTransport(context_response),
        )
    )
    try:
        for facade, method, args in [
            (SandboxManagerSync, SandboxManagerSync.get_diagnostic_context, ("sbx-1",)),
            (SandboxSync, SandboxSync.get_diagnostic_context, ()),
        ]:
            obj = object.__new__(facade)
            obj._diagnostics_service = adapter
            if facade is SandboxSync:
                obj.id = "sbx-1"
            result = method(obj, *args)
            assert isinstance(result, DiagnosticContext)
            assert result.backend_type == "kubernetes"
            assert result.state is None
    finally:
        adapter._httpx_client.close()


@pytest.mark.asyncio
@pytest.mark.parametrize("status", [403, 404, 501])
async def test_context_maps_errors(status):
    from opensandbox.exceptions import SandboxApiException

    adapter = DiagnosticsAdapter(
        ConnectionConfig(
            domain="localhost:8080",
            transport=httpx.MockTransport(
                lambda request: httpx.Response(
                    status, json={"code": "TEST_ERROR", "message": "unavailable"}
                )
            ),
        )
    )
    try:
        with pytest.raises(SandboxApiException):
            await adapter.get_context("sbx-1")
    finally:
        await adapter._httpx_client.aclose()
