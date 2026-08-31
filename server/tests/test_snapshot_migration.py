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

from datetime import datetime, timezone
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys

import psycopg
import pytest

from opensandbox_server.cli import _build_parser
from opensandbox_server.repositories.snapshots.migrate import (
    migrate_sqlite_snapshots_to_postgresql,
)
from opensandbox_server.repositories.snapshots.postgresql import PostgreSQLSnapshotRepository
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_models import SnapshotState
from opensandbox_server.services.snapshot_repository import SnapshotListQuery
from tests.snapshot_repository_contract import snapshot_record

TEST_POSTGRESQL_DSN_ENV_VAR = "OPENSANDBOX_TEST_POSTGRESQL_DSN"


@pytest.fixture(scope="module")
def postgresql_dsn() -> str:
    dsn = os.environ.get(TEST_POSTGRESQL_DSN_ENV_VAR)
    if not dsn:
        pytest.skip(f"{TEST_POSTGRESQL_DSN_ENV_VAR} is not set")
    return dsn


def _truncate_postgresql(dsn: str) -> None:
    repo = PostgreSQLSnapshotRepository(dsn)
    repo.close()
    with psycopg.connect(dsn) as conn:
        conn.execute("TRUNCATE TABLE snapshots")


def _create_sqlite_repository(tmp_path, records) -> SQLiteSnapshotRepository:
    repo = SQLiteSnapshotRepository(tmp_path / "opensandbox.db")
    for record in records:
        repo.create(record)
    return repo


def _records_from_postgresql(dsn: str, limit: int = 100) -> list:
    repo = PostgreSQLSnapshotRepository(dsn)
    try:
        return list(repo.list(SnapshotListQuery(page=1, page_size=limit)).items)
    finally:
        repo.close()


def _utc(dt: datetime) -> datetime:
    if dt.tzinfo is None:
        return dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc)


