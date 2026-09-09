# Copyright 2026 Alibaba Group Holding Ltd.
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

"""Tests for Server business lifecycle metrics."""

from unittest.mock import MagicMock, patch

import pytest
from fastapi import HTTPException
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import InMemoryMetricReader
import opensandbox_server.integrations.otel.metrics as otel_metrics
from opensandbox_server.integrations.otel import (
    instrument_lifecycle,
    instrument_proxy_http,
    record_access_renew_outcome,
    record_lifecycle_operation,
    record_proxy_request,
    record_snapshot_operation,
)


def _collect(reader):
    metrics_data = reader.get_metrics_data()
    collected = {}
    for resource_metrics in metrics_data.resource_metrics:
        for scope_metrics in resource_metrics.scope_metrics:
            for metric in scope_metrics.metrics:
                for point in metric.data.data_points:
                    collected[(metric.name, tuple(sorted((point.attributes or {}).items())))] = point
    return collected


def _installed_provider():
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    instruments = otel_metrics._business_instruments_from_provider(provider)
    patcher = patch.object(otel_metrics, "_business_instruments", instruments)
    patcher.start()
    return provider, reader, patcher


# ---------------------------------------------------------------------------
# record API attribute shapes
# ---------------------------------------------------------------------------

def test_record_lifecycle_create_uses_bounded_attributes() -> None:
    instruments = otel_metrics._business_instruments_from_provider(MeterProvider())
    counter, duration = MagicMock(), MagicMock()
    instruments.lifecycle_counters["create"] = counter
    instruments.lifecycle_durations["create"] = duration

    with patch.object(otel_metrics, "_business_instruments", instruments):
        record_lifecycle_operation(
            "create",
            runtime="docker",
            result="error",
            duration_ms=123.0,
            error_type="quota",
            source="image",
        )

    counter.add.assert_called_once_with(
        1,
        {"runtime": "docker", "result": "error", "error_type": "quota", "source": "image"},
    )
    duration.record.assert_called_once_with(
        123.0, {"runtime": "docker", "result": "error"}
    )


def test_record_lifecycle_delete_trigger_and_orphan_attributes() -> None:
    instruments = otel_metrics._business_instruments_from_provider(MeterProvider())
    delete_counter, orphan_counter = MagicMock(), MagicMock()
    instruments.lifecycle_counters["delete"] = delete_counter
    instruments.lifecycle_counters["orphan_cleaned"] = orphan_counter

    with patch.object(otel_metrics, "_business_instruments", instruments):
        record_lifecycle_operation("delete", runtime="docker", trigger="expiry")
        record_lifecycle_operation("orphan_cleaned", runtime="docker")

    delete_counter.add.assert_called_once_with(
        1, {"runtime": "docker", "result": "success", "trigger": "expiry"}
    )
    orphan_counter.add.assert_called_once_with(1, {"runtime": "docker"})


def test_record_snapshot_and_proxy_and_renew_shapes() -> None:
    instruments = otel_metrics._business_instruments_from_provider(MeterProvider())
    snapshot_counter = MagicMock()
    snapshot_duration = MagicMock()
    proxy_counter = MagicMock()
    proxy_duration = MagicMock()
    renew_counter = MagicMock()
    instruments.snapshot_counters["create"] = snapshot_counter
    instruments.snapshot_durations["create"] = snapshot_duration
    instruments.proxy_request_counter = proxy_counter
    instruments.proxy_request_duration = proxy_duration
    instruments.access_renew_counter = renew_counter

    with patch.object(otel_metrics, "_business_instruments", instruments):
        record_snapshot_operation("create", runtime="kubernetes", result="success", duration_ms=5.0)
        record_proxy_request(proxy_type="websocket", method="GET", status_code=101, duration_ms=42.0)
        record_access_renew_outcome("extended")

    snapshot_counter.add.assert_called_once_with(1, {"runtime": "kubernetes", "result": "success"})
    snapshot_duration.record.assert_called_once_with(5.0, {"runtime": "kubernetes", "result": "success"})
    proxy_attrs = {"proxy_type": "websocket", "http_method": "GET", "http_status_code": 101}
    proxy_counter.add.assert_called_once_with(1, proxy_attrs)
    proxy_duration.record.assert_called_once_with(42.0, proxy_attrs)
    renew_counter.add.assert_called_once_with(1, {"outcome": "extended"})


