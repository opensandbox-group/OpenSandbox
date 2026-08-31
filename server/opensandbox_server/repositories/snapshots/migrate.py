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

"""
One-shot migration of snapshot records from SQLite to PostgreSQL.

The PostgreSQL backend added in the snapshot store feature is opt-in, and it
does not read existing SQLite databases. Operators who switch a running server
from ``store.type = "sqlite"`` to ``store.type = "postgresql"`` use this module
to copy the persisted snapshot catalog before restarting the server.

The source SQLite database is opened read-only and its schema is never
modified. Dry runs only inspect the target: the PostgreSQL schema is created
only on a real migration run.
"""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

DEFAULT_SQLITE_SNAPSHOT_PATH = Path.home() / ".opensandbox" / "opensandbox.db"

_SCHEMA_LOCK_NAME = "opensandbox-server-snapshot-schema"

_SNAPSHOT_TABLE_COLUMNS = (
    "id",
    "source_sandbox_id",
    "namespace",
    "name",
    "description",
    "restore_config",
    "state",
    "reason",
    "message",
    "last_transition_at",
    "created_at",
    "updated_at",
)

_CREATE_SCHEMA_STATEMENTS = (
    """
    CREATE TABLE IF NOT EXISTS snapshots (
        id TEXT PRIMARY KEY,
        source_sandbox_id TEXT NOT NULL,
        namespace TEXT DEFAULT NULL,
        name TEXT,
        description TEXT,
        restore_config JSONB NOT NULL,
        state TEXT NOT NULL,
        reason TEXT,
        message TEXT,
        last_transition_at TIMESTAMPTZ,
        created_at TIMESTAMPTZ NOT NULL,
        updated_at TIMESTAMPTZ NOT NULL
    )
    """,
    "CREATE INDEX IF NOT EXISTS idx_snapshots_source_sandbox_id ON snapshots(source_sandbox_id)",
    "CREATE INDEX IF NOT EXISTS idx_snapshots_state ON snapshots(state)",
    "CREATE INDEX IF NOT EXISTS idx_snapshots_created_at ON snapshots(created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_snapshots_name_namespace ON snapshots(name, namespace)",
)

_INSERT_SNAPSHOT = """
    INSERT INTO snapshots (
        id,
        source_sandbox_id,
        namespace,
        name,
        description,
        restore_config,
        state,
        reason,
        message,
        last_transition_at,
        created_at,
        updated_at
    ) VALUES (
        %(id)s,
        %(source_sandbox_id)s,
        %(namespace)s,
        %(name)s,
        %(description)s,
        %(restore_config)s,
        %(state)s,
        %(reason)s,
        %(message)s,
        %(last_transition_at)s,
        %(created_at)s,
        %(updated_at)s
    )
    ON CONFLICT (id) DO NOTHING
    RETURNING id
"""


@dataclass(slots=True)
class SnapshotMigrationResult:
    """Counts for a completed SQLite-to-PostgreSQL migration run."""

    total: int
    migrated: int
    skipped: int
    dry_run: bool


def migrate_sqlite_snapshots_to_postgresql(
    sqlite_path: str | Path,
    postgresql_dsn: str,
    *,
    dry_run: bool = False,
) -> SnapshotMigrationResult:
    """
    Copy snapshot records from a SQLite database into PostgreSQL.

    The source SQLite database is opened read-only and is never modified.
    The PostgreSQL schema is created only when a real migration run needs it;
    a dry run inspects the target without creating or altering anything.
    Records whose id already exists in PostgreSQL are skipped, so the command
    can be re-run safely.

    Args:
        sqlite_path: Path to the source SQLite database file.
        postgresql_dsn: PostgreSQL connection string for the target database.
        dry_run: Report what would be migrated without writing anything.

    Returns:
        A SnapshotMigrationResult with the record counts.

    Raises:
        FileNotFoundError: If the SQLite database file does not exist.
    """
    source_path = Path(sqlite_path).expanduser()
    if not source_path.is_file():
        raise FileNotFoundError(f"SQLite snapshot database not found: {source_path}")

    records = _read_sqlite_snapshots_read_only(source_path)
    if dry_run:
        existing_ids = _read_postgresql_snapshot_ids(postgresql_dsn)
        migrated = sum(1 for record in records if record["id"] not in existing_ids)
    else:
        migrated = _write_postgresql_snapshots(postgresql_dsn, records)

    return SnapshotMigrationResult(
        total=len(records),
        migrated=migrated,
        skipped=len(records) - migrated,
        dry_run=dry_run,
    )


def _read_sqlite_snapshots_read_only(db_path: Path) -> list[dict[str, Any]]:
    """
    Read every snapshot row without initializing or modifying the schema.

    The database is opened with SQLite's read-only URI mode so a backup on a
    read-only mount can be migrated, and databases that predate the namespace
    column are read with the missing column defaulting to None.
    """
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    conn.row_factory = sqlite3.Row
    try:
        table_info = conn.execute("PRAGMA table_info(snapshots)").fetchall()
        columns = {row["name"] for row in table_info}
        if not columns:
            return []
        selected = [column for column in _SNAPSHOT_TABLE_COLUMNS if column in columns]
        rows = conn.execute(
            f"SELECT {', '.join(selected)} FROM snapshots ORDER BY created_at DESC, id DESC"
        ).fetchall()
        records: list[dict[str, Any]] = []
        for row in rows:
            values = {column: row[column] for column in selected}
            for column in _SNAPSHOT_TABLE_COLUMNS:
                values.setdefault(column, None)
            records.append(values)
        return records
    finally:
        conn.close()


def _read_postgresql_snapshot_ids(dsn: str) -> set[str]:
    """Return existing target ids without creating or altering the schema."""
    import psycopg

    with psycopg.connect(dsn) as conn:
        row = conn.execute("SELECT to_regclass('snapshots')").fetchone()
        if row is None or row[0] is None:
            return set()
        ids = conn.execute("SELECT id FROM snapshots").fetchall()
        return {item[0] for item in ids}


def _write_postgresql_snapshots(dsn: str, records: list[dict[str, Any]]) -> int:
    """Create the target schema and insert records in one transaction."""
    import psycopg
    from psycopg.types.json import Jsonb

    migrated = 0
    with psycopg.connect(dsn) as conn:
        conn.execute(
            "SELECT pg_advisory_xact_lock(hashtext(%s))",
            (_SCHEMA_LOCK_NAME,),
        )
        for statement in _CREATE_SCHEMA_STATEMENTS:
            conn.execute(statement)
        for record in records:
            params = {
                **record,
                "restore_config": Jsonb(json.loads(record["restore_config"])),
                "last_transition_at": _normalize_datetime(record["last_transition_at"]),
                "created_at": _require_datetime(record["created_at"]),
                "updated_at": _require_datetime(record["updated_at"]),
            }
            if conn.execute(_INSERT_SNAPSHOT, params).fetchone() is not None:
                migrated += 1
    return migrated


def _normalize_datetime(value: str | None) -> datetime | None:
    if value is None:
        return None
    result = datetime.fromisoformat(value)
    if result.tzinfo is None:
        return result.replace(tzinfo=timezone.utc)
    return result.astimezone(timezone.utc)


def _require_datetime(value: str | None) -> datetime:
    result = _normalize_datetime(value)
    if result is None:
        raise ValueError("snapshot row is missing a required timestamp")
    return result


__all__ = [
    "DEFAULT_SQLITE_SNAPSHOT_PATH",
    "SnapshotMigrationResult",
    "migrate_sqlite_snapshots_to_postgresql",
]
