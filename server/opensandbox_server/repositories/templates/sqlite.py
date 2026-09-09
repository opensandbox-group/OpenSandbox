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
SQLite-backed fsb template repository.

The store is the source of truth for the public template catalog; the
SandboxTemplate CRD is only the execution projection, so losing this table
means re-creating templates, not losing running sandboxes.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from pathlib import Path
import sqlite3
import uuid

from opensandbox_server.services.templates.template_models import (
    FsbTemplateListQuery,
    FsbTemplateListResult,
    FsbTemplatePhase,
    FsbTemplateRecord,
)

SQLITE_BUSY_TIMEOUT_MS = 5000


class SQLiteFsbTemplateRepository:
    """File-backed repository for persisted fsb template rows."""

    def __init__(self, db_path: str | Path) -> None:
        self._db_path = Path(db_path).expanduser()
        self._db_path.parent.mkdir(parents=True, exist_ok=True)
        self._initialize_schema()

    @property
    def db_path(self) -> Path:
        return self._db_path

    def create(self, record: FsbTemplateRecord) -> FsbTemplateRecord:
        with self._connect() as conn:
            conn.execute(
                """
                INSERT INTO templates (
                    template_id, namespace, crd_name, spec_json, metadata_json,
                    source_image, publish, format, phase, manifest_ref, message,
                    created_at, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                self._to_db_tuple(record),
            )
        return record

    def get(self, template_id: str, namespace: str) -> FsbTemplateRecord | None:
        with self._connect() as conn:
            row = conn.execute(
                self._select_sql() + " WHERE template_id = ? AND namespace = ?",
                (template_id, namespace),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def get_by_crd_name(self, namespace: str, crd_name: str) -> FsbTemplateRecord | None:
        with self._connect() as conn:
            row = conn.execute(
                self._select_sql() + " WHERE namespace = ? AND crd_name = ?",
                (namespace, crd_name),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def list(self, query: FsbTemplateListQuery) -> FsbTemplateListResult:
        clauses = ["namespace = ?"]
        params: list[object] = [query.namespace]
        if query.metadata:
            # AND semantics over json_extract; correctness over index speed at
            # the expected template-catalog scale. Keys are quoted inside the
            # path so label keys containing dots or slashes (e.g.
            # app.example.com/team) stay literal instead of nesting.
            for key, value in query.metadata.items():
                clauses.append("json_extract(metadata_json, ?) = ?")
                params.extend([f'$."{key}"', value])
        where_clause = f"WHERE {' AND '.join(clauses)}"
        page = max(query.page, 1)
        page_size = max(query.page_size, 1)
        offset = (page - 1) * page_size

        with self._connect() as conn:
            total_items = conn.execute(
                f"SELECT COUNT(*) FROM templates {where_clause}",
                tuple(params),
            ).fetchone()[0]
            rows = conn.execute(
                self._select_sql()
                + f" {where_clause} ORDER BY created_at DESC, template_id DESC LIMIT ? OFFSET ?",
                tuple([*params, page_size, offset]),
            ).fetchall()
        return FsbTemplateListResult(
            items=[self._row_to_record(row) for row in rows],
            total_items=total_items,
        )

    def namespaces(self) -> list[str]:
        """Distinct namespaces that own template rows (watch reactor scope)."""
        with self._connect() as conn:
            rows = conn.execute(
                "SELECT DISTINCT namespace FROM templates ORDER BY namespace"
            ).fetchall()
        return [row["namespace"] for row in rows]

    def update_status(
        self,
        template_id: str,
        namespace: str,
        *,
        phase: FsbTemplatePhase,
        manifest_ref: str | None,
        message: str | None,
    ) -> bool:
        with self._connect() as conn:
            cursor = conn.execute(
                """
                UPDATE templates
                SET phase = ?, manifest_ref = ?, message = ?, updated_at = ?
                WHERE template_id = ? AND namespace = ?
                """,
                (
                    phase.value,
                    manifest_ref,
                    message,
                    _now_iso(),
                    template_id,
                    namespace,
                ),
            )
            return cursor.rowcount == 1

    def delete(self, template_id: str, namespace: str) -> None:
        with self._connect() as conn:
            conn.execute(
                "DELETE FROM templates WHERE template_id = ? AND namespace = ?",
                (template_id, namespace),
            )

    def close(self) -> None:
        """SQLite connections are scoped to individual operations."""

    def _initialize_schema(self) -> None:
        with self._connect() as conn:
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS templates (
                    template_id      TEXT PRIMARY KEY,
                    namespace        TEXT NOT NULL,
                    crd_name         TEXT NOT NULL,
                    spec_json        TEXT NOT NULL,
                    metadata_json    TEXT NOT NULL DEFAULT '{}',
                    source_image     TEXT NOT NULL,
                    publish          TEXT NOT NULL,
                    format           TEXT NOT NULL DEFAULT 'overlaybd',
                    phase            TEXT NOT NULL,
                    manifest_ref     TEXT,
                    message          TEXT,
                    created_at       TEXT NOT NULL,
                    updated_at       TEXT NOT NULL,
                    UNIQUE (namespace, crd_name)
                );

                CREATE INDEX IF NOT EXISTS idx_templates_namespace
                    ON templates(namespace);

                CREATE INDEX IF NOT EXISTS idx_templates_phase
                    ON templates(phase);

                CREATE INDEX IF NOT EXISTS idx_templates_created_at
                    ON templates(created_at DESC);
                """
            )

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self._db_path)
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute(f"PRAGMA busy_timeout={SQLITE_BUSY_TIMEOUT_MS}")
        conn.row_factory = sqlite3.Row
        return conn

    @staticmethod
    def _select_sql() -> str:
        return """
            SELECT
                template_id, namespace, crd_name, spec_json, metadata_json,
                source_image, publish, format, phase, manifest_ref, message,
                created_at, updated_at
            FROM templates
        """

    @staticmethod
    def _to_db_tuple(record: FsbTemplateRecord) -> tuple[object, ...]:
        now = _now_iso()
        return (
            record.template_id,
            record.namespace,
            record.crd_name,
            json.dumps(record.spec, sort_keys=True),
            json.dumps(record.metadata, sort_keys=True),
            record.source_image,
            record.publish,
            str(record.spec.get("format", "overlaybd")),
            record.phase.value,
            record.manifest_ref,
            record.message,
            _iso(record.created_at) or now,
            _iso(record.updated_at) or now,
        )

    @staticmethod
    def _row_to_record(row: sqlite3.Row) -> FsbTemplateRecord:
        spec = json.loads(row["spec_json"])
        spec["format"] = row["format"]
        return FsbTemplateRecord(
            template_id=row["template_id"],
            namespace=row["namespace"],
            crd_name=row["crd_name"],
            spec=spec,
            metadata=json.loads(row["metadata_json"]),
            phase=FsbTemplatePhase(row["phase"]),
            manifest_ref=row["manifest_ref"],
            message=row["message"],
            created_at=_parse(row["created_at"]),
            updated_at=_parse(row["updated_at"]),
        )


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _iso(value: datetime | None) -> str | None:
    return value.isoformat() if value is not None else None


def _parse(value: str | None) -> datetime | None:
    return datetime.fromisoformat(value) if value is not None else None


def generate_template_id() -> str:
    """Public template ID: ``tpl_<uuid>``; never reused after deletion."""
    return f"tpl-{uuid.uuid4()}"


__all__ = [
    "SQLITE_BUSY_TIMEOUT_MS",
    "SQLiteFsbTemplateRepository",
    "generate_template_id",
]