# ---------------------------------------------------------------------------
# lifecycle decorator
# ---------------------------------------------------------------------------

class _Service:
    @instrument_lifecycle("create")
    async def create(self, request) -> str:
        return "ok"

    @instrument_lifecycle("delete")
    def delete(self, sandbox_id: str) -> None:
        pass

    @instrument_lifecycle("pause")
    def pause(self, sandbox_id: str) -> None:
        raise HTTPException(status_code=501, detail="not implemented")


def test_lifecycle_decorator_records_success_with_runtime_resolution() -> None:
    with patch.object(otel_metrics, "record_lifecycle_operation") as record, patch.object(
        otel_metrics, "_configured_runtime_label", return_value="docker"
    ):
        assert _Service().delete("sbx-1") is None

    kwargs = record.call_args.kwargs
    assert record.call_args.args == ("delete",)
    assert kwargs["runtime"] == "docker"
    assert kwargs["trigger"] == "user"
    assert kwargs["result"] == "success"
    assert kwargs["duration_ms"] >= 0


def test_lifecycle_decorator_routes_fleets_ids_under_kubernetes_composite() -> None:
    with patch.object(otel_metrics, "record_lifecycle_operation") as record, patch.object(
        otel_metrics, "_configured_runtime_label", return_value="kubernetes"
    ):
        _Service().delete("flt-123")

    assert record.call_args.kwargs["runtime"] == "fleets"


@pytest.mark.asyncio
async def test_lifecycle_decorator_create_extracts_source_and_maps_error_type() -> None:
    class _Request:
        snapshot_id = "snap-1"
        extensions = None

    service = _Service()

    with patch.object(otel_metrics, "record_lifecycle_operation") as record, patch.object(
        otel_metrics, "_configured_runtime_label", return_value="docker"
    ):
        assert await service.create(_Request()) == "ok"

    assert record.call_args.kwargs["source"] == "snapshot"

    with patch.object(otel_metrics, "record_lifecycle_operation") as record, patch.object(
        otel_metrics, "_configured_runtime_label", return_value="docker"
    ):
        with pytest.raises(HTTPException):
            service.pause("sbx-1")

    assert record.call_args.kwargs["result"] == "error"


@pytest.mark.parametrize(
    ("status_code", "detail", "expected"),
    [
        (403, {"code": "KUBERNETES::QUOTA_EXCEEDED"}, "quota"),
        (429, {"code": "POOL_TIMEOUT"}, "pool_timeout"),
        (404, {"code": "SNAPSHOT::NOT_FOUND"}, "image"),
        (504, {"code": "INTERNAL"}, "timeout"),
        (400, {"code": "INVALID_PARAMETER"}, "invalid_request"),
        (500, {"code": "INTERNAL"}, "runtime"),
    ],
)
def test_create_error_type_mapping(status_code, detail, expected) -> None:
    assert (
        otel_metrics._create_error_type_from_exception(
            HTTPException(status_code=status_code, detail=detail)
        )
        == expected
    )


def test_create_error_type_non_http_is_unknown() -> None:
    assert otel_metrics._create_error_type_from_exception(ValueError("boom")) == "unknown"


# ---------------------------------------------------------------------------
# proxy decorator
# ---------------------------------------------------------------------------

class _FakeRequest:
    method = "GET"


class _FakeResponse:
    status_code = 200


@pytest.mark.asyncio
async def test_proxy_decorator_records_status_and_duration() -> None:
    @instrument_proxy_http
    async def handler(request, sandbox_id, port, full_path):
        return _FakeResponse()

    with patch.object(otel_metrics, "record_proxy_request") as record:
        response = await handler(_FakeRequest(), "sbx-1", 8080, "")

    assert response.status_code == 200
    kwargs = record.call_args.kwargs
    assert kwargs["proxy_type"] == "http"
    assert kwargs["method"] == "GET"
    assert kwargs["status_code"] == 200
    assert kwargs["duration_ms"] >= 0


