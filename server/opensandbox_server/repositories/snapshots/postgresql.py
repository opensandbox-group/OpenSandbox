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

"""PostgreSQL-backed snapshot repository."""

from __future__ import annotations

from collections.abc import Mapping
from datetime import datetime, timezone
from typing import Any, overload

from psycopg import sql
from psycopg.rows import dict_row
from psycopg.types.json import Jsonb
from psycopg_pool import ConnectionPool

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

_SCHEMA_LOCK_NAME = "opensandbox-server-snapshot-schema"

_SELECT_COLUMNS = """
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
"""

_UPDATE_COLUMNS = """
    source_sandbox_id = %(source_sandbox_id)s,
    namespace = %(namespace)s,
    name = %(name)s,
    description = %(description)s,
    restore_config = %(restore_config)s,
    state = %(state)s,
    reason = %(reason)s,
    message = %(message)s,
    last_transition_at = %(last_transition_at)s,
    created_at = %(created_at)s,
    updated_at = %(updated_at)s
"""


class PostgreSQLSnapshotRepository:
    """Connection-pooled PostgreSQL repository for persisted snapshot records."""

    def __init__(
        self,
        dsn: str,
        *,
        min_pool_size: int = 1,
        max_pool_size: int = 10,
        connect_timeout_seconds: int = 5,
        pool_timeout_seconds: float = 5.0,
    ) -> None:
        self._pool = ConnectionPool(
            conninfo=dsn,
            min_size=min_pool_size,
            max_size=max_pool_size,
            timeout=pool_timeout_seconds,
            kwargs={
                "connect_timeout": connect_timeout_seconds,
                "row_factory": dict_row,
            },
            open=False,
        )
        try:
            self._pool.open(wait=True, timeout=connect_timeout_seconds)
            self._initialize_schema()
        except BaseException:
            self._pool.close()
            raise

    def create(self, record: SnapshotRecord) -> SnapshotRecord:
        with self._pool.connection() as conn:
            conn.execute(
                """
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
                """,
                self._to_db_params(record),
            )
        return record

    def get(self, snapshot_id: str) -> SnapshotRecord | None:
        with self._pool.connection() as conn:
            row = conn.execute(
                f"SELECT {_SELECT_COLUMNS} FROM snapshots WHERE id = %s",
                (snapshot_id,),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def list(self, query: SnapshotListQuery) -> SnapshotListResult:
        clauses: list[sql.SQL] = []
        params: dict[str, Any] = {}

        if query.namespace is not None:
            clauses.append(sql.SQL("namespace = %(namespace)s"))
            params["namespace"] = query.namespace
        if query.source_sandbox_id:
            clauses.append(sql.SQL("source_sandbox_id = %(source_sandbox_id)s"))
            params["source_sandbox_id"] = query.source_sandbox_id
        if query.name is not None:
            clauses.append(sql.SQL("name = %(name)s"))
            params["name"] = query.name
        if query.states:
            clauses.append(sql.SQL("state = ANY(%(states)s)"))
            params["states"] = query.states

        where_sql = (
            sql.SQL("WHERE {}").format(sql.SQL(" AND ").join(clauses)) if clauses else sql.SQL("")
        )
        page = max(query.page, 1)
        page_size = max(query.page_size, 1)
        params["page_size"] = page_size
        params["offset"] = (page - 1) * page_size

        with self._pool.connection() as conn:
            total_row = conn.execute(
                sql.SQL("SELECT COUNT(*) AS total_items FROM snapshots {}").format(where_sql),
                params,
            ).fetchone()
            rows = conn.execute(
                sql.SQL("""
                SELECT {}
                FROM snapshots
                {}
                ORDER BY created_at DESC, id DESC
                LIMIT %(page_size)s OFFSET %(offset)s
                """).format(sql.SQL(_SELECT_COLUMNS), where_sql),
                params,
            ).fetchall()

        return SnapshotListResult(
            items=[self._row_to_record(row) for row in rows],
            total_items=int(total_row["total_items"]) if total_row is not None else 0,
        )

    def update(self, record: SnapshotRecord) -> SnapshotRecord:
        with self._pool.connection() as conn:
            conn.execute(
                f"""
                UPDATE snapshots
                SET {_UPDATE_COLUMNS}
                WHERE id = %(id)s
                """,
                self._to_db_params(record),
            )
        return record

    def update_if_state(
        self,
        record: SnapshotRecord,
        expected_state: SnapshotState,
    ) -> bool:
        params = self._to_db_params(record)
        params["expected_state"] = expected_state.value
        with self._pool.connection() as conn:
            row = conn.execute(
                f"""
                UPDATE snapshots
                SET {_UPDATE_COLUMNS}
                WHERE id = %(id)s AND state = %(expected_state)s
                RETURNING id
                """,
                params,
            ).fetchone()
        return row is not None

    def delete(self, snapshot_id: str) -> None:
        with self._pool.connection() as conn:
            conn.execute("DELETE FROM snapshots WHERE id = %s", (snapshot_id,))

    def close(self) -> None:
        self._pool.close()

    def _initialize_schema(self) -> None:
        with self._pool.connection() as conn:
            conn.execute(
                "SELECT pg_advisory_xact_lock(hashtext(%s))",
                (_SCHEMA_LOCK_NAME,),
            )
            conn.execute(
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
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_snapshots_source_sandbox_id
                    ON snapshots(source_sandbox_id)
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_snapshots_state
                    ON snapshots(state)
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_snapshots_created_at
                    ON snapshots(created_at DESC)
                """
            )
            conn.execute(
                """
                CREATE INDEX IF NOT EXISTS idx_snapshots_name_namespace
                    ON snapshots(name, namespace)
                """
            )

    @staticmethod
    def _to_db_params(record: SnapshotRecord) -> dict[str, Any]:
        return {
            "id": record.id,
            "source_sandbox_id": record.source_sandbox_id,
            "namespace": record.namespace,
            "name": record.name,
            "description": record.description,
            "restore_config": Jsonb(record.restore_config.to_dict()),
            "state": record.status.state.value,
            "reason": record.status.reason,
            "message": record.status.message,
            "last_transition_at": PostgreSQLSnapshotRepository._normalize_datetime(
                record.status.last_transition_at
            ),
            "created_at": PostgreSQLSnapshotRepository._normalize_datetime(record.created_at),
            "updated_at": PostgreSQLSnapshotRepository._normalize_datetime(record.updated_at),
        }

    @staticmethod
    @overload
    def _normalize_datetime(value: None) -> None: ...

    @staticmethod
    @overload
    def _normalize_datetime(value: datetime) -> datetime: ...

    @staticmethod
    def _normalize_datetime(value: datetime | None) -> datetime | None:
        if value is None:
            return None
        if value.tzinfo is None:
            return value.replace(tzinfo=timezone.utc)
        return value.astimezone(timezone.utc)

    @staticmethod
    def _row_to_record(row: Mapping[str, Any]) -> SnapshotRecord:
        restore_config = row["restore_config"]
        return SnapshotRecord(
            id=row["id"],
            source_sandbox_id=row["source_sandbox_id"],
            namespace=row["namespace"],
            name=row["name"],
            description=row["description"],
            restore_config=SnapshotRestoreConfig.from_dict(restore_config),
            status=SnapshotStatusRecord(
                state=SnapshotState(row["state"]),
                reason=row["reason"],
                message=row["message"],
                last_transition_at=PostgreSQLSnapshotRepository._normalize_datetime(
                    row["last_transition_at"]
                ),
            ),
            created_at=PostgreSQLSnapshotRepository._normalize_datetime(row["created_at"]),
            updated_at=PostgreSQLSnapshotRepository._normalize_datetime(row["updated_at"]),
        )


__all__ = [
    "PostgreSQLSnapshotRepository",
]
