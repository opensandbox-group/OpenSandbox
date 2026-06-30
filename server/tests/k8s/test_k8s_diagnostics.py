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
from unittest.mock import MagicMock

import pytest
from fastapi import HTTPException
from kubernetes.client import ApiException, V1ResourceRequirements

from opensandbox_server.services.constants import SANDBOX_ID_LABEL
from opensandbox_server.services.k8s.k8s_diagnostics import (
    K8sDiagnosticsMixin,
    _parse_since,
)


class _DiagnosticsService(K8sDiagnosticsMixin):
    def __init__(self, pods):
        self.namespace = "sandbox-system"
        self.k8s_client = MagicMock()
        self.k8s_client.list_pods.return_value = pods
        self.core_v1 = MagicMock()
        self.k8s_client.get_core_v1_api.return_value = self.core_v1


def _status(
    *,
    running=None,
    waiting=None,
    terminated=None,
    last_terminated=None,
):
    return SimpleNamespace(
        name="main",
        ready=True,
        restart_count=2,
        image="python:3.12",
        state=SimpleNamespace(
            running=running,
            waiting=waiting,
            terminated=terminated,
        ),
        last_state=SimpleNamespace(terminated=last_terminated),
    )


def _pod(
    container_statuses=None,
    init_container_statuses=None,
    conditions=None,
    containers=None,
    init_containers=None,
):
    return SimpleNamespace(
        metadata=SimpleNamespace(
            name="pod-1",
            namespace="sandbox-system",
            labels={SANDBOX_ID_LABEL: "sbx-1", "app": "opensandbox"},
        ),
        spec=SimpleNamespace(
            node_name="node-1",
            runtime_class_name="gvisor",
            containers=containers
            if containers is not None
            else [
                SimpleNamespace(
                    name="sandbox",
                    resources=V1ResourceRequirements(
                        requests={"cpu": "500m"},
                        limits={"memory": "512Mi"},
                    ),
                )
            ],
            init_containers=init_containers if init_containers is not None else [],
        ),
        status=SimpleNamespace(
            phase="Running",
            pod_ip="10.1.2.3",
            host_ip="192.168.1.10",
            start_time="2026-01-01T00:00:00Z",
            container_statuses=container_statuses or [],
            init_container_statuses=init_container_statuses or [],
            conditions=conditions or [],
        ),
    )


def test_parse_since_supports_units_and_default() -> None:
    assert _parse_since("5m") == 300
    assert _parse_since("2 h") == 7200
    assert _parse_since("invalid") == 600


def test_find_pod_uses_label_selector_and_maps_errors() -> None:
    service = _DiagnosticsService([_pod()])

    pod = service._find_pod_for_sandbox("sbx-1")

    assert pod.metadata.name == "pod-1"
    service.k8s_client.list_pods.assert_called_once_with(
        namespace="sandbox-system",
        label_selector=f"{SANDBOX_ID_LABEL}=sbx-1",
    )

    service.k8s_client.list_pods.return_value = []
    with pytest.raises(HTTPException) as not_found:
        service._find_pod_for_sandbox("missing")
    assert not_found.value.status_code == 404

    service.k8s_client.list_pods.side_effect = RuntimeError("api down")
    with pytest.raises(HTTPException) as api_error:
        service._find_pod_for_sandbox("sbx-1")
    assert api_error.value.status_code == 500


def test_get_sandbox_logs_passes_tail_and_since() -> None:
    service = _DiagnosticsService([_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = "log line"

    assert service.get_sandbox_logs("sbx-1", tail=10, since="1h") == "log line"

    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="sandbox",
        tail_lines=10,
        timestamps=True,
        since_seconds=3600,
    )


def test_get_sandbox_logs_returns_placeholder_for_empty_output() -> None:
    service = _DiagnosticsService([_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = ""

    assert service.get_sandbox_logs("sbx-1") == "(no logs)"


def test_get_sandbox_logs_prefers_sandbox_container_for_multi_container_pods() -> None:
    service = _DiagnosticsService([
        _pod(
            containers=[
                SimpleNamespace(name="sandbox", resources=V1ResourceRequirements()),
                SimpleNamespace(name="egress", resources=V1ResourceRequirements()),
            ]
        )
    ])
    service.core_v1.read_namespaced_pod_log.return_value = "sandbox logs"

    assert service.get_sandbox_logs("sbx-1", tail=20) == "sandbox logs"

    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="sandbox",
        tail_lines=20,
        timestamps=True,
    )


def test_get_sandbox_logs_falls_back_to_first_regular_container_then_init_container() -> None:
    service = _DiagnosticsService([
        _pod(
            containers=[SimpleNamespace(name="worker", resources=V1ResourceRequirements())],
        )
    ])
    service.core_v1.read_namespaced_pod_log.return_value = "worker logs"

    assert service.get_sandbox_logs("sbx-1") == "worker logs"
    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="worker",
        tail_lines=100,
        timestamps=True,
    )

    service = _DiagnosticsService([
        _pod(
            containers=[],
            container_statuses=[],
            init_containers=[SimpleNamespace(name="execd-installer")],
        )
    ])
    service.core_v1.read_namespaced_pod_log.return_value = "init logs"

    assert service.get_sandbox_logs("sbx-1") == "init logs"
    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="execd-installer",
        tail_lines=100,
        timestamps=True,
    )


