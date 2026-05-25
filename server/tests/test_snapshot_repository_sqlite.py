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

from datetime import datetime, timedelta

from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
from opensandbox_server.repositories.snapshots.sqlite import (
    SQLITE_BUSY_TIMEOUT_MS,
    SQLiteSnapshotRepository,
)
from opensandbox_server.config import AppConfig, RuntimeConfig, StoreConfig
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_repository import SnapshotListQuery


def _record(
    snapshot_id: str,
    sandbox_id: str,
    created_at: datetime,
    state: SnapshotState = SnapshotState.CREATING,
) -> SnapshotRecord:
    return SnapshotRecord(
        id=snapshot_id,
        source_sandbox_id=sandbox_id,
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


def test_sqlite_snapshot_repository_persists_and_fetches_records(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    record = _record("snap-001", "sbx-001", datetime.utcnow())

    repo.create(record)
    loaded = repo.get("snap-001")

    assert loaded is not None
    assert loaded.id == "snap-001"
    assert loaded.source_sandbox_id == "sbx-001"
    assert loaded.restore_config.image == record.restore_config.image
    assert loaded.status.state == SnapshotState.CREATING


def test_sqlite_snapshot_repository_enables_wal_and_busy_timeout(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")

    with repo._connect() as conn:
        journal_mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        busy_timeout = conn.execute("PRAGMA busy_timeout").fetchone()[0]

    assert journal_mode.lower() == "wal"
    assert busy_timeout == SQLITE_BUSY_TIMEOUT_MS


def test_sqlite_snapshot_repository_lists_and_updates_records(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    now = datetime.utcnow()
    first = _record("snap-001", "sbx-001", now)
    second = _record("snap-002", "sbx-001", now + timedelta(seconds=1), state=SnapshotState.READY)
    third = _record("snap-003", "sbx-002", now + timedelta(seconds=2), state=SnapshotState.FAILED)

    repo.create(first)
    repo.create(second)
    repo.create(third)

    page = repo.list(
        SnapshotListQuery(page=1, page_size=10, source_sandbox_id="sbx-001", states=["Ready"])
    )

    assert page.total_items == 1
    assert [item.id for item in page.items] == ["snap-002"]

    updated = SnapshotRecord(
        id=first.id,
        source_sandbox_id=first.source_sandbox_id,
        name=first.name,
        description=first.description,
        restore_config=SnapshotRestoreConfig(image="registry.example.com/snapshots/snap-001:v2"),
        status=SnapshotStatusRecord(
            state=SnapshotState.READY,
            reason="snapshot_ready",
            message="Snapshot is ready.",
            last_transition_at=now + timedelta(seconds=3),
        ),
        created_at=first.created_at,
        updated_at=now + timedelta(seconds=3),
    )
    repo.update(updated)

    loaded = repo.get("snap-001")
    assert loaded is not None
    assert loaded.status.state == SnapshotState.READY
    assert loaded.restore_config.image == "registry.example.com/snapshots/snap-001:v2"


def test_sqlite_snapshot_repository_update_if_state_is_atomic(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")
    now = datetime.utcnow()
    original = _record("snap-001", "sbx-001", now, state=SnapshotState.CREATING)
    repo.create(original)

    ready = SnapshotRecord(
        id=original.id,
        source_sandbox_id=original.source_sandbox_id,
        name=original.name,
        description=original.description,
        restore_config=SnapshotRestoreConfig(image="opensandbox-snapshots:snap-001"),
        status=SnapshotStatusRecord(
            state=SnapshotState.READY,
            reason="snapshot_ready",
            message="Snapshot is ready.",
            last_transition_at=now + timedelta(seconds=1),
        ),
        created_at=original.created_at,
        updated_at=now + timedelta(seconds=1),
    )
    failed = SnapshotRecord(
        id=original.id,
        source_sandbox_id=original.source_sandbox_id,
        name=original.name,
        description=original.description,
        restore_config=original.restore_config,
        status=SnapshotStatusRecord(
            state=SnapshotState.FAILED,
            reason="snapshot_failed",
            message="Snapshot failed.",
            last_transition_at=now + timedelta(seconds=2),
        ),
        created_at=original.created_at,
        updated_at=now + timedelta(seconds=2),
    )

    assert repo.update_if_state(ready, SnapshotState.CREATING) is True
    assert repo.update_if_state(failed, SnapshotState.CREATING) is False

    loaded = repo.get(original.id)
    assert loaded is not None
    assert loaded.status.state == SnapshotState.READY
    assert loaded.restore_config.image == "opensandbox-snapshots:snap-001"


def test_snapshot_repository_factory_defaults_to_sqlite(tmp_path) -> None:
    db_path = tmp_path / "factory-snapshots.db"
    config = AppConfig(
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"),
        store=StoreConfig(path=str(db_path)),
    )

    repo = create_snapshot_repository(config)

    assert isinstance(repo, SQLiteSnapshotRepository)
    assert repo.db_path == db_path
