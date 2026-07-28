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

import asyncio
from unittest.mock import MagicMock, patch

import pytest
from fastapi import HTTPException

import opensandbox_server.integrations.otel.metrics as otel_metrics
from opensandbox_server.integrations.otel.instrument import instrumented_operation
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


class TestInstrumentedOperation:
    """The decorator must be invisible to the handler it wraps."""

    def test_sync_success(self):
        @instrumented_operation("pause")
        def handler(sandbox_id: str) -> str:
            return f"paused {sandbox_id}"

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            assert handler("sbx-1") == "paused sbx-1"

        call = record.call_args.kwargs
        assert call["operation"] == "pause"
        assert call["outcome"] == "success"
        assert call["error_code"] is None
        assert call["duration_ms"] >= 0

    def test_async_success_preserves_the_signature(self):
        @instrumented_operation("create")
        async def handler(request: str, x_request_id: str = "") -> str:
            return f"created {request}"

        # FastAPI builds the request model from the signature, so it has to survive.
        import inspect

        assert list(inspect.signature(handler).parameters) == ["request", "x_request_id"]

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            assert asyncio.run(handler("spec")) == "created spec"

        assert record.call_args.kwargs["outcome"] == "success"

    def test_http_exception_with_code_is_counted_and_reraised(self):
        @instrumented_operation("delete")
        def handler() -> None:
            raise HTTPException(
                status_code=404,
                detail={"code": SandboxErrorCodes.SANDBOX_NOT_FOUND, "message": "gone"},
            )

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            with pytest.raises(HTTPException) as raised:
                handler()

        assert raised.value.status_code == 404
        call = record.call_args.kwargs
        assert call["outcome"] == "error"
        assert call["error_code"] == SandboxErrorCodes.SANDBOX_NOT_FOUND

    def test_http_exception_without_a_code_falls_back_to_the_status(self):
        @instrumented_operation("resume")
        def handler() -> None:
            raise HTTPException(status_code=409, detail="conflict")

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            with pytest.raises(HTTPException):
                handler()

        assert record.call_args.kwargs["error_code"] == "HTTP_409"

    def test_unexpected_exception_is_counted_and_reraised(self):
        @instrumented_operation("create_snapshot")
        def handler() -> None:
            raise ValueError("boom")

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation"
        ) as record:
            with pytest.raises(ValueError, match="boom"):
                handler()

        assert record.call_args.kwargs["error_code"] == SandboxErrorCodes.UNKNOWN_ERROR

    def test_a_metrics_failure_does_not_break_the_handler(self):
        """Instrumentation sits on every mutating route; it must not be able to fail one."""

        @instrumented_operation("renew")
        def handler() -> str:
            return "renewed"

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation",
            side_effect=RuntimeError("metrics down"),
        ):
            assert handler() == "renewed"

    def test_a_metrics_failure_does_not_mask_a_handler_error(self):
        """And it must not swallow the failure the caller actually needs to see."""

        @instrumented_operation("delete")
        def handler() -> None:
            raise HTTPException(status_code=404, detail="gone")

        with patch(
            "opensandbox_server.integrations.otel.instrument.record_sandbox_operation",
            side_effect=RuntimeError("metrics down"),
        ):
            with pytest.raises(HTTPException) as raised:
                handler()

        assert raised.value.status_code == 404
