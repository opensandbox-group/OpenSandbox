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
from unittest.mock import call, MagicMock

import pytest
from fastapi import HTTPException
from kubernetes.client import V1ResourceRequirements

from opensandbox_server.services.constants import SANDBOX_ID_LABEL, SandboxErrorCodes
from opensandbox_server.services.k8s.k8s_diagnostics import (
    K8sDiagnosticsMixin,
    _parse_since,
)


class _DiagnosticsService(K8sDiagnosticsMixin):
    def __init__(self, pods, resolved_namespace: str | None = None):
        self.namespace = "sandbox-system"
        self.resolved_namespace = resolved_namespace or self.namespace
        self.k8s_client = MagicMock()
        self.k8s_client.list_pods.return_value = pods
        self.core_v1 = MagicMock()
        self.k8s_client.get_core_v1_api.return_value = self.core_v1

    def _resolve_namespace(self) -> str:
        return self.resolved_namespace


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
    namespace="sandbox-system",
):
    return SimpleNamespace(
        metadata=SimpleNamespace(
            name="pod-1",
            namespace=namespace,
            labels={SANDBOX_ID_LABEL: "sbx-1", "app": "opensandbox"},
        ),
        spec=SimpleNamespace(
            node_name="node-1",
            runtime_class_name="gvisor",
            containers=[
                SimpleNamespace(
                    name="main",
                    resources=V1ResourceRequirements(
                        requests={"cpu": "500m"},
                        limits={"memory": "512Mi"},
                    ),
                )
            ],
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


def test_find_pod_uses_resolved_tenant_namespace() -> None:
    service = _DiagnosticsService([_pod(namespace="tenant-a")], resolved_namespace="tenant-a")

    service._find_pod_for_sandbox("sbx-1")

    service.k8s_client.list_pods.assert_called_once_with(
        namespace="tenant-a",
        label_selector=f"{SANDBOX_ID_LABEL}=sbx-1",
    )


def test_get_sandbox_logs_passes_tail_and_since() -> None:
    service = _DiagnosticsService([_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = "log line"

    assert service.get_sandbox_logs("sbx-1", tail=10, since="1h") == "log line"

    # Single-container pod uses its only container ("main") as the default.
    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="main",
        tail_lines=10,
        timestamps=True,
        since_seconds=3600,
    )


def test_get_sandbox_logs_uses_found_pod_namespace() -> None:
    service = _DiagnosticsService([_pod(namespace="tenant-a")], resolved_namespace="tenant-a")
    service.core_v1.read_namespaced_pod_log.return_value = "tenant log"

    assert service.get_sandbox_logs("sbx-1") == "tenant log"

    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="tenant-a",
        container="main",
        tail_lines=100,
        timestamps=True,
    )


def test_get_sandbox_logs_returns_placeholder_for_empty_output() -> None:
    service = _DiagnosticsService([_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = ""

    assert service.get_sandbox_logs("sbx-1") == "(no logs)"


def _multi_container_pod():
    """Pod modelled after a real OSB sandbox: init + user "sandbox" + egress sidecar."""
    return SimpleNamespace(
        metadata=SimpleNamespace(
            name="pod-1",
            namespace="sandbox-system",
            labels={SANDBOX_ID_LABEL: "sbx-1"},
        ),
        spec=SimpleNamespace(
            node_name="node-1",
            runtime_class_name=None,
            containers=[
                SimpleNamespace(name="sandbox", resources=None),
                SimpleNamespace(name="egress", resources=None),
            ],
            init_containers=[SimpleNamespace(name="execd-installer", resources=None)],
        ),
        status=SimpleNamespace(
            phase="Running",
            pod_ip="10.1.2.3",
            host_ip="192.168.1.10",
            start_time="2026-01-01T00:00:00Z",
            container_statuses=[],
            init_container_statuses=[],
            conditions=[],
        ),
    )


def test_get_sandbox_logs_defaults_to_sandbox_container_on_multi_container_pod() -> None:
    service = _DiagnosticsService([_multi_container_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = "user log"

    assert service.get_sandbox_logs("sbx-1", tail=5) == "user log"

    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="sandbox",
        tail_lines=5,
        timestamps=True,
    )


def test_get_sandbox_logs_accepts_container_override_for_sidecars() -> None:
    service = _DiagnosticsService([_multi_container_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = "egress log"

    assert service.get_sandbox_logs("sbx-1", container="egress") == "egress log"

    service.core_v1.read_namespaced_pod_log.assert_called_once_with(
        name="pod-1",
        namespace="sandbox-system",
        container="egress",
        tail_lines=100,
        timestamps=True,
    )


def test_get_sandbox_logs_allows_init_container_by_name() -> None:
    service = _DiagnosticsService([_multi_container_pod()])
    service.core_v1.read_namespaced_pod_log.return_value = "init log"

    assert service.get_sandbox_logs("sbx-1", container="execd-installer") == "init log"


def test_get_sandbox_logs_unknown_container_returns_404() -> None:
    service = _DiagnosticsService([_multi_container_pod()])

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_logs("sbx-1", container="not-a-container")
    assert exc.value.status_code == 404
    assert "not-a-container" in str(exc.value.detail["message"])


def test_get_sandbox_logs_maps_kubernetes_400_to_400_response() -> None:
    from kubernetes.client.exceptions import ApiException

    service = _DiagnosticsService([_multi_container_pod()])
    api_exc = ApiException(status=400, reason="Bad Request")
    api_exc.body = (
        'a container name must be specified for pod sbx-1-0, '
        'choose one of: [sandbox egress]'
    )
    service.core_v1.read_namespaced_pod_log.side_effect = api_exc

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_logs("sbx-1")
    assert exc.value.status_code == 400
    assert "sandbox" in str(exc.value.detail["message"])


def test_get_sandbox_logs_maps_kubernetes_403_to_forbidden_response() -> None:
    from kubernetes.client.exceptions import ApiException

    service = _DiagnosticsService([_multi_container_pod()])
    api_exc = ApiException(status=403, reason="Forbidden")
    api_exc.body = "pods/log forbidden"
    service.core_v1.read_namespaced_pod_log.side_effect = api_exc

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_logs("sbx-1")
    assert exc.value.status_code == 403


@pytest.mark.parametrize("api_status", [None, 503])
def test_get_sandbox_logs_maps_undocumented_kubernetes_failures_to_500(
    api_status: int | None,
) -> None:
    from kubernetes.client.exceptions import ApiException

    service = _DiagnosticsService([_multi_container_pod()])
    api_exc = ApiException(status=api_status, reason="Kubernetes log API error")
    api_exc.body = "log API unavailable"
    service.core_v1.read_namespaced_pod_log.side_effect = api_exc

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_logs("sbx-1")

    assert exc.value.status_code == 500
    detail = cast(dict[str, str], exc.value.detail)
    assert detail["code"] == SandboxErrorCodes.K8S_API_ERROR
    assert "pod-1" in detail["message"]
    assert api_exc.body in detail["message"]


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


def test_get_sandbox_events_uses_found_pod_namespace() -> None:
    service = _DiagnosticsService([_pod(namespace="tenant-a")], resolved_namespace="tenant-a")
    service.core_v1.list_namespaced_event.return_value = SimpleNamespace(items=[])

    assert service.get_sandbox_events("sbx-1") == "(no events)"

    service.core_v1.list_namespaced_event.assert_called_once_with(
        namespace="tenant-a",
        field_selector="involvedObject.name=pod-1",
        limit=50,
    )


def test_get_sandbox_events_follows_continuation_until_limit() -> None:
    service = _DiagnosticsService([_pod()])

    def event(index: int) -> SimpleNamespace:
        return SimpleNamespace(
            last_timestamp=f"2026-01-01T00:00:{index:02d}Z",
            event_time=None,
            first_timestamp=None,
            type="Normal",
            reason="Started",
            message=f"event-{index}",
        )

    service.core_v1.list_namespaced_event.side_effect = [
        SimpleNamespace(
            items=[event(index) for index in range(50)],
            metadata=SimpleNamespace(_continue="next-page-token"),
        ),
        SimpleNamespace(
            items=[event(50)],
            metadata=SimpleNamespace(_continue=None),
        ),
    ]

    output = service.get_sandbox_events("sbx-1", limit=51)

    assert len(output.splitlines()) == 51
    assert "event-50" in output
    assert service.core_v1.list_namespaced_event.call_args_list == [
        call(
            namespace="sandbox-system",
            field_selector="involvedObject.name=pod-1",
            limit=51,
        ),
        call(
            namespace="sandbox-system",
            field_selector="involvedObject.name=pod-1",
            limit=51,
            _continue="next-page-token",
        ),
    ]


def test_stable_event_diagnostics_policy_is_owned_by_kubernetes_service() -> None:
    service = _DiagnosticsService([_pod()])
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


def test_stable_log_diagnostics_policy_is_owned_by_kubernetes_service() -> None:
    service = _DiagnosticsService([_pod()])
    lines = [f"line {index}" for index in range(101)]
    service.get_sandbox_logs = MagicMock(return_value="\n".join(lines))

    result = service.get_sandbox_log_diagnostics("sbx-1", scope="ALL")

    assert result.scope == "all"
    assert result.content.splitlines() == lines[-100:]
    assert result.truncated is True
    assert result.warnings == (
        "The current backend only contributes sandbox container logs to the all scope.",
    )
    service.get_sandbox_logs.assert_called_once_with(
        "sbx-1",
        tail=101,
        since=None,
        container=None,
    )


@pytest.mark.parametrize(
    ("method_name", "scope", "kind", "supported"),
    [
        ("get_sandbox_log_diagnostics", "lifecycle", "logs", "container, all"),
        ("get_sandbox_event_diagnostics", "network", "events", "runtime, all"),
    ],
)
def test_stable_diagnostics_reject_unsupported_kubernetes_scopes(
    method_name: str,
    scope: str,
    kind: str,
    supported: str,
) -> None:
    service = _DiagnosticsService([_pod()])

    with pytest.raises(HTTPException) as exc:
        getattr(service, method_name)("sbx-1", scope)

    assert exc.value.status_code == 400
    assert exc.value.detail == {
        "code": "DIAGNOSTICS_SCOPE_UNSUPPORTED",
        "message": (
            f"Unsupported {kind} diagnostics scope {scope!r}. Supported scopes: {supported}."
        ),
    }
    service.k8s_client.list_pods.assert_not_called()


@pytest.mark.parametrize(
    ("api_status", "expected_status", "expected_code"),
    [
        (400, 400, SandboxErrorCodes.K8S_API_ERROR),
        (403, 403, SandboxErrorCodes.K8S_API_ERROR),
        (404, 404, SandboxErrorCodes.K8S_SANDBOX_NOT_FOUND),
        (503, 500, SandboxErrorCodes.K8S_API_ERROR),
    ],
)
def test_get_sandbox_events_maps_kubernetes_api_errors(
    api_status: int,
    expected_status: int,
    expected_code: str,
) -> None:
    from kubernetes.client.exceptions import ApiException

    service = _DiagnosticsService([_pod()])
    api_exc = ApiException(status=api_status, reason="Kubernetes event API error")
    api_exc.body = f"event API failed with status {api_status}"
    service.core_v1.list_namespaced_event.side_effect = api_exc

    with pytest.raises(HTTPException) as exc:
        service.get_sandbox_events("sbx-1")

    assert exc.value.status_code == expected_status
    detail = exc.value.detail
    assert isinstance(detail, dict)
    typed_detail = cast(dict[str, str], detail)
    assert typed_detail["code"] == expected_code
    assert "pod-1" in typed_detail["message"]
    assert api_exc.body in typed_detail["message"]
