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

from fastapi.testclient import TestClient

from opensandbox_server.api import devops


def test_diagnostics_logs_with_scope_returns_not_implemented(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_logs(sandbox_id: str, tail: int, since: str | None = None) -> str:
            raise AssertionError("stable diagnostics requests must not call legacy logs")

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=container",
        headers=auth_headers,
    )

    assert response.status_code == 501
    assert response.headers["content-type"].startswith("application/json")
    assert response.json()["code"] == "DIAGNOSTICS_NOT_IMPLEMENTED"


def test_diagnostics_logs_without_scope_preserves_deprecated_plain_text(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_logs(sandbox_id: str, tail: int, since: str | None = None) -> str:
            assert sandbox_id == "sbx-001"
            assert tail == 25
            assert since == "5m"
            return "legacy logs"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?tail=25&since=5m",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("text/plain")
    assert response.headers["deprecation"] == "true"
    assert response.text == "legacy logs"


def test_diagnostics_events_with_scope_returns_not_implemented(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            raise AssertionError("stable diagnostics requests must not call legacy events")

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=runtime",
        headers=auth_headers,
    )

    assert response.status_code == 501
    assert response.headers["content-type"].startswith("application/json")
    assert response.json()["code"] == "DIAGNOSTICS_NOT_IMPLEMENTED"


def test_diagnostics_summary_redacts_unexpected_exception_details(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_inspect(sandbox_id: str) -> str:
            raise RuntimeError("backend secret token")

        @staticmethod
        def get_sandbox_events(sandbox_id: str, limit: int) -> str:
            return "events ok"

        @staticmethod
        def get_sandbox_logs(sandbox_id: str, tail: int) -> str:
            return "logs ok"

    monkeypatch.setattr(devops, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/summary",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert "[error] Failed to collect inspect diagnostics." in response.text
    assert "backend secret token" not in response.text
    assert "events ok" in response.text
    assert "logs ok" in response.text