@pytest.mark.asyncio
async def test_proxy_decorator_records_http_exception_status() -> None:
    @instrument_proxy_http
    async def handler(request, sandbox_id, port, full_path):
        raise HTTPException(status_code=502, detail="bad gateway")

    with patch.object(otel_metrics, "record_proxy_request") as record:
        with pytest.raises(HTTPException):
            await handler(_FakeRequest(), "sbx-1", 8080, "")

    assert record.call_args.kwargs["status_code"] == 502


# ---------------------------------------------------------------------------
# end-to-end collection through a real provider
# ---------------------------------------------------------------------------

def test_business_metrics_are_collectable() -> None:
    provider, reader, patcher = _installed_provider()
    try:
        record_lifecycle_operation(
            "create", runtime="docker", result="success", duration_ms=1500.0,
            error_type="unknown", source="image",
        )
        record_snapshot_operation("create", runtime="docker", result="success")
        record_proxy_request(proxy_type="http", method="GET", status_code=200, duration_ms=10.0)
        record_access_renew_outcome("extended")

        collected = _collect(reader)
        create_count = collected[
            (
                "server.sandbox.create.count",
                (("error_type", "unknown"), ("result", "success"), ("runtime", "docker"), ("source", "image")),
            )
        ]
        assert create_count.value == 1
        snapshot_count = collected[
            ("server.snapshot.create.count", (("result", "success"), ("runtime", "docker")))
        ]
        assert snapshot_count.value == 1
        proxy_count = collected[
            (
                "server.proxy.request.count",
                (("http_method", "GET"), ("http_status_code", 200), ("proxy_type", "http")),
            )
        ]
        assert proxy_count.value == 1
        renew_count = collected[("server.access_renew.outcome.count", (("outcome", "extended"),))]
        assert renew_count.value == 1
    finally:
        patcher.stop()
        provider.shutdown()


def test_sandbox_active_gauge_reports_state_counts() -> None:
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])

    class _Status:
        def __init__(self, state):
            self.state = state

    class _Item:
        def __init__(self, sandbox_id, state):
            self.id = sandbox_id
            self.status = _Status(state)

    class _Pagination:
        has_next_page = False

    class _Response:
        items = [_Item("sbx-1", "Running"), _Item("flt-2", "Paused")]
        pagination = _Pagination()

    class _Service:
        def list_sandboxes(self, request):
            return _Response()

    fake_module = MagicMock()
    fake_module.sandbox_service = _Service()
    with patch.dict(
        "sys.modules", {"opensandbox_server.api.lifecycle": fake_module}
    ), patch.object(
        otel_metrics, "_configured_runtime_label", return_value="kubernetes"
    ):
        otel_metrics._register_sandbox_active_gauge(provider)
        collected = _collect(reader)

    running = collected[("server.sandbox.active", (("runtime", "kubernetes"), ("state", "running")))]
    paused = collected[("server.sandbox.active", (("runtime", "fleets"), ("state", "paused")))]
    assert running.value == 1
    assert paused.value == 1
    provider.shutdown()


def test_record_functions_noop_without_setup() -> None:
    with patch.object(otel_metrics, "_business_instruments", None):
        record_lifecycle_operation("create", runtime="docker", result="success")
        record_snapshot_operation("create", runtime="docker", result="success")
        record_proxy_request(proxy_type="http", method="GET", status_code=200, duration_ms=1.0)
        record_access_renew_outcome("extended")


# ---------------------------------------------------------------------------
# review regressions
# ---------------------------------------------------------------------------

def test_lifecycle_decorators_live_on_implementations_not_composite() -> None:
    """Exactly one layer instruments each call path (no double counting)."""
    from opensandbox_server.services.composite_service import CompositeSandboxService
    from opensandbox_server.services.docker.docker_service import DockerSandboxService
    from opensandbox_server.services.fleets.fleet_service import FleetSandboxService
    from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService

    for operation in ("create_sandbox", "delete_sandbox", "pause_sandbox", "resume_sandbox", "renew_expiration"):
        assert hasattr(getattr(KubernetesSandboxService, operation), "__wrapped__")
        assert hasattr(getattr(FleetSandboxService, operation), "__wrapped__")
        assert hasattr(getattr(DockerSandboxService, operation), "__wrapped__")
        # The composite is a pure pass-through; instrumented backends cover it.
        assert not hasattr(getattr(CompositeSandboxService, operation), "__wrapped__")


