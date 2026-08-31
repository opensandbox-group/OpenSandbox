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

"""Tests for low-cardinality Server HTTP request metrics."""

from collections.abc import AsyncIterator
from unittest.mock import MagicMock, patch

import pytest
from fastapi import FastAPI
from fastapi.testclient import TestClient
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import Histogram, InMemoryMetricReader
from starlette.responses import StreamingResponse

import opensandbox_server.integrations.otel.metrics as otel_metrics
from opensandbox_server.middleware.http_metrics import HttpMetricsMiddleware


def _test_app() -> FastAPI:
    app = FastAPI()

    @app.get("/items/{item_id}")
    async def get_item(item_id: int) -> dict[str, int]:
        return {"item_id": item_id}

    @app.get("/explode")
    async def explode() -> None:
        raise RuntimeError("boom")

    @app.get("/stream-error")
    async def stream_error() -> StreamingResponse:
        async def broken_body() -> AsyncIterator[bytes]:
            yield b"partial"
            raise RuntimeError("stream boom")

        return StreamingResponse(broken_body())

    app.add_middleware(HttpMetricsMiddleware)
    return app


@pytest.mark.parametrize(
    ("path", "expected_status", "expected_route"),
    [
        ("/items/42", 200, "/items/{item_id}"),
        ("/items/not-an-int", 422, "/items/{item_id}"),
        ("/missing", 404, "unknown"),
        ("/explode", 500, "/explode"),
    ],
)
def test_http_middleware_records_status_and_route_template(
    path: str,
    expected_status: int,
    expected_route: str,
) -> None:
    client = TestClient(_test_app(), raise_server_exceptions=False)

    with patch("opensandbox_server.middleware.http_metrics.record_http_request_duration") as record:
        response = client.get(path)

    assert response.status_code == expected_status
    record.assert_called_once()
    assert record.call_args.kwargs["method"] == "GET"
    assert record.call_args.kwargs["route"] == expected_route
    assert record.call_args.kwargs["status_code"] == expected_status
    assert record.call_args.kwargs["duration_ms"] >= 0


@pytest.mark.parametrize("path", ["/docs", "/redoc", "/openapi.json"])
def test_http_middleware_records_registered_starlette_routes(path: str) -> None:
    client = TestClient(_test_app())

    with patch("opensandbox_server.middleware.http_metrics.record_http_request_duration") as record:
        response = client.get(path)

    assert response.status_code == 200
    record.assert_called_once()
    assert record.call_args.kwargs["route"] == path


def test_http_middleware_covers_auth_rejection(client: TestClient) -> None:
    with patch("opensandbox_server.middleware.http_metrics.record_http_request_duration") as record:
        response = client.get("/v1/sandboxes")

    assert response.status_code == 401
    record.assert_called_once()
    assert record.call_args.kwargs["route"] == "unknown"
    assert record.call_args.kwargs["status_code"] == 401


def test_http_middleware_does_not_fail_request_when_recorder_raises() -> None:
    client = TestClient(_test_app())

    with patch(
        "opensandbox_server.middleware.http_metrics.record_http_request_duration",
        side_effect=RuntimeError("boom"),
    ):
        response = client.get("/items/42")

    assert response.status_code == 200
    assert response.json() == {"item_id": 42}


def test_http_middleware_records_streaming_failures_as_500() -> None:
    client = TestClient(_test_app(), raise_server_exceptions=False)

    with patch(
        "opensandbox_server.middleware.http_metrics.record_http_request_duration"
    ) as record:
        client.get("/stream-error")

    record.assert_called_once()
    assert record.call_args.kwargs["status_code"] == 500


def test_record_http_request_duration_uses_low_cardinality_attributes() -> None:
    histogram = MagicMock()

    with patch.object(otel_metrics, "_http_request_duration_histogram", histogram):
        otel_metrics.record_http_request_duration(
            duration_ms=12.5,
            method="GET",
            route="/sandboxes/{sandbox_id}",
            status_code=200,
        )

    histogram.record.assert_called_once_with(
        12.5,
        attributes={
            "http_method": "GET",
            "http_route": "/sandboxes/{sandbox_id}",
            "http_status_code": 200,
        },
    )


def test_record_http_request_duration_bounds_unknown_methods() -> None:
    histogram = MagicMock()

    with patch.object(otel_metrics, "_http_request_duration_histogram", histogram):
        otel_metrics.record_http_request_duration(
            duration_ms=12.5,
            method="BREW-sandbox-123",
            route="/sandboxes/{sandbox_id}",
            status_code=200,
        )

    assert histogram.record.call_args.kwargs["attributes"]["http_method"] == "OTHER"


def test_http_request_histogram_is_collectable() -> None:
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    histogram = otel_metrics._http_request_histogram_from_provider(provider)

    with patch.object(otel_metrics, "_http_request_duration_histogram", histogram):
        otel_metrics.record_http_request_duration(
            duration_ms=12.5,
            method="GET",
            route="/sandboxes/{sandbox_id}",
            status_code=200,
        )

    metrics_data = reader.get_metrics_data()
    assert metrics_data is not None
    metric = metrics_data.resource_metrics[0].scope_metrics[0].metrics[0]
    assert isinstance(metric.data, Histogram)
    point = metric.data.data_points[0]
    assert metric.name == "server.http.request.duration"
    assert metric.unit == "ms"
    assert point.count == 1
    assert point.attributes == {
        "http_method": "GET",
        "http_route": "/sandboxes/{sandbox_id}",
        "http_status_code": 200,
    }
    provider.shutdown()


def test_record_http_request_duration_swallows_errors() -> None:
    histogram = MagicMock()
    histogram.record.side_effect = RuntimeError("boom")

    with patch.object(otel_metrics, "_http_request_duration_histogram", histogram):
        otel_metrics.record_http_request_duration(
            duration_ms=1.0,
            method="GET",
            route="/health",
            status_code=200,
        )

    histogram.record.assert_called_once()
