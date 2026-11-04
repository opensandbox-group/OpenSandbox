# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

from __future__ import annotations

from typing import cast
import pytest

from opensandbox_server.api.schema import CreateSnapshotRequest
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.fast_sandbox.fastpath_client import (
    FastPathClient,
    FastPathError,
    FastPathNotFound,
)
from opensandbox_server.services.fast_sandbox.generated import fastpath_pb2 as pb2
from opensandbox_server.services.fast_sandbox.snapshot_runtime import (
    PLURAL,
    SNAPSHOT_ID_LABEL_KEY,
    SNAPSHOT_ID_METADATA_KEY,
    FastSandboxSnapshotRuntime,
    snapshot_id_from_crd,
)
from opensandbox_server.services.k8s.snapshot_runtime import build_public_snapshot_name
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_runtime import (
    SNAPSHOT_CREATE_CONFLICT_REASON,
    SnapshotRuntimePreflightError,
)
from opensandbox_server.services.snapshot_service import PersistedSnapshotService

SNAPSHOT_ID = "11111111-2222-4333-8444-555555555555"
SANDBOX_ID = "fsb-aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"


class FakeFastPathClient:
    def __init__(self) -> None:
        self.create_requests: list = []
        self.deleted: list[tuple[str, str]] = []
        self.get_by_name: dict[tuple[str, str], object] = {}
        self.get_snapshot_error: Exception | None = None
        self.create_error: Exception | None = None
        self.get_sandbox_error: Exception | None = None
        self.get_sandbox_calls: list[tuple[str, str]] = []

    def get_sandbox(self, namespace: str, sandbox_name: str, *, expected_uid: str = ""):
        if self.get_sandbox_error is not None:
            raise self.get_sandbox_error
        self.get_sandbox_calls.append((namespace, sandbox_name))

        class _Sandbox:
            ready = True

        return _Sandbox()

    def create_sandbox_snapshot(self, request):
        if self.create_error is not None:
            raise self.create_error
        self.create_requests.append(request)

        class _Response:
            snapshot = object()

        return _Response()

    def get_sandbox_snapshot(self, namespace: str, snapshot_name: str, *, expected_uid: str = ""):
        if self.get_snapshot_error is not None:
            raise self.get_snapshot_error
        info = self.get_by_name.get((namespace, snapshot_name))
        if info is None:
            raise FastPathNotFound("NOT_FOUND", "no such snapshot")
        return info

    def delete_sandbox_snapshot(self, namespace: str, snapshot_name: str, *, expected_uid: str = ""):
        self.deleted.append((namespace, snapshot_name))


class _Info:
    def __init__(
        self,
        phase,
        message: str = "",
        manifest_ref: str = "",
        template_name: str = "",
    ):
        self.phase = phase
        self.message = message
        self.manifest_ref = manifest_ref
        self.template_name = template_name


class _Response:
    def __init__(self, info: _Info):
        self.snapshot = info


class WatchRecordingK8sClient:
    def __init__(self) -> None:
        self.watch_calls: list[tuple[str, str, str, str]] = []
        self.watch_handlers: list = []
        self.stopped = 0
        self.sandbox_crs: dict[tuple[str, str], dict] = {}
        self.sandbox_cr_error: Exception | None = None
        self.custom_object_reads: list[tuple[str, str, str, str, str]] = []

    def watch_custom_objects(self, group, version, namespace, plural, event_handler):
        self.watch_calls.append((group, version, namespace, plural))
        self.watch_handlers.append(event_handler)
        return object()

    def get_custom_object(self, group, version, namespace, plural, name):
        self.custom_object_reads.append((group, version, namespace, plural, name))
        if self.sandbox_cr_error is not None:
            raise self.sandbox_cr_error
        cr = self.sandbox_crs.get((namespace, name))
        if cr is None:
            raise KeyError((namespace, name))
        return cr

    def stop_informers(self) -> None:
        self.stopped += 1


def _sandbox_cr(image: str) -> dict:
    return {"spec": {"image": image}}


