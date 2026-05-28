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

from fastapi import status
from fastapi.exceptions import HTTPException
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus
from tests.test_helpers import minimal_sandbox


def test_pause_route_calls_service_and_returns_202(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[str] = []

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def pause_sandbox(sandbox_id: str) -> None:
            calls.append(sandbox_id)

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post("/v1/sandboxes/sbx-001/pause", headers=auth_headers)

    assert response.status_code == 202
    assert calls == ["sbx-001"]


def test_resume_route_calls_service_and_returns_202(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[str] = []

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def resume_sandbox(sandbox_id: str) -> None:
            calls.append(sandbox_id)

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post("/v1/sandboxes/sbx-001/resume", headers=auth_headers)

    assert response.status_code == 202
    assert calls == ["sbx-001"]


def test_pause_route_propagates_service_http_error(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str) -> Sandbox:
            if sandbox_id == "missing":
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": "SANDBOX_NOT_FOUND",
                        "message": f"Sandbox {sandbox_id} not found",
                    },
                )
            now = datetime.now(timezone.utc)
            return Sandbox(
                id=sandbox_id,
                image=ImageSpec(uri="t"),
                status=SandboxStatus(state="Running"),
                metadata={},
                entrypoint=["sh"],
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
            )

        @staticmethod
        def pause_sandbox(sandbox_id: str) -> None:
            raise AssertionError("pause_sandbox should not be called when get_sandbox fails")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post("/v1/sandboxes/missing/pause", headers=auth_headers)

    assert response.status_code == 404
    assert response.json() == {
        "code": "SANDBOX_NOT_FOUND",
        "message": "Sandbox missing not found",
    }


def test_pause_route_requires_api_key(client: TestClient) -> None:
    response = client.post("/v1/sandboxes/sbx-001/pause")

    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"
