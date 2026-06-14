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
"""
Tests for the Pool SDK layer:
  - Domain models (models/pools.py)
  - API lifecycle models (api/lifecycle/models/pool_*.py)
  - PoolsAdapter (adapters/pools_adapter.py)
  - PoolManager (pool_manager.py)
"""

from __future__ import annotations

from datetime import datetime, timezone
from http import HTTPStatus
from typing import Any
from unittest.mock import AsyncMock, MagicMock, patch

import pytest

from opensandbox.api.lifecycle.models.list_pools_response import ApiListPoolsResponse
from opensandbox.api.lifecycle.models.pool_capacity_spec import ApiPoolCapacitySpec
from opensandbox.api.lifecycle.models.pool_response import ApiPoolResponse
from opensandbox.api.lifecycle.models.pool_status import ApiPoolStatus
from opensandbox.api.lifecycle.types import UNSET
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.pools import (
    CreatePoolParams,
    PoolCapacitySpec,
    PoolInfo,
    PoolListResponse,
    PoolStatus,
    UpdatePoolParams,
)
from opensandbox.pool_manager import PoolManager


# ---------------------------------------------------------------------------
# Domain model tests
# ---------------------------------------------------------------------------

class TestPoolCapacitySpecModel:
    def test_accepts_alias_names(self):
        spec = PoolCapacitySpec(bufferMax=3, bufferMin=1, poolMax=10, poolMin=0)
        assert spec.buffer_max == 3
        assert spec.pool_max == 10

    def test_accepts_snake_names(self):
        spec = PoolCapacitySpec(buffer_max=3, buffer_min=1, pool_max=10, pool_min=0)
        assert spec.buffer_max == 3

    def test_rejects_negative_values(self):
        from pydantic import ValidationError
        with pytest.raises(ValidationError):
            PoolCapacitySpec(bufferMax=-1, bufferMin=0, poolMax=5, poolMin=0)

    def test_zero_values_are_valid(self):
        spec = PoolCapacitySpec(bufferMax=0, bufferMin=0, poolMax=0, poolMin=0)
        assert spec.buffer_max == 0


class TestCreatePoolParamsModel:
    def test_valid_name_accepted(self):
        p = CreatePoolParams(
            name="my-pool",
            template={},
            capacitySpec=PoolCapacitySpec(bufferMax=1, bufferMin=0, poolMax=5, poolMin=0),
        )
        assert p.name == "my-pool"

    def test_invalid_name_rejected(self):
        from pydantic import ValidationError
        with pytest.raises(ValidationError):
            CreatePoolParams(
                name="Invalid_Name",
                template={},
                capacitySpec=PoolCapacitySpec(bufferMax=1, bufferMin=0, poolMax=5, poolMin=0),
            )

    def test_uppercase_name_rejected(self):
        from pydantic import ValidationError
        with pytest.raises(ValidationError):
            CreatePoolParams(
                name="MyPool",
                template={},
                capacitySpec=PoolCapacitySpec(bufferMax=1, bufferMin=0, poolMax=5, poolMin=0),
            )


# ---------------------------------------------------------------------------
# API lifecycle model tests
# ---------------------------------------------------------------------------

class TestApiPoolCapacitySpec:
    def test_to_dict_uses_camel_case_keys(self):
        spec = ApiPoolCapacitySpec(buffer_max=3, buffer_min=1, pool_max=10, pool_min=0)
        d = spec.to_dict()
        assert d["bufferMax"] == 3
        assert d["bufferMin"] == 1
        assert d["poolMax"] == 10
        assert d["poolMin"] == 0

    def test_round_trip(self):
        original = ApiPoolCapacitySpec(buffer_max=5, buffer_min=2, pool_max=20, pool_min=1)
        restored = ApiPoolCapacitySpec.from_dict(original.to_dict())
        assert restored.buffer_max == 5
        assert restored.pool_min == 1


