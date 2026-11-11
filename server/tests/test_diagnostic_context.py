# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

from types import SimpleNamespace
from unittest.mock import Mock, PropertyMock

import pytest
from fastapi import HTTPException

from opensandbox_server.api import devops
from opensandbox_server.services.diagnostics import DiagnosticContext
from opensandbox_server.services.docker.docker_diagnostics import DockerDiagnosticsMixin
from opensandbox_server.services.k8s.k8s_diagnostics import K8sDiagnosticsMixin
from opensandbox_server.services.fast_sandbox.service import FastSandboxService
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.sandbox_service import SandboxService


@pytest.mark.parametrize("backend", ["docker", "kubernetes", "fast-sandbox"])
def test_context_route(client, auth_headers, monkeypatch, backend):
    service = Mock()
    service.get_sandbox_diagnostic_context.return_value = DiagnosticContext(
        sandbox_id="sbx-1",
        state="Paused",
        backend_type=backend,
        backend_identifiers={"resource": "runtime-1"},
    )
    monkeypatch.setattr(devops, "sandbox_service", service)
    response = client.get("/v1/sandboxes/sbx-1/diagnostics/context", headers=auth_headers)
    assert response.status_code == 200
    assert response.json() == {
        "sandboxId": "sbx-1",
        "state": "Paused",
        "backendType": backend,
        "backendIdentifiers": {"resource": "runtime-1"},
    }
    service.get_sandbox_diagnostic_context.assert_called_once_with("sbx-1")


@pytest.mark.parametrize("state_fields", [{}, {"state": None}])
def test_context_route_omits_unavailable_state(client, auth_headers, monkeypatch, state_fields):
    service = Mock()
    service.get_sandbox_diagnostic_context.return_value = DiagnosticContext(
        sandbox_id="sbx-1",
        backend_type="extension",
        backend_identifiers={"resource": "runtime-1"},
        **state_fields,
    )
    monkeypatch.setattr(devops, "sandbox_service", service)

    response = client.get("/v1/sandboxes/sbx-1/diagnostics/context", headers=auth_headers)

    assert response.status_code == 200
    assert response.json() == {
        "sandboxId": "sbx-1",
        "backendType": "extension",
        "backendIdentifiers": {"resource": "runtime-1"},
    }
    service.get_sandbox_diagnostic_context.assert_called_once_with("sbx-1")


@pytest.mark.parametrize(
    "status,code", [(404, "NOT_FOUND"), (403, "FORBIDDEN"), (501, "DIAGNOSTICS_NOT_IMPLEMENTED")]
)
def test_context_route_preserves_backend_errors(client, auth_headers, monkeypatch, status, code):
    service = Mock()
    service.get_sandbox_diagnostic_context.side_effect = HTTPException(
        status,
        detail={"code": code, "message": "Unavailable"},
    )
    monkeypatch.setattr(devops, "sandbox_service", service)
    response = client.get("/v1/sandboxes/sbx-1/diagnostics/context", headers=auth_headers)
    assert response.status_code == status


@pytest.mark.parametrize(
    "container_state,expected_state",
    [
        ({"Status": "running", "Running": True}, "Running"),
        ({"Status": "paused", "Running": True, "Paused": True}, "Paused"),
        ({"Status": "restarting", "Restarting": True}, "Running"),
        ({"Status": "created"}, "Pending"),
        ({"Status": "starting"}, "Pending"),
        ({"Status": "exited", "ExitCode": 0}, "Terminated"),
        ({"Status": "dead", "ExitCode": 1}, "Failed"),
        ({"Status": "exited"}, "Failed"),
        ({"Status": "removing"}, "Unknown"),
        ({}, "Unknown"),
    ],
)
def test_docker_context_uses_owned_container_without_image_lookup(container_state, expected_state):
    service = DockerDiagnosticsMixin()
    container = Mock(id="container-1", attrs={"State": container_state}, status="")
    container.name = "sandbox-1"
    image = PropertyMock(side_effect=RuntimeError("Image inspect unavailable"))
    type(container).image = image
    service._get_container_by_sandbox_id = Mock(return_value=container)
    context = service.get_sandbox_diagnostic_context("sbx-1")
    assert context.state == expected_state
    assert context.backend_type == "docker"
    assert context.backend_identifiers == {
        "containerId": "container-1",
        "containerName": "sandbox-1",
    }
    service._get_container_by_sandbox_id.assert_called_once_with("sbx-1")
    image.assert_not_called()


