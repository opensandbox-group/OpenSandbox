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

"""Server-side lifecycle operation metrics."""

from unittest.mock import MagicMock, patch

import pytest
from fastapi import APIRouter, FastAPI, HTTPException
from fastapi.testclient import TestClient
from pydantic import BaseModel

import opensandbox_server.integrations.otel.metrics as otel_metrics
from opensandbox_server.integrations.otel.instrument import (
    InstrumentedRoute,
    lifecycle_operation,
)
from opensandbox_server.integrations.otel.metrics import (
    normalize_error_code,
    record_sandbox_operation,
)
from opensandbox_server.services.constants import SandboxErrorCodes


class TestNormalizeErrorCode:
    """Codes are source constants, but the attribute must stay bounded regardless."""

    @pytest.mark.parametrize(
        "code",
        [
            SandboxErrorCodes.IMAGE_PULL_FAILED,
            SandboxErrorCodes.K8S_POD_READY_TIMEOUT,
            "INVALID_METADATA_FORMAT",
            "HTTP_409",
        ],
    )
    def test_passes_through_code_shaped_values(self, code):
        assert normalize_error_code(code) == code

    @pytest.mark.parametrize(
        "value",
        [
            "Invalid metadata format: unexpected token at line 4",  # a message, not a code
            "sbx_0a1b2c3d",  # an identifier
            "",
            None,
            42,
            "X" * 100,  # unbounded length
        ],
    )
    def test_rejects_anything_else(self, value):
        assert normalize_error_code(value) == "OTHER"


class TestRecordSandboxOperation:
    def test_noop_when_instruments_are_absent(self):
        """Export disabled must not raise, and must not touch anything."""
        with patch.object(otel_metrics, "_operation_duration_histogram", None), patch.object(
            otel_metrics, "_operation_counter", None
        ):
            record_sandbox_operation(operation="create", duration_ms=12.0, outcome="success")

    def test_records_duration_and_count_on_success(self):
        histogram, counter = MagicMock(), MagicMock()
        with patch.object(otel_metrics, "_operation_duration_histogram", histogram), patch.object(
            otel_metrics, "_operation_counter", counter
        ):
            record_sandbox_operation(operation="delete", duration_ms=34.5, outcome="success")

        histogram.record.assert_called_once_with(
            34.5, attributes={"operation": "delete", "outcome": "success"}
        )
        counter.add.assert_called_once_with(
            1, attributes={"operation": "delete", "outcome": "success"}
        )

    def test_error_code_is_attached_to_the_counter_only(self):
        """The histogram stays low-cardinality; the error taxonomy lives on the counter."""
        histogram, counter = MagicMock(), MagicMock()
        with patch.object(otel_metrics, "_operation_duration_histogram", histogram), patch.object(
            otel_metrics, "_operation_counter", counter
        ):
            record_sandbox_operation(
                operation="create",
                duration_ms=1.0,
                outcome="error",
                error_code=SandboxErrorCodes.IMAGE_PULL_FAILED,
            )

        assert "error.code" not in histogram.record.call_args.kwargs["attributes"]
        assert counter.add.call_args.kwargs["attributes"]["error.code"] == (
            SandboxErrorCodes.IMAGE_PULL_FAILED
        )

    def test_never_raises_when_the_instrument_fails(self):
        histogram, counter = MagicMock(), MagicMock()
        histogram.record.side_effect = RuntimeError("exporter exploded")
        with patch.object(otel_metrics, "_operation_duration_histogram", histogram), patch.object(
            otel_metrics, "_operation_counter", counter
        ):
            record_sandbox_operation(operation="pause", duration_ms=1.0, outcome="success")


class _Body(BaseModel):
    name: str


def _app() -> FastAPI:
    """A router using the real route class, with one marked and one unmarked endpoint."""
    router = APIRouter(route_class=InstrumentedRoute)

    @router.post("/marked")
    @lifecycle_operation("create")
    async def marked(body: _Body) -> dict:
        if body.name == "boom":
            raise ValueError("boom")
        if body.name == "missing":
            raise HTTPException(
                status_code=404,
                detail={"code": SandboxErrorCodes.SANDBOX_NOT_FOUND, "message": "gone"},
            )
        if body.name == "conflict":
            raise HTTPException(status_code=409, detail="conflict")
        return {"created": body.name}

    @router.get("/unmarked")
    def unmarked() -> dict:
        return {"ok": True}

    app = FastAPI()
    app.include_router(router)
    return app