def test_get_sandbox_logs_maps_k8s_api_exceptions() -> None:
    service = _DiagnosticsService([_pod()])
    bad_request_exc = ApiException(status=400, reason="Bad Request")
    bad_request_exc.body = '{"message":"choose one of: [sandbox egress]"}'
    service.core_v1.read_namespaced_pod_log.side_effect = bad_request_exc

    with pytest.raises(HTTPException) as bad_request:
        service.get_sandbox_logs("sbx-1")
    assert bad_request.value.status_code == 400
    assert bad_request.value.detail["code"] == "SANDBOX::INVALID_PARAMETER"
    assert "choose one of" in bad_request.value.detail["message"]

    service.core_v1.read_namespaced_pod_log.side_effect = ApiException(status=403, reason="Forbidden")
    with pytest.raises(HTTPException) as forbidden:
        service.get_sandbox_logs("sbx-1")
    assert forbidden.value.status_code == 403

    service.core_v1.read_namespaced_pod_log.side_effect = ApiException(status=500, reason="boom")
    with pytest.raises(HTTPException) as upstream_error:
        service.get_sandbox_logs("sbx-1")
    assert upstream_error.value.status_code == 502


def test_get_sandbox_inspect_formats_runtime_statuses_and_resources() -> None:
    running_status = _status(running=SimpleNamespace(started_at="2026-01-01T00:00:01Z"))
    waiting_status = _status(waiting=SimpleNamespace(reason="ImagePullBackOff", message="pull failed"))
    terminated_status = _status(
        terminated=SimpleNamespace(exit_code=1, reason="Error", message="boom"),
        last_terminated=SimpleNamespace(exit_code=2, reason="PreviousError"),
    )
    init_status = _status(terminated=SimpleNamespace(exit_code=0, reason="Completed"))
    waiting_init = _status(waiting=SimpleNamespace(reason="PodInitializing"))
    condition = SimpleNamespace(type="Ready", status="False", reason="ContainersNotReady", message="not ready")
    service = _DiagnosticsService(
        [
            _pod(
                container_statuses=[running_status, waiting_status, terminated_status],
                init_container_statuses=[init_status, waiting_init],
                conditions=[condition],
            )
        ]
    )

    output = service.get_sandbox_inspect("sbx-1")

    assert "Pod Name:       pod-1" in output
    assert "Runtime Class:  gvisor" in output
    assert "State:          Running (since 2026-01-01T00:00:01Z)" in output
    assert "Waiting (ImagePullBackOff)" in output
    assert "Terminated (exit=1, reason=Error)" in output
    assert "Last State:     Terminated (exit=2, reason=PreviousError)" in output
    assert "Init Containers:" in output
    assert "Ready: False (reason=ContainersNotReady)" in output
    assert f"{SANDBOX_ID_LABEL}=sbx-1" in output
    assert "Requests: {'cpu': '500m'}" in output
    assert "Limits:   {'memory': '512Mi'}" in output


def test_get_sandbox_events_formats_events_and_empty_result() -> None:
    service = _DiagnosticsService([_pod()])
    service.core_v1.list_namespaced_event.return_value = SimpleNamespace(
        items=[
            SimpleNamespace(
                last_timestamp="2026-01-01T00:00:00Z",
                event_time=None,
                first_timestamp=None,
                type="Warning",
                reason="Failed",
                message="container failed",
            ),
            SimpleNamespace(
                last_timestamp=None,
                event_time="2026-01-01T00:00:01Z",
                first_timestamp=None,
                type="Normal",
                reason=None,
                message=None,
            ),
        ]
    )

    output = service.get_sandbox_events("sbx-1", limit=2)

    assert "[2026-01-01T00:00:00Z] Warning" in output
    assert "Failed" in output
    assert "container failed" in output
    assert "N/A" in output
    service.core_v1.list_namespaced_event.assert_called_once_with(
        namespace="sandbox-system",
        field_selector="involvedObject.name=pod-1",
        limit=2,
    )

    service.core_v1.list_namespaced_event.return_value = SimpleNamespace(items=[])
    assert service.get_sandbox_events("sbx-1") == "(no events)"