def test_snapshot_worker_records_count_once_and_uses_persisted_result(tmp_path) -> None:
    """READY without a restorable image persists as FAILED; count once, duration once."""
    from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
    from opensandbox_server.services.snapshot_models import SnapshotState
    from opensandbox_server.services.snapshot_runtime import SnapshotRuntimeStatus
    from opensandbox_server.services.snapshot_service import PersistedSnapshotService

    class _ReadyNoImageRuntime:
        def supports_create_snapshot(self) -> bool:
            return True

        def create_snapshot_unsupported_message(self) -> str:
            return ""

        def preflight_create_snapshot(self, sandbox_id, *, namespace=None) -> None:
            return None

        def create_snapshot(self, snapshot_id, sandbox_id, *, namespace=None):
            return SnapshotRuntimeStatus(state=SnapshotState.READY, image=None)

        def get_snapshot_status(self, snapshot_id):
            return None

        def delete_snapshot(self, snapshot_id, image=None, *, namespace=None) -> None:
            return None

        def inspect_snapshot(self, snapshot_id, image=None, *, namespace=None):
            return SnapshotRuntimeStatus(state=SnapshotState.FAILED, image=None)

    class _ImmediateExecutor:
        def submit(self, fn, *args, **kwargs):
            from concurrent.futures import Future

            future = Future()
            try:
                future.set_result(fn(*args, **kwargs))
            except Exception as exc:  # noqa: BLE001
                future.set_exception(exc)
            return future

        def shutdown(self, wait: bool = True) -> None:
            pass

    from opensandbox_server.api.schema import CreateSnapshotRequest

    class _SandboxService:
        @staticmethod
        def get_sandbox(sandbox_id):
            return {"id": sandbox_id, "status": {"state": "Running"}}

    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    service = PersistedSnapshotService(
        repo,
        _SandboxService(),
        snapshot_runtime=_ReadyNoImageRuntime(),
        snapshot_executor=_ImmediateExecutor(),
    )

    with patch("opensandbox_server.services.snapshot_service.record_snapshot_operation") as record:
        created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="n"))

    # The response reflects the accepted CREATING record; the inline worker
    # already persisted the terminal state.
    persisted = repo.get(created.id)
    assert persisted is not None
    assert persisted.status.state == SnapshotState.FAILED
    create_calls = [c for c in record.call_args_list if c.args[0] == "create"]
    assert len(create_calls) == 2
    count_call, duration_call = create_calls
    assert count_call.kwargs["result"] == "error"
    assert "duration_ms" not in count_call.kwargs
    assert duration_call.kwargs["count"] is False
    assert duration_call.kwargs["result"] == "error"
    assert duration_call.kwargs["duration_ms"] >= 0


def test_sandbox_active_gauge_sweeps_tenant_namespaces_and_dedupes() -> None:
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])

    class _Status:
        def __init__(self, state):
            self.state = state

    class _Item:
        def __init__(self, sandbox_id, state):
            self.id = sandbox_id
            self.status = _Status(state)

    class _Pagination:
        has_next_page = False

    class _Response:
        items = [_Item("sbx-1", "Running"), _Item("flt-2", "Paused")]
        pagination = _Pagination()

    class _Service:
        def list_sandboxes(self, request):
            return _Response()

    class _TenantProvider:
        supports_enumeration = True

        @staticmethod
        def list_tenants():
            from types import SimpleNamespace

            return [SimpleNamespace(namespace="tenant-a")]

    fake_lifecycle = MagicMock()
    fake_lifecycle.sandbox_service = _Service()
    fake_main = MagicMock()
    fake_main.tenant_provider = _TenantProvider()
    with patch.dict(
        "sys.modules",
        {
            "opensandbox_server.api.lifecycle": fake_lifecycle,
            "opensandbox_server.main": fake_main,
        },
    ), patch.object(
        otel_metrics, "_configured_runtime_label", return_value="kubernetes"
    ):
        otel_metrics._register_sandbox_active_gauge(provider)
        collected = _collect(reader)

    # Same service listed for default + tenant namespaces; IDs dedupe.
    running = collected[("server.sandbox.active", (("runtime", "kubernetes"), ("state", "running")))]
    paused = collected[("server.sandbox.active", (("runtime", "fleets"), ("state", "paused")))]
    assert running.value == 1
    assert paused.value == 1
    provider.shutdown()
