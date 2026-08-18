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

"""Tests for the HTTP request duration metrics middleware."""

import asyncio
from unittest.mock import MagicMock, patch

from fastapi import FastAPI
from fastapi.responses import PlainTextResponse, StreamingResponse
from starlette.testclient import TestClient

from opensandbox_server.middleware.http_request_metrics import HttpRequestMetricsMiddleware


def _make_app() -> FastAPI:
    app = FastAPI()
    app.add_middleware(HttpRequestMetricsMiddleware)

    @app.get("/things/{thing_id}")
    async def ok_handler(thing_id: str):
        return PlainTextResponse("ok")

    @app.get("/boom")
    async def boom_handler():
        raise RuntimeError("boom")

    @app.get("/stream")
    async def stream_handler():
        async def gen():
            yield b"first"
            await asyncio.sleep(0.2)
            yield b"second"

        return StreamingResponse(gen())

    return app


class TestHttpRequestMetricsMiddleware:
    def test_records_route_template_not_raw_path(self):
        app = _make_app()
        with patch(
            "opensandbox_server.middleware.http_request_metrics.record_http_request_duration"
        ) as record:
            with TestClient(app) as client:
                response = client.get("/things/sbx-12345")
            assert response.status_code == 200

        record.assert_called_once()
        call = record.call_args.kwargs
        assert call["http_method"] == "GET"
        # Matched route template, never the raw URL path.
        assert call["http_route"] == "/things/{thing_id}"
        assert call["http_status_code"] == 200
        assert call["duration_ms"] >= 0

    def test_records_unknown_route_for_unmatched_path(self):
        app = _make_app()
        with patch(
            "opensandbox_server.middleware.http_request_metrics.record_http_request_duration"
        ) as record:
            with TestClient(app) as client:
                response = client.get("/does/not/exist")
            assert response.status_code == 404

        record.assert_called_once()
        call = record.call_args.kwargs
        assert call["http_route"] == "unknown"
        assert call["http_status_code"] == 404

    def test_records_500_for_unhandled_exception(self):
        app = _make_app()
        with patch(
            "opensandbox_server.middleware.http_request_metrics.record_http_request_duration"
        ) as record:
            with TestClient(app, raise_server_exceptions=False) as client:
                response = client.get("/boom")
            assert response.status_code == 500

        record.assert_called_once()
        call = record.call_args.kwargs
        assert call["http_route"] == "/boom"
        assert call["http_status_code"] == 500

    def test_records_full_streaming_duration(self):
        app = _make_app()
        with patch(
            "opensandbox_server.middleware.http_request_metrics.record_http_request_duration"
        ) as record:
            with TestClient(app) as client:
                response = client.get("/stream")
            assert response.status_code == 200
            assert response.content == b"firstsecond"

        record.assert_called_once()
        call = record.call_args.kwargs
        assert call["http_route"] == "/stream"
        assert call["http_status_code"] == 200
        # The stream body is delayed by 200ms after the headers; the metric must
        # cover the full response duration, not just time-to-headers.
        assert call["duration_ms"] >= 150


class TestHttpRequestMetricsWiring:
    def test_middleware_wired_into_lifecycle_app(self, client):
        # Authentication failures are covered: AuthMiddleware short-circuits
        # before routing, so no route template is available.
        with patch(
            "opensandbox_server.middleware.http_request_metrics.record_http_request_duration"
        ) as record:
            response = client.get("/v1/sandboxes/sbx-123/diagnostics/summary")
            assert response.status_code == 401

        record.assert_called_once()
        call = record.call_args.kwargs
        assert call["http_method"] == "GET"
        assert call["http_route"] == "unknown"
        assert call["http_status_code"] == 401


class TestOtelHttpMetricsSetup:
    def test_setup_binds_http_request_histogram(self):
        import opensandbox_server.integrations.otel.metrics as otel_metrics

        mock_meter = MagicMock()
        hist_create = MagicMock()
        hist_http = MagicMock()
        mock_meter.create_histogram.side_effect = [hist_create, hist_http]

        class FakeMeterProvider:
            def __init__(self, **kwargs):
                pass

            def get_meter(self, name):
                return mock_meter

        class FakeConfig:
            enabled = True
            endpoint = None
            export_interval_millis = 60000
            service_name = "test"

        with (
            patch.object(otel_metrics, "MeterProvider", FakeMeterProvider),
            patch.object(otel_metrics, "PeriodicExportingMetricReader"),
            patch.object(otel_metrics, "Resource"),
            patch.object(otel_metrics.metrics, "get_meter_provider", return_value=None),
            patch.object(otel_metrics.metrics, "set_meter_provider"),
        ):
            otel_metrics.setup_otel_metrics(FakeConfig())

        # The HTTP histogram must be bound at module scope (and declared global),
        # otherwise every sample recorded by the middleware is silently discarded.
        assert otel_metrics._create_duration_histogram is hist_create
        assert otel_metrics._http_request_duration_histogram is hist_http

        # Restore module state so later tests / record calls don't carry over.
        otel_metrics._meter_provider = None
        otel_metrics._create_duration_histogram = None
        otel_metrics._http_request_duration_histogram = None

    def test_setup_disabled_resets_http_histogram(self):
        import opensandbox_server.integrations.otel.metrics as otel_metrics

        # Simulate a prior enabled setup having bound both instruments.
        otel_metrics._create_duration_histogram = MagicMock()
        otel_metrics._http_request_duration_histogram = MagicMock()

        class FakeConfig:
            enabled = False
            endpoint = None
            export_interval_millis = 60000
            service_name = "test"

        otel_metrics.setup_otel_metrics(FakeConfig())

        # A disabled setup must unbind the HTTP histogram too, otherwise samples
        # would be recorded against a stale provider.
        assert otel_metrics._create_duration_histogram is None
        assert otel_metrics._http_request_duration_histogram is None

        otel_metrics._meter_provider = None
