# Copyright 2025 Alibaba Group Holding Ltd.
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

from concurrent.futures import Future
from types import SimpleNamespace

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import CreateSnapshotRequest, ListSnapshotsRequest, SnapshotFilter
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_runtime import NoopSnapshotRuntime, SnapshotRuntimeStatus
from opensandbox_server.services.snapshot_service import PersistedSnapshotService


class StubSandboxService:
    @staticmethod
    def get_sandbox(sandbox_id: str):
        if sandbox_id == "missing":
            raise HTTPException(
                status_code=404,
                detail={"code": "SANDBOX::NOT_FOUND", "message": f"Sandbox {sandbox_id} not found"},
        )
        return {
            "id": sandbox_id,
            "status": {
                "state": "Running",
            },
        }


class ImmediateExecutor:
    def __init__(self) -> None:
        self.shutdown_called = False

    def submit(self, fn, *args, **kwargs) -> Future:
        future = Future()
        try:
            future.set_result(fn(*args, **kwargs))
        except Exception as exc:  # noqa: BLE001
            future.set_exception(exc)
        return future

    def shutdown(self, wait: bool = True) -> None:
        self.shutdown_called = True


class CapturingExecutor:
    def __init__(self) -> None:
        self.submitted: list[tuple[object, tuple, dict]] = []
        self.shutdown_called = False
        self.shutdown_wait = None

    def submit(self, fn, *args, **kwargs) -> Future:
        self.submitted.append((fn, args, kwargs))
        return Future()

    def shutdown(self, wait: bool = True) -> None:
        self.shutdown_called = True
        self.shutdown_wait = wait


class StubSnapshotRuntime:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str]] = []
        self.delete_calls: list[tuple[str, str | None]] = []
        self.inspect_status_by_snapshot_id: dict[str, SnapshotRuntimeStatus] = {}

    def supports_create_snapshot(self) -> bool:
        return True

    def create_snapshot_unsupported_message(self) -> str:
        return ""

    def create_snapshot(self, snapshot_id: str, sandbox_id: str):
        self.calls.append((snapshot_id, sandbox_id))
        return None

    def get_snapshot_status(self, snapshot_id: str):
        return None

    def delete_snapshot(self, snapshot_id: str, image: str | None = None) -> None:
        self.delete_calls.append((snapshot_id, image))

    def inspect_snapshot(self, snapshot_id: str, image: str | None = None) -> SnapshotRuntimeStatus:
        return self.inspect_status_by_snapshot_id.get(
            snapshot_id,
            SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_recovery_missing_image",
                message="Snapshot creation was interrupted and no snapshot image was found.",
            ),
        )


def _snapshot_record(
    snapshot_id: str,
    state: SnapshotState,
    *,
    image: str | None = None,
) -> SnapshotRecord:
    return SnapshotRecord(
        id=snapshot_id,
        source_sandbox_id="sbx-001",
        restore_config=SnapshotRestoreConfig(image=image),
        status=SnapshotStatusRecord(state=state),
    )


