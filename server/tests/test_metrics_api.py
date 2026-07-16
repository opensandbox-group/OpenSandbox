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

"""API tests for SDK metrics ingestion."""

from unittest.mock import MagicMock, patch

from fastapi.testclient import TestClient

import opensandbox_server.integrations.otel.metrics as otel_metrics


def _event_body(**overrides):
    body = {
        "eventType": "sandbox.create",
        "sandboxId": "sbx_test",
        "image": "python:3.12",
        "createDurationMs": 1234,
        "success": True,
    }
    body.update(overrides)
    return {k: v for k, v in body.items() if v is not None}


class TestMetricsAuthentication:
    def test_without_api_key_returns_401(self, client: TestClient):
        response = client.post("/v1/metrics/events", json=_event_body())
        assert response.status_code == 401

    def test_unprefixed_path_is_not_mounted(self, client: TestClient, auth_headers: dict):
        response = client.post(
            "/metrics/events",
            json=_event_body(),
            headers=auth_headers,
        )
        assert response.status_code == 404


class TestReportMetricsEvent:
    def test_accepts_valid_event(self, client: TestClient, auth_headers: dict):
        headers = {
            **auth_headers,
            "User-Agent": "OpenSandbox-Python-SDK/0.1.0",
        }
        with patch(
            "opensandbox_server.api.metrics.record_sandbox_create_duration"
        ) as record:
            response = client.post(
                "/v1/metrics/events",
                json=_event_body(),
                headers=headers,
            )
        assert response.status_code == 204
        assert response.content == b""
        record.assert_called_once_with(
            create_duration_ms=1234,
            sdk_language="python",
            sdk_version="0.1.0",
            success=True,
        )

    def test_parses_language_from_user_agent(
        self, client: TestClient, auth_headers: dict
    ):
        headers = {
            **auth_headers,
            "User-Agent": "OpenSandbox-Kotlin-SDK/1.2.3",
        }
        with patch(
            "opensandbox_server.api.metrics.record_sandbox_create_duration"
        ) as record:
            response = client.post(
                "/v1/metrics/events",
                json=_event_body(),
                headers=headers,
            )
        assert response.status_code == 204
        record.assert_called_once_with(
            create_duration_ms=1234,
            sdk_language="kotlin",
            sdk_version="1.2.3",
            success=True,
        )

    def test_unknown_user_agent_falls_back(
        self, client: TestClient, auth_headers: dict
    ):
        headers = {**auth_headers, "User-Agent": "curl/8.0"}
        with patch(
            "opensandbox_server.api.metrics.record_sandbox_create_duration"
        ) as record:
            response = client.post(
                "/v1/metrics/events",
                json=_event_body(),
                headers=headers,
            )
        assert response.status_code == 204
        record.assert_called_once_with(
            create_duration_ms=1234,
            sdk_language="unknown",
            sdk_version="unknown",
            success=True,
        )

    def test_accepts_failure_without_sandbox_id(
        self, client: TestClient, auth_headers: dict
    ):
        body = _event_body(success=False)
        body.pop("sandboxId", None)
        headers = {
            **auth_headers,
            "User-Agent": "OpenSandbox-Go-SDK/0.1.0",
        }
        with patch(
            "opensandbox_server.api.metrics.record_sandbox_create_duration"
        ) as record:
            response = client.post(
                "/v1/metrics/events",
                json=body,
                headers=headers,
            )
        assert response.status_code == 204
        record.assert_called_once_with(
            create_duration_ms=1234,
            sdk_language="go",
            sdk_version="0.1.0",
            success=False,
        )

    def test_invalid_body_returns_422(self, client: TestClient, auth_headers: dict):
        response = client.post(
            "/v1/metrics/events",
            json={"eventType": "sandbox.create"},
            headers=auth_headers,
        )
        assert response.status_code == 422

    def test_record_helper_swallows_errors(self):
        mock_hist = MagicMock()
        mock_hist.record.side_effect = RuntimeError("boom")
        with patch.object(otel_metrics, "_create_duration_histogram", mock_hist):
            otel_metrics.record_sandbox_create_duration(
                create_duration_ms=1,
                sdk_language="python",
                sdk_version="0.0.0",
                success=False,
            )
        mock_hist.record.assert_called_once()