class TestApiPoolResponse:
    def _make_raw(self, name="pool-a", with_status=True, with_created=True) -> ApiPoolResponse:
        cap = ApiPoolCapacitySpec(buffer_max=3, buffer_min=1, pool_max=10, pool_min=0)
        status = ApiPoolStatus(total=2, allocated=1, available=1, revision="r1") if with_status else UNSET
        created_at = datetime(2025, 1, 1, tzinfo=timezone.utc) if with_created else UNSET
        return ApiPoolResponse(name=name, capacity_spec=cap, status=status, created_at=created_at)

    def test_to_dict_includes_all_fields(self):
        obj = self._make_raw()
        d = obj.to_dict()
        assert d["name"] == "pool-a"
        assert "capacitySpec" in d
        assert "status" in d
        assert "createdAt" in d

    def test_to_dict_omits_unset_fields(self):
        obj = self._make_raw(with_status=False, with_created=False)
        d = obj.to_dict()
        assert "status" not in d
        assert "createdAt" not in d

    def test_from_dict_with_full_payload(self):
        payload = {
            "name": "pool-b",
            "capacitySpec": {"bufferMax": 2, "bufferMin": 0, "poolMax": 8, "poolMin": 0},
            "status": {"total": 3, "allocated": 2, "available": 1, "revision": "abc"},
            "createdAt": "2025-06-01T00:00:00+00:00",
        }
        obj = ApiPoolResponse.from_dict(payload)
        assert obj.name == "pool-b"
        assert obj.capacity_spec.pool_max == 8
        assert not isinstance(obj.status, type(UNSET))
        assert obj.status.total == 3

    def test_from_dict_without_status(self):
        payload = {
            "name": "pool-c",
            "capacitySpec": {"bufferMax": 1, "bufferMin": 0, "poolMax": 5, "poolMin": 0},
        }
        obj = ApiPoolResponse.from_dict(payload)
        assert isinstance(obj.status, type(UNSET))


class TestApiListPoolsResponse:
    def test_from_dict_empty_items(self):
        obj = ApiListPoolsResponse.from_dict({"items": []})
        assert obj.items == []

    def test_from_dict_multiple_items(self):
        payload = {
            "items": [
                {
                    "name": "p1",
                    "capacitySpec": {"bufferMax": 1, "bufferMin": 0, "poolMax": 5, "poolMin": 0},
                },
                {
                    "name": "p2",
                    "capacitySpec": {"bufferMax": 2, "bufferMin": 1, "poolMax": 10, "poolMin": 0},
                },
            ]
        }
        obj = ApiListPoolsResponse.from_dict(payload)
        assert len(obj.items) == 2
        assert {p.name for p in obj.items} == {"p1", "p2"}


# ---------------------------------------------------------------------------
# Stub pool service for PoolManager tests
# ---------------------------------------------------------------------------

class _PoolServiceStub:
    """In-memory Pools stub – no HTTP calls."""

    def __init__(self) -> None:
        self._pools: dict[str, PoolInfo] = {}
        self.create_calls: list[CreatePoolParams] = []
        self.update_calls: list[tuple[str, UpdatePoolParams]] = []
        self.delete_calls: list[str] = []

    def _make_pool(self, name: str, cap: PoolCapacitySpec) -> PoolInfo:
        return PoolInfo(
            name=name,
            capacitySpec=cap,
            status=PoolStatus(total=0, allocated=0, available=0, revision="init"),
        )

    async def create_pool(self, params: CreatePoolParams) -> PoolInfo:
        self.create_calls.append(params)
        pool = self._make_pool(params.name, params.capacity_spec)
        self._pools[params.name] = pool
        return pool

    async def get_pool(self, pool_name: str) -> PoolInfo:
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        return self._pools[pool_name]

    async def list_pools(self) -> PoolListResponse:
        return PoolListResponse(items=list(self._pools.values()))

    async def update_pool(self, pool_name: str, params: UpdatePoolParams) -> PoolInfo:
        self.update_calls.append((pool_name, params))
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        updated = self._make_pool(pool_name, params.capacity_spec)
        self._pools[pool_name] = updated
        return updated

    async def delete_pool(self, pool_name: str) -> None:
        self.delete_calls.append(pool_name)
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        del self._pools[pool_name]


# ---------------------------------------------------------------------------
# PoolManager tests (using stub service)
# ---------------------------------------------------------------------------