def test_migrate_copies_all_snapshot_records(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    now = datetime(2026, 1, 2, 3, 4, 5)
    sqlite_repo = _create_sqlite_repository(
        tmp_path,
        [
            snapshot_record(
                "snap-ready", "sbx-001", now, SnapshotState.READY, namespace="tenant-a"
            ),
            snapshot_record("snap-failed", "sbx-002", now, SnapshotState.FAILED),
            snapshot_record("snap-creating", "sbx-003", now, namespace="tenant-b"),
        ],
    )
    sqlite_repo.close()

    result = migrate_sqlite_snapshots_to_postgresql(
        tmp_path / "opensandbox.db", postgresql_dsn
    )

    assert result.total == 3
    assert result.migrated == 3
    assert result.skipped == 0
    assert result.dry_run is False

    stored = _records_from_postgresql(postgresql_dsn)
    assert len(stored) == 3
    by_id = {record.id: record for record in stored}
    assert set(by_id) == {"snap-ready", "snap-failed", "snap-creating"}

    ready = by_id["snap-ready"]
    assert ready.source_sandbox_id == "sbx-001"
    assert ready.namespace == "tenant-a"
    assert ready.name == "name-snap-ready"
    assert ready.description == "description-snap-ready"
    assert ready.restore_config.image == "registry.example.com/snapshots/snap-ready:latest"
    assert ready.status.state == SnapshotState.READY
    assert ready.status.reason == "reason-snap-ready"
    assert ready.status.message == "message-snap-ready"
    assert _utc(ready.created_at) == datetime(2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc)
    assert ready.created_at.tzinfo is timezone.utc
    assert ready.updated_at.tzinfo is timezone.utc


def test_migrate_is_idempotent(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    now = datetime(2026, 1, 2, 3, 4, 5)
    sqlite_repo = _create_sqlite_repository(
        tmp_path,
        [snapshot_record("snap-a", "sbx-001", now, SnapshotState.READY)],
    )
    sqlite_repo.close()

    first = migrate_sqlite_snapshots_to_postgresql(tmp_path / "opensandbox.db", postgresql_dsn)
    second = migrate_sqlite_snapshots_to_postgresql(tmp_path / "opensandbox.db", postgresql_dsn)

    assert first.migrated == 1
    assert second.migrated == 0
    assert second.skipped == 1
    assert len(_records_from_postgresql(postgresql_dsn)) == 1


def test_migrate_skips_records_already_in_postgresql(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    now = datetime(2026, 1, 2, 3, 4, 5)
    sqlite_repo = _create_sqlite_repository(
        tmp_path,
        [
            snapshot_record("snap-existing", "sbx-001", now, SnapshotState.READY),
            snapshot_record("snap-new", "sbx-002", now, SnapshotState.CREATING),
        ],
    )
    sqlite_repo.close()

    target = PostgreSQLSnapshotRepository(postgresql_dsn)
    target.create(snapshot_record("snap-existing", "sbx-001", now, SnapshotState.READY))
    target.close()

    result = migrate_sqlite_snapshots_to_postgresql(tmp_path / "opensandbox.db", postgresql_dsn)

    assert result.migrated == 1
    assert result.skipped == 1
    stored = _records_from_postgresql(postgresql_dsn)
    assert len(stored) == 2


def test_migrate_dry_run_writes_nothing(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    now = datetime(2026, 1, 2, 3, 4, 5)
    sqlite_repo = _create_sqlite_repository(
        tmp_path,
        [snapshot_record("snap-dry", "sbx-001", now, SnapshotState.READY)],
    )
    sqlite_repo.close()

    result = migrate_sqlite_snapshots_to_postgresql(
        tmp_path / "opensandbox.db", postgresql_dsn, dry_run=True
    )

    assert result.total == 1
    assert result.migrated == 1
    assert result.skipped == 0
    assert result.dry_run is True
    assert _records_from_postgresql(postgresql_dsn) == []


def test_migrate_empty_sqlite_database(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    sqlite_repo = SQLiteSnapshotRepository(tmp_path / "opensandbox.db")
    sqlite_repo.close()

    result = migrate_sqlite_snapshots_to_postgresql(
        tmp_path / "opensandbox.db", postgresql_dsn
    )

    assert result.total == 0
    assert result.migrated == 0
    assert result.skipped == 0
    assert _records_from_postgresql(postgresql_dsn) == []


def test_migrate_handles_more_than_one_page(tmp_path, postgresql_dsn: str) -> None:
    _truncate_postgresql(postgresql_dsn)
    now = datetime(2026, 1, 2, 3, 4, 5)
    records = [
        snapshot_record(f"snap-{index:04d}", f"sbx-{index:04d}", now, SnapshotState.READY)
        for index in range(120)
    ]
    sqlite_repo = _create_sqlite_repository(tmp_path, records)
    sqlite_repo.close()

    result = migrate_sqlite_snapshots_to_postgresql(
        tmp_path / "opensandbox.db", postgresql_dsn
    )

    assert result.total == 120
    assert result.migrated == 120
    assert result.skipped == 0
    assert len(_records_from_postgresql(postgresql_dsn, limit=200)) == 120


def test_migrate_dry_run_does_not_create_postgresql_schema(
    tmp_path, postgresql_dsn: str
) -> None:
    with psycopg.connect(postgresql_dsn) as conn:
        conn.execute("DROP TABLE IF EXISTS snapshots")
    sqlite_repo = _create_sqlite_repository(
        tmp_path,
        [snapshot_record("snap-dry", "sbx-001", datetime(2026, 1, 2, 3, 4, 5))],
    )
    sqlite_repo.close()

    result = migrate_sqlite_snapshots_to_postgresql(
        tmp_path / "opensandbox.db", postgresql_dsn, dry_run=True
    )

    assert result.total == 1
    assert result.migrated == 1
    assert result.skipped == 0
    assert result.dry_run is True
    with psycopg.connect(postgresql_dsn) as conn:
        row = conn.execute("SELECT to_regclass('snapshots')").fetchone()
    assert row is None or row[0] is None


def test_migrate_reads_old_sqlite_schema_without_modifying_it(
    tmp_path, postgresql_dsn: str
) -> None:
    _truncate_postgresql(postgresql_dsn)
    db_path = tmp_path / "legacy.db"
    conn = sqlite3.connect(db_path)
    conn.execute(
        """
        CREATE TABLE snapshots (
            id TEXT PRIMARY KEY,
            source_sandbox_id TEXT NOT NULL,
            name TEXT,
            description TEXT,
            restore_config TEXT NOT NULL,
            state TEXT NOT NULL,
            reason TEXT,
            message TEXT,
            last_transition_at TEXT,
            created_at TEXT NOT NULL,
            updated_at TEXT NOT NULL
        )
        """
    )
    conn.execute(
        """
        INSERT INTO snapshots (
            id, source_sandbox_id, name, description, restore_config, state,
            reason, message, last_transition_at, created_at, updated_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            "snap-legacy",
            "sbx-legacy",
            "name-snap-legacy",
            "description-snap-legacy",
            json.dumps({"image": "registry.example.com/snapshots/snap-legacy:latest"}),
            "Ready",
            "reason-snap-legacy",
            "message-snap-legacy",
            "2026-01-02T03:04:05",
            "2026-01-02T03:04:05",
            "2026-01-02T03:04:05",
        ),
    )
    conn.commit()
    conn.close()

    result = migrate_sqlite_snapshots_to_postgresql(db_path, postgresql_dsn)

    assert result.total == 1
    assert result.migrated == 1
    conn = sqlite3.connect(db_path)
    try:
        columns = {
            row[1]
            for row in conn.execute("PRAGMA table_info(snapshots)").fetchall()
        }
    finally:
        conn.close()
    assert "namespace" not in columns
    stored = _records_from_postgresql(postgresql_dsn)
    assert len(stored) == 1
    legacy = stored[0]
    assert legacy.id == "snap-legacy"
    assert legacy.namespace is None
    assert legacy.status.state == SnapshotState.READY
    assert legacy.restore_config.image == "registry.example.com/snapshots/snap-legacy:latest"


def test_migrate_requires_existing_sqlite_file(tmp_path) -> None:
    with pytest.raises(FileNotFoundError):
        migrate_sqlite_snapshots_to_postgresql(
            tmp_path / "missing.db", "postgresql://user:pass@localhost:5432/db"
        )


def test_migrate_snapshots_cli_parser_accepts_arguments() -> None:
    parser = _build_parser()
    args = parser.parse_args(
        [
            "migrate-snapshots",
            "--from",
            "/tmp/source.db",
            "--to",
            "postgresql://user:pass@localhost:5432/db",
            "--dry-run",
        ]
    )
    assert args.command == "migrate-snapshots"
    assert args.sqlite_path == "/tmp/source.db"
    assert args.postgresql_dsn == "postgresql://user:pass@localhost:5432/db"
    assert args.dry_run is True


def test_migrate_snapshots_cli_cold_start(tmp_path, postgresql_dsn: str) -> None:
    with psycopg.connect(postgresql_dsn) as conn:
        conn.execute("DROP TABLE IF EXISTS snapshots")
    sqlite_repo = SQLiteSnapshotRepository(tmp_path / "opensandbox.db")
    sqlite_repo.close()

    result = subprocess.run(
        [
            sys.executable,
            "-m",
            "opensandbox_server.cli",
            "migrate-snapshots",
            "--from",
            str(tmp_path / "opensandbox.db"),
            "--to",
            postgresql_dsn,
        ],
        cwd=Path(__file__).parents[1],
        capture_output=True,
        text=True,
        check=False,
    )

    assert result.returncode == 0, result.stderr
    assert "Snapshots migrated: total=0, migrated=0, skipped=0" in result.stdout
    assert "partially initialized module" not in result.stdout + result.stderr
