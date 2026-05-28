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

from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import CreateSandboxResponse, SandboxStatus
from opensandbox_server.services.constants import SandboxErrorCodes
from tests.test_helpers import minimal_sandbox


def test_create_sandbox_returns_202_and_service_payload(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    calls: list[object] = []

    class StubService:
        @staticmethod
        async def create_sandbox(request) -> CreateSandboxResponse:
            calls.append(request)
            return CreateSandboxResponse(
                id="sbx-001",
                status=SandboxStatus(state="Pending"),
                metadata={"project": "test-project"},
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=["python", "-c", "print('Hello from sandbox')"],
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json=sample_sandbox_request,
    )

    assert response.status_code == 202
    payload = response.json()
    assert payload["id"] == "sbx-001"
    assert payload["status"]["state"] == "Pending"
    assert payload["metadata"]["project"] == "test-project"
    assert payload["entrypoint"] == ["python", "-c", "print('Hello from sandbox')"]
    assert len(calls) == 1
    assert calls[0].image.uri == "python:3.11"


def test_create_sandbox_rejects_invalid_extensions(
    client: TestClient,
    auth_headers: dict,
    sample_sandbox_request: dict,
) -> None:
    payload = {
        **sample_sandbox_request,
        "extensions": {"access.renew.extend.seconds": "not-an-int"},
    }
    response = client.post("/v1/sandboxes", headers=auth_headers, json=payload)

    assert response.status_code == 400
    payload = response.json()
    code = payload.get("code")
    if code is None and isinstance(payload.get("detail"), dict):
        code = payload["detail"].get("code")
    assert code == SandboxErrorCodes.INVALID_PARAMETER


def test_create_sandbox_rejects_invalid_request(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.post(
        "/v1/sandboxes",
        headers=auth_headers,
        json={"timeout": 10},
    )

    assert response.status_code == 422


def test_delete_sandbox_returns_204_and_calls_service(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[str] = []

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def delete_sandbox(sandbox_id: str) -> None:
            calls.append(sandbox_id)

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.delete("/v1/sandboxes/sbx-001", headers=auth_headers)

    assert response.status_code == 204
    assert response.text == ""
    assert calls == ["sbx-001"]


def test_delete_sandbox_requires_api_key(client: TestClient) -> None:
    response = client.delete("/v1/sandboxes/sbx-001")

    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"
