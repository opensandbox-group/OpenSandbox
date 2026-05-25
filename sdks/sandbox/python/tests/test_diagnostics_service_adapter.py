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

from datetime import datetime, timezone

import pytest

from opensandbox.adapters.diagnostics_adapter import DiagnosticsAdapter
from opensandbox.api.diagnostic.models import (
    DiagnosticContentResponse,
    DiagnosticContentResponseDelivery,
    DiagnosticContentResponseKind,
)
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.sync.adapters.diagnostics_adapter import DiagnosticsAdapterSync


class _Resp:
    def __init__(self, *, status_code: int, parsed) -> None:
        self.status_code = status_code
        self.parsed = parsed
        self.headers = {}


def _api_diagnostic_content(kind: DiagnosticContentResponseKind) -> DiagnosticContentResponse:
    return DiagnosticContentResponse(
        sandbox_id="sbx-123",
        kind=kind,
        scope="runtime",
        delivery=DiagnosticContentResponseDelivery.INLINE,
        content_type="text/plain; charset=utf-8",
        truncated=False,
        content="diagnostic line",
        expires_at=datetime(2026, 1, 1, tzinfo=timezone.utc),
        warnings=["partial"],
    )


@pytest.mark.asyncio
async def test_async_diagnostics_adapter_maps_logs_request_and_response(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    called = {}

    async def _fake_asyncio_detailed(*, sandbox_id, client, scope):
        called["sandbox_id"] = sandbox_id
        called["client"] = client
        called["scope"] = scope
        return _Resp(
            status_code=200,
            parsed=_api_diagnostic_content(DiagnosticContentResponseKind.LOGS),
        )

    monkeypatch.setattr(
        "opensandbox.api.diagnostic.api.diagnostics."
        "get_sandboxes_sandbox_id_diagnostics_logs.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = DiagnosticsAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))
    out = await adapter.get_logs("sbx-123", scope="runtime")

    assert called["sandbox_id"] == "sbx-123"
    assert called["scope"] == "runtime"
    assert out.kind == "logs"
    assert out.scope == "runtime"
    assert out.content == "diagnostic line"
    assert out.warnings == ["partial"]


def test_sync_diagnostics_adapter_maps_events_request_and_response(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    called = {}

    def _fake_sync_detailed(*, sandbox_id, client, scope):
        called["sandbox_id"] = sandbox_id
        called["client"] = client
        called["scope"] = scope
        return _Resp(
            status_code=200,
            parsed=_api_diagnostic_content(DiagnosticContentResponseKind.EVENTS),
        )

    monkeypatch.setattr(
        "opensandbox.api.diagnostic.api.diagnostics."
        "get_sandboxes_sandbox_id_diagnostics_events.sync_detailed",
        _fake_sync_detailed,
    )

    adapter = DiagnosticsAdapterSync(
        ConnectionConfigSync(domain="example.com:8080", api_key="k")
    )
    out = adapter.get_events("sbx-123", scope="runtime")

    assert called["sandbox_id"] == "sbx-123"
    assert called["scope"] == "runtime"
    assert out.kind == "events"
    assert out.scope == "runtime"
    assert out.content == "diagnostic line"
