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

"""
Integration-style tests for Pool API routes (opensandbox_server/api/pool.py).

Routes are exercised via FastAPI TestClient.  The K8s PoolService is patched
so no real cluster connection is needed.
"""

from unittest.mock import MagicMock, patch
from fastapi.testclient import TestClient
from fastapi import HTTPException, status as http_status

from opensandbox_server.api.schema import (
    CreatePoolRequest,
    ListPoolsResponse,
    PoolCapacitySpec,
    PoolResponse,
    PoolStatus,
    UpdatePoolRequest,
)
from opensandbox_server.services.constants import SandboxErrorCodes


_POOL_SERVICE_PATCH = "opensandbox_server.api.pool._get_pool_service"


def _cap(buffer_max=3, buffer_min=1, pool_max=10, pool_min=0) -> PoolCapacitySpec:
    return PoolCapacitySpec(
        bufferMax=buffer_max,
        bufferMin=buffer_min,
        poolMax=pool_max,
        poolMin=pool_min,
    )


def _pool_response(
    name: str = "test-pool",
    buffer_max: int = 3,
    pool_max: int = 10,
    total: int = 2,
    allocated: int = 1,
    available: int = 1,
) -> PoolResponse:
    return PoolResponse(
        name=name,
        capacitySpec=_cap(buffer_max=buffer_max, pool_max=pool_max),
        status=PoolStatus(
            total=total,
            allocated=allocated,
            available=available,
            revision="rev-1",
        ),
    )


def _create_request_body(name: str = "test-pool") -> dict:
    return {
        "name": name,
        "template": {
            "spec": {
                "containers": [
                    {
                        "name": "sandbox",
                        "image": "python:3.11",
                        "command": ["tail", "-f", "/dev/null"],
                    }
                ]
            }
        },
        "capacitySpec": {
            "bufferMax": 3,
            "bufferMin": 1,
            "poolMax": 10,
            "poolMin": 0,
        },
    }


class TestPoolAuthentication:
    def test_list_pools_without_api_key_returns_401(self, client: TestClient):
        response = client.get("/pools")
        assert response.status_code == 401

    def test_pool_routes_exist_on_v1_prefix(self, client: TestClient, auth_headers: dict):
        """Verify the /v1/pools routes are registered (even if they return 501 on docker runtime)."""
        with patch(_POOL_SERVICE_PATCH) as mock_svc_factory:
            mock_svc_factory.side_effect = HTTPException(
                status_code=http_status.HTTP_501_NOT_IMPLEMENTED,
                detail={"code": "X", "message": "y"},
            )
            response = client.get("/v1/pools", headers=auth_headers)
        assert response.status_code == 501


class TestPoolNotSupportedOnDockerRuntime:
    """Pool endpoints return 501 when PoolService raises 501 (non-k8s runtime)."""

    def test_list_pools_returns_501(self, client: TestClient, auth_headers: dict):
        with patch(_POOL_SERVICE_PATCH, side_effect=HTTPException(
            status_code=501,
            detail={"code": SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED, "message": "not k8s"},
        )):
            response = client.get("/pools", headers=auth_headers)
        assert response.status_code == 501
        assert SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED in response.json()["code"]

    def test_create_pool_returns_501(self, client: TestClient, auth_headers: dict):
        with patch(_POOL_SERVICE_PATCH, side_effect=HTTPException(
            status_code=501,
            detail={"code": SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED, "message": "not k8s"},
        )):
            response = client.post("/pools", json=_create_request_body(), headers=auth_headers)
        assert response.status_code == 501