def test_kubernetes_context_is_available_before_pod_allocation(monkeypatch):
    service = K8sDiagnosticsMixin()
    service._resolve_namespace_for_lookup = Mock(return_value="tenant-ns")
    service.workload_provider = Mock()
    workload = {"metadata": {"name": "workload-1", "uid": "uid-1"}}
    owned = Mock(return_value=workload)
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.k8s_diagnostics._get_owned_workload_or_404", owned
    )
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.k8s_diagnostics._build_sandbox_from_workload",
        Mock(return_value=SimpleNamespace(status=SimpleNamespace(state="Pending"))),
    )
    context = service.get_sandbox_diagnostic_context("sbx-1")
    assert context.state == "Pending"
    assert context.backend_identifiers == {
        "namespace": "tenant-ns",
        "workloadName": "workload-1",
        "workloadUid": "uid-1",
    }
    owned.assert_called_once_with(service.workload_provider, "tenant-ns", "sbx-1")


def test_fast_context_uses_lifecycle_identity():
    service = Mock()
    service.get_sandbox.return_value = SimpleNamespace(status=SimpleNamespace(state="Running"))
    context = FastSandboxService.get_sandbox_diagnostic_context(service, "fsb-1")
    assert context.backend_type == "fast-sandbox"
    assert context.backend_identifiers == {}
    service.get_sandbox.assert_called_once_with("fsb-1")


@pytest.mark.parametrize("failure_stage", ["namespace", "workload", "mapping"])
def test_kubernetes_context_normalizes_errors(client, auth_headers, monkeypatch, failure_stage):
    service = K8sDiagnosticsMixin()
    service._resolve_namespace_for_lookup = Mock(return_value="tenant-ns")
    service.workload_provider = Mock()
    owned = Mock(return_value={"metadata": {}})
    mapper = Mock()
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.k8s_diagnostics._get_owned_workload_or_404", owned
    )
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.k8s_diagnostics._build_sandbox_from_workload", mapper
    )
    stages = {"namespace": service._resolve_namespace_for_lookup, "workload": owned, "mapping": mapper}
    stages[failure_stage].side_effect = RuntimeError("API server unavailable")
    monkeypatch.setattr(devops, "sandbox_service", service)

    response = client.get("/v1/sandboxes/sbx-1/diagnostics/context", headers=auth_headers)

    assert response.status_code == 500
    assert response.json() == {
        "code": "KUBERNETES::API_ERROR",
        "message": "Failed to get sandbox diagnostic context: API server unavailable",
    }


@pytest.mark.parametrize("status_code", [403, 404])
def test_kubernetes_context_preserves_http_errors(monkeypatch, status_code):
    service = K8sDiagnosticsMixin()
    service._resolve_namespace_for_lookup = Mock(return_value="tenant-ns")
    service.workload_provider = Mock()
    error = HTTPException(status_code, detail={"code": "TEST_ERROR", "message": "Unavailable"})
    monkeypatch.setattr(
        "opensandbox_server.services.k8s.k8s_diagnostics._get_owned_workload_or_404",
        Mock(side_effect=error),
    )
    with pytest.raises(HTTPException) as raised:
        service.get_sandbox_diagnostic_context("sbx-1")
    assert raised.value is error


@pytest.mark.parametrize("sandbox_id,backend", [("fsb-1", "fsb"), ("sbx-1", "kubernetes")])
def test_composite_routes_context(sandbox_id, backend):
    k8s, fsb = Mock(), Mock()
    service = CompositeSandboxService(k8s, fsb)
    expected = fsb if backend == "fsb" else k8s
    assert (
        service.get_sandbox_diagnostic_context(sandbox_id)
        is expected.get_sandbox_diagnostic_context.return_value
    )
    expected.get_sandbox_diagnostic_context.assert_called_once_with(sandbox_id)


def test_extension_context_defaults_to_not_implemented():
    with pytest.raises(HTTPException) as error:
        SandboxService.get_sandbox_diagnostic_context(Mock(), "sbx-1")
    assert error.value.status_code == 501
