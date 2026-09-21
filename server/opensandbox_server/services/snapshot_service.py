# Copyright 2025 The OpenSandbox Authors
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

The service persists the snapshot record and submits creation to the runtime.
Status converges asynchronously, mirroring the template catalog pattern:
a runtime status watch (when available) reacts to terminal transitions and
updates rows directly, and every read re-checks non-terminal rows against the
runtime so convergence never depends on the watch alone. Runtimes without a
change stream (Docker) complete inline as before.
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from concurrent.futures import Future, ThreadPoolExecutor
from datetime import datetime, timezone
import logging
from math import ceil
from threading import Event, Lock, Thread
import time
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
from opensandbox_server.config import get_config
from opensandbox_server.repositories.snapshots.factory import get_snapshot_repository
from opensandbox_server.services.constants import SnapshotErrorCodes
from opensandbox_server.services.snapshot_runtime import (
    NoopSnapshotRuntime,
    SnapshotRuntime,
    SnapshotRuntimePreflightError,
    SnapshotRuntimeStatus,
    SnapshotRuntimeUnsupportedError,
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
from opensandbox_server.tenants.context import get_current_tenant

logger = logging.getLogger(__name__)
SNAPSHOT_RECOVERY_PAGE_SIZE = 200
SNAPSHOT_WORKER_MAX_WORKERS = 2
# Read-time sync budget for list requests: each CREATING row costs one
# runtime inspection (a gRPC round trip for fsb), so converging a page is
# capped by deadline rather than by page size. Rows past the budget stay
# stale until the next read; the watch reactor and the PostgreSQL recovery
# loop converge them regardless.
SNAPSHOT_LIST_SYNC_BUDGET_SECONDS = 2.0


class SnapshotService(ABC):

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

    def start_background_sync(self) -> None:
        """
        Start reacting to runtime status changes; default is read-time sync only.
        """

    def close(self) -> None:
        """
        Release resources owned by the snapshot service.
        """


class PersistedSnapshotService(SnapshotService):
    """
    Snapshot service backed by the configured repository.
    """

    _preserve_deleting_on_cleanup_failure = False

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

        namespace = self._get_tenant_namespace()
        try:
            self._snapshot_runtime.preflight_create_snapshot(
                sandbox_id,
                namespace=namespace,
            )
        except SnapshotRuntimeUnsupportedError as exc:
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SnapshotErrorCodes.UNSUPPORTED_RUNTIME,
                    "message": str(exc),
                },
            ) from exc
        except SnapshotRuntimePreflightError as exc:
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SnapshotErrorCodes.RUNTIME_PREFLIGHT_FAILED,
                    "message": str(exc),
                },
            ) from exc

        now = datetime.now(timezone.utc)
        record = SnapshotRecord(
            id=str(uuid4()),
            source_sandbox_id=sandbox_id,
            namespace=namespace,
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
        self._submit_snapshot_worker(record)
        return self._to_snapshot_response(record)

    def list_snapshots(self, request: ListSnapshotsRequest) -> ListSnapshotsResponse:
        pagination = request.pagination or self._default_pagination()
        tenant = get_current_tenant()
        result = self._snapshot_repository.list(
            SnapshotListQuery(
                page=pagination.page,
                page_size=pagination.page_size,
                source_sandbox_id=request.filter.sandbox_id,
                name=request.filter.name,
                states=request.filter.state or [],
                namespace=tenant.namespace if tenant else None,
            )
        )

        total_pages = ceil(result.total_items / pagination.page_size) if result.total_items > 0 else 0
        page_items = list(result.items)
        self._sync_creating_records(page_items)
        if request.filter.state:
            # Convergence may have moved rows out of the requested states
            # after the repository filtered them; reapply the state filter
            # so callers never see a row outside the requested states.
            # Pagination totals reflect the repository read and settle on
            # the next request (same eventual consistency as the template
            # catalog).
            wanted_states = set(request.filter.state)
            page_items = [item for item in page_items if item.status.state.value in wanted_states]
        return ListSnapshotsResponse(
            items=[self._to_snapshot_response(item) for item in page_items],
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
        self._verify_tenant_access(record)
        return self._to_snapshot_response(self._sync_creating_record(record))

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
        self._verify_tenant_access(record)

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
            namespace=record.namespace,
            source_sandbox_id=record.source_sandbox_id,
        )
        self._snapshot_repository.delete(snapshot_id)

    def close(self) -> None:
        """
        Stop accepting new snapshot work and wait for in-flight workers.
        """
        close_runtime = getattr(self._snapshot_runtime, "close", None)
        if close_runtime is not None:
            close_runtime()
        self._snapshot_executor.shutdown(wait=True)

    # -- background status sync ------------------------------------------------

    def start_background_sync(self) -> None:
        """
        React to runtime status changes by converging rows directly.

        Runtimes with a change stream expose ``start_status_watch``; the watch
        reacts to terminal transitions without polling. Convergence never
        depends on it: every read re-checks non-terminal rows, and the
        PostgreSQL recovery loop re-checks them periodically.
        """
        start_status_watch = getattr(self._snapshot_runtime, "start_status_watch", None)
        if start_status_watch is None:
            return
        try:
            namespaces = self._active_snapshot_namespaces()
        except Exception as exc:  # noqa: BLE001 - catalog may be empty/unavailable
            logger.warning(f"Snapshot namespace scan failed while starting watches: {exc}")
            namespaces = set()
        start_status_watch(self._on_runtime_change, namespaces)

    def _active_snapshot_namespaces(self) -> set[str | None]:
        """Distinct namespaces that own non-terminal rows."""
        namespaces: set[str | None] = set()
        page = 1
        while True:
            result = self._snapshot_repository.list(
                SnapshotListQuery(
                    page=page,
                    page_size=SNAPSHOT_RECOVERY_PAGE_SIZE,
                    states=[SnapshotState.CREATING.value, SnapshotState.DELETING.value],
                )
            )
            namespaces.update(record.namespace for record in result.items)
            if len(result.items) < SNAPSHOT_RECOVERY_PAGE_SIZE:
                return namespaces
            page += 1

    def _on_runtime_change(self, snapshot_id: str, namespace: str) -> None:
        """Watch callback (informer threads): converge a CREATING row once."""
        record = self._snapshot_repository.get(snapshot_id)
        if record is None or record.status.state != SnapshotState.CREATING:
            return
        self._converge_from_runtime(record)

    def _converge_from_runtime(self, record: SnapshotRecord) -> bool:
        """One runtime observation; CAS-complete the row when terminal."""
        runtime_status = self._observe_runtime(record)
        if runtime_status is None or runtime_status.state not in (
            SnapshotState.READY,
            SnapshotState.FAILED,
        ):
            return False
        self._complete_snapshot(record, runtime_status)
        return True

    def _observe_runtime(self, record: SnapshotRecord):
        try:
            return self._snapshot_runtime.inspect_snapshot(
                record.id,
                image=record.restore_config.image,
                namespace=record.namespace,
                source_sandbox_id=record.source_sandbox_id,
            )
        except Exception as exc:  # noqa: BLE001 - convergence retries on the next read
            logger.warning(
                f"Snapshot status read failed for {record.id}: {exc}"
            )
            return None

    def _sync_creating_records(self, records: list[SnapshotRecord]) -> None:
        """Converge CREATING rows in place within a bounded per-request budget."""
        if getattr(self._snapshot_runtime, "start_status_watch", None) is None:
            # Inline runtimes complete their own rows; observing them here
            # would misreport in-flight work.
            return
        deadline = time.monotonic() + SNAPSHOT_LIST_SYNC_BUDGET_SECONDS
        for index, record in enumerate(records):
            if record.status.state != SnapshotState.CREATING:
                continue
            if time.monotonic() >= deadline:
                return
            if self._converge_from_runtime(record):
                records[index] = self._snapshot_repository.get(record.id) or record

    def _sync_creating_record(self, record: SnapshotRecord) -> SnapshotRecord:
        """Read-time sync: re-check a non-terminal row before responding."""
        if record.status.state != SnapshotState.CREATING:
            return record
        if getattr(self._snapshot_runtime, "start_status_watch", None) is None:
            # Inline runtimes complete their own rows; observing them here
            # would misreport in-flight work.
            return record
        if not self._converge_from_runtime(record):
            return record
        return self._snapshot_repository.get(record.id) or record

    @staticmethod
    def _default_restore_config():
        return SnapshotRestoreConfig(image=None)

    @staticmethod
    def _default_pagination():
        from opensandbox_server.api.schema import PaginationRequest

        return PaginationRequest(page=1, pageSize=20)

    @staticmethod
    def _get_tenant_namespace() -> str | None:
        tenant = get_current_tenant()
        return tenant.namespace if tenant else None

    @staticmethod
    def _verify_tenant_access(record: SnapshotRecord) -> None:
        tenant = get_current_tenant()
        if tenant is None:
            return
        if record.namespace is None or record.namespace != tenant.namespace:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": "SNAPSHOT::NOT_FOUND",
                    "message": f"Snapshot {record.id} not found",
                },
            )

    def _mark_snapshot_deleting(self, record: SnapshotRecord) -> SnapshotRecord | None:
        now = datetime.now(timezone.utc)
        deleting_record = SnapshotRecord(
            id=record.id,
            source_sandbox_id=record.source_sandbox_id,
            namespace=record.namespace,
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
                namespace=record.namespace,
            )
        except Exception as exc:  # noqa: BLE001
            logger.exception(
                f"Failed to create snapshot {record.id} from sandbox "
                f"{record.source_sandbox_id}: {exc}"
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
            logger.exception(f"Snapshot worker exited unexpectedly: {exc}")

    def _submit_snapshot_worker(self, record: SnapshotRecord) -> None:
        future = self._snapshot_executor.submit(
            self._create_snapshot_worker,
            record,
        )
        future.add_done_callback(self._log_worker_failure)

    def _complete_snapshot(self, record: SnapshotRecord, runtime_status) -> None:
        current_record = self._snapshot_repository.get(record.id)
        if current_record is None:
            self._cleanup_runtime_artifact(
                record.id,
                runtime_status.image,
                record.namespace,
                record.source_sandbox_id,
            )
            return

        if current_record.status.state == SnapshotState.DELETING:
            cleaned = self._cleanup_runtime_artifact(
                current_record.id,
                runtime_status.image,
                current_record.namespace,
                current_record.source_sandbox_id,
            )
            if self._preserve_deleting_on_cleanup_failure and not cleaned:
                return
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
                f"Snapshot {current_record.id} was already transitioned before "
                "worker completion; skipping update"
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
                        f"Failed to recover unfinished snapshot {record.id}: {exc}",
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
                namespace=record.namespace,
                source_sandbox_id=record.source_sandbox_id,
            )
            if runtime_status.state == SnapshotState.CREATING:
                self._submit_snapshot_worker(record)
                return False
            self._complete_snapshot(record, runtime_status)
            return True

        if record.status.state == SnapshotState.DELETING:
            try:
                self._snapshot_runtime.delete_snapshot(
                    record.id,
                    image=record.restore_config.image,
                    namespace=record.namespace,
                    source_sandbox_id=record.source_sandbox_id,
                )
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    f"Failed to recover deleting snapshot {record.id}: {exc}",
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
                    namespace=record.namespace,
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
                namespace=record.namespace,
                name=record.name,
                description=record.description,
                restore_config=SnapshotRestoreConfig(
                    image=runtime_status.image,
                    backend=runtime_status.backend,
                ),
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
                namespace=record.namespace,
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

    def _cleanup_runtime_artifact(
        self,
        snapshot_id: str,
        image: str | None,
        namespace: str | None = "default",
        source_sandbox_id: str | None = None,
    ) -> bool:
        if not image:
            return False

        try:
            self._snapshot_runtime.delete_snapshot(
                snapshot_id,
                image=image,
                namespace=namespace,
                source_sandbox_id=source_sandbox_id,
            )
            return True
        except Exception as exc:  # noqa: BLE001
            logger.warning(
                f"Failed to cleanup snapshot artifact for {snapshot_id}: {exc}",
                exc_info=True,
            )
            return False

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


