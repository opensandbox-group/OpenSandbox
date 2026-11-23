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

"""BatchSandbox pause refuses runtimes the rootfs snapshot committer cannot capture."""

from typing import Any
from unittest.mock import MagicMock

import pytest
from fastapi import HTTPException
from kubernetes.client import (
    ApiException,
    V1Container,
    V1ObjectMeta,
    V1Pod,
    V1PodSpec,
    V1PodStatus,
    V1RuntimeClass,
)
from urllib3.exceptions import ProtocolError

from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.client import K8sClient

SANDBOX_ID = "test-id"
NAMESPACE = "test-ns"


def _client(
    pod_spec: dict[str, Any],
    runtime_classes: dict[str, Any],
    *,
    workload_metadata: dict[str, Any] | None = None,
    pods: dict[str, Any] | None = None,
    conditions: list[dict[str, Any]] | None = None,
) -> MagicMock:
    client = MagicMock(spec=K8sClient)
    client.get_custom_object.return_value = {
        "metadata": {"name": SANDBOX_ID, "namespace": NAMESPACE, **(workload_metadata or {})},
        "status": {"phase": "Succeed", "conditions": conditions or []},
    }
    known_pods = (
        pods
        if pods is not None
        else {f"{SANDBOX_ID}-0": {"spec": pod_spec, "status": {"phase": "Running"}}}
    )
    client.read_pod.side_effect = lambda namespace, name: known_pods.get(name)
    client.list_pods.return_value = []

    def read_runtime_class(name: str) -> Any:
        runtime_class = runtime_classes.get(name)
        if isinstance(runtime_class, Exception):
            raise runtime_class
        if runtime_class is None:
            raise ApiException(status=404, reason="Not Found")
        return runtime_class

    client.read_runtime_class.side_effect = read_runtime_class
    return client


@pytest.mark.parametrize("handler", ["runsc", "runsc-kvm", "RUNSC"])
def test_pause_refuses_gvisor_handler_and_leaves_box_untouched(handler: str) -> None:
    client = _client({"runtimeClassName": "sandboxed"}, {"sandboxed": {"handler": handler}})
    provider = BatchSandboxProvider(client)

    with pytest.raises(
        ValueError,
        match=rf"gVisor RuntimeClass 'sandboxed' \(handler '{handler}'\)",
    ):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_retry_after_pause_failed_also_refuses_gvisor() -> None:
    client = _client(
        {"runtimeClassName": "sandboxed"},
        {"sandboxed": {"handler": "runsc"}},
        conditions=[{"type": "PauseFailed", "status": "True", "reason": "SnapshotFailed"}],
    )
    provider = BatchSandboxProvider(client)

    with pytest.raises(ValueError, match="gVisor RuntimeClass"):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_refuses_gvisor_handler_of_allocated_pool_pod() -> None:
    client = _client(
        {},
        {"sandboxed": {"handler": "runsc"}},
        workload_metadata={
            "annotations": {
                "sandbox.opensandbox.io/alloc-status": (
                    '{"pods":["pool-pod-1"],"poolRef":"gvisor-pool"}'
                ),
            },
        },
        pods={
            "pool-pod-1": {
                "spec": {"runtimeClassName": "sandboxed"},
                "status": {"phase": "Running"},
            },
        },
    )
    provider = BatchSandboxProvider(client)

    with pytest.raises(ValueError, match="gVisor RuntimeClass"):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_without_runtime_class_patches_pause() -> None:
    client = _client({}, {})
    provider = BatchSandboxProvider(client)

    provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.read_runtime_class.assert_not_called()
    client.patch_custom_object.assert_called_once()
    assert client.patch_custom_object.call_args.kwargs["body"] == {"spec": {"pause": True}}


@pytest.mark.parametrize("handler", ["runc", "kata", "kata-qemu"])
def test_pause_with_supported_handler_patches_pause(handler: str) -> None:
    client = _client({"runtimeClassName": "native"}, {"native": {"handler": handler}})
    provider = BatchSandboxProvider(client)

    provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_called_once()
    assert client.patch_custom_object.call_args.kwargs["body"] == {"spec": {"pause": True}}


