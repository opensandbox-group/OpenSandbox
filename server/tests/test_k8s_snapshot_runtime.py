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


class WatchRecordingK8sClient(FakeK8sClient):
    """FakeK8sClient that records watch handlers for reactor tests."""

    def __init__(self) -> None:
        super().__init__()
        self.watch_calls: list[tuple[str, str, str, str]] = []
        self.watch_handlers: list = []
        self.stopped = 0

    def watch_custom_objects(self, group, version, namespace, plural, event_handler):
        self.watch_calls.append((group, version, namespace, plural))
        self.watch_handlers.append(event_handler)
        return object()

    def stop_informers(self) -> None:
        self.stopped += 1


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


def test_create_snapshot_converges_once_runtime_reads_stop_failing() -> None:
    k8s_client = TransientThenReadyK8sClient(failures=1)
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    submitted = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)
    first_read = runtime.inspect_snapshot(SNAPSHOT_ID)
    converged = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert submitted.state == SnapshotState.CREATING
    assert first_read.state == SnapshotState.CREATING
    assert converged.state == SnapshotState.READY
    assert converged.image == "registry/sandbox:snap"


def test_postgresql_ha_observation_error_keeps_snapshot_creating() -> None:
    k8s_client = TransientGetK8sClient(failures=1)
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        postgresql_ha_enabled=True,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_inspect_failed"
    assert k8s_client.created == []


def test_postgresql_ha_observes_existing_cr_before_create() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = _snapshot_cr(
        phase="Succeed",
        containers=[
            {"containerName": "sandbox", "imageUri": "registry/sandbox:snap"},
        ],
    )
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        postgresql_ha_enabled=True,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.READY
    assert k8s_client.created == []


def test_postgresql_ha_can_create_cr_missing_after_creator_crash() -> None:
    k8s_client = TransientThenReadyK8sClient(failures=0)
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        postgresql_ha_enabled=True,
    )

    recovered = runtime.inspect_snapshot(SNAPSHOT_ID)
    submitted = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)
    converged = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert recovered.state == SnapshotState.CREATING
    assert recovered.reason == "snapshot_recovery_missing_snapshot"
    assert submitted.state == SnapshotState.CREATING
    assert converged.state == SnapshotState.READY
    assert len(k8s_client.created) == 1


def test_postgresql_ha_submitted_create_stays_creating_without_terminal_cr() -> None:
    k8s_client = FakeK8sClient()
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        postgresql_ha_enabled=True,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"


def test_submitted_create_stays_creating_without_terminal_cr() -> None:
    k8s_client = FakeK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"


def test_start_status_watch_registers_namespaces_and_invokes_callback() -> None:
    k8s_client = WatchRecordingK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")
    observed: list[tuple[str, str]] = []

    runtime.start_status_watch(
        lambda snapshot_id, namespace: observed.append((snapshot_id, namespace)),
        namespaces=["tenant-a", None],
    )
    k8s_client.watch_handlers[0]("MODIFIED", _snapshot_cr(phase="Succeed"))
    k8s_client.watch_handlers[0]("DELETED", _snapshot_cr(phase="Succeed"))

    assert k8s_client.watch_calls == [
        ("sandbox.opensandbox.io", "v1alpha1", "default", "sandboxsnapshots"),
        ("sandbox.opensandbox.io", "v1alpha1", "tenant-a", "sandboxsnapshots"),
    ]
    assert observed == [(SNAPSHOT_ID, "default"), (SNAPSHOT_ID, "default")]


def test_status_watch_ignores_objects_without_snapshot_label() -> None:
    k8s_client = WatchRecordingK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")
    observed: list[tuple[str, str]] = []

    runtime.start_status_watch(lambda snapshot_id, ns: observed.append((snapshot_id, ns)))
    k8s_client.watch_handlers[0]("MODIFIED", {"metadata": {"name": "other", "namespace": "default"}})
    k8s_client.watch_handlers[0]("SYNC", "not-a-dict")

    assert observed == []


def test_create_snapshot_registers_namespace_watch_when_sync_started() -> None:
    k8s_client = WatchRecordingK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")
    runtime.start_status_watch(lambda snapshot_id, namespace: None)

    runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-b")

    watched = [call[2] for call in k8s_client.watch_calls]
    assert watched == ["default", "tenant-b"]


def test_close_stops_informers_when_client_supports_it() -> None:
    k8s_client = WatchRecordingK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    runtime.close()

    assert k8s_client.stopped == 1


def test_delete_snapshot_deletes_cr_and_ignores_missing_cr() -> None:
    k8s_client = FakeK8sClient()
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    runtime.delete_snapshot(SNAPSHOT_ID)

    assert k8s_client.deleted == [build_public_snapshot_name(SNAPSHOT_ID)]


def test_create_snapshot_submits_without_waiting_for_terminal_status() -> None:
    k8s_client = FakeK8sClient()
    snapshot_name = build_public_snapshot_name(SNAPSHOT_ID)
    runtime = KubernetesSnapshotRuntime(k8s_client, namespace="default")

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert k8s_client.created, "the SandboxSnapshot CR must be persisted"
    assert k8s_client.objects[snapshot_name]["spec"]["sandboxName"] == SANDBOX_ID
    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"


def test_create_snapshot_fails_when_existing_cr_points_to_different_sandbox() -> None:
    k8s_client = FakeK8sClient()
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = _snapshot_cr(
        phase="Pending",
        sandbox_id="different-sandbox",
    )
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_conflict"
    assert "different source sandbox" in (status.message or "")


def test_postgresql_ha_rejects_existing_cr_without_source_sandbox() -> None:
    k8s_client = FakeK8sClient()
    snapshot = _snapshot_cr(phase="Pending")
    snapshot["spec"] = {}
    k8s_client.objects[build_public_snapshot_name(SNAPSHOT_ID)] = snapshot
    runtime = KubernetesSnapshotRuntime(
        k8s_client,
        namespace="default",
        postgresql_ha_enabled=True,
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_conflict"


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
    )

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_restore_image_ambiguous"
