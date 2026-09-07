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

from __future__ import annotations

from copy import deepcopy

import pytest
from kubernetes.client import ApiException

from opensandbox_server.services.k8s.snapshot_runtime import (
    KubernetesSnapshotRuntime,
    build_public_snapshot_name,
    build_public_snapshot_tag,
)
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import SnapshotRuntimeUnsupportedError


SNAPSHOT_ID = "11111111-2222-4333-8444-555555555555"
SNAPSHOT_HEX = "11111111222243338444555555555555"
SANDBOX_ID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"


class FakeK8sClient:
    def __init__(self) -> None:
        self.objects: dict[str, dict] = {}
        self.workloads: dict[str, dict] = {}
        self.pods: dict[str, dict] = {}
        self.runtime_classes: dict[str, dict] = {}
        self.created: list[dict] = []
        self.deleted: list[str] = []

    def create_custom_object(self, *, group: str, version: str, namespace: str, plural: str, body: dict):
        self.created.append(deepcopy(body))
        name = body["metadata"]["name"]
        if name in self.objects:
            raise ApiException(status=409, reason="Already Exists")
        stored = deepcopy(body)
        self.objects[name] = stored
        return stored

    def get_custom_object(self, *, group: str, version: str, namespace: str, plural: str, name: str):
        objects = {
            "batchsandboxes": self.workloads,
            "sandboxsnapshots": self.objects,
        }[plural]
        obj = objects.get(name)
        return deepcopy(obj) if obj is not None else None

    def read_pod(self, namespace: str, name: str):
        pod = self.pods.get(name)
        return deepcopy(pod) if pod is not None else None

    def list_pods(self, namespace: str, label_selector: str = ""):
        return [deepcopy(pod) for pod in self.pods.values()]

    def read_runtime_class(self, name: str):
        runtime_class = self.runtime_classes.get(name)
        if runtime_class is None:
            raise ApiException(status=404, reason="Not Found")
        return deepcopy(runtime_class)

    def delete_custom_object(self, *, group: str, version: str, namespace: str, plural: str, name: str, **kwargs):
        self.deleted.append(name)
        if name not in self.objects:
            raise ApiException(status=404, reason="Not Found")
        del self.objects[name]


class TransientGetK8sClient(FakeK8sClient):
    def __init__(self, *, failures: int) -> None:
        super().__init__()
        self.failures = failures

    def get_custom_object(self, *, group: str, version: str, namespace: str, plural: str, name: str):
        if self.failures > 0:
            self.failures -= 1
            raise ApiException(status=500, reason="temporary apiserver error")
        return super().get_custom_object(
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )


class TransientThenReadyK8sClient(TransientGetK8sClient):
    def get_custom_object(self, *, group: str, version: str, namespace: str, plural: str, name: str):
        obj = super().get_custom_object(
            group=group,
            version=version,
            namespace=namespace,
            plural=plural,
            name=name,
        )
        if obj is not None:
            obj["status"] = {
                "phase": "Succeed",
                "containers": [
                    {"containerName": "sandbox", "imageUri": "registry/sandbox:snap"},
                ],
            }
            self.objects[name] = deepcopy(obj)
        return obj


def _snapshot_cr(*, phase: str, containers: list[dict] | None = None, sandbox_id: str = SANDBOX_ID) -> dict:
    name = build_public_snapshot_name(SNAPSHOT_ID)
    return {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "SandboxSnapshot",
        "metadata": {
            "name": name,
            "namespace": "default",
            "labels": {
                "opensandbox.io/snapshot-id": SNAPSHOT_ID,
                "opensandbox.io/source-sandbox-id": sandbox_id,
                "opensandbox.io/snapshot-scope": "public",
            },
        },
        "spec": {
            "sandboxName": sandbox_id,
        },
        "status": {
            "phase": phase,
            "containers": containers or [],
        },
    }


def test_public_snapshot_name_and_tag_are_derived_from_snapshot_id() -> None:
    assert build_public_snapshot_name(SNAPSHOT_ID) == f"osb-snap-{SNAPSHOT_HEX}"
    assert build_public_snapshot_tag(SNAPSHOT_ID) == f"snap-{SNAPSHOT_HEX}"


def test_preflight_rejects_gvisor_runtimeclass_before_snapshot_cr_creation() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.workloads[SANDBOX_ID] = {
        "spec": {"template": {"spec": {"runtimeClassName": "sandboxed"}}},
    }
    k8s_client.pods[f"{SANDBOX_ID}-0"] = {
        "spec": {"runtimeClassName": "sandboxed"},
        "status": {"phase": "Running"},
    }
    k8s_client.runtime_classes["sandboxed"] = {"handler": "runsc"}
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    with pytest.raises(SnapshotRuntimeUnsupportedError, match="gVisor"):
        runtime.preflight_create_snapshot(SANDBOX_ID)

    assert k8s_client.created == []


