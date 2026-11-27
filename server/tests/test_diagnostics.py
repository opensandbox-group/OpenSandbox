# Copyright 2026 The OpenSandbox Authors
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

from fastapi import HTTPException
from fastapi.testclient import TestClient

from opensandbox_server.api import diagnostics
from opensandbox_server.services.diagnostics import DiagnosticResult


def test_diagnostics_logs_require_scope(client: TestClient, auth_headers: dict) -> None:
    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs",
        headers=auth_headers,
    )

    assert response.status_code == 422


def test_diagnostics_events_require_scope(client: TestClient, auth_headers: dict) -> None:
    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events",
        headers=auth_headers,
    )

    assert response.status_code == 422


def test_diagnostics_logs_with_scope_returns_stable_inline_descriptor(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    content = "sandbox log: 测试"

    class StubService:
        @staticmethod
        def get_sandbox_log_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            assert sandbox_id == "sbx-001"
            assert scope == "container"
            return DiagnosticResult(
                sandbox_id=sandbox_id,
                kind="logs",
                scope=scope,
                content=content,
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=container",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/json")
    assert response.json() == {
        "sandboxId": "sbx-001",
        "kind": "logs",
        "scope": "container",
        "delivery": "inline",
        "contentType": "text/plain; charset=utf-8",
        "content": content,
        "contentLength": len(content.encode("utf-8")),
        "truncated": False,
    }


def test_diagnostics_logs_serializes_service_truncation_result(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    lines = [f"line {index}" for index in range(101)]

    class StubService:
        @staticmethod
        def get_sandbox_log_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            assert sandbox_id == "sbx-001"
            assert scope == "container"
            return DiagnosticResult(
                sandbox_id=sandbox_id,
                kind="logs",
                scope=scope,
                content="\n".join(lines[-100:]),
                truncated=True,
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=container",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["content"].splitlines() == lines[-100:]
    assert response.json()["truncated"] is True


def test_diagnostics_logs_rejects_unsupported_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_log_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            raise HTTPException(
                status_code=400,
                detail={
                    "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
                    "message": (
                        f"Unsupported logs diagnostics scope {scope!r}. "
                        "Supported scopes: container, all."
                    ),
                },
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=lifecycle",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json() == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            "Unsupported logs diagnostics scope 'lifecycle'. Supported scopes: container, all."
        ),
    }


def test_diagnostics_logs_all_scope_discloses_backend_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_log_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            return DiagnosticResult(
                sandbox_id,
                "logs",
                scope,
                "container logs only",
                warnings=(
                    "The current backend only contributes sandbox container logs to the all scope.",
                ),
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/logs?scope=all",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["warnings"] == [
        "The current backend only contributes sandbox container logs to the all scope."
    ]


def test_diagnostics_events_with_scope_returns_stable_inline_descriptor(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_event_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            assert sandbox_id == "sbx-001"
            assert scope == "RUNTIME"
            return DiagnosticResult(
                sandbox_id,
                "events",
                scope.lower(),
                "runtime event",
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=RUNTIME",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/json")
    assert response.json() == {
        "sandboxId": "sbx-001",
        "kind": "events",
        "scope": "runtime",
        "delivery": "inline",
        "contentType": "text/plain; charset=utf-8",
        "content": "runtime event",
        "contentLength": 13,
        "truncated": False,
    }


def test_diagnostics_events_serializes_service_truncation_result(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    events = [f"event {index}" for index in range(51)]

    class StubService:
        @staticmethod
        def get_sandbox_event_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            assert sandbox_id == "sbx-001"
            assert scope == "runtime"
            return DiagnosticResult(
                sandbox_id,
                "events",
                scope,
                "\n".join(events[:50]),
                truncated=True,
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=runtime",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["content"].splitlines() == events[:50]
    assert response.json()["truncated"] is True


def test_diagnostics_events_rejects_unavailable_lifecycle_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    calls: list[tuple[str, str]] = []

    class StubService:
        @staticmethod
        def get_sandbox_event_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            calls.append((sandbox_id, scope))
            raise HTTPException(
                status_code=400,
                detail={
                    "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
                    "message": (
                        f"Unsupported events diagnostics scope {scope!r}. "
                        "Supported scopes: runtime, all."
                    ),
                },
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=lifecycle",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json() == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            "Unsupported events diagnostics scope 'lifecycle'. "
            "Supported scopes: runtime, all."
        ),
    }
    assert calls == [("sbx-001", "lifecycle")]


def test_diagnostics_events_all_scope_discloses_backend_limit(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_event_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            return DiagnosticResult(
                sandbox_id,
                "events",
                scope,
                "runtime event",
                warnings=("The current backend only contributes runtime events to the all scope.",),
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=all",
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert response.json()["warnings"] == [
        "The current backend only contributes runtime events to the all scope."
    ]


def test_diagnostics_events_rejects_unsupported_scope(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    class StubService:
        @staticmethod
        def get_sandbox_event_diagnostics(
            sandbox_id: str,
            scope: str,
        ) -> DiagnosticResult:
            raise HTTPException(
                status_code=400,
                detail={
                    "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
                    "message": (
                        f"Unsupported events diagnostics scope {scope!r}. "
                        "Supported scopes: runtime, all."
                    ),
                },
            )

    monkeypatch.setattr(diagnostics, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes/sbx-001/diagnostics/events?scope=network",
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json()["code"] == "DIAGNOSTICS_SCOPE_UNSUPPORTED"
