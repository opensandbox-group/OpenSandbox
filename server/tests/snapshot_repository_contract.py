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

"""Shared behavioral contract for snapshot repository implementations."""

from datetime import datetime, timedelta, timezone

import pytest

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


def snapshot_record(
    snapshot_id: str,
    sandbox_id: str,
    created_at: datetime,
    state: SnapshotState = SnapshotState.CREATING,
    *,
    namespace: str | None = None,
) -> SnapshotRecord:
    return SnapshotRecord(
        id=snapshot_id,
        source_sandbox_id=sandbox_id,
        namespace=namespace,
        name=f"name-{snapshot_id}",
        description=f"description-{snapshot_id}",
        restore_config=SnapshotRestoreConfig(
            image=f"registry.example.com/snapshots/{snapshot_id}:latest"
        ),
        status=SnapshotStatusRecord(
            state=state,
            reason=f"reason-{snapshot_id}",
            message=f"message-{snapshot_id}",
            last_transition_at=created_at,
        ),
        created_at=created_at,
        updated_at=created_at,
    )


class SnapshotRepositoryContract:
    """Tests inherited by every concrete snapshot repository backend."""

    @pytest.fixture
    def repository(self) -> SnapshotRepository:
        raise NotImplementedError

    def test_persists_and_fetches_records(self, repository: SnapshotRepository) -> None:
        record = snapshot_record(
            "snap-001",
            "sbx-001",
            datetime.now(timezone.utc),
            namespace="tenant-a",
        )

        repository.create(record)
        loaded = repository.get(record.id)

        assert loaded is not None
        assert loaded.id == record.id
        assert loaded.source_sandbox_id == record.source_sandbox_id
        assert loaded.namespace == "tenant-a"
        assert loaded.name == record.name
        assert loaded.description == record.description
        assert loaded.restore_config.image == record.restore_config.image
        assert loaded.status == record.status
        assert loaded.created_at == record.created_at
        assert loaded.updated_at == record.updated_at
        assert repository.get("missing") is None

    def test_lists_and_updates_records(self, repository: SnapshotRepository) -> None:
        now = datetime.now(timezone.utc)
        first = snapshot_record("snap-001", "sbx-001", now, namespace="tenant-a")
        second = snapshot_record(
            "snap-002",
            "sbx-001",
            now + timedelta(seconds=1),
            SnapshotState.READY,
            namespace="tenant-a",
        )
        third = snapshot_record(
            "snap-003",
            "sbx-002",
            now + timedelta(seconds=2),
            SnapshotState.FAILED,
            namespace="tenant-b",
        )
        for record in (first, second, third):
            repository.create(record)

        page = repository.list(
            SnapshotListQuery(
                page=1,
                page_size=10,
                source_sandbox_id="sbx-001",
                name="name-snap-002",
                states=[SnapshotState.READY.value],
                namespace="tenant-a",
            )
        )
        assert page.total_items == 1
        assert [item.id for item in page.items] == ["snap-002"]

        tenant_b = repository.list(SnapshotListQuery(page=1, page_size=10, namespace="tenant-b"))
        assert tenant_b.total_items == 1
        assert [item.id for item in tenant_b.items] == ["snap-003"]

        partial_name = repository.list(SnapshotListQuery(page=1, page_size=10, name="name-snap"))
        assert partial_name.total_items == 0
        assert partial_name.items == []

        ordered = repository.list(SnapshotListQuery(page=1, page_size=2))
        assert ordered.total_items == 3
        assert [item.id for item in ordered.items] == ["snap-003", "snap-002"]

        second_page = repository.list(SnapshotListQuery(page=2, page_size=2))
        assert second_page.total_items == 3
        assert [item.id for item in second_page.items] == ["snap-001"]

        updated = snapshot_record(
            first.id,
            first.source_sandbox_id,
            first.created_at,
            SnapshotState.READY,
            namespace=first.namespace,
        )
        updated.restore_config.image = "registry.example.com/snapshots/snap-001:v2"
        updated.updated_at = now + timedelta(seconds=3)
        repository.update(updated)

        loaded = repository.get(first.id)
        assert loaded is not None
        assert loaded.status.state == SnapshotState.READY
        assert loaded.restore_config.image == "registry.example.com/snapshots/snap-001:v2"

    def test_compare_and_swap_and_delete(self, repository: SnapshotRepository) -> None:
        now = datetime.now(timezone.utc)
        original = snapshot_record("snap-001", "sbx-001", now)
        repository.create(original)

        ready = snapshot_record(
            original.id,
            original.source_sandbox_id,
            original.created_at,
            SnapshotState.READY,
        )
        failed = snapshot_record(
            original.id,
            original.source_sandbox_id,
            original.created_at,
            SnapshotState.FAILED,
        )
        ready.restore_config.image = "registry.example.com/snapshots/snap-001:cas"

        assert repository.update_if_state(ready, SnapshotState.CREATING) is True
        assert repository.update_if_state(failed, SnapshotState.CREATING) is False
        loaded = repository.get(original.id)
        assert loaded is not None
        assert loaded.status.state == SnapshotState.READY
        assert loaded.restore_config.image == "registry.example.com/snapshots/snap-001:cas"

        repository.delete(original.id)
        repository.delete(original.id)
        assert repository.get(original.id) is None