class TestInstrumentedRoute:
    """Recording happens at the route, so it covers the whole API boundary."""

    def _call(self, method: str, path: str, **kwargs):
        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            response = getattr(TestClient(_app(), raise_server_exceptions=False), method)(
                path, **kwargs
            )
        return response, record

    def test_success(self):
        response, record = self._call("post", "/marked", json={"name": "sbx"})

        assert response.status_code == 200
        call = record.call_args.kwargs
        assert call["operation"] == "create"
        assert call["outcome"] == "success"
        assert call["error_code"] is None
        assert call["duration_ms"] >= 0

    def test_http_exception_with_code(self):
        response, record = self._call("post", "/marked", json={"name": "missing"})

        assert response.status_code == 404
        call = record.call_args.kwargs
        assert call["outcome"] == "error"
        assert call["error_code"] == SandboxErrorCodes.SANDBOX_NOT_FOUND

    def test_http_exception_without_a_code_falls_back_to_the_status(self):
        response, record = self._call("post", "/marked", json={"name": "conflict"})

        assert response.status_code == 409
        assert record.call_args.kwargs["error_code"] == "HTTP_409"

    def test_unexpected_exception(self):
        response, record = self._call("post", "/marked", json={"name": "boom"})

        assert response.status_code == 500
        assert record.call_args.kwargs["error_code"] == SandboxErrorCodes.UNKNOWN_ERROR

    def test_request_rejected_before_the_handler_runs_is_still_counted(self):
        """A malformed body never reaches the endpoint; the route still sees the failure."""
        response, record = self._call("post", "/marked", json={"wrong": "field"})

        assert response.status_code == 422
        call = record.call_args.kwargs
        assert call["operation"] == "create"
        assert call["outcome"] == "error"
        assert call["error_code"] == "HTTP_422"

    def test_unmarked_routes_are_not_recorded(self):
        response, record = self._call("get", "/unmarked")

        assert response.status_code == 200
        record.assert_not_called()

    def test_recorded_exactly_once(self):
        """The marker must not wrap the handler as well, or every call counts twice."""
        _, record = self._call("post", "/marked", json={"name": "sbx"})

        assert record.call_count == 1

    def test_a_metrics_failure_does_not_break_the_request(self):
        """Instrumentation sits on every mutating route; it must not be able to fail one."""
        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation",
            side_effect=RuntimeError("metrics down"),
        ):
            response = TestClient(_app()).post("/marked", json={"name": "sbx"})

        assert response.status_code == 200
        assert response.json() == {"created": "sbx"}

    def test_a_metrics_failure_does_not_mask_a_handler_error(self):
        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation",
            side_effect=RuntimeError("metrics down"),
        ):
            response = TestClient(_app()).post("/marked", json={"name": "missing"})

        assert response.status_code == 404


class TestLifecycleOperationMarker:
    def test_returns_the_handler_untouched(self):
        """FastAPI reads the signature; the marker must not stand in front of it."""

        async def handler(request: str, x_request_id: str = "") -> str:
            return request

        marked = lifecycle_operation("create")(handler)

        assert marked is handler


class TestLifecycleRouterIsInstrumented:
    """Guards the wiring itself: decorator order and route_class are easy to get wrong."""

    def test_mutating_routes_are_marked_and_use_the_route_class(self):
        from opensandbox_server.api.lifecycle import router

        marked = {
            route.endpoint.__name__: getattr(route.endpoint, "__opensandbox_operation__")
            for route in router.routes
            if hasattr(route.endpoint, "__opensandbox_operation__")
        }

        assert marked == {
            "create_sandbox": "create",
            "patch_sandbox_metadata": "update_metadata",
            "delete_sandbox": "delete",
            "pause_sandbox": "pause",
            "resume_sandbox": "resume",
            "renew_sandbox_expiration": "renew",
            "create_snapshot": "create_snapshot",
            "delete_snapshot": "delete_snapshot",
        }
        # A marker with the wrong decorator order, or a router without route_class, would
        # leave the metric silently unrecorded.
        assert all(isinstance(route, InstrumentedRoute) for route in router.routes)
