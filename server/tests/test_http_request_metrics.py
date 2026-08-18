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

from unittest.mock import patch

from fastapi import FastAPI
from fastapi.responses import PlainTextResponse
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