def test_preflight_allows_non_gvisor_runtimeclass() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.workloads[SANDBOX_ID] = {
        "spec": {"template": {"spec": {"runtimeClassName": "native"}}},
    }
    k8s_client.pods[f"{SANDBOX_ID}-0"] = {
        "spec": {"runtimeClassName": "native"},
        "status": {"phase": "Running"},
    }
    k8s_client.runtime_classes["native"] = {"handler": "runc"}
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    runtime.preflight_create_snapshot(SANDBOX_ID)

    assert k8s_client.created == []


def test_preflight_uses_allocated_pool_pod_runtimeclass() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.workloads[SANDBOX_ID] = {
        "metadata": {
            "annotations": {
                "sandbox.opensandbox.io/alloc-status": (
                    '{"pods":["pool-pod-1"],"poolRef":"gvisor-pool"}'
                ),
            },
        },
        "spec": {"poolRef": "gvisor-pool"},
    }
    k8s_client.pods["pool-pod-1"] = {
        "spec": {"runtimeClassName": "sandboxed"},
        "status": {"phase": "Running"},
    }
    k8s_client.runtime_classes["sandboxed"] = {"handler": "runsc"}
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    with pytest.raises(SnapshotRuntimeUnsupportedError, match="gVisor"):
        runtime.preflight_create_snapshot(SANDBOX_ID)

    assert k8s_client.created == []


def test_create_snapshot_creates_cr_and_maps_succeed_to_ready() -> None:
    k8s_client = FakeK8sClient()
    snapshot_name = build_public_snapshot_name(SNAPSHOT_ID)
    k8s_client.objects[snapshot_name] = _snapshot_cr(
        phase="Succeed",
        containers=[
            {"containerName": "egress", "imageUri": "registry/egress:snap"},
            {"containerName": "sandbox", "imageUri": "registry/sandbox:snap"},
        ],
    )
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        wait_timeout_seconds=0,
        poll_interval_seconds=0,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert k8s_client.created == [
        {
            "apiVersion": "sandbox.opensandbox.io/v1alpha1",
            "kind": "SandboxSnapshot",
            "metadata": {
                "name": snapshot_name,
                "namespace": "default",
                "labels": {
                    "opensandbox.io/snapshot-id": SNAPSHOT_ID,
                    "opensandbox.io/source-sandbox-id": SANDBOX_ID,
                    "opensandbox.io/snapshot-scope": "public",
                },
            },
            "spec": {
                "sandboxName": SANDBOX_ID,
            },
        }
    ]
    assert status.state == SnapshotState.READY
    assert status.image == "registry/sandbox:snap"
    assert status.reason == "snapshot_runtime_ready"


def test_inspect_snapshot_keeps_pending_snapshot_creating() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = _snapshot_cr(phase="Committing")
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_in_progress"


def test_inspect_snapshot_rejects_qemu_snapshot_without_public_restore_plan() -> None:
    k8s_client = FakeK8sClient()
    snapshot = _snapshot_cr(
        phase="Succeed",
        containers=[{"containerName": "sandbox", "imageUri": "registry/sandbox:snap"}],
    )
    snapshot["status"]["format"] = "qemu-v1"
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = snapshot
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_restore_qemu_not_supported"
    assert "BatchSandbox pause/resume" in (status.message or "")


def test_inspect_snapshot_maps_failed_condition() -> None:
    k8s_client = FakeK8sClient()
    failed = _snapshot_cr(phase="Failed")
    failed["status"]["conditions"] = [
        {
            "type": "Failed",
            "status": "True",
            "reason": "CommitJobFailed",
            "message": "commit job failed",
        }
    ]
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = failed
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "CommitJobFailed"
    assert status.message == "commit job failed"


def test_inspect_snapshot_keeps_transient_read_error_creating() -> None:
    k8s_client = TransientGetK8sClient(failures=1)
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_inspect_failed"
    assert "temporary apiserver error" in (status.message or "")


def test_create_snapshot_retries_transient_inspect_error_until_controller_ready() -> None:
    k8s_client = TransientThenReadyK8sClient(failures=1)
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        wait_timeout_seconds=1,
        poll_interval_seconds=0,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.READY
    assert status.image == "registry/sandbox:snap"


def test_delete_snapshot_deletes_cr_and_ignores_missing_cr() -> None:
    k8s_client = FakeK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    runtime.delete_snapshot(SNAPSHOT_ID)

    assert k8s_client.deleted == [build_public_snapshot_name(SNAPSHOT_ID)]


def test_create_snapshot_fails_when_existing_cr_points_to_different_sandbox() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = _snapshot_cr(
        phase="Pending",
        sandbox_id="different-sandbox",
    )
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        wait_timeout_seconds=0,
        poll_interval_seconds=0,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_conflict"
    assert "different source sandbox" in (status.message or "")


def test_create_snapshot_marks_ambiguous_multi_container_restore_failed() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = _snapshot_cr(
        phase="Succeed",
        containers=[
            {"containerName": "worker", "imageUri": "registry/worker:snap"},
            {"containerName": "sidecar", "imageUri": "registry/sidecar:snap"},
        ],
    )
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        wait_timeout_seconds=0,
        poll_interval_seconds=0,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_restore_image_ambiguous"
