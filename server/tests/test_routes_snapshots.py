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

from datetime import datetime, timezone

from fastapi import HTTPException
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import (
    ListSnapshotsResponse,
    PaginationInfo,
    Snapshot,
    SnapshotStatus,
)
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_runtime import NoopSnapshotRuntime
from opensandbox_server.services.snapshot_service import PersistedSnapshotService


def _stub_sandbox_service():
    """Returns a minimal sandbox_service stub that satisfies scope-checking in create_snapshot."""

    class _Stub:
        @staticmethod
        def get_sandbox(_sandbox_id: str):
            return {"id": _sandbox_id, "metadata": {}}

    return _Stub()


def _sample_snapshot(now: datetime, snapshot_id: str = "snap-001") -> Snapshot:
    return Snapshot(
        id=snapshot_id,
        sandboxId="sbx-001",
        name="checkpoint-before-import",
        status=SnapshotStatus(state="Creating"),
        createdAt=now,
    )


def test_create_snapshot_returns_202_and_location_header(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[object] = []

    class StubService:
        @staticmethod
        def create_snapshot(sandbox_id: str, request, *, access_owner=None, access_team=None) -> Snapshot:
            calls.append((sandbox_id, request))
            return _sample_snapshot(now)

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())
    monkeypatch.setattr(lifecycle, "sandbox_service", _stub_sandbox_service())

    response = client.post(
        "/v1/sandboxes/sbx-001/snapshots",
        headers=auth_headers,
        json={"name": "checkpoint-before-import"},
    )

    assert response.status_code == 202
    assert response.headers["location"] == "/v1/snapshots/snap-001"
    assert response.json()["sandboxId"] == "sbx-001"
    assert calls[0][0] == "sbx-001"
    assert calls[0][1].name == "checkpoint-before-import"


def test_create_snapshot_accepts_empty_body(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        def create_snapshot(sandbox_id: str, request, *, access_owner=None, access_team=None) -> Snapshot:
            assert sandbox_id == "sbx-001"
            assert request.name is None
            return _sample_snapshot(now)

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())
    monkeypatch.setattr(lifecycle, "sandbox_service", _stub_sandbox_service())

    response = client.post("/v1/sandboxes/sbx-001/snapshots", headers=auth_headers)

    assert response.status_code == 202


def test_list_snapshots_parses_filters_and_pagination(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    captured_requests: list[object] = []

    class StubService:
        @staticmethod
        def list_snapshots(request, *, access_owner=None, access_team=None) -> ListSnapshotsResponse:
            captured_requests.append(request)
            return ListSnapshotsResponse(
                items=[_sample_snapshot(now)],
                pagination=PaginationInfo(
                    page=2,
                    pageSize=5,
                    totalItems=6,
                    totalPages=2,
                    hasNextPage=False,
                ),
            )

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())

    response = client.get(
        "/v1/snapshots",
        params={"sandboxId": "sbx-001", "state": ["Ready", "Failed"], "page": 2, "pageSize": 5},
        headers=auth_headers,
    )

    assert response.status_code == 200
    payload = response.json()
    assert payload["items"][0]["id"] == "snap-001"
    assert captured_requests[0].filter.sandbox_id == "sbx-001"
    assert captured_requests[0].filter.state == ["Ready", "Failed"]
    assert captured_requests[0].pagination.page == 2
    assert captured_requests[0].pagination.page_size == 5


def test_get_snapshot_returns_service_payload(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        def get_snapshot(snapshot_id: str) -> Snapshot:
            assert snapshot_id == "snap-001"
            return _sample_snapshot(now, snapshot_id=snapshot_id)

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())

    response = client.get("/v1/snapshots/snap-001", headers=auth_headers)

    assert response.status_code == 200
    assert response.json()["id"] == "snap-001"


def test_delete_snapshot_returns_204_and_calls_service(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[str] = []

    class StubService:
        @staticmethod
        def delete_snapshot(snapshot_id: str) -> None:
            calls.append(snapshot_id)

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())

    response = client.delete("/v1/snapshots/snap-001", headers=auth_headers)

    assert response.status_code == 204
    assert response.text == ""
    assert calls == ["snap-001"]


def test_delete_snapshot_returns_409_when_snapshot_delete_conflicts(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def delete_snapshot(snapshot_id: str) -> None:
            raise HTTPException(
                status_code=409,
                detail={
                    "code": "SNAPSHOT::DELETE_CONFLICT",
                    "message": "snapshot image cannot be deleted due to a conflict",
                },
            )

    monkeypatch.setattr(lifecycle, "snapshot_service", StubService())

    response = client.delete("/v1/snapshots/snap-001", headers=auth_headers)

    assert response.status_code == 409
    assert response.json() == {
        "code": "SNAPSHOT::DELETE_CONFLICT",
        "message": "snapshot image cannot be deleted due to a conflict",
    }


def test_snapshot_routes_can_use_persisted_service(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
    tmp_path,
) -> None:
    class StubSandboxService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return {"id": sandbox_id, "status": {"state": "Running"}}

    class StubSnapshotRuntime:
        @staticmethod
        def supports_create_snapshot() -> bool:
            return True

        @staticmethod
        def create_snapshot_unsupported_message() -> str:
            return ""

        @staticmethod
        def create_snapshot(snapshot_id: str, sandbox_id: str):
            return None

        @staticmethod
        def get_snapshot_status(snapshot_id: str):
            return None

        @staticmethod
        def delete_snapshot(snapshot_id: str, image: str | None = None) -> None:
            return None

    service = PersistedSnapshotService(
        SQLiteSnapshotRepository(tmp_path / "snapshots.db"),
        StubSandboxService(),
        snapshot_runtime=StubSnapshotRuntime(),
    )
    monkeypatch.setattr(lifecycle, "snapshot_service", service)
    monkeypatch.setattr(lifecycle, "sandbox_service", _stub_sandbox_service())

    created = client.post("/v1/sandboxes/sbx-001/snapshots", headers=auth_headers)
    assert created.status_code == 202

    listing = client.get("/v1/snapshots", headers=auth_headers)
    assert listing.status_code == 200
    assert listing.json()["pagination"]["totalItems"] == 1


def test_create_snapshot_returns_501_when_runtime_is_not_supported(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
    tmp_path,
) -> None:
    class StubSandboxService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return {"id": sandbox_id, "status": {"state": "Running"}}

    service = PersistedSnapshotService(
        SQLiteSnapshotRepository(tmp_path / "snapshots.db"),
        StubSandboxService(),
        snapshot_runtime=NoopSnapshotRuntime(),
    )
    monkeypatch.setattr(lifecycle, "snapshot_service", service)
    monkeypatch.setattr(lifecycle, "sandbox_service", _stub_sandbox_service())

    response = client.post("/v1/sandboxes/sbx-001/snapshots", headers=auth_headers)

    assert response.status_code == 501
    assert response.json()["code"] == "SNAPSHOT::NOT_IMPLEMENTED"