class PostgreSQLKubernetesSnapshotService(PersistedSnapshotService):
    """Periodic unfinished-operation recovery for PostgreSQL + Kubernetes only."""

    _preserve_deleting_on_cleanup_failure = True

    def __init__(
        self,
        snapshot_repository: SnapshotRepository,
        sandbox_service,
        snapshot_runtime: SnapshotRuntime,
        *,
        recovery_interval_seconds: float,
        snapshot_executor=None,
    ) -> None:
        if recovery_interval_seconds <= 0:
            raise ValueError("recovery_interval_seconds must be greater than zero")
        self._recovery_interval_seconds = recovery_interval_seconds
        self._recovery_stop = Event()
        self._inflight_snapshot_ids: set[str] = set()
        self._inflight_lock = Lock()
        super().__init__(
            snapshot_repository,
            sandbox_service,
            snapshot_runtime=snapshot_runtime,
            snapshot_executor=snapshot_executor,
            recover_unfinished_snapshots=False,
        )
        self._recovery_thread = Thread(
            target=self._run_recovery_loop,
            name="postgresql-kubernetes-snapshot-recovery",
            daemon=True,
        )
        self._recovery_thread.start()

    def close(self) -> None:
        self._recovery_stop.set()
        self._recovery_thread.join()
        super().close()

    def _run_recovery_loop(self) -> None:
        while not self._recovery_stop.is_set():
            try:
                self.recover_unfinished_snapshots()
            except Exception as exc:  # noqa: BLE001
                logger.warning(
                    f"PostgreSQL Kubernetes snapshot recovery scan failed: {exc}",
                    exc_info=True,
                )
            self._recovery_stop.wait(self._recovery_interval_seconds)

    def _submit_snapshot_worker(self, record: SnapshotRecord) -> None:
        with self._inflight_lock:
            if record.id in self._inflight_snapshot_ids:
                return
            self._inflight_snapshot_ids.add(record.id)

        def run_tracked_worker() -> None:
            try:
                self._create_snapshot_worker(record)
            finally:
                with self._inflight_lock:
                    self._inflight_snapshot_ids.discard(record.id)

        try:
            future = self._snapshot_executor.submit(run_tracked_worker)
        except BaseException:
            with self._inflight_lock:
                self._inflight_snapshot_ids.discard(record.id)
            raise
        future.add_done_callback(self._log_worker_failure)


def create_snapshot_service(sandbox_service) -> SnapshotService:
    active_config = get_config()
    snapshot_runtime: SnapshotRuntime = create_snapshot_runtime(
        active_config,
        docker_client=getattr(sandbox_service, "docker_client", None),
    )

    if (
        active_config.store.type == "postgresql"
        and active_config.runtime.type == "kubernetes"
    ):
        return PostgreSQLKubernetesSnapshotService(
            snapshot_repository=get_snapshot_repository(),
            sandbox_service=sandbox_service,
            snapshot_runtime=snapshot_runtime,
            recovery_interval_seconds=(
                active_config.store.postgresql.snapshot_recovery_interval_seconds
            ),
        )

    return PersistedSnapshotService(
        snapshot_repository=get_snapshot_repository(),
        sandbox_service=sandbox_service,
        snapshot_runtime=snapshot_runtime,
    )


__all__ = [
    "SnapshotService",
    "PersistedSnapshotService",
    "PostgreSQLKubernetesSnapshotService",
    "create_snapshot_service",
    "SNAPSHOT_WORKER_MAX_WORKERS",
]