class TestPoolManagerCreate:
    @pytest.mark.asyncio
    async def test_create_pool_stores_and_returns_pool(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        pool = await mgr.create_pool(
            name="ci-pool",
            template={"spec": {}},
            buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
        )

        assert pool.name == "ci-pool"
        assert pool.capacity_spec.buffer_max == 3
        assert pool.capacity_spec.pool_max == 10

    @pytest.mark.asyncio
    async def test_create_pool_passes_correct_params_to_service(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        await mgr.create_pool(
            name="my-pool",
            template={"spec": {}},
            buffer_max=5, buffer_min=2, pool_max=20, pool_min=1,
        )

        assert len(stub.create_calls) == 1
        params = stub.create_calls[0]
        assert params.name == "my-pool"
        assert params.capacity_spec.buffer_max == 5
        assert params.capacity_spec.pool_min == 1

    @pytest.mark.asyncio
    async def test_create_pool_propagates_service_exception(self):
        stub = _PoolServiceStub()
        stub.create_pool = AsyncMock(
            side_effect=SandboxApiException("already exists", status_code=409)
        )
        mgr = PoolManager(stub, ConnectionConfig())

        with pytest.raises(SandboxApiException) as exc_info:
            await mgr.create_pool("dup", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        assert exc_info.value.status_code == 409


class TestPoolManagerGet:
    @pytest.mark.asyncio
    async def test_get_existing_pool(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("p1", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        pool = await mgr.get_pool("p1")
        assert pool.name == "p1"

    @pytest.mark.asyncio
    async def test_get_missing_pool_raises_exception(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        with pytest.raises(SandboxApiException) as exc_info:
            await mgr.get_pool("ghost")

        assert exc_info.value.status_code == 404


class TestPoolManagerList:
    @pytest.mark.asyncio
    async def test_list_empty(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        result = await mgr.list_pools()
        assert result.items == []

    @pytest.mark.asyncio
    async def test_list_returns_all_pools(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("a", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)
        await mgr.create_pool("b", {}, buffer_max=2, buffer_min=1, pool_max=8, pool_min=0)

        result = await mgr.list_pools()
        assert len(result.items) == 2
        assert {p.name for p in result.items} == {"a", "b"}


class TestPoolManagerUpdate:
    @pytest.mark.asyncio
    async def test_update_pool_capacity(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("p", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        updated = await mgr.update_pool(
            "p", buffer_max=9, buffer_min=3, pool_max=50, pool_min=0
        )
        assert updated.capacity_spec.buffer_max == 9
        assert updated.capacity_spec.pool_max == 50

    @pytest.mark.asyncio
    async def test_update_passes_correct_params_to_service(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("p", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        await mgr.update_pool("p", buffer_max=7, buffer_min=2, pool_max=30, pool_min=0)

        assert len(stub.update_calls) == 1
        name, params = stub.update_calls[0]
        assert name == "p"
        assert params.capacity_spec.buffer_max == 7

    @pytest.mark.asyncio
    async def test_update_missing_pool_raises_exception(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        with pytest.raises(SandboxApiException) as exc_info:
            await mgr.update_pool("ghost", buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        assert exc_info.value.status_code == 404


class TestPoolManagerDelete:
    @pytest.mark.asyncio
    async def test_delete_existing_pool(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("bye", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        await mgr.delete_pool("bye")
        assert "bye" not in stub._pools

    @pytest.mark.asyncio
    async def test_delete_calls_service_with_correct_name(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())
        await mgr.create_pool("to-delete", {}, buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)

        await mgr.delete_pool("to-delete")
        assert stub.delete_calls == ["to-delete"]

    @pytest.mark.asyncio
    async def test_delete_missing_pool_raises_exception(self):
        stub = _PoolServiceStub()
        mgr = PoolManager(stub, ConnectionConfig())

        with pytest.raises(SandboxApiException):
            await mgr.delete_pool("ghost")


# ---------------------------------------------------------------------------
# PoolManager lifecycle tests
# ---------------------------------------------------------------------------

class TestPoolManagerLifecycle:
    @pytest.mark.asyncio
    async def test_async_context_manager_calls_close(self):
        stub = _PoolServiceStub()
        config = ConnectionConfig()
        mgr = PoolManager(stub, config)
        close_called = []

        async def fake_close():
            close_called.append(True)

        mgr.close = fake_close
        async with mgr:
            pass

        assert close_called == [True]

    @pytest.mark.asyncio
    async def test_create_factory_method_returns_manager(self):
        with patch("opensandbox.pool_manager.AdapterFactory") as MockFactory:
            mock_pool_svc = MagicMock()
            MockFactory.return_value.create_pool_service.return_value = mock_pool_svc

            mgr = await PoolManager.create()

        assert isinstance(mgr, PoolManager)
        assert mgr._pool_service is mock_pool_svc


# ---------------------------------------------------------------------------
# PoolsAdapter conversion tests
# ---------------------------------------------------------------------------

class TestPoolsAdapterConversion:
    """Test the static helpers in PoolsAdapter without HTTP."""

    def test_to_api_capacity_maps_fields(self):
        from opensandbox.adapters.pools_adapter import PoolsAdapter

        spec = PoolCapacitySpec(bufferMax=3, bufferMin=1, poolMax=10, poolMin=0)
        api_spec = PoolsAdapter._to_api_capacity(spec)
        assert api_spec.buffer_max == 3
        assert api_spec.pool_max == 10

    def test_from_api_pool_with_status(self):
        from opensandbox.adapters.pools_adapter import PoolsAdapter

        cap = ApiPoolCapacitySpec(buffer_max=2, buffer_min=0, pool_max=8, pool_min=0)
        status = ApiPoolStatus(total=3, allocated=1, available=2, revision="rev-x")
        raw = ApiPoolResponse(
            name="test-pool",
            capacity_spec=cap,
            status=status,
            created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        )
        info = PoolsAdapter._from_api_pool(raw)

        assert info.name == "test-pool"
        assert info.capacity_spec.buffer_max == 2
        assert info.capacity_spec.pool_max == 8
        assert info.status is not None
        assert info.status.total == 3
        assert info.status.revision == "rev-x"
        assert info.created_at is not None

    def test_from_api_pool_without_status(self):
        from opensandbox.adapters.pools_adapter import PoolsAdapter

        cap = ApiPoolCapacitySpec(buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)
        raw = ApiPoolResponse(name="no-status", capacity_spec=cap)
        info = PoolsAdapter._from_api_pool(raw)

        assert info.status is None
        assert info.created_at is None

    def test_from_api_pool_with_unset_status(self):
        from opensandbox.adapters.pools_adapter import PoolsAdapter

        cap = ApiPoolCapacitySpec(buffer_max=1, buffer_min=0, pool_max=5, pool_min=0)
        raw = ApiPoolResponse(name="unset-status", capacity_spec=cap, status=UNSET)
        info = PoolsAdapter._from_api_pool(raw)

        assert info.status is None


# ---------------------------------------------------------------------------
# PoolsAdapter HTTP call tests (mocked httpx)
# ---------------------------------------------------------------------------

def _make_mock_response(status_code: int, json_body: dict) -> MagicMock:
    """Create a fake httpx.Response-like object."""
    resp = MagicMock()
    resp.status_code = status_code
    resp.json.return_value = json_body
    resp.content = b""
    resp.headers = {}
    return resp


def _make_adapter() -> tuple:
    """Return (adapter, mock_async_client)."""
    from opensandbox.adapters.pools_adapter import PoolsAdapter

    config = ConnectionConfig(api_key="test-key")
    with patch("opensandbox.adapters.pools_adapter.httpx.AsyncClient") as MockClient:
        mock_async_http = AsyncMock()
        MockClient.return_value = mock_async_http

        with patch("opensandbox.adapters.pools_adapter.AuthenticatedClient"):
            adapter = PoolsAdapter(config)
            adapter._client = MagicMock()
            adapter._client.raise_on_unexpected_status = False
            adapter._client.get_async_httpx_client.return_value = mock_async_http

    return adapter, mock_async_http


class TestPoolsAdapterHTTP:
    @pytest.mark.asyncio
    async def test_create_pool_calls_post_endpoint(self):
        adapter, mock_http = _make_adapter()
        resp_json = {
            "name": "new-pool",
            "capacitySpec": {"bufferMax": 3, "bufferMin": 1, "poolMax": 10, "poolMin": 0},
            "status": {"total": 0, "allocated": 0, "available": 0, "revision": "init"},
        }
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(201, resp_json)
        )

        params = CreatePoolParams(
            name="new-pool",
            template={},
            capacitySpec=PoolCapacitySpec(bufferMax=3, bufferMin=1, poolMax=10, poolMin=0),
        )
        result = await adapter.create_pool(params)

        assert result.name == "new-pool"
        assert mock_http.request.called
        call_kwargs = mock_http.request.call_args.kwargs
        assert call_kwargs["method"] == "post"
        assert call_kwargs["url"] == "/pools"

    @pytest.mark.asyncio
    async def test_get_pool_calls_correct_url(self):
        adapter, mock_http = _make_adapter()
        resp_json = {
            "name": "my-pool",
            "capacitySpec": {"bufferMax": 2, "bufferMin": 0, "poolMax": 8, "poolMin": 0},
        }
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(200, resp_json)
        )

        result = await adapter.get_pool("my-pool")

        assert result.name == "my-pool"
        call_kwargs = mock_http.request.call_args.kwargs
        assert "/pools/my-pool" in call_kwargs["url"]

    @pytest.mark.asyncio
    async def test_list_pools_returns_all_items(self):
        adapter, mock_http = _make_adapter()
        resp_json = {
            "items": [
                {
                    "name": "p1",
                    "capacitySpec": {"bufferMax": 1, "bufferMin": 0, "poolMax": 5, "poolMin": 0},
                },
                {
                    "name": "p2",
                    "capacitySpec": {"bufferMax": 2, "bufferMin": 1, "poolMax": 10, "poolMin": 0},
                },
            ]
        }
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(200, resp_json)
        )

        result = await adapter.list_pools()

        assert len(result.items) == 2
        assert {p.name for p in result.items} == {"p1", "p2"}
        call_kwargs = mock_http.request.call_args.kwargs
        assert call_kwargs["url"] == "/pools"
        assert call_kwargs["method"] == "get"

    @pytest.mark.asyncio
    async def test_update_pool_calls_put_endpoint(self):
        adapter, mock_http = _make_adapter()
        resp_json = {
            "name": "target",
            "capacitySpec": {"bufferMax": 9, "bufferMin": 3, "poolMax": 50, "poolMin": 0},
        }
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(200, resp_json)
        )

        params = UpdatePoolParams(
            capacitySpec=PoolCapacitySpec(bufferMax=9, bufferMin=3, poolMax=50, poolMin=0)
        )
        result = await adapter.update_pool("target", params)

        assert result.capacity_spec.buffer_max == 9
        call_kwargs = mock_http.request.call_args.kwargs
        assert call_kwargs["method"] == "put"
        assert "/pools/target" in call_kwargs["url"]

    @pytest.mark.asyncio
    async def test_delete_pool_calls_delete_endpoint(self):
        adapter, mock_http = _make_adapter()
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(204, {})
        )

        await adapter.delete_pool("bye-pool")

        call_kwargs = mock_http.request.call_args.kwargs
        assert call_kwargs["method"] == "delete"
        assert "/pools/bye-pool" in call_kwargs["url"]

    @pytest.mark.asyncio
    async def test_create_pool_raises_on_4xx_error(self):
        adapter, mock_http = _make_adapter()
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(409, {"code": "CONFLICT", "message": "exists"})
        )

        params = CreatePoolParams(
            name="dup",
            template={},
            capacitySpec=PoolCapacitySpec(bufferMax=1, bufferMin=0, poolMax=5, poolMin=0),
        )
        with pytest.raises(SandboxApiException):
            await adapter.create_pool(params)

    @pytest.mark.asyncio
    async def test_get_pool_raises_on_404(self):
        adapter, mock_http = _make_adapter()
        mock_http.request = AsyncMock(
            return_value=_make_mock_response(404, {"code": "NOT_FOUND", "message": "not found"})
        )

        with pytest.raises(SandboxApiException):
            await adapter.get_pool("ghost")