class TestCreatePoolRoute:
    def test_create_pool_success_returns_201(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.create_pool.return_value = _pool_response(name="new-pool")

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.post("/pools", json=_create_request_body("new-pool"), headers=auth_headers)

        assert response.status_code == 201
        body = response.json()
        assert body["name"] == "new-pool"
        assert body["capacitySpec"]["bufferMax"] == 3
        assert body["status"]["total"] == 2

    def test_create_pool_missing_name_returns_422(self, client: TestClient, auth_headers: dict):
        body = _create_request_body()
        del body["name"]
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.post("/pools", json=body, headers=auth_headers)
        assert response.status_code == 422

    def test_create_pool_missing_template_returns_422(self, client: TestClient, auth_headers: dict):
        body = _create_request_body()
        del body["template"]
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.post("/pools", json=body, headers=auth_headers)
        assert response.status_code == 422

    def test_create_pool_missing_capacity_spec_returns_422(self, client: TestClient, auth_headers: dict):
        body = _create_request_body()
        del body["capacitySpec"]
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.post("/pools", json=body, headers=auth_headers)
        assert response.status_code == 422

    def test_create_pool_invalid_name_pattern_returns_422(self, client: TestClient, auth_headers: dict):
        """Pool name must be a valid k8s name (no uppercase, no spaces)."""
        body = _create_request_body("Invalid_Name")
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.post("/pools", json=body, headers=auth_headers)
        assert response.status_code == 422

    def test_create_pool_negative_buffer_max_returns_422(self, client: TestClient, auth_headers: dict):
        body = _create_request_body()
        body["capacitySpec"]["bufferMax"] = -1
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.post("/pools", json=body, headers=auth_headers)
        assert response.status_code == 422

    def test_create_pool_duplicate_returns_409(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.create_pool.side_effect = HTTPException(
            status_code=409,
            detail={
                "code": SandboxErrorCodes.K8S_POOL_ALREADY_EXISTS,
                "message": "Pool 'dup-pool' already exists.",
            },
        )
        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.post("/pools", json=_create_request_body("dup-pool"), headers=auth_headers)

        assert response.status_code == 409
        assert SandboxErrorCodes.K8S_POOL_ALREADY_EXISTS in response.json()["code"]

    def test_create_pool_passes_request_to_service(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.create_pool.return_value = _pool_response()

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            client.post("/pools", json=_create_request_body("my-pool"), headers=auth_headers)

        call_args = mock_svc.create_pool.call_args
        req: CreatePoolRequest = call_args.args[0]
        assert req.name == "my-pool"
        assert req.capacity_spec.buffer_max == 3
        assert req.capacity_spec.pool_max == 10


class TestListPoolsRoute:
    def test_list_pools_returns_200_and_items(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.list_pools.return_value = ListPoolsResponse(
            items=[_pool_response("pool-a"), _pool_response("pool-b")]
        )

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.get("/pools", headers=auth_headers)

        assert response.status_code == 200
        body = response.json()
        assert len(body["items"]) == 2
        names = {p["name"] for p in body["items"]}
        assert names == {"pool-a", "pool-b"}

    def test_list_pools_response_has_status_fields(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.list_pools.return_value = ListPoolsResponse(
            items=[_pool_response("p", total=5, allocated=3, available=2)]
        )
        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.get("/pools", headers=auth_headers)

        pool = response.json()["items"][0]
        assert pool["status"]["total"] == 5
        assert pool["status"]["allocated"] == 3
        assert pool["status"]["available"] == 2


class TestGetPoolRoute:
    def test_get_pool_success_returns_200(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.get_pool.return_value = _pool_response(name="my-pool")

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.get("/pools/my-pool", headers=auth_headers)

        assert response.status_code == 200
        assert response.json()["name"] == "my-pool"

    def test_get_pool_not_found_returns_404(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.get_pool.side_effect = HTTPException(
            status_code=404,
            detail={
                "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                "message": "Pool 'ghost' not found.",
            },
        )
        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.get("/pools/ghost", headers=auth_headers)

        assert response.status_code == 404
        assert SandboxErrorCodes.K8S_POOL_NOT_FOUND in response.json()["code"]


class TestUpdatePoolRoute:
    def _update_body(self, buffer_max=5, pool_max=20) -> dict:
        return {
            "capacitySpec": {
                "bufferMax": buffer_max,
                "bufferMin": 1,
                "poolMax": pool_max,
                "poolMin": 0,
            }
        }

    def test_update_pool_success_returns_200(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.update_pool.return_value = _pool_response(buffer_max=5, pool_max=20)

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.put("/pools/my-pool", json=self._update_body(), headers=auth_headers)

        assert response.status_code == 200
        assert response.json()["capacitySpec"]["bufferMax"] == 5

    def test_update_pool_calls_service_with_name_and_request(
        self, client: TestClient, auth_headers: dict
    ):
        mock_svc = MagicMock()
        mock_svc.update_pool.return_value = _pool_response()

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            client.put("/pools/target", json=self._update_body(buffer_max=9), headers=auth_headers)

        call_args = mock_svc.update_pool.call_args
        assert call_args.args[0] == "target"
        req: UpdatePoolRequest = call_args.args[1]
        assert req.capacity_spec.buffer_max == 9

    def test_update_pool_not_found_returns_404(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.update_pool.side_effect = HTTPException(
            status_code=404,
            detail={
                "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                "message": "Pool 'x' not found.",
            },
        )
        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.put("/pools/x", json=self._update_body(), headers=auth_headers)

        assert response.status_code == 404

    def test_update_pool_missing_capacity_spec_returns_422(
        self, client: TestClient, auth_headers: dict
    ):
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.put("/pools/p", json={}, headers=auth_headers)
        assert response.status_code == 422

    def test_update_pool_negative_pool_max_returns_422(
        self, client: TestClient, auth_headers: dict
    ):
        with patch(_POOL_SERVICE_PATCH, return_value=MagicMock()):
            response = client.put(
                "/pools/p",
                json={"capacitySpec": {"bufferMax": 1, "bufferMin": 0, "poolMax": -5, "poolMin": 0}},
                headers=auth_headers,
            )
        assert response.status_code == 422


class TestDeletePoolRoute:
    def test_delete_pool_success_returns_204(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.delete_pool.return_value = None

        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.delete("/pools/my-pool", headers=auth_headers)

        assert response.status_code == 204
        assert response.content == b""

    def test_delete_pool_not_found_returns_404(self, client: TestClient, auth_headers: dict):
        mock_svc = MagicMock()
        mock_svc.delete_pool.side_effect = HTTPException(
            status_code=404,
            detail={
                "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                "message": "Pool 'gone' not found.",
            },
        )
        with patch(_POOL_SERVICE_PATCH, return_value=mock_svc):
            response = client.delete("/pools/gone", headers=auth_headers)

        assert response.status_code == 404
        assert SandboxErrorCodes.K8S_POOL_NOT_FOUND in response.json()["code"]
