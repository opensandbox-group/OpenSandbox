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

from concurrent.futures import Future, ThreadPoolExecutor
from copy import deepcopy
from datetime import datetime, timezone
import os
from threading import Barrier, Lock
import time

from kubernetes.client import ApiException
import psycopg
import pytest
from pydantic import SecretStr

from opensandbox_server.api.schema import CreateSnapshotRequest
from opensandbox_server.config import (
    AppConfig,
    KubernetesRuntimeConfig,
    PostgreSQLStoreConfig,
    RuntimeConfig,
    StoreConfig,
)
from opensandbox_server.repositories.snapshots.postgresql import PostgreSQLSnapshotRepository
from opensandbox_server.services.k8s.snapshot_runtime import build_public_snapshot_name
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_runtime import SnapshotRuntimeStatus
from opensandbox_server.services.snapshot_runtime_factory import create_snapshot_runtime
from opensandbox_server.services.snapshot_repository import (
    SnapshotListQuery,
    SnapshotListResult,
)
from opensandbox_server.services.snapshot_service import (
    PostgreSQLKubernetesSnapshotService,
)


TEST_POSTGRESQL_DSN_ENV_VAR = "OPENSANDBOX_TEST_POSTGRESQL_DSN"
SNAPSHOT_ID = "11111111-2222-4333-8444-555555555555"
SANDBOX_ID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"


@pytest.fixture(scope="module")
def postgresql_dsn() -> str:
    dsn = os.environ.get(TEST_POSTGRESQL_DSN_ENV_VAR)
    if not dsn:
        pytest.skip(f"{TEST_POSTGRESQL_DSN_ENV_VAR} is not set")
    return dsn


def _repository(dsn: str) -> PostgreSQLSnapshotRepository:
    return PostgreSQLSnapshotRepository(
        dsn,
        min_pool_size=0,
        max_pool_size=4,
        connect_timeout_seconds=5,
        pool_timeout_seconds=5,
    )


def _truncate(dsn: str) -> None:
    with psycopg.connect(dsn) as conn:
        conn.execute("TRUNCATE TABLE snapshots")


def _config(dsn: str) -> AppConfig:
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="opensandbox/execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="default",
            snapshot_create_timeout_seconds=1,
        ),
        store=StoreConfig(
            type="postgresql",
            postgresql=PostgreSQLStoreConfig(
                dsn=SecretStr(dsn),
                min_pool_size=0,
                max_pool_size=4,
                snapshot_recovery_interval_seconds=0.05,
            ),
        ),
    )


def _creating_record() -> SnapshotRecord:
    now = datetime.now(timezone.utc)
    return SnapshotRecord(
        id=SNAPSHOT_ID,
        source_sandbox_id=SANDBOX_ID,
        namespace="default",
        restore_config=SnapshotRestoreConfig(image=None),
        status=SnapshotStatusRecord(
            state=SnapshotState.CREATING,
            reason="snapshot_accepted",
            message="Snapshot creation accepted.",
            last_transition_at=now,
        ),
        created_at=now,
        updated_at=now,
    )


class _SandboxService:
    @staticmethod
    def get_sandbox(sandbox_id: str):
        return {"id": sandbox_id, "status": {"state": "Running"}}


class _ImmediateExecutor:
    def submit(self, fn, *args, **kwargs) -> Future:
        future = Future()
        try:
            future.set_result(fn(*args, **kwargs))
        except Exception as exc:  # noqa: BLE001
            future.set_exception(exc)
        return future

    def shutdown(self, wait: bool = True) -> None:
        return None


class _CapturingExecutor:
    def __init__(self) -> None:
        self.submitted: list[tuple[object, tuple, dict]] = []

    def submit(self, fn, *args, **kwargs) -> Future:
        self.submitted.append((fn, args, kwargs))
        return Future()

    def shutdown(self, wait: bool = True) -> None:
        return None


