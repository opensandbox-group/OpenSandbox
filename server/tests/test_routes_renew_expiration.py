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
from opensandbox_server.api.schema import RenewSandboxExpirationResponse
from tests.test_helpers import minimal_sandbox


def test_renew_expiration_returns_updated_timestamp(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    target = datetime.now(timezone.utc) + timedelta(hours=2)
    calls: list[tuple[str, datetime]] = []

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def renew_expiration(sandbox_id: str, request) -> RenewSandboxExpirationResponse:
            calls.append((sandbox_id, request.expires_at))
            return RenewSandboxExpirationResponse(expiresAt=target)

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes/sbx-001/renew-expiration",
        headers=auth_headers,
        json={"expiresAt": target.isoformat()},
    )

    assert response.status_code == 200
    expires_at = datetime.fromisoformat(response.json()["expiresAt"].replace("Z", "+00:00"))
    assert expires_at == target
    assert calls == [("sbx-001", target)]


def test_renew_expiration_rejects_invalid_payload(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.post(
        "/v1/sandboxes/sbx-001/renew-expiration",
        headers=auth_headers,
        json={"expiresAt": "not-a-datetime"},
    )

    assert response.status_code == 422


def test_renew_expiration_propagates_service_http_error(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def renew_expiration(sandbox_id: str, request) -> RenewSandboxExpirationResponse:
            raise HTTPException(
                status_code=409,
                detail={
                    "code": "INVALID_EXPIRES_AT",
                    "message": f"Requested expiresAt is not valid for sandbox {sandbox_id}",
                },
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.post(
        "/v1/sandboxes/sbx-001/renew-expiration",
        headers=auth_headers,
        json={"expiresAt": "2030-01-01T00:00:00Z"},
    )

    assert response.status_code == 409
    assert response.json() == {
        "code": "INVALID_EXPIRES_AT",
        "message": "Requested expiresAt is not valid for sandbox sbx-001",
    }


def test_renew_expiration_requires_api_key(client: TestClient) -> None:
    response = client.post(
        "/v1/sandboxes/sbx-001/renew-expiration",
        json={"expiresAt": "2030-01-01T00:00:00Z"},
    )

    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"
