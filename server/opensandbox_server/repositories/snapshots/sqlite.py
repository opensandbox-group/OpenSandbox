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
SQLite-backed snapshot repository.
"""

from __future__ import annotations

import json
from pathlib import Path
import sqlite3

from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_repository import (
    SnapshotListQuery,
    SnapshotListResult,
)

SQLITE_BUSY_TIMEOUT_MS = 5000


class SQLiteSnapshotRepository:
    """
    File-backed repository for persisted snapshot records.
    """

    def __init__(self, db_path: str | Path) -> None:
        self._db_path = Path(db_path).expanduser()
        self._db_path.parent.mkdir(parents=True, exist_ok=True)
        self._initialize_schema()

    @property
    def db_path(self) -> Path:
        return self._db_path

    def create(self, record: SnapshotRecord) -> SnapshotRecord:
        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO snapshots (
                    id,
                    source_sandbox_id,
                    name,
                    description,
                    restore_config,
                    state,
                    reason,
                    message,
                    last_transition_at,
                    created_at,
                    updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                self._to_db_tuple(record),
            )
        return record

    def get(self, snapshot_id: str) -> SnapshotRecord | None:
        with self._connect() as conn:
            row = conn.execute(
                """
                SELECT
                    id,
                    source_sandbox_id,
                    name,
                    description,
                    restore_config,
                    state,
                    reason,
                    message,
                    last_transition_at,
                    created_at,
                    updated_at
                FROM snapshots
                WHERE id = ?
                """,
                (snapshot_id,),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def list(self, query: SnapshotListQuery) -> SnapshotListResult:
        clauses: list[str] = []
        params: list[object] = []

        if query.source_sandbox_id:
            clauses.append("source_sandbox_id = ?")
            params.append(query.source_sandbox_id)

        if query.states:
            clauses.append(
                f"state IN ({', '.join('?' for _ in query.states)})"
            )
            params.extend(query.states)

        where_clause = f"WHERE {' AND '.join(clauses)}" if clauses else ""
        page = max(query.page, 1)
        page_size = max(query.page_size, 1)
        offset = (page - 1) * page_size

        with self._connect() as conn:
            total_items = conn.execute(
                f"SELECT COUNT(*) FROM snapshots {where_clause}",
                tuple(params),
            ).fetchone()[0]
            rows = conn.execute(
                f"""
                SELECT
                    id,
                    source_sandbox_id,
                    name,
                    description,
                    restore_config,
                    state,
                    reason,
                    message,
                    last_transition_at,
                    created_at,
                    updated_at
                FROM snapshots
                {where_clause}
                ORDER BY created_at DESC, id DESC
                LIMIT ? OFFSET ?
                """,
                tuple([*params, page_size, offset]),
            ).fetchall()

        return SnapshotListResult(
            items=[self._row_to_record(row) for row in rows],
            total_items=total_items,
        )

    def update(self, record: SnapshotRecord) -> SnapshotRecord:
        with self._connect() as conn:
            conn.execute(
                """
                UPDATE snapshots
                SET
                    source_sandbox_id = ?,
                    name = ?,
                    description = ?,
                    restore_config = ?,
                    state = ?,
                    reason = ?,
                    message = ?,
                    last_transition_at = ?,
                    created_at = ?,
                    updated_at = ?
                WHERE id = ?
                """,
                (
                    record.source_sandbox_id,
                    record.name,
                    record.description,
                    json.dumps(self._restore_config_to_dict(record.restore_config), sort_keys=True),
                    record.status.state.value,
                    record.status.reason,
                    record.status.message,
                    self._datetime_to_str(record.status.last_transition_at),
                    self._datetime_to_str(record.created_at),
                    self._datetime_to_str(record.updated_at),
                    record.id,
                ),
            )
        return record

    def update_if_state(
        self,
        record: SnapshotRecord,
        expected_state: SnapshotState,
    ) -> bool:
        with self._connect() as conn:
            cursor = conn.execute(
                """
                UPDATE snapshots
                SET
                    source_sandbox_id = ?,
                    name = ?,
                    description = ?,
                    restore_config = ?,
                    state = ?,
                    reason = ?,
                    message = ?,
                    last_transition_at = ?,
                    created_at = ?,
                    updated_at = ?
                WHERE id = ? AND state = ?
                """,
                (
                    record.source_sandbox_id,
                    record.name,
                    record.description,
                    json.dumps(self._restore_config_to_dict(record.restore_config), sort_keys=True),
                    record.status.state.value,
                    record.status.reason,
                    record.status.message,
                    self._datetime_to_str(record.status.last_transition_at),
                    self._datetime_to_str(record.created_at),
                    self._datetime_to_str(record.updated_at),
                    record.id,
                    expected_state.value,
                ),
            )
            return cursor.rowcount == 1

    def delete(self, snapshot_id: str) -> None:
        with self._connect() as conn:
            conn.execute("DELETE FROM snapshots WHERE id = ?", (snapshot_id,))

    def _initialize_schema(self) -> None:
        with self._connect() as conn:
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS snapshots (
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
                );

                CREATE INDEX IF NOT EXISTS idx_snapshots_source_sandbox_id
                    ON snapshots(source_sandbox_id);

                CREATE INDEX IF NOT EXISTS idx_snapshots_state
                    ON snapshots(state);

                CREATE INDEX IF NOT EXISTS idx_snapshots_created_at
                    ON snapshots(created_at DESC);
                """
            )

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self._db_path)
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute(f"PRAGMA busy_timeout={SQLITE_BUSY_TIMEOUT_MS}")
        conn.row_factory = sqlite3.Row
        return conn

    def _to_db_tuple(self, record: SnapshotRecord) -> tuple[object, ...]:
        return (
            record.id,
            record.source_sandbox_id,
            record.name,
            record.description,
            json.dumps(self._restore_config_to_dict(record.restore_config), sort_keys=True),
            record.status.state.value,
            record.status.reason,
            record.status.message,
            self._datetime_to_str(record.status.last_transition_at),
            self._datetime_to_str(record.created_at),
            self._datetime_to_str(record.updated_at),
        )

    @staticmethod
    def _restore_config_to_dict(config: SnapshotRestoreConfig) -> dict[str, str | None]:
        return {
            "image": config.image,
        }

    @staticmethod
    def _datetime_to_str(value) -> str | None:
        return value.isoformat() if value is not None else None

    @staticmethod
    def _row_to_record(row: sqlite3.Row) -> SnapshotRecord:
        restore_config = json.loads(row["restore_config"])
        return SnapshotRecord(
            id=row["id"],
            source_sandbox_id=row["source_sandbox_id"],
            name=row["name"],
            description=row["description"],
            restore_config=SnapshotRestoreConfig(
                image=restore_config.get("image"),
            ),
            status=SnapshotStatusRecord(
                state=SnapshotState(row["state"]),
                reason=row["reason"],
                message=row["message"],
                last_transition_at=SQLiteSnapshotRepository._str_to_datetime(row["last_transition_at"]),
            ),
            created_at=SQLiteSnapshotRepository._str_to_datetime(row["created_at"]),
            updated_at=SQLiteSnapshotRepository._str_to_datetime(row["updated_at"]),
        )

    @staticmethod
    def _str_to_datetime(value: str | None):
        from datetime import datetime

        return datetime.fromisoformat(value) if value is not None else None


__all__ = [
    "SQLiteSnapshotRepository",
    "SQLITE_BUSY_TIMEOUT_MS",
]
