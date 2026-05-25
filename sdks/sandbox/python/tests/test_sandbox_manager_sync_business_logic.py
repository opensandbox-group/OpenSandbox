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
from __future__ import annotations

from datetime import datetime, timedelta, timezone
from uuid import uuid4

import httpx

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.sync.manager import SandboxManagerSync


class _SandboxServiceStub:
    def __init__(self) -> None:
        self.renew_calls: list[tuple[object, datetime]] = []
        self.snapshot_calls: list[tuple[str, object]] = []

    def list_sandboxes(self, _filter):  # pragma: no cover
        raise RuntimeError("not used")

    def get_sandbox_info(self, _sandbox_id):  # pragma: no cover
        raise RuntimeError("not used")

    def kill_sandbox(self, _sandbox_id):  # pragma: no cover
        raise RuntimeError("not used")

    def renew_sandbox_expiration(self, sandbox_id, new_expiration_time: datetime) -> None:
        self.renew_calls.append((sandbox_id, new_expiration_time))

    def pause_sandbox(self, _sandbox_id) -> None:  # pragma: no cover
        raise RuntimeError("not used")

    def resume_sandbox(self, _sandbox_id):  # pragma: no cover
        raise RuntimeError("not used")

    def create_snapshot(self, sandbox_id, request):
        self.snapshot_calls.append(("create", (sandbox_id, request.name)))
        return type("Snapshot", (), {"id": "snap-1"})()

    def get_snapshot(self, snapshot_id):
        self.snapshot_calls.append(("get", snapshot_id))
        return type("Snapshot", (), {"id": snapshot_id})()

    def list_snapshots(self, _filter):
        self.snapshot_calls.append(("list", _filter))
        return type("Paged", (), {"snapshot_infos": [type("Snapshot", (), {"id": "snap-1"})()]})()

    def delete_snapshot(self, snapshot_id):
        self.snapshot_calls.append(("delete", snapshot_id))


class _DiagnosticsServiceStub:
    def __init__(self) -> None:
        self.calls: list[tuple[str, object, str | None]] = []

    def get_logs(self, sandbox_id, scope) -> DiagnosticContent:
        self.calls.append(("logs", sandbox_id, scope))
        return DiagnosticContent(
            sandboxId=sandbox_id,
            kind="logs",
            scope=scope or "container",
            delivery="inline",
            contentType="text/plain; charset=utf-8",
            content="log line",
            truncated=False,
        )

    def get_events(self, sandbox_id, scope) -> DiagnosticContent:
        self.calls.append(("events", sandbox_id, scope))
        return DiagnosticContent(
            sandboxId=sandbox_id,
            kind="events",
            scope=scope or "runtime",
            delivery="inline",
            contentType="text/plain; charset=utf-8",
            content="event line",
            truncated=False,
        )


def test_sync_manager_renew_uses_utc_datetime() -> None:
    svc = _SandboxServiceStub()
    mgr = SandboxManagerSync(svc, ConnectionConfigSync())

    sid = str(uuid4())
    mgr.renew_sandbox(sid, timedelta(seconds=5))

    assert len(svc.renew_calls) == 1
    _, dt = svc.renew_calls[0]
    assert dt.tzinfo is timezone.utc


def test_sync_manager_close_does_not_close_user_transport() -> None:
    class CustomTransport(httpx.BaseTransport):
        def __init__(self) -> None:
            self.closed = False

        def handle_request(self, request: httpx.Request) -> httpx.Response:  # pragma: no cover
            raise RuntimeError("not used")

        def close(self) -> None:
            self.closed = True

    t = CustomTransport()
    cfg = ConnectionConfigSync(transport=t)

    mgr = SandboxManagerSync(_SandboxServiceStub(), cfg)
    mgr.close()
    assert t.closed is False


def test_sync_manager_snapshot_methods_delegate() -> None:
    svc = _SandboxServiceStub()
    mgr = SandboxManagerSync(svc, ConnectionConfigSync())

    created = mgr.create_snapshot("sbx-1", "before-upgrade")
    loaded = mgr.get_snapshot("snap-1")
    listed = mgr.list_snapshots(type("Filter", (), {})())
    mgr.delete_snapshot("snap-1")

    assert created.id == "snap-1"
    assert loaded.id == "snap-1"
    assert listed.snapshot_infos[0].id == "snap-1"
    assert svc.snapshot_calls[0] == ("create", ("sbx-1", "before-upgrade"))
    assert svc.snapshot_calls[1] == ("get", "snap-1")
    assert svc.snapshot_calls[3] == ("delete", "snap-1")


def test_sync_manager_diagnostic_methods_delegate_by_sandbox_id() -> None:
    diagnostics_service = _DiagnosticsServiceStub()
    mgr = SandboxManagerSync(
        _SandboxServiceStub(),
        ConnectionConfigSync(),
        diagnostics_service=diagnostics_service,
    )

    logs = mgr.get_diagnostic_logs("sbx-1", scope="container")
    events = mgr.get_diagnostic_events("sbx-1", scope="runtime")

    assert logs.kind == "logs"
    assert events.kind == "events"
    assert diagnostics_service.calls == [
        ("logs", "sbx-1", "container"),
        ("events", "sbx-1", "runtime"),
    ]
