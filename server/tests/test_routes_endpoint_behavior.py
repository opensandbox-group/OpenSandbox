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

from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import Endpoint
from tests.test_helpers import minimal_sandbox


def test_get_endpoint_returns_service_result(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[tuple[str, int]] = []

    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, **kwargs) -> Endpoint:
            calls.append((sandbox_id, port))
            return Endpoint(endpoint="10.57.1.91:40109/proxy/44772")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/endpoints/44772",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["endpoint"] == "10.57.1.91:40109/proxy/44772"
    assert calls == [("sbx-001", 44772)]


def test_get_endpoint_use_server_proxy_rewrites_url(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return minimal_sandbox(sandbox_id)

        @staticmethod
        def get_endpoint(sandbox_id: str, port: int, **kwargs) -> Endpoint:
            return Endpoint(endpoint="10.57.1.91:40109/proxy/44772")

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/endpoints/44772",
        params={"use_server_proxy": "true"},
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["endpoint"] == "testserver/sandboxes/sbx-001/proxy/44772"


def test_get_endpoint_rejects_non_numeric_port(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.get(
        "/v1/sandboxes/sbx-001/endpoints/not-a-port",
        headers=auth_headers,
    )

    assert response.status_code == 422
