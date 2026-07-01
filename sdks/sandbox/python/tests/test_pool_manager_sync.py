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
Tests for the synchronous Pool SDK layer:
  - PoolsSync Protocol (sync/services/pool.py)
  - PoolsAdapterSync (sync/adapters/pools_adapter.py)
  - PoolManagerSync (sync/pool_manager.py)
  - AdapterFactorySync.create_pool_service() (sync/adapters/factory.py)
  - Public export via opensandbox.sync and opensandbox root
"""

from __future__ import annotations

from datetime import datetime, timezone
from http import HTTPStatus
from typing import Any
from unittest.mock import MagicMock, patch

import pytest

from opensandbox.api.lifecycle.models.list_pools_response import ApiListPoolsResponse
from opensandbox.api.lifecycle.models.pool_capacity_spec import ApiPoolCapacitySpec
from opensandbox.api.lifecycle.models.pool_response import ApiPoolResponse
from opensandbox.api.lifecycle.models.pool_status import ApiPoolStatus
from opensandbox.api.lifecycle.types import UNSET, Response
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.pools import (
    CreatePoolParams,
    PoolCapacitySpec,
    PoolInfo,
    PoolListResponse,
    PoolStatus,
    UpdatePoolParams,
)
from opensandbox.sync.pool_manager import PoolManagerSync
from opensandbox.sync.services.pool import PoolsSync


# ---------------------------------------------------------------------------
# Helpers / fixtures
# ---------------------------------------------------------------------------

def _make_capacity_spec(
    buffer_max: int = 3, buffer_min: int = 1, pool_max: int = 10, pool_min: int = 0
) -> ApiPoolCapacitySpec:
    return ApiPoolCapacitySpec(
        buffer_max=buffer_max, buffer_min=buffer_min,
        pool_max=pool_max, pool_min=pool_min,
    )


def _make_pool_response(
    name: str = "test-pool",
    buffer_max: int = 3,
    pool_max: int = 10,
    with_status: bool = True,
    with_created: bool = True,
) -> ApiPoolResponse:
    status = (
        ApiPoolStatus(total=2, allocated=1, available=1, revision="rev-1")
        if with_status else UNSET
    )
    created_at = (
        datetime(2025, 6, 1, tzinfo=timezone.utc) if with_created else UNSET
    )
    return ApiPoolResponse(
        name=name,
        capacity_spec=_make_capacity_spec(buffer_max=buffer_max, pool_max=pool_max),
        status=status,
        created_at=created_at,
    )


def _make_http_response(status_code: int, parsed: Any) -> Response:
    """Wrap a parsed object in the API Response envelope."""
    return Response(
        status_code=HTTPStatus(status_code),
        content=b"",
        headers={},
        parsed=parsed,
    )


# ---------------------------------------------------------------------------
# Stub PoolsSync service
# ---------------------------------------------------------------------------

class _PoolServiceSyncStub(PoolsSync):
    """In-memory PoolsSync stub — no HTTP calls."""

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

    def create_pool(self, params: CreatePoolParams) -> PoolInfo:
        self.create_calls.append(params)
        pool = self._make_pool(params.name, params.capacity_spec)
        self._pools[params.name] = pool
        return pool

    def get_pool(self, pool_name: str) -> PoolInfo:
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        return self._pools[pool_name]

    def list_pools(self) -> PoolListResponse:
        return PoolListResponse(items=list(self._pools.values()))

    def update_pool(self, pool_name: str, params: UpdatePoolParams) -> PoolInfo:
        self.update_calls.append((pool_name, params))
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        updated = self._make_pool(pool_name, params.capacity_spec)
        self._pools[pool_name] = updated
        return updated

    def delete_pool(self, pool_name: str) -> None:
        self.delete_calls.append(pool_name)
        if pool_name not in self._pools:
            raise SandboxApiException(message=f"Pool '{pool_name}' not found.", status_code=404)
        del self._pools[pool_name]


def _make_manager() -> tuple[PoolManagerSync, _PoolServiceSyncStub]:
    stub = _PoolServiceSyncStub()
    manager = PoolManagerSync(stub, ConnectionConfigSync())
    return manager, stub


# ===========================================================================
# PoolsSync Protocol structural test
# ===========================================================================

class TestPoolsSyncProtocol:
    def test_protocol_methods_present(self):
        """The Protocol exposes the five expected methods."""
        methods = {"create_pool", "get_pool", "list_pools", "update_pool", "delete_pool"}
        assert methods.issubset(dir(PoolsSync))

    def test_stub_satisfies_protocol(self):
        """_PoolServiceSyncStub satisfies the PoolsSync structural Protocol."""
        stub = _PoolServiceSyncStub()
        # Runtime isinstance check via typing.runtime_checkable is not mandatory
        # (Protocol is not decorated with @runtime_checkable), but we can confirm
        # the attribute surface matches.
        for method in ("create_pool", "get_pool", "list_pools", "update_pool", "delete_pool"):
            assert callable(getattr(stub, method, None)), f"Missing: {method}"


# ===========================================================================
# PoolManagerSync.create_pool
# ===========================================================================

class TestPoolManagerSyncCreatePool:
    def test_returns_pool_info_with_correct_fields(self):
        manager, _ = _make_manager()
        pool = manager.create_pool(
            name="ci-pool",
            template={"spec": {}},
            buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
        )
        assert pool.name == "ci-pool"
        assert pool.capacity_spec.buffer_max == 3
        assert pool.capacity_spec.pool_max == 10

    def test_delegates_full_request_to_service(self):
        manager, stub = _make_manager()
        manager.create_pool(
            name="my-pool",
            template={"spec": {"containers": []}},
            buffer_max=5, buffer_min=2, pool_max=20, pool_min=1,
        )
        assert len(stub.create_calls) == 1
        params = stub.create_calls[0]
        assert params.name == "my-pool"
        assert params.capacity_spec.buffer_max == 5
        assert params.capacity_spec.pool_min == 1

    def test_propagates_sandbox_api_exception_on_conflict(self):
        manager, stub = _make_manager()
        stub.create_pool = MagicMock(
            side_effect=SandboxApiException(message="already exists", status_code=409)
        )
        with pytest.raises(SandboxApiException) as exc_info:
            manager.create_pool(
                name="dup",
                template={},
                buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
            )
        assert exc_info.value.status_code == 409

    def test_propagates_sandbox_api_exception_on_501(self):
        manager, stub = _make_manager()
        stub.create_pool = MagicMock(
            side_effect=SandboxApiException(message="not supported", status_code=501)
        )
        with pytest.raises(SandboxApiException):
            manager.create_pool(
                name="p",
                template={},
                buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
            )

    def test_template_passed_through(self):
        manager, stub = _make_manager()
        tmpl = {"spec": {"containers": [{"name": "sbx", "image": "python:3.11"}]}}
        manager.create_pool(
            name="p",
            template=tmpl,
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        assert stub.create_calls[0].template == tmpl


# ===========================================================================
# PoolManagerSync.get_pool
# ===========================================================================

class TestPoolManagerSyncGetPool:
    def test_returns_existing_pool(self):
        manager, _ = _make_manager()
        manager.create_pool(
            name="p1", template={},
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        pool = manager.get_pool("p1")
        assert pool.name == "p1"

    def test_raises_404_for_missing_pool(self):
        manager, _ = _make_manager()
        with pytest.raises(SandboxApiException) as exc_info:
            manager.get_pool("ghost")
        assert exc_info.value.status_code == 404

    def test_delegates_with_correct_pool_name(self):
        manager, stub = _make_manager()
        stub.get_pool = MagicMock(
            return_value=PoolInfo(
                name="target",
                capacitySpec=PoolCapacitySpec(
                    bufferMax=1, bufferMin=0, poolMax=5, poolMin=0
                ),
            )
        )
        manager.get_pool("target")
        stub.get_pool.assert_called_once_with("target")

    def test_returns_pool_with_status(self):
        manager, stub = _make_manager()
        expected_pool = PoolInfo(
            name="p",
            capacitySpec=PoolCapacitySpec(bufferMax=3, bufferMin=1, poolMax=10, poolMin=0),
            status=PoolStatus(total=5, allocated=2, available=3, revision="v1"),
        )
        stub.get_pool = MagicMock(return_value=expected_pool)
        pool = manager.get_pool("p")
        assert pool.status is not None
        assert pool.status.total == 5
        assert pool.status.revision == "v1"


# ===========================================================================
# PoolManagerSync.list_pools
# ===========================================================================

class TestPoolManagerSyncListPools:
    def test_returns_empty_list_when_no_pools(self):
        manager, _ = _make_manager()
        result = manager.list_pools()
        assert result.items == []

    def test_returns_all_pools(self):
        manager, _ = _make_manager()
        for name in ("a", "b", "c"):
            manager.create_pool(
                name=name, template={},
                buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
            )
        result = manager.list_pools()
        assert len(result.items) == 3
        names = {p.name for p in result.items}
        assert names == {"a", "b", "c"}

    def test_delegates_to_pool_service(self):
        manager, stub = _make_manager()
        stub.list_pools = MagicMock(return_value=PoolListResponse(items=[]))
        manager.list_pools()
        stub.list_pools.assert_called_once()

    def test_raises_501_on_non_kubernetes(self):
        manager, stub = _make_manager()
        stub.list_pools = MagicMock(
            side_effect=SandboxApiException(message="not supported", status_code=501)
        )
        with pytest.raises(SandboxApiException) as exc_info:
            manager.list_pools()
        assert exc_info.value.status_code == 501


# ===========================================================================
# PoolManagerSync.update_pool
# ===========================================================================

class TestPoolManagerSyncUpdatePool:
    def test_updates_capacity_and_returns_new_pool_info(self):
        manager, _ = _make_manager()
        manager.create_pool(
            name="p", template={},
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        updated = manager.update_pool(
            "p", buffer_max=9, buffer_min=3, pool_max=50, pool_min=0,
        )
        assert updated.capacity_spec.buffer_max == 9
        assert updated.capacity_spec.pool_max == 50

    def test_delegates_with_correct_pool_name_and_params(self):
        manager, stub = _make_manager()
        manager.create_pool(
            name="p", template={},
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        manager.update_pool(
            "p", buffer_max=7, buffer_min=2, pool_max=30, pool_min=0,
        )
        assert len(stub.update_calls) == 1
        pool_name, params = stub.update_calls[0]
        assert pool_name == "p"
        assert params.capacity_spec.buffer_max == 7
        assert params.capacity_spec.pool_max == 30

    def test_raises_404_for_missing_pool(self):
        manager, _ = _make_manager()
        with pytest.raises(SandboxApiException) as exc_info:
            manager.update_pool(
                "ghost", buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
            )
        assert exc_info.value.status_code == 404

    def test_all_capacity_fields_forwarded(self):
        manager, stub = _make_manager()
        manager.create_pool(
            name="p", template={},
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        manager.update_pool(
            "p", buffer_max=7, buffer_min=3, pool_max=30, pool_min=2,
        )
        _, params = stub.update_calls[0]
        cap = params.capacity_spec
        assert cap.buffer_max == 7
        assert cap.buffer_min == 3
        assert cap.pool_max == 30
        assert cap.pool_min == 2


# ===========================================================================
# PoolManagerSync.delete_pool
# ===========================================================================

class TestPoolManagerSyncDeletePool:
    def test_deletes_existing_pool(self):
        manager, stub = _make_manager()
        manager.create_pool(
            name="bye", template={},
            buffer_max=1, buffer_min=0, pool_max=5, pool_min=0,
        )
        manager.delete_pool("bye")
        assert "bye" in stub.delete_calls
        assert "bye" not in stub._pools

    def test_delegates_with_correct_pool_name(self):
        manager, stub = _make_manager()
        stub.delete_pool = MagicMock(return_value=None)
        manager.delete_pool("to-delete")
        stub.delete_pool.assert_called_once_with("to-delete")

    def test_raises_404_for_missing_pool(self):
        manager, _ = _make_manager()
        with pytest.raises(SandboxApiException) as exc_info:
            manager.delete_pool("ghost")
        assert exc_info.value.status_code == 404

    def test_returns_none_on_success(self):
        manager, stub = _make_manager()
        stub.delete_pool = MagicMock(return_value=None)
        result = manager.delete_pool("p")
        assert result is None


# ===========================================================================
# PoolManagerSync.create factory
# ===========================================================================

class TestPoolManagerSyncFactory:
    def test_create_returns_instance(self):
        manager = PoolManagerSync.create(ConnectionConfigSync())
        assert isinstance(manager, PoolManagerSync)
        manager.close()

    def test_create_without_args_uses_defaults(self):
        manager = PoolManagerSync.create()
        assert isinstance(manager, PoolManagerSync)
        manager.close()

    def test_create_with_api_key(self):
        config = ConnectionConfigSync(api_key="secret-key")
        manager = PoolManagerSync.create(config)
        assert manager.connection_config.get_api_key() == "secret-key"
        manager.close()

    def test_create_with_domain(self):
        config = ConnectionConfigSync(domain="api.example.com")
        manager = PoolManagerSync.create(config)
        assert "api.example.com" in manager.connection_config.get_base_url()
        manager.close()

    def test_exposes_all_crud_methods(self):
        manager = PoolManagerSync.create()
        for method in ("create_pool", "get_pool", "list_pools", "update_pool", "delete_pool", "close"):
            assert callable(getattr(manager, method, None)), f"Missing method: {method}"
        manager.close()


# ===========================================================================
# PoolManagerSync.close / context manager
# ===========================================================================

class TestPoolManagerSyncLifecycle:
    def test_close_calls_transport_cleanup(self):
        stub = _PoolServiceSyncStub()
        config = ConnectionConfigSync()
        manager = PoolManagerSync(stub, config)

        with patch.object(ConnectionConfigSync, "close_transport_if_owned") as mock_close:
            manager.close()
            mock_close.assert_called_once()

    def test_close_does_not_raise_on_error(self):
        stub = _PoolServiceSyncStub()
        config = ConnectionConfigSync()
        manager = PoolManagerSync(stub, config)

        with patch.object(
            ConnectionConfigSync, "close_transport_if_owned", side_effect=RuntimeError("oops")
        ):
            # Should swallow the exception and not propagate it.
            manager.close()

    def test_context_manager_calls_close(self):
        stub = _PoolServiceSyncStub()
        config = ConnectionConfigSync()
        manager = PoolManagerSync(stub, config)

        with patch.object(ConnectionConfigSync, "close_transport_if_owned") as mock_close:
            with manager:
                pass
            mock_close.assert_called_once()

    def test_context_manager_exits_on_exception(self):
        stub = _PoolServiceSyncStub()
        config = ConnectionConfigSync()
        manager = PoolManagerSync(stub, config)

        with patch.object(ConnectionConfigSync, "close_transport_if_owned") as mock_close:
            with pytest.raises(ValueError):
                with manager:
                    raise ValueError("inner error")
            # close() must still be called even when an exception was raised.
            mock_close.assert_called_once()

    def test_context_manager_returns_self(self):
        manager, _ = _make_manager()
        with manager as m:
            assert m is manager


# ===========================================================================
# PoolsAdapterSync HTTP behaviour (patching sync_detailed calls)
# ===========================================================================

class TestPoolsAdapterSync:
    """
    Tests that exercise PoolsAdapterSync method signatures and HTTP routing,
    by patching the module-level sync_detailed functions in the API layer.
    """

    def _make_config(self) -> ConnectionConfigSync:
        return ConnectionConfigSync(api_key="test-key").with_transport_if_missing()

    def test_create_pool_calls_post_pools_sync_detailed(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("new-pool")
        resp = _make_http_response(201, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.post_pools.sync_detailed",
            return_value=resp,
        ) as mock_call:
            result = adapter.create_pool(
                CreatePoolParams(
                    name="new-pool",
                    template={},
                    capacitySpec=PoolCapacitySpec(
                        bufferMax=3, bufferMin=1, poolMax=10, poolMin=0
                    ),
                )
            )
        mock_call.assert_called_once()
        assert result.name == "new-pool"

    def test_get_pool_calls_get_pools_pool_name_sync_detailed(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p1")
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ) as mock_call:
            result = adapter.get_pool("p1")
        mock_call.assert_called_once()
        assert result.name == "p1"

    def test_list_pools_calls_get_pools_sync_detailed(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw_list = ApiListPoolsResponse(
            items=[_make_pool_response("a"), _make_pool_response("b")]
        )
        resp = _make_http_response(200, raw_list)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools.sync_detailed",
            return_value=resp,
        ) as mock_call:
            result = adapter.list_pools()
        mock_call.assert_called_once()
        assert len(result.items) == 2

    def test_update_pool_calls_put_pools_pool_name_sync_detailed(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p", buffer_max=9, pool_max=50)
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.put_pools_pool_name.sync_detailed",
            return_value=resp,
        ) as mock_call:
            result = adapter.update_pool(
                "p",
                UpdatePoolParams(
                    capacitySpec=PoolCapacitySpec(
                        bufferMax=9, bufferMin=3, poolMax=50, poolMin=0
                    )
                ),
            )
        mock_call.assert_called_once()
        assert result.capacity_spec.buffer_max == 9
        assert result.capacity_spec.pool_max == 50

    def test_delete_pool_calls_delete_pools_pool_name_sync_detailed(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        resp = _make_http_response(204, None)

        with patch(
            "opensandbox.api.lifecycle.api.pools.delete_pools_pool_name.sync_detailed",
            return_value=resp,
        ) as mock_call:
            result = adapter.delete_pool("bye")
        mock_call.assert_called_once()
        assert result is None

    def test_create_pool_raises_sandbox_api_exception_on_409(self):
        from opensandbox.api.lifecycle.models.error_response import ErrorResponse
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        err_body = ErrorResponse(code="POOL_ALREADY_EXISTS", message="already exists")
        resp = _make_http_response(409, err_body)

        with patch(
            "opensandbox.api.lifecycle.api.pools.post_pools.sync_detailed",
            return_value=resp,
        ):
            with pytest.raises(SandboxApiException):
                adapter.create_pool(
                    CreatePoolParams(
                        name="dup",
                        template={},
                        capacitySpec=PoolCapacitySpec(
                            bufferMax=1, bufferMin=0, poolMax=5, poolMin=0
                        ),
                    )
                )

    def test_get_pool_raises_sandbox_api_exception_on_404(self):
        from opensandbox.api.lifecycle.models.error_response import ErrorResponse
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        err_body = ErrorResponse(code="NOT_FOUND", message="not found")
        resp = _make_http_response(404, err_body)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            with pytest.raises(SandboxApiException):
                adapter.get_pool("ghost")

    def test_list_pools_raises_on_501(self):
        from opensandbox.api.lifecycle.models.error_response import ErrorResponse
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        err_body = ErrorResponse(code="NOT_SUPPORTED", message="non-k8s")
        resp = _make_http_response(501, err_body)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools.sync_detailed",
            return_value=resp,
        ):
            with pytest.raises(SandboxApiException):
                adapter.list_pools()

    def test_delete_pool_raises_on_404(self):
        from opensandbox.api.lifecycle.models.error_response import ErrorResponse
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        err_body = ErrorResponse(code="NOT_FOUND", message="not found")
        resp = _make_http_response(404, err_body)

        with patch(
            "opensandbox.api.lifecycle.api.pools.delete_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            with pytest.raises(SandboxApiException):
                adapter.delete_pool("ghost")


# ===========================================================================
# PoolsAdapterSync mapping helpers (capacitySpec / status / createdAt)
# ===========================================================================

class TestPoolsAdapterSyncMapping:
    """Test the internal response mapping via create_pool/get_pool/list_pools."""

    def _make_config(self) -> ConnectionConfigSync:
        return ConnectionConfigSync(api_key="test-key").with_transport_if_missing()

    def test_capacity_spec_all_fields_mapped(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = ApiPoolResponse(
            name="p",
            capacity_spec=ApiPoolCapacitySpec(
                buffer_max=7, buffer_min=3, pool_max=30, pool_min=2
            ),
            status=UNSET,
            created_at=UNSET,
        )
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            result = adapter.get_pool("p")

        assert result.capacity_spec.buffer_max == 7
        assert result.capacity_spec.buffer_min == 3
        assert result.capacity_spec.pool_max == 30
        assert result.capacity_spec.pool_min == 2

    def test_status_mapped_when_present(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p", with_status=True)
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            result = adapter.get_pool("p")

        assert result.status is not None
        assert result.status.total == 2
        assert result.status.allocated == 1
        assert result.status.available == 1
        assert result.status.revision == "rev-1"

    def test_status_is_none_when_absent(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p", with_status=False)
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            result = adapter.get_pool("p")

        assert result.status is None

    def test_created_at_mapped_when_present(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p", with_created=True)
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            result = adapter.get_pool("p")

        assert result.created_at is not None
        assert isinstance(result.created_at, datetime)
        assert result.created_at.year == 2025

    def test_created_at_is_none_when_absent(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw = _make_pool_response("p", with_created=False)
        resp = _make_http_response(200, raw)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools_pool_name.sync_detailed",
            return_value=resp,
        ):
            result = adapter.get_pool("p")

        assert result.created_at is None

    def test_list_pools_maps_each_item(self):
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = self._make_config()
        adapter = PoolsAdapterSync(config)
        raw_list = ApiListPoolsResponse(
            items=[
                _make_pool_response("pool-a", with_status=True),
                _make_pool_response("pool-b", with_status=False, with_created=False),
            ]
        )
        resp = _make_http_response(200, raw_list)

        with patch(
            "opensandbox.api.lifecycle.api.pools.get_pools.sync_detailed",
            return_value=resp,
        ):
            result = adapter.list_pools()

        assert len(result.items) == 2
        assert result.items[0].name == "pool-a"
        assert result.items[0].status is not None
        assert result.items[1].name == "pool-b"
        assert result.items[1].status is None
        assert result.items[1].created_at is None


# ===========================================================================
# AdapterFactorySync.create_pool_service
# ===========================================================================

class TestAdapterFactorySyncPoolService:
    def test_create_pool_service_returns_pools_adapter_sync(self):
        from opensandbox.sync.adapters.factory import AdapterFactorySync
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = ConnectionConfigSync().with_transport_if_missing()
        factory = AdapterFactorySync(config)
        svc = factory.create_pool_service()
        assert isinstance(svc, PoolsAdapterSync)

    def test_create_pool_service_honours_connection_config(self):
        from opensandbox.sync.adapters.factory import AdapterFactorySync
        from opensandbox.sync.adapters.pools_adapter import PoolsAdapterSync

        config = ConnectionConfigSync(api_key="abc").with_transport_if_missing()
        factory = AdapterFactorySync(config)
        svc = factory.create_pool_service()
        # Confirm an adapter was returned (API key embedded inside httpx client)
        assert isinstance(svc, PoolsAdapterSync)


# ===========================================================================
# Public exports
# ===========================================================================

class TestPublicExports:
    def test_pool_manager_sync_exported_from_sync_module(self):
        from opensandbox import sync as sync_module
        assert hasattr(sync_module, "PoolManagerSync")

    def test_pool_manager_sync_exported_from_root(self):
        import opensandbox
        assert hasattr(opensandbox, "PoolManagerSync")

    def test_pool_manager_sync_in_root_all(self):
        import opensandbox
        assert "PoolManagerSync" in opensandbox.__all__

    def test_pool_manager_sync_in_sync_all(self):
        from opensandbox import sync as sync_module
        assert "PoolManagerSync" in sync_module.__all__

    def test_pool_manager_sync_importable_directly(self):
        from opensandbox.sync.pool_manager import PoolManagerSync as PM
        assert PM is PoolManagerSync
