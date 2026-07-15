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

from datetime import datetime, timedelta, timezone

from fastapi.exceptions import HTTPException
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus


def test_get_sandbox_returns_service_payload(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            assert sandbox_id == "sbx-001"
            return Sandbox(
                id=sandbox_id,
                image=ImageSpec(uri="python:3.11"),
                status=SandboxStatus(state="Running"),
                metadata={"team": "infra"},
                entrypoint=["python", "-V"],
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get("/v1/sandboxes/sbx-001", headers=auth_headers)

    assert response.status_code == 200
    payload = response.json()
    assert payload["id"] == "sbx-001"
    assert payload["status"]["state"] == "Running"
    assert payload["image"]["uri"] == "python:3.11"


def test_get_sandbox_propagates_not_found(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            raise HTTPException(
                status_code=404,
                detail={
                    "code": "SANDBOX_NOT_FOUND",
                    "message": f"Sandbox {sandbox_id} not found",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get("/v1/sandboxes/missing", headers=auth_headers)

    assert response.status_code == 404
    assert response.json() == {
        "code": "SANDBOX_NOT_FOUND",
        "message": "Sandbox missing not found",
    }


def test_get_sandbox_omits_none_fields(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            return Sandbox(
                id=sandbox_id,
                image=ImageSpec(uri="python:3.11"),
                status=SandboxStatus(state="Running"),
                metadata=None,
                entrypoint=["python", "-V"],
                expiresAt=None,
                createdAt=now,
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get("/v1/sandboxes/sbx-manual", headers=auth_headers)

    assert response.status_code == 200
    payload = response.json()
    assert "expiresAt" not in payload
    assert "metadata" not in payload
    assert "reason" not in payload["status"]
    assert "message" not in payload["status"]
    assert "lastTransitionAt" not in payload["status"]


def test_get_sandbox_requires_api_key(client: TestClient) -> None:
    response = client.get("/v1/sandboxes/sbx-001")

    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"
