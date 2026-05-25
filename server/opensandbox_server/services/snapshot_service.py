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

"""
Snapshot service orchestration for server-managed snapshot resources.

The preferred path is to persist the snapshot record and, when supported by the
runtime, complete snapshot creation inline so the repository reaches a terminal
state within the request lifecycle.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from concurrent.futures import Future, ThreadPoolExecutor
from datetime import datetime, timezone
import logging
from math import ceil
from uuid import uuid4

from fastapi import HTTPException, status

from opensandbox_server.api.schema import (
    CreateSnapshotRequest,
    ListSnapshotsRequest,
    ListSnapshotsResponse,
    PaginationInfo,
    Snapshot,
    SnapshotStatus,
)
from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
from opensandbox_server.services.constants import SnapshotErrorCodes
from opensandbox_server.services.snapshot_runtime import (
    NoopSnapshotRuntime,
    SnapshotRuntime,
    SnapshotRuntimeStatus,
)
from opensandbox_server.services.snapshot_runtime_factory import create_snapshot_runtime
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_repository import (
    SnapshotListQuery,
    SnapshotRepository,
)

logger = logging.getLogger(__name__)
SNAPSHOT_RECOVERY_PAGE_SIZE = 200
SNAPSHOT_WORKER_MAX_WORKERS = 2


class SnapshotService(ABC):
    """
    Abstract service interface for snapshot lifecycle operations.
    """

    @abstractmethod
    def create_snapshot(self, sandbox_id: str, request: CreateSnapshotRequest) -> Snapshot:
        pass

    @abstractmethod
    def list_snapshots(self, request: ListSnapshotsRequest) -> ListSnapshotsResponse:
        pass

    @abstractmethod
    def get_snapshot(self, snapshot_id: str) -> Snapshot:
        pass

    @abstractmethod
    def delete_snapshot(self, snapshot_id: str) -> None:
        pass

    def close(self) -> None:
        """
        Release resources owned by the snapshot service.
        """


class PersistedSnapshotService(SnapshotService):
    """
    Snapshot service backed by the configured repository.
    """

    def __init__(
        self,
        snapshot_repository: SnapshotRepository,
        sandbox_service,
        snapshot_runtime: SnapshotRuntime | None = None,
        snapshot_executor=None,
        *,
        recover_unfinished_snapshots: bool = True,
    ) -> None:
        self._snapshot_repository = snapshot_repository
        self._sandbox_service = sandbox_service
        self._snapshot_runtime = snapshot_runtime or NoopSnapshotRuntime()
        self._snapshot_executor = snapshot_executor or ThreadPoolExecutor(
            max_workers=SNAPSHOT_WORKER_MAX_WORKERS,
            thread_name_prefix="snapshot-create",
        )
        if recover_unfinished_snapshots:
            self.recover_unfinished_snapshots()

    def create_snapshot(self, sandbox_id: str, request: CreateSnapshotRequest) -> Snapshot:
        sandbox = self._sandbox_service.get_sandbox(sandbox_id)
        self._ensure_source_sandbox_running(sandbox)

        if not self._snapshot_runtime.supports_create_snapshot():
            raise HTTPException(
                status_code=status.HTTP_501_NOT_IMPLEMENTED,
                detail={
                    "code": "SNAPSHOT::NOT_IMPLEMENTED",
                    "message": self._snapshot_runtime.create_snapshot_unsupported_message(),
                },
            )

        now = datetime.now(timezone.utc)
        record = SnapshotRecord(
            id=str(uuid4()),
            source_sandbox_id=sandbox_id,
            name=request.name,
            restore_config=self._default_restore_config(),
            status=SnapshotStatusRecord(
                state=SnapshotState.CREATING,
                reason="snapshot_accepted",
                message="Snapshot creation accepted.",
                last_transition_at=now,
            ),
            created_at=now,
            updated_at=now,
        )
        self._snapshot_repository.create(record)
        future = self._snapshot_executor.submit(
            self._create_snapshot_worker,
            record,
        )
        future.add_done_callback(self._log_worker_failure)
        return self._to_snapshot_response(record)

    def list_snapshots(self, request: ListSnapshotsRequest) -> ListSnapshotsResponse:
        pagination = request.pagination or self._default_pagination()
        result = self._snapshot_repository.list(
            SnapshotListQuery(
                page=pagination.page,
                page_size=pagination.page_size,
                source_sandbox_id=request.filter.sandbox_id,
                states=request.filter.state or [],
            )
        )

        total_pages = ceil(result.total_items / pagination.page_size) if result.total_items > 0 else 0
        return ListSnapshotsResponse(
            items=[self._to_snapshot_response(item) for item in result.items],
            pagination=PaginationInfo(
                page=pagination.page,
                pageSize=pagination.page_size,
                totalItems=result.total_items,
                totalPages=total_pages,
                hasNextPage=pagination.page < total_pages,
            ),
        )

    def get_snapshot(self, snapshot_id: str) -> Snapshot:
        record = self._snapshot_repository.get(snapshot_id)
        if record is None:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": "SNAPSHOT::NOT_FOUND",
                    "message": f"Snapshot {snapshot_id} not found",
                },
            )
        return self._to_snapshot_response(record)

    def delete_snapshot(self, snapshot_id: str) -> None:
        record = self._snapshot_repository.get(snapshot_id)
        if record is None:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": "SNAPSHOT::NOT_FOUND",
                    "message": f"Snapshot {snapshot_id} not found",
                },
            )

        if record.status.state == SnapshotState.CREATING:
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": "SNAPSHOT::INVALID_STATE",
                    "message": f"Snapshot {snapshot_id} is still being created and cannot be deleted",
                },
            )

        if record.status.state != SnapshotState.DELETING:
            record = self._mark_snapshot_deleting(record)
            if record is None:
                return

        self._snapshot_runtime.delete_snapshot(
            snapshot_id,
            image=record.restore_config.image,
        )
        self._snapshot_repository.delete(snapshot_id)

    def close(self) -> None:
        """
        Stop accepting new snapshot work and wait for in-flight workers.
        """
        self._snapshot_executor.shutdown(wait=True)

    @staticmethod
    def _default_restore_config():
        return SnapshotRestoreConfig(image=None)

    @staticmethod
    def _default_pagination():
        from opensandbox_server.api.schema import PaginationRequest

        return PaginationRequest()

    def _mark_snapshot_deleting(self, record: SnapshotRecord) -> SnapshotRecord | None:
        now = datetime.now(timezone.utc)
        deleting_record = SnapshotRecord(
            id=record.id,
            source_sandbox_id=record.source_sandbox_id,
            name=record.name,
            description=record.description,
            restore_config=record.restore_config,
            status=SnapshotStatusRecord(
                state=SnapshotState.DELETING,
                reason="snapshot_delete_requested",
                message="Snapshot deletion requested.",
                last_transition_at=now,
            ),
            created_at=record.created_at,
            updated_at=now,
        )
        if self._snapshot_repository.update_if_state(
            deleting_record,
            record.status.state,
        ):
            return deleting_record

        current_record = self._snapshot_repository.get(record.id)
        if current_record is None:
            return None
        if current_record.status.state == SnapshotState.DELETING:
            return current_record

        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": "SNAPSHOT::INVALID_STATE",
                "message": f"Snapshot {record.id} changed state and cannot be deleted",
            },
        )

    def _create_snapshot_worker(self, record: SnapshotRecord) -> None:
        try:
            runtime_status = self._snapshot_runtime.create_snapshot(
                record.id,
                record.source_sandbox_id,
            )
        except Exception as exc:  # noqa: BLE001
            logger.exception(
                "Failed to create snapshot %s from sandbox %s: %s",
                record.id,
                record.source_sandbox_id,
                exc,
            )
            runtime_status = SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_failed",
                message=str(exc),
            )
            self._complete_snapshot(record, runtime_status)
            return

        if runtime_status is None:
            runtime_status = SnapshotRuntimeStatus(
                state=SnapshotState.FAILED,
                reason="snapshot_runtime_missing_result",
                message="Snapshot runtime did not return a final status.",
            )

        self._complete_snapshot(record, runtime_status)

    def _log_worker_failure(self, future: Future) -> None:
        try:
            future.result()
        except Exception as exc:  # noqa: BLE001
            logger.exception("Snapshot worker exited unexpectedly: %s", exc)

    def _complete_snapshot(self, record: SnapshotRecord, runtime_status) -> None:
        current_record = self._snapshot_repository.get(record.id)
        if current_record is None:
            self._cleanup_runtime_artifact(record.id, runtime_status.image)
            return

        if current_record.status.state == SnapshotState.DELETING:
            self._cleanup_runtime_artifact(current_record.id, runtime_status.image)
            self._snapshot_repository.delete(current_record.id)
            return

        if current_record.status.state != SnapshotState.CREATING:
            return

        updated = self._build_runtime_status_record(current_record, runtime_status)
        if updated is None:
            return

        updated_applied = self._snapshot_repository.update_if_state(
            updated,
            SnapshotState.CREATING,
        )
        if not updated_applied:
            logger.info(
                "Snapshot %s was already transitioned before worker completion; skipping update",
                current_record.id,
            )

    def recover_unfinished_snapshots(self) -> None:
        while True:
            result = self._snapshot_repository.list(
                SnapshotListQuery(
                    page=1,
                    page_size=SNAPSHOT_RECOVERY_PAGE_SIZE,
                    states=[SnapshotState.CREATING.value, SnapshotState.DELETING.value],
                )
            )
            if not result.items:
                return

            progressed = False
            for record in result.items:
                try:
                    progressed = self._recover_unfinished_snapshot(record) or progressed
                except Exception as exc:  # noqa: BLE001
                    logger.warning(
                        "Failed to recover unfinished snapshot %s: %s",
                        record.id,
                        exc,
                        exc_info=True,
                    )
                    failed_status = SnapshotRuntimeStatus(
                        state=SnapshotState.FAILED,
                        reason="snapshot_recovery_failed",
                        message=f"Failed to recover unfinished snapshot: {exc}",
                    )
                    self._complete_snapshot(record, failed_status)
                    progressed = True

            if not progressed:
                return

    def _recover_unfinished_snapshot(self, record: SnapshotRecord) -> bool:
        if record.status.state == SnapshotState.CREATING:
            runtime_status = self._snapshot_runtime.inspect_snapshot(
                record.id,
                image=record.restore_config.image,
            )
            if runtime_status.state == SnapshotState.CREATING:
                future = self._snapshot_executor.submit(
                    self._create_snapshot_worker,
                    record,
                )
                future.add_done_callback(self._log_worker_failure)
                return False
            self._complete_snapshot(record, runtime_status)
            return True

        if record.status.state == SnapshotState.DELETING:
            try:
                self._snapshot_runtime.delete_snapshot(
                    record.id,
                    image=record.restore_config.image,
                )
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    "Failed to recover deleting snapshot %s: %s",
                    record.id,
                    exc,
                    exc_info=True,
                )
                return False

            self._snapshot_repository.delete(record.id)
            return True

        return False

    def _build_runtime_status_record(
        self,
        record: SnapshotRecord,
        runtime_status,
    ) -> SnapshotRecord | None:
        now = datetime.now(timezone.utc)
        if runtime_status.state == SnapshotState.READY:
            if not runtime_status.image:
                return SnapshotRecord(
                    id=record.id,
                    source_sandbox_id=record.source_sandbox_id,
                    name=record.name,
                    description=record.description,
                    restore_config=record.restore_config,
                    status=SnapshotStatusRecord(
                        state=SnapshotState.FAILED,
                        reason="snapshot_runtime_missing_image",
                        message="Runtime reported Ready without a snapshot image.",
                        last_transition_at=now,
                    ),
                    created_at=record.created_at,
                    updated_at=now,
                )

            return SnapshotRecord(
                id=record.id,
                source_sandbox_id=record.source_sandbox_id,
                name=record.name,
                description=record.description,
                restore_config=SnapshotRestoreConfig(image=runtime_status.image),
                status=SnapshotStatusRecord(
                    state=SnapshotState.READY,
                    reason=runtime_status.reason,
                    message=runtime_status.message,
                    last_transition_at=now,
                ),
                created_at=record.created_at,
                updated_at=now,
            )

        if runtime_status.state == SnapshotState.FAILED:
            return SnapshotRecord(
                id=record.id,
                source_sandbox_id=record.source_sandbox_id,
                name=record.name,
                description=record.description,
                restore_config=record.restore_config,
                status=SnapshotStatusRecord(
                    state=SnapshotState.FAILED,
                    reason=runtime_status.reason,
                    message=runtime_status.message,
                    last_transition_at=now,
                ),
                created_at=record.created_at,
                updated_at=now,
            )

        return None

    def _cleanup_runtime_artifact(self, snapshot_id: str, image: str | None) -> None:
        if not image:
            return

        try:
            self._snapshot_runtime.delete_snapshot(snapshot_id, image=image)
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                "Failed to cleanup snapshot artifact for %s: %s",
                snapshot_id,
                exc,
                exc_info=True,
            )

    @staticmethod
    def _ensure_source_sandbox_running(sandbox) -> None:
        state = PersistedSnapshotService._sandbox_state(sandbox)
        if state == "Running":
            return

        raise HTTPException(
            status_code=status.HTTP_409_CONFLICT,
            detail={
                "code": SnapshotErrorCodes.INVALID_SOURCE_STATE,
                "message": "Snapshot can only be created from a Running sandbox.",
            },
        )

    @staticmethod
    def _sandbox_state(sandbox) -> str | None:
        if isinstance(sandbox, dict):
            status_value = sandbox.get("status")
            if isinstance(status_value, dict):
                return status_value.get("state")
            return getattr(status_value, "state", None)

        status_value = getattr(sandbox, "status", None)
        if isinstance(status_value, dict):
            return status_value.get("state")
        return getattr(status_value, "state", None)

    @staticmethod
    def _to_snapshot_response(record: SnapshotRecord) -> Snapshot:
        return Snapshot(
            id=record.id,
            sandboxId=record.source_sandbox_id,
            name=record.name,
            status=SnapshotStatus(
                state=record.status.state.value,
                reason=record.status.reason,
                message=record.status.message,
                lastTransitionAt=record.status.last_transition_at,
            ),
            createdAt=record.created_at,
        )


def create_snapshot_service(sandbox_service) -> SnapshotService:
    """
    Build the default persisted snapshot service.
    """
    snapshot_runtime: SnapshotRuntime = create_snapshot_runtime(
        docker_client=getattr(sandbox_service, "docker_client", None),
    )

    return PersistedSnapshotService(
        snapshot_repository=create_snapshot_repository(),
        sandbox_service=sandbox_service,
        snapshot_runtime=snapshot_runtime,
    )


__all__ = [
    "SnapshotService",
    "PersistedSnapshotService",
    "create_snapshot_service",
    "SNAPSHOT_WORKER_MAX_WORKERS",
]
