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

from types import SimpleNamespace
from typing import cast
from unittest.mock import MagicMock, patch

from docker.errors import DockerException
from fastapi import HTTPException, status
import pytest

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.docker.docker_diagnostics import (
    DockerDiagnosticsMixin,
    _parse_since_to_timestamp,
)


class _DiagnosticsService(DockerDiagnosticsMixin):
    def __init__(self, container):
        self.container = container

    def _get_container_by_sandbox_id(self, sandbox_id: str):
        assert sandbox_id == "sbx-1"
        return self.container


def _container(attrs: dict):
    return SimpleNamespace(
        id="abcdef1234567890",
        name="opensandbox-sbx-1",
        attrs=attrs,
        logs=MagicMock(return_value=b"2026-01-01T00:00:00Z booted\n"),
    )


def test_parse_since_to_timestamp_uses_valid_units_and_default() -> None:
    with patch("opensandbox_server.services.docker.docker_diagnostics.time.time", return_value=1000):
        assert _parse_since_to_timestamp("2m") == 880
        assert _parse_since_to_timestamp("1 h") == -2600
        assert _parse_since_to_timestamp("nonsense") == 400


def test_get_sandbox_logs_decodes_bytes_and_passes_since() -> None:
    container = _container({"State": {}})
    service = _DiagnosticsService(container)

    with patch(
        "opensandbox_server.services.docker.docker_diagnostics._parse_since_to_timestamp",
        return_value=123,
    ):
        result = service.get_sandbox_logs("sbx-1", tail=25, since="5m")

    assert result == "2026-01-01T00:00:00Z booted\n"
    container.logs.assert_called_once_with(tail=25, timestamps=True, since=123)


def test_get_sandbox_logs_returns_placeholder_for_empty_output() -> None:
    container = _container({"State": {}})
    container.logs.return_value = ""
    service = _DiagnosticsService(container)

    assert service.get_sandbox_logs("sbx-1") == "(no logs)"


def test_get_sandbox_logs_maps_docker_errors_to_contract_response() -> None:
    container = _container({"State": {}})
    container.logs.side_effect = DockerException("daemon disconnected")
    service = _DiagnosticsService(container)

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_logs("sbx-1")

    assert exc.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
    detail = cast(dict[str, str], exc.value.detail)
    assert detail["code"] == SandboxErrorCodes.CONTAINER_QUERY_FAILED
    assert detail["message"] == "Failed to read logs for sandbox sbx-1: daemon disconnected"


def test_stable_log_diagnostics_policy_is_owned_by_docker_service() -> None:
    container = _container({"State": {}})
    lines = [f"line {index}" for index in range(101)]
    container.logs.return_value = "\n".join(lines)
    service = _DiagnosticsService(container)

    result = service.get_sandbox_log_diagnostics("sbx-1", scope="ALL")

    assert result.scope == "all"
    assert result.content.splitlines() == lines[-100:]
    assert result.truncated is True
    assert result.warnings == (
        "The current backend only contributes sandbox container logs to the all scope.",
    )
    container.logs.assert_called_once_with(tail=101, timestamps=True)


def test_stable_event_diagnostics_policy_is_owned_by_docker_service() -> None:
    service = _DiagnosticsService(_container({"State": {}}))
    events = [f"event {index}" for index in range(51)]
    service.get_sandbox_events = MagicMock(return_value="\n".join(events))

    result = service.get_sandbox_event_diagnostics("sbx-1", scope="ALL")

    assert result.scope == "all"
    assert result.content.splitlines() == events[:50]
    assert result.truncated is True
    assert result.warnings == (
        "The current backend only contributes runtime events to the all scope.",
    )
    service.get_sandbox_events.assert_called_once_with("sbx-1", limit=51)


@pytest.mark.parametrize(
    ("method_name", "scope", "kind", "supported"),
    [
        ("get_sandbox_log_diagnostics", "lifecycle", "logs", "container, all"),
        ("get_sandbox_event_diagnostics", "network", "events", "runtime, all"),
    ],
)
def test_stable_diagnostics_reject_unsupported_docker_scopes(
    method_name: str,
    scope: str,
    kind: str,
    supported: str,
) -> None:
    container = _container({"State": {}})
    service = _DiagnosticsService(container)

    with pytest.raises(HTTPException) as exc:
        getattr(service, method_name)("sbx-1", scope)

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert exc.value.detail == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            f"Unsupported {kind} diagnostics scope {scope!r}. Supported scopes: {supported}."
        ),
    }
    container.logs.assert_not_called()


def test_get_sandbox_inspect_formats_state_resources_ports_and_safe_env() -> None:
    container = _container(
        {
            "Created": "2026-01-01T00:00:00Z",
            "State": {
                "Status": "running",
                "Running": True,
                "Paused": False,
                "OOMKilled": False,
                "ExitCode": 0,
                "StartedAt": "2026-01-01T00:00:01Z",
                "FinishedAt": "0001-01-01T00:00:00Z",
                "Error": "minor warning",
            },
            "Config": {
                "Image": "python:3.12",
                "Labels": {"b": "2", "a": "1"},
                "Env": ["PLAIN=value", "API_TOKEN=secret"],
            },
            "NetworkSettings": {
                "Networks": {"bridge": {"IPAddress": "172.17.0.2"}},
                "Ports": {
                    "80/tcp": [{"HostIp": "127.0.0.1", "HostPort": "8080"}],
                    "81/tcp": None,
                },
            },
            "HostConfig": {
                "NanoCpus": 2_000_000_000,
                "Memory": 512 * 1024 * 1024,
                "PidsLimit": 256,
            },
        }
    )
    service = _DiagnosticsService(container)

    output = service.get_sandbox_inspect("sbx-1")

    assert "Container ID:   abcdef123456" in output
    assert "CPU:          2.00 cores" in output
    assert "Memory:       512 MiB" in output
    assert "bridge: 172.17.0.2" in output
    assert "80/tcp -> 127.0.0.1:8080" in output
    assert "81/tcp (not bound)" in output
    assert "a=1" in output
    assert "PLAIN=value" in output
    assert "API_TOKEN=***" in output


def test_get_sandbox_events_reports_state_health_and_fallback() -> None:
    container = _container(
        {
            "Created": "2026-01-01T00:00:00Z",
            "State": {
                "Status": "exited",
                "StartedAt": "2026-01-01T00:00:01Z",
                "FinishedAt": "2026-01-01T00:00:02Z",
                "OOMKilled": True,
                "ExitCode": 137,
                "Error": "killed",
                "Health": {
                    "Status": "unhealthy",
                    "Log": [
                        {"Start": "t1", "ExitCode": 1, "Output": "failed\n"},
                        {"Start": "t2", "ExitCode": 0, "Output": "ok\n"},
                    ],
                },
            },
        }
    )
    service = _DiagnosticsService(container)

    output = service.get_sandbox_events("sbx-1", limit=1)

    assert "OOMKilled" in output
    assert "Exited with code 137" in output
    assert "Error - killed" in output
    assert "Health:     unhealthy" in output
    assert "[t2] exit=0 ok" in output
    assert "[t1]" not in output

    quiet = _DiagnosticsService(_container({"State": {"Status": "running"}}))
    assert "(no notable events)" in quiet.get_sandbox_events("sbx-1")