class _SharedK8sClient:
    def __init__(self, *, force_create_conflict: bool = False) -> None:
        self._lock = Lock()
        self._observe_count = 0
        self._observe_barriers = [Barrier(2), Barrier(2)] if force_create_conflict else []
        self._delete_barrier = Barrier(2) if force_create_conflict else None
        self.objects: dict[str, dict] = {}
        self.create_attempts = 0
        self.successful_creates = 0
        self.delete_attempts = 0
        self.successful_deletes = 0

    def create_custom_object(self, *, body: dict, **kwargs):
        name = body["metadata"]["name"]
        with self._lock:
            self.create_attempts += 1
            if name in self.objects:
                raise ApiException(status=409, reason="Already Exists")
            stored = deepcopy(body)
            stored["status"] = {
                "phase": "Succeed",
                "containers": [
                    {
                        "containerName": "sandbox",
                        "imageUri": "registry/sandbox:snapshot",
                    }
                ],
            }
            self.objects[name] = stored
            self.successful_creates += 1
            return deepcopy(stored)

    def get_custom_object(self, *, name: str, **kwargs):
        barrier = None
        with self._lock:
            if self._observe_count < len(self._observe_barriers) * 2:
                barrier = self._observe_barriers[self._observe_count // 2]
                self._observe_count += 1
        if barrier is not None:
            barrier.wait(timeout=5)
            return None
        with self._lock:
            obj = self.objects.get(name)
            return deepcopy(obj) if obj is not None else None

    def delete_custom_object(self, *, name: str, **kwargs) -> None:
        with self._lock:
            self.delete_attempts += 1
        if self._delete_barrier is not None:
            self._delete_barrier.wait(timeout=5)
        with self._lock:
            if self.objects.pop(name, None) is None:
                raise ApiException(status=404, reason="Not Found")
            self.successful_deletes += 1


class _CountingRepository:
    def __init__(self, repository: PostgreSQLSnapshotRepository, winners: list[str]) -> None:
        self._repository = repository
        self._winners = winners

    def create(self, record: SnapshotRecord) -> SnapshotRecord:
        return self._repository.create(record)

    def get(self, snapshot_id: str) -> SnapshotRecord | None:
        return self._repository.get(snapshot_id)

    def list(self, query: SnapshotListQuery) -> SnapshotListResult:
        return self._repository.list(query)

    def update(self, record: SnapshotRecord) -> SnapshotRecord:
        return self._repository.update(record)

    def update_if_state(self, record: SnapshotRecord, expected_state: SnapshotState) -> bool:
        updated = self._repository.update_if_state(record, expected_state)
        if updated:
            self._winners.append(record.status.state.value)
        return updated

    def delete(self, snapshot_id: str) -> None:
        self._repository.delete(snapshot_id)

    def close(self) -> None:
        self._repository.close()


def test_two_active_services_share_one_cr_and_one_terminal_cas(
    postgresql_dsn: str,
) -> None:
    repositories = [_repository(postgresql_dsn), _repository(postgresql_dsn)]
    services: list[PostgreSQLKubernetesSnapshotService] = []
    try:
        _truncate(postgresql_dsn)
        k8s_client = _SharedK8sClient(force_create_conflict=True)
        cas_winners: list[str] = []

        for repository in repositories:
            runtime = create_snapshot_runtime(_config(postgresql_dsn), k8s_client=k8s_client)
            service = PostgreSQLKubernetesSnapshotService(
                _CountingRepository(repository, cas_winners),
                _SandboxService(),
                snapshot_runtime=runtime,
                snapshot_executor=_ImmediateExecutor(),
                recovery_interval_seconds=60,
            )
            service._recovery_stop.set()
            service._recovery_thread.join()
            services.append(service)

        repositories[0].create(_creating_record())

        with ThreadPoolExecutor(max_workers=2) as executor:
            futures = [executor.submit(service.recover_unfinished_snapshots) for service in services]
            for future in futures:
                future.result(timeout=10)

        stored = repositories[0].get(SNAPSHOT_ID)
        assert stored is not None
        assert stored.status.state == SnapshotState.READY
        assert stored.restore_config.image == "registry/sandbox:snapshot"
        assert cas_winners == [SnapshotState.READY.value]
        assert k8s_client.successful_creates == 1
        assert list(k8s_client.objects) == [build_public_snapshot_name(SNAPSHOT_ID)]

        deleting = SnapshotRecord(
            id=stored.id,
            source_sandbox_id=stored.source_sandbox_id,
            namespace=stored.namespace,
            restore_config=stored.restore_config,
            status=SnapshotStatusRecord(state=SnapshotState.DELETING),
            created_at=stored.created_at,
            updated_at=datetime.now(timezone.utc),
        )
        assert repositories[0].update_if_state(deleting, SnapshotState.READY)
        services[0]._complete_snapshot(
            deleting,
            SnapshotRuntimeStatus(
                state=SnapshotState.CREATING,
                reason="snapshot_runtime_timeout",
                message="A stale worker has no artifact to clean up.",
            ),
        )
        still_deleting = repositories[0].get(SNAPSHOT_ID)
        assert still_deleting is not None
        assert still_deleting.status.state == SnapshotState.DELETING

        with ThreadPoolExecutor(max_workers=2) as executor:
            futures = [executor.submit(service.recover_unfinished_snapshots) for service in services]
            for future in futures:
                future.result(timeout=10)

        assert repositories[0].get(SNAPSHOT_ID) is None
        assert k8s_client.successful_deletes == 1
        assert k8s_client.delete_attempts == 2
    finally:
        for service in services:
            service.close()
        for repository in repositories:
            repository.close()


def test_active_peer_periodically_recovers_after_creator_crash(
    postgresql_dsn: str,
) -> None:
    repositories = [_repository(postgresql_dsn), _repository(postgresql_dsn)]
    creator = None
    peer = None
    try:
        _truncate(postgresql_dsn)
        k8s_client = _SharedK8sClient()
        creator_executor = _CapturingExecutor()
        creator = PostgreSQLKubernetesSnapshotService(
            repositories[0],
            _SandboxService(),
            snapshot_runtime=create_snapshot_runtime(
                _config(postgresql_dsn),
                k8s_client=k8s_client,
            ),
            recovery_interval_seconds=60,
            snapshot_executor=creator_executor,
        )
        peer = PostgreSQLKubernetesSnapshotService(
            repositories[1],
            _SandboxService(),
            snapshot_runtime=create_snapshot_runtime(
                _config(postgresql_dsn),
                k8s_client=k8s_client,
            ),
            recovery_interval_seconds=0.05,
            snapshot_executor=_ImmediateExecutor(),
        )

        created = creator.create_snapshot(
            SANDBOX_ID,
            CreateSnapshotRequest(name="creator-crash"),
        )
        assert len(creator_executor.submitted) == 1

        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            stored = repositories[0].get(created.id)
            if stored is not None and stored.status.state == SnapshotState.READY:
                break
            time.sleep(0.02)
        else:
            pytest.fail("active peer did not recover the creator's unfinished snapshot")

        assert stored.restore_config.image == "registry/sandbox:snapshot"
        assert k8s_client.successful_creates == 1

        peer.close()
        peer = None
        restarted_peer = PostgreSQLKubernetesSnapshotService(
            repositories[1],
            _SandboxService(),
            snapshot_runtime=create_snapshot_runtime(
                _config(postgresql_dsn),
                k8s_client=k8s_client,
            ),
            recovery_interval_seconds=0.05,
            snapshot_executor=_ImmediateExecutor(),
        )
        time.sleep(0.1)
        restarted_peer.close()
        assert k8s_client.successful_creates == 1
    finally:
        if peer is not None:
            peer.close()
        if creator is not None:
            creator.close()
        for repository in repositories:
            repository.close()