def _runtime(
    fastpath: FakeFastPathClient | None = None,
    k8s: WatchRecordingK8sClient | None = None,
) -> tuple[FastSandboxSnapshotRuntime, FakeFastPathClient, WatchRecordingK8sClient]:
    fastpath = fastpath or FakeFastPathClient()
    k8s = k8s or WatchRecordingK8sClient()
    # Default source image is native; OCI-source tests override it.
    for namespace in ("default", "tenant-a"):
        k8s.sandbox_crs.setdefault((namespace, SANDBOX_ID), _sandbox_cr("alpine:3.19"))
    runtime = FastSandboxSnapshotRuntime(
        cast(FastPathClient, fastpath),
        cast(object, k8s),
        namespace="default",
    )
    return runtime, fastpath, k8s


def test_supports_create_snapshot() -> None:
    runtime, _, _ = _runtime()
    assert runtime.supports_create_snapshot() is True


def test_supports_synchronous_create() -> None:
    runtime, _, _ = _runtime()
    assert runtime.supports_synchronous_create() is True


def test_preflight_raises_when_source_sandbox_is_missing() -> None:
    fastpath = FakeFastPathClient()
    fastpath.get_sandbox_error = FastPathNotFound("NOT_FOUND", "no sandbox")
    runtime, _, _ = _runtime(fastpath=fastpath)

    with pytest.raises(SnapshotRuntimePreflightError, match="not found"):
        runtime.preflight_create_snapshot(SANDBOX_ID, namespace="tenant-a")


def test_create_snapshot_submits_intent_without_waiting() -> None:
    runtime, fastpath, _ = _runtime()

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"
    assert status.backend == "fsb"

    request = fastpath.create_requests[0]
    assert request.request_id == build_public_snapshot_name(SNAPSHOT_ID)
    assert request.template_name == build_public_snapshot_name(SNAPSHOT_ID)
    assert request.sandbox.namespaced_name.namespace == "tenant-a"
    assert request.sandbox.namespaced_name.name == SANDBOX_ID
    assert request.metadata[SNAPSHOT_ID_METADATA_KEY] == SNAPSHOT_ID


def test_create_snapshot_maps_fastpath_errors_to_failed() -> None:
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathNotFound("NOT_FOUND", "sandbox gone")
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_create_failed"