def test_snapshot_service_persists_create_and_get(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint-before-import"))
    fetched = service.get_snapshot(created.id)

    assert created.status.state == "Creating"
    assert created.status.reason == "snapshot_accepted"
    assert fetched.id == created.id
    assert fetched.sandbox_id == "sbx-001"
    assert runtime.calls == [(created.id, "sbx-001")]


def test_snapshot_service_rejects_create_when_source_sandbox_not_running(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    sandbox_service = SimpleNamespace(
        get_sandbox=lambda sandbox_id: SimpleNamespace(
            id=sandbox_id,
            status=SimpleNamespace(state="Paused"),
        )
    )
    service = PersistedSnapshotService(
        repo,
        sandbox_service,
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    with pytest.raises(HTTPException) as exc_info:
        service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail["code"] == "SNAPSHOT::INVALID_SOURCE_STATE"
    assert runtime.calls == []


def test_snapshot_service_rejects_create_when_source_sandbox_state_missing(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    sandbox_service = SimpleNamespace(
        get_sandbox=lambda sandbox_id: SimpleNamespace(id=sandbox_id)
    )
    service = PersistedSnapshotService(
        repo,
        sandbox_service,
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    with pytest.raises(HTTPException) as exc_info:
        service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail["code"] == "SNAPSHOT::INVALID_SOURCE_STATE"
    assert runtime.calls == []


def test_snapshot_service_marks_snapshot_ready_from_worker(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    ready_status = SnapshotRuntimeStatus(
        state=SnapshotState.READY,
        image="opensandbox-snapshots:snap-ready",
        reason="snapshot_runtime_ready",
        message="Docker snapshot image created successfully.",
    )

    def create_snapshot(snapshot_id: str, sandbox_id: str):
        runtime.calls.append((snapshot_id, sandbox_id))
        return ready_status

    runtime.create_snapshot = create_snapshot

    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint-before-import"))
    stored = repo.get(created.id)

    assert created.status.state == "Creating"
    assert created.status.reason == "snapshot_accepted"
    assert stored is not None
    assert stored.status.state == SnapshotState.READY
    assert stored.restore_config.image == "opensandbox-snapshots:snap-ready"


def test_snapshot_service_marks_snapshot_failed_from_worker(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    failed_status = SnapshotRuntimeStatus(
        state=SnapshotState.FAILED,
        reason="snapshot_runtime_timeout",
        message="Docker snapshot creation timed out after 45 seconds.",
    )

    def create_snapshot(snapshot_id: str, sandbox_id: str):
        runtime.calls.append((snapshot_id, sandbox_id))
        return failed_status

    runtime.create_snapshot = create_snapshot

    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint-before-import"))
    stored = repo.get(created.id)

    assert created.status.state == "Creating"
    assert created.status.reason == "snapshot_accepted"
    assert stored is not None
    assert stored.status.state == SnapshotState.FAILED
    assert stored.status.reason == "snapshot_runtime_timeout"


def test_snapshot_service_marks_snapshot_failed_when_worker_returns_none(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint-before-import"))
    stored = repo.get(created.id)

    assert created.status.state == "Creating"
    assert stored is not None
    assert stored.status.state == SnapshotState.FAILED
    assert stored.status.reason == "snapshot_runtime_missing_result"


def test_recover_unfinished_snapshot_reschedules_creating_runtime_status_without_progress(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    record = _snapshot_record("snap-in-progress", SnapshotState.CREATING)
    repo.create(record)
    runtime = StubSnapshotRuntime()
    runtime.inspect_status_by_snapshot_id[record.id] = SnapshotRuntimeStatus(
        state=SnapshotState.CREATING,
        reason="snapshot_runtime_in_progress",
        message="Snapshot is still being committed.",
    )
    executor = CapturingExecutor()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=executor,
        recover_unfinished_snapshots=False,
    )

    progressed = service._recover_unfinished_snapshot(record)

    stored = repo.get(record.id)
    assert progressed is False
    assert stored is not None
    assert stored.status.state == SnapshotState.CREATING
    assert len(executor.submitted) == 1


def test_snapshot_service_lists_and_deletes_records(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    first = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="first"))
    second = service.create_snapshot("sbx-002", CreateSnapshotRequest(name="second"))

    page = service.list_snapshots(
        ListSnapshotsRequest(
            filter=SnapshotFilter(sandboxId="sbx-001"),
        )
    )

    assert page.pagination.total_items == 1
    assert [item.id for item in page.items] == [first.id]

    second_record = repo.get(second.id)
    assert second_record is not None
    second_record.status = SnapshotStatusRecord(state=SnapshotState.FAILED)
    repo.update(second_record)

    service.delete_snapshot(second.id)
    assert runtime.delete_calls == [(second.id, None)]
    with pytest.raises(HTTPException) as exc_info:
        service.get_snapshot(second.id)
    assert exc_info.value.status_code == 404


def test_snapshot_service_rejects_delete_while_creating(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    executor = CapturingExecutor()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=executor,
    )

    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))
    with pytest.raises(HTTPException) as exc_info:
        service.delete_snapshot(created.id)

    stored = repo.get(created.id)

    assert exc_info.value.status_code == 409
    assert exc_info.value.detail["code"] == "SNAPSHOT::INVALID_STATE"
    assert stored is not None
    assert stored.status.state == SnapshotState.CREATING
    assert runtime.delete_calls == []
    assert len(executor.submitted) == 1


def test_snapshot_service_deletes_runtime_artifact_before_metadata(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    ready_status = SnapshotRuntimeStatus(
        state=SnapshotState.READY,
        image="opensandbox-snapshots:snap-ready",
        reason="snapshot_runtime_ready",
        message="Docker snapshot image created successfully.",
    )

    def create_snapshot(snapshot_id: str, sandbox_id: str):
        runtime.calls.append((snapshot_id, sandbox_id))
        return ready_status

    def delete_snapshot(snapshot_id: str, image: str | None = None) -> None:
        stored = repo.get(snapshot_id)
        assert stored is not None
        assert stored.status.state == SnapshotState.DELETING
        runtime.delete_calls.append((snapshot_id, image))

    runtime.create_snapshot = create_snapshot
    runtime.delete_snapshot = delete_snapshot
    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))

    service.delete_snapshot(created.id)

    assert runtime.delete_calls == [(created.id, "opensandbox-snapshots:snap-ready")]
    assert repo.get(created.id) is None


def test_snapshot_service_propagates_snapshot_delete_conflict(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
    )

    record = _snapshot_record(
        "snap-in-use",
        SnapshotState.READY,
        image="opensandbox-snapshots:snap-in-use",
    )
    repo.create(record)

    def delete_snapshot(snapshot_id: str, image: str | None = None) -> None:
        raise HTTPException(
            status_code=409,
            detail={
                "code": "SNAPSHOT::DELETE_CONFLICT",
                "message": "snapshot image cannot be deleted due to a conflict",
            },
        )

    runtime.delete_snapshot = delete_snapshot

    with pytest.raises(HTTPException) as exc_info:
        service.delete_snapshot("snap-in-use")

    stored = repo.get("snap-in-use")
    assert exc_info.value.status_code == 409
    assert stored is not None
    assert stored.status.state == SnapshotState.DELETING


def test_snapshot_service_recovers_delete_after_runtime_cleanup_succeeds(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=ImmediateExecutor(),
    )

    record = _snapshot_record(
        "snap-delete-crash",
        SnapshotState.READY,
        image="opensandbox-snapshots:snap-delete-crash",
    )
    repo.create(record)

    original_delete = repo.delete

    def crash_delete(snapshot_id: str) -> None:
        raise RuntimeError("simulated metadata delete crash")

    repo.delete = crash_delete
    with pytest.raises(RuntimeError, match="simulated metadata delete crash"):
        service.delete_snapshot("snap-delete-crash")

    stored = repo.get("snap-delete-crash")
    assert stored is not None
    assert stored.status.state == SnapshotState.DELETING
    assert runtime.delete_calls == [("snap-delete-crash", "opensandbox-snapshots:snap-delete-crash")]

    repo.delete = original_delete
    recovery_runtime = StubSnapshotRuntime()
    PersistedSnapshotService(repo, StubSandboxService(), snapshot_runtime=recovery_runtime)

    assert recovery_runtime.delete_calls == [("snap-delete-crash", "opensandbox-snapshots:snap-delete-crash")]
    assert repo.get("snap-delete-crash") is None


def test_snapshot_service_worker_cleans_up_snapshot_deleted_during_creation(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=CapturingExecutor(),
    )

    ready_status = SnapshotRuntimeStatus(
        state=SnapshotState.READY,
        image="opensandbox-snapshots:snap-ready",
        reason="snapshot_runtime_ready",
        message="Docker snapshot image created successfully.",
    )

    def create_snapshot(snapshot_id: str, sandbox_id: str):
        runtime.calls.append((snapshot_id, sandbox_id))
        return ready_status

    runtime.create_snapshot = create_snapshot
    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))
    repo.delete(created.id)

    service._create_snapshot_worker(_snapshot_record(created.id, SnapshotState.CREATING))

    assert runtime.delete_calls == [(created.id, "opensandbox-snapshots:snap-ready")]
    assert repo.get(created.id) is None


def test_snapshot_service_worker_does_not_overwrite_transitioned_snapshot(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    runtime = StubSnapshotRuntime()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=runtime,
        snapshot_executor=CapturingExecutor(),
    )

    ready_status = SnapshotRuntimeStatus(
        state=SnapshotState.READY,
        image="opensandbox-snapshots:snap-ready",
        reason="snapshot_runtime_ready",
        message="Docker snapshot image created successfully.",
    )

    def create_snapshot(snapshot_id: str, sandbox_id: str):
        runtime.calls.append((snapshot_id, sandbox_id))
        return ready_status

    runtime.create_snapshot = create_snapshot
    created = service.create_snapshot("sbx-001", CreateSnapshotRequest(name="checkpoint"))

    failed_record = repo.get(created.id)
    assert failed_record is not None
    failed_record.status = SnapshotStatusRecord(
        state=SnapshotState.FAILED,
        reason="external_transition",
        message="Snapshot was transitioned by another worker.",
    )
    repo.update(failed_record)

    service._create_snapshot_worker(_snapshot_record(created.id, SnapshotState.CREATING))

    stored = repo.get(created.id)
    assert stored is not None
    assert stored.status.state == SnapshotState.FAILED
    assert stored.status.reason == "external_transition"
    assert stored.restore_config.image is None


def test_snapshot_service_close_shuts_down_executor(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    executor = CapturingExecutor()
    service = PersistedSnapshotService(
        repo,
        StubSandboxService(),
        snapshot_runtime=StubSnapshotRuntime(),
        snapshot_executor=executor,
    )

    service.close()

    assert executor.shutdown_called is True
    assert executor.shutdown_wait is True


def test_snapshot_service_propagates_missing_sandbox(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    service = PersistedSnapshotService(repo, StubSandboxService())

    with pytest.raises(HTTPException) as exc_info:
        service.create_snapshot("missing", CreateSnapshotRequest())

    assert exc_info.value.status_code == 404


def test_snapshot_service_returns_501_when_runtime_is_not_supported(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    service = PersistedSnapshotService(repo, StubSandboxService(), snapshot_runtime=NoopSnapshotRuntime())

    with pytest.raises(HTTPException) as exc_info:
        service.create_snapshot("sbx-001", CreateSnapshotRequest())

    assert exc_info.value.status_code == 501
    assert exc_info.value.detail["code"] == "SNAPSHOT::NOT_IMPLEMENTED"


def test_snapshot_service_recovers_creating_snapshot_with_existing_artifact(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(_snapshot_record("snap-ready", SnapshotState.CREATING))
    runtime = StubSnapshotRuntime()
    runtime.inspect_status_by_snapshot_id["snap-ready"] = SnapshotRuntimeStatus(
        state=SnapshotState.READY,
        image="opensandbox-snapshots:snap-ready",
        reason="snapshot_recovery_ready",
        message="Recovered snapshot image after server restart.",
    )

    PersistedSnapshotService(repo, StubSandboxService(), snapshot_runtime=runtime)

    recovered = repo.get("snap-ready")
    assert recovered is not None
    assert recovered.status.state == SnapshotState.READY
    assert recovered.restore_config.image == "opensandbox-snapshots:snap-ready"


def test_snapshot_service_recovers_creating_snapshot_without_artifact(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(_snapshot_record("snap-missing", SnapshotState.CREATING))
    runtime = StubSnapshotRuntime()

    PersistedSnapshotService(repo, StubSandboxService(), snapshot_runtime=runtime)

    recovered = repo.get("snap-missing")
    assert recovered is not None
    assert recovered.status.state == SnapshotState.FAILED
    assert recovered.status.reason == "snapshot_recovery_missing_image"


def test_snapshot_service_recovers_deleting_snapshot(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    repo.create(
        _snapshot_record(
            "snap-delete",
            SnapshotState.DELETING,
            image="opensandbox-snapshots:snap-delete",
        )
    )
    runtime = StubSnapshotRuntime()

    PersistedSnapshotService(repo, StubSandboxService(), snapshot_runtime=runtime)

    assert runtime.delete_calls == [("snap-delete", "opensandbox-snapshots:snap-delete")]
    assert repo.get("snap-delete") is None