@pytest.mark.parametrize(
    "runtime_classes",
    [
        {"sandboxed": ApiException(status=403, reason="Forbidden")},
        {"sandboxed": ProtocolError("Connection aborted.")},
        {},
        {"sandboxed": {"handler": ""}},
    ],
    ids=["forbidden", "unreachable", "missing", "no-handler"],
)
def test_pause_fails_closed_when_runtime_class_cannot_be_read(
    runtime_classes: dict[str, Any],
) -> None:
    client = _client({"runtimeClassName": "sandboxed"}, runtime_classes)
    provider = BatchSandboxProvider(client)

    with pytest.raises(ValueError, match="Cannot pause: cannot verify the runtime of sandbox"):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_reads_pods_of_a_workload_named_differently_from_the_sandbox() -> None:
    client = _client(
        {},
        {"sandboxed": {"handler": "runsc"}},
        pods={
            "legacy-name-0": {
                "spec": {"runtimeClassName": "sandboxed"},
                "status": {"phase": "Running"},
            },
        },
    )
    client.get_custom_object.return_value["metadata"]["name"] = "legacy-name"
    provider = BatchSandboxProvider(client)

    with pytest.raises(ValueError, match="gVisor RuntimeClass"):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_fails_closed_without_running_source_pod() -> None:
    client = _client({}, {}, pods={})
    provider = BatchSandboxProvider(client)

    with pytest.raises(ValueError, match="no running source Pod was found"):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def _typed_pod(runtime_class_name: str | None) -> V1Pod:
    return V1Pod(
        metadata=V1ObjectMeta(name=f"{SANDBOX_ID}-0", namespace=NAMESPACE),
        spec=V1PodSpec(
            containers=[V1Container(name="sandbox")],
            runtime_class_name=runtime_class_name,
        ),
        status=V1PodStatus(phase="Running"),
    )


def test_pause_refuses_gvisor_handler_read_from_kubernetes_client_objects() -> None:
    client = _client(
        {},
        {"sandboxed": V1RuntimeClass(handler="runsc", metadata=V1ObjectMeta(name="sandboxed"))},
        pods={f"{SANDBOX_ID}-0": _typed_pod("sandboxed")},
    )
    provider = BatchSandboxProvider(client)

    with pytest.raises(
        ValueError,
        match=r"gVisor RuntimeClass 'sandboxed' \(handler 'runsc'\)",
    ):
        provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.patch_custom_object.assert_not_called()


def test_pause_of_kubernetes_client_pod_without_runtime_class_patches_pause() -> None:
    client = _client({}, {}, pods={f"{SANDBOX_ID}-0": _typed_pod(None)})
    provider = BatchSandboxProvider(client)

    provider.pause_sandbox(SANDBOX_ID, NAMESPACE)

    client.read_runtime_class.assert_not_called()
    client.patch_custom_object.assert_called_once()
    assert client.patch_custom_object.call_args.kwargs["body"] == {"spec": {"pause": True}}


def test_service_maps_gvisor_pause_refusal_to_409(k8s_service) -> None:
    client = _client({"runtimeClassName": "sandboxed"}, {"sandboxed": {"handler": "runsc"}})
    k8s_service.workload_provider = BatchSandboxProvider(client)

    with pytest.raises(HTTPException) as exc_info:
        k8s_service.pause_sandbox(SANDBOX_ID)

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail == {
        "code": SandboxErrorCodes.INVALID_STATE,
        "message": (
            "Cannot pause: gVisor RuntimeClass 'sandboxed' (handler 'runsc') does not "
            "support the built-in rootfs snapshot committer."
        ),
    }
    client.patch_custom_object.assert_not_called()


def test_service_maps_unreadable_runtime_class_to_409(k8s_service) -> None:
    client = _client(
        {"runtimeClassName": "sandboxed"},
        {"sandboxed": ApiException(status=403, reason="Forbidden")},
    )
    k8s_service.workload_provider = BatchSandboxProvider(client)

    with pytest.raises(HTTPException) as exc_info:
        k8s_service.pause_sandbox(SANDBOX_ID)

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail == {
        "code": SandboxErrorCodes.INVALID_STATE,
        "message": (
            f"Cannot pause: cannot verify the runtime of sandbox {SANDBOX_ID}: "
            "the Kubernetes API read failed."
        ),
    }
    client.patch_custom_object.assert_not_called()