@pytest.mark.parametrize(
    "phase",
    [pb2.SNAPSHOT_PHASE_PUBLISHING, pb2.SNAPSHOT_PHASE_FAILED],
    ids=["publishing", "failed"],
)
def test_create_snapshot_reports_snapshot_persisted_before_the_error(phase: int) -> None:
    # The request is durable and idempotent by request_id: when the reply is
    # lost (e.g. the client deadline expires), the snapshot may already exist.
    # It is reported as submitted even if it already failed, so the service
    # writes its row instead of leaving it without a record.
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathError("DEADLINE_EXCEEDED", "deadline exceeded")
    fastpath.get_by_name[("tenant-a", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(phase)
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"


def test_service_records_a_recovered_snapshot_that_already_failed(tmp_path) -> None:
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathError("DEADLINE_EXCEEDED", "deadline exceeded")
    # The snapshot id is generated by the service, so answer for any name.
    fastpath.get_sandbox_snapshot = lambda namespace, snapshot_name, **_: _Response(  # type: ignore[method-assign]
        _Info(pb2.SNAPSHOT_PHASE_FAILED, message="commit job crashed")
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    class _SandboxService:
        @staticmethod
        def get_sandbox(sandbox_id: str):
            return {"id": sandbox_id, "status": {"state": "Running"}}

    service = PersistedSnapshotService(
        SQLiteSnapshotRepository(tmp_path / "snapshots.db"),
        _SandboxService(),
        snapshot_runtime=runtime,
    )
    # The create succeeds and writes the row; reading it converges the row to
    # the snapshot's real state, and the snapshot is kept for delete to clean up.
    try:
        created = service.create_snapshot(SANDBOX_ID, CreateSnapshotRequest(name="lost-reply"))
        snapshot = service.get_snapshot(created.id)
    finally:
        service.close()

    assert snapshot.status.state == SnapshotState.FAILED.value
    assert fastpath.deleted == []


def test_create_snapshot_fails_when_no_snapshot_was_persisted() -> None:
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathError("DEADLINE_EXCEEDED", "deadline exceeded")
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_create_failed"
    assert "deadline exceeded" in (status.message or "")


def test_create_snapshot_is_submitted_when_the_probe_also_fails() -> None:
    # Only NotFound proves the snapshot was never persisted. If FastPath is
    # restarting, the probe fails like the create did; report it as submitted
    # so the row is written and converges, instead of risking an orphan.
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathError("UNAVAILABLE", "connection refused")
    fastpath.get_snapshot_error = FastPathError("UNAVAILABLE", "connection refused")
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_submitted"


def test_create_snapshot_maps_failed_precondition_to_conflict() -> None:
    fastpath = FakeFastPathClient()
    fastpath.create_error = FastPathError(
        "FAILED_PRECONDITION",
        'Sandbox already has snapshot "osb-snap-other"',
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == SNAPSHOT_CREATE_CONFLICT_REASON
    assert "already has snapshot" in (status.message or "")


def test_create_snapshot_reads_source_sandbox_cr_for_encoding() -> None:
    runtime, fastpath, k8s = _runtime()

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.CREATING
    assert k8s.custom_object_reads == [
        ("sandbox.fast.io", "v1alpha2", "tenant-a", "sandboxes", SANDBOX_ID)
    ]
    # Native source image: the template-name index key is required.
    assert fastpath.create_requests[0].template_name == build_public_snapshot_name(SNAPSHOT_ID)


def test_create_snapshot_oci_source_omits_template_name() -> None:
    fastpath = FakeFastPathClient()
    k8s = WatchRecordingK8sClient()
    k8s.sandbox_crs[("tenant-a", SANDBOX_ID)] = _sandbox_cr(
        "oci://registry.example.com/sandbox/templates@sha256:abc123"
    )
    runtime, fastpath, _ = _runtime(fastpath=fastpath, k8s=k8s)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID, namespace="tenant-a")

    assert status.state == SnapshotState.CREATING
    request = fastpath.create_requests[0]
    assert request.template_name == ""


def test_create_snapshot_unreadable_sandbox_cr_fails_without_rpc() -> None:
    fastpath = FakeFastPathClient()
    k8s = WatchRecordingK8sClient()
    k8s.sandbox_cr_error = KeyError("api unavailable")
    runtime, fastpath, _ = _runtime(fastpath=fastpath, k8s=k8s)

    status = runtime.create_snapshot(SNAPSHOT_ID, SANDBOX_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_create_failed"
    assert "image could not be read" in (status.message or "")
    assert fastpath.create_requests == []


def test_inspect_maps_succeeded_phase_to_ready_with_template_name_image() -> None:
    fastpath = FakeFastPathClient()
    snapshot_name = build_public_snapshot_name(SNAPSHOT_ID)
    fastpath.get_by_name[("tenant-a", snapshot_name)] = _Response(
        _Info(
            pb2.SNAPSHOT_PHASE_SUCCEEDED,
            manifest_ref="s3://bucket/manifests/abc.json",
            template_name=snapshot_name,
        )
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID, namespace="tenant-a")

    # The restore image is the template name (the published index key), not
    # the raw manifest_ref URI.
    assert status.state == SnapshotState.READY
    assert status.image == snapshot_name
    assert status.backend == "fsb"
    assert status.reason == "snapshot_runtime_ready"


def test_inspect_fails_when_succeeded_without_template_name() -> None:
    fastpath = FakeFastPathClient()
    fastpath.get_by_name[("default", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(pb2.SNAPSHOT_PHASE_SUCCEEDED, manifest_ref="s3://bucket/manifests/abc.json")
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_missing_image"


def test_inspect_succeeded_oci_manifest_restores_from_manifest_ref() -> None:
    """OCI snapshots restore from the immutable manifest reference."""
    fastpath = FakeFastPathClient()
    manifest_ref = "oci://registry.example.com/sandbox/snapshots@sha256:def456"
    fastpath.get_by_name[("default", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(pb2.SNAPSHOT_PHASE_SUCCEEDED, manifest_ref=manifest_ref, template_name="")
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.READY
    assert status.image == manifest_ref
    assert status.backend == "fsb"
    assert status.reason == "snapshot_runtime_ready"


def test_inspect_succeeded_without_template_name_or_oci_manifest_fails() -> None:
    fastpath = FakeFastPathClient()
    fastpath.get_by_name[("default", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(pb2.SNAPSHOT_PHASE_SUCCEEDED, manifest_ref="", template_name="")
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_runtime_missing_image"


def test_inspect_maps_failed_phase_with_message() -> None:
    fastpath = FakeFastPathClient()
    fastpath.get_by_name[("default", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(pb2.SNAPSHOT_PHASE_FAILED, message="commit job crashed")
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.message == "commit job crashed"


def test_inspect_maps_nonterminal_phases_to_creating() -> None:
    fastpath = FakeFastPathClient()
    fastpath.get_by_name[("default", build_public_snapshot_name(SNAPSHOT_ID))] = _Response(
        _Info(pb2.SNAPSHOT_PHASE_PUBLISHING)
    )
    runtime, _, _ = _runtime(fastpath=fastpath)

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.CREATING
    assert status.reason == "snapshot_runtime_in_progress"


def test_inspect_maps_missing_snapshot_to_failed() -> None:
    runtime, _, _ = _runtime()

    status = runtime.inspect_snapshot(SNAPSHOT_ID)

    assert status.state == SnapshotState.FAILED
    assert status.reason == "snapshot_recovery_missing_snapshot"


def test_delete_snapshot_ignores_missing_snapshot() -> None:
    runtime, _, _ = _runtime()

    runtime.delete_snapshot(SNAPSHOT_ID, namespace="tenant-a")


def test_delete_snapshot_deletes_by_namespaced_name() -> None:
    runtime, fastpath, _ = _runtime()

    runtime.delete_snapshot(SNAPSHOT_ID, namespace="tenant-a")

    assert fastpath.deleted == [("tenant-a", build_public_snapshot_name(SNAPSHOT_ID))]


def test_start_status_watch_registers_namespaces_and_invokes_callback() -> None:
    k8s = WatchRecordingK8sClient()
    runtime, _, _ = _runtime(k8s=k8s)
    observed: list[tuple[str, str]] = []

    runtime.start_status_watch(
        lambda snapshot_id, namespace: observed.append((snapshot_id, namespace)),
        namespaces=["tenant-a"],
    )
    cr = {
        "metadata": {
            "name": build_public_snapshot_name(SNAPSHOT_ID),
            "namespace": "tenant-a",
            "labels": {SNAPSHOT_ID_LABEL_KEY: SNAPSHOT_ID},
        }
    }
    k8s.watch_handlers[0]("MODIFIED", cr)

    assert PLURAL == "sandboxsnapshots"
    assert k8s.watch_calls == [
        ("sandbox.fast.io", "v1alpha2", "default", PLURAL),
        ("sandbox.fast.io", "v1alpha2", "tenant-a", PLURAL),
    ]
    assert observed == [(SNAPSHOT_ID, "tenant-a")]


def test_watch_callback_falls_back_to_name_derived_snapshot_id() -> None:
    k8s = WatchRecordingK8sClient()
    runtime, _, _ = _runtime(k8s=k8s)
    observed: list[tuple[str, str]] = []
    runtime.start_status_watch(lambda s, ns: observed.append((s, ns)))

    cr = {
        "metadata": {
            "name": build_public_snapshot_name(SNAPSHOT_ID),
            "namespace": "default",
        }
    }
    k8s.watch_handlers[0]("SYNC", cr)

    assert observed == [(SNAPSHOT_ID, "default")]


def test_watch_ignores_unrecognized_objects() -> None:
    k8s = WatchRecordingK8sClient()
    runtime, _, _ = _runtime(k8s=k8s)
    observed: list[tuple[str, str]] = []
    runtime.start_status_watch(lambda s, ns: observed.append((s, ns)))

    k8s.watch_handlers[0]("MODIFIED", {"metadata": {"name": "unrelated"}})
    k8s.watch_handlers[0]("MODIFIED", "not-a-dict")

    assert observed == []


def test_snapshot_id_from_crd_prefers_label_over_name() -> None:
    cr = {
        "metadata": {
            "name": build_public_snapshot_name(SNAPSHOT_ID),
            "labels": {SNAPSHOT_ID_LABEL_KEY: "explicit-id"},
        }
    }

    assert snapshot_id_from_crd(cr) == "explicit-id"


def test_close_stops_informers_when_client_supports_it() -> None:
    k8s = WatchRecordingK8sClient()
    runtime, _, _ = _runtime(k8s=k8s)

    runtime.close()

    assert k8s.stopped == 1
