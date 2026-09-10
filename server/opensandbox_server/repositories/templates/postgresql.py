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

"""PostgreSQL-backed fsb template repository."""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from psycopg import sql
from psycopg.rows import dict_row
from psycopg.types.json import Jsonb
from psycopg_pool import ConnectionPool

from opensandbox_server.services.templates.template_models import (
    FsbTemplateListQuery,
    FsbTemplateListResult,
    FsbTemplatePhase,
    FsbTemplateRecord,
)

_SCHEMA_LOCK_NAME = "opensandbox-server-fsb-template-schema"

_SELECT_COLUMNS = """
    template_id,
    namespace,
    crd_name,
    spec_json,
    metadata_json,
    source_image,
    publish,
    format,
    phase,
    manifest_ref,
    message,
    created_at,
    updated_at
"""


class PostgreSQLFsbTemplateRepository:
    """Connection-pooled PostgreSQL repository for fsb template rows."""

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

    def create(self, record: FsbTemplateRecord) -> FsbTemplateRecord:
        with self._pool.connection() as conn:
            conn.execute(
                """
                INSERT INTO templates (
                    template_id, namespace, crd_name, spec_json, metadata_json,
                    source_image, publish, format, phase, manifest_ref, message,
                    created_at, updated_at
                ) VALUES (
                    %(template_id)s, %(namespace)s, %(crd_name)s, %(spec_json)s,
                    %(metadata_json)s, %(source_image)s, %(publish)s, %(format)s,
                    %(phase)s, %(manifest_ref)s, %(message)s,
                    %(created_at)s, %(updated_at)s
                )
                """,
                self._to_db_params(record),
            )
        return record

    def get(self, template_id: str, namespace: str) -> FsbTemplateRecord | None:
        with self._pool.connection() as conn:
            row = conn.execute(
                f"SELECT {_SELECT_COLUMNS} FROM templates"
                " WHERE template_id = %s AND namespace = %s",
                (template_id, namespace),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def get_by_crd_name(self, namespace: str, crd_name: str) -> FsbTemplateRecord | None:
        with self._pool.connection() as conn:
            row = conn.execute(
                f"SELECT {_SELECT_COLUMNS} FROM templates"
                " WHERE namespace = %s AND crd_name = %s",
                (namespace, crd_name),
            ).fetchone()
        return self._row_to_record(row) if row is not None else None

    def list(self, query: FsbTemplateListQuery) -> FsbTemplateListResult:
        clauses: list[sql.SQL] = [sql.SQL("namespace = %(namespace)s")]
        params: dict[str, Any] = {"namespace": query.namespace}
        if query.metadata:
            # JSONB containment gives the AND semantics for the whole filter,
            # backed by the GIN index.
            clauses.append(sql.SQL("metadata_json @> %(metadata_filter)s"))
            params["metadata_filter"] = Jsonb(query.metadata)
        where_clause = sql.SQL(" AND ").join(clauses)
        page = max(query.page, 1)
        page_size = max(query.page_size, 1)
        offset = (page - 1) * page_size

        with self._pool.connection() as conn:
            total_items = conn.execute(
                sql.SQL("SELECT COUNT(*) FROM templates WHERE {}").format(where_clause),
                params,
            ).fetchone()["count"]
            rows = conn.execute(
                sql.SQL(
                    f"SELECT {_SELECT_COLUMNS} FROM templates"  # noqa: S608 - static column list
                    " WHERE {} ORDER BY created_at DESC, template_id DESC"
                    " LIMIT %(page_size)s OFFSET %(offset)s"
                ).format(where_clause),
                {**params, "page_size": page_size, "offset": offset},
            ).fetchall()
        return FsbTemplateListResult(
            items=[self._row_to_record(row) for row in rows],
            total_items=int(total_items),
        )

    def namespaces(self) -> list[str]:
        """Distinct namespaces that own template rows (watch reactor scope)."""
        with self._pool.connection() as conn:
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
        with self._pool.connection() as conn:
            rowcount = conn.execute(
                """
                UPDATE templates
                SET phase = %s, manifest_ref = %s, message = %s, updated_at = %s
                WHERE template_id = %s AND namespace = %s
                """,
                (
                    phase.value,
                    manifest_ref,
                    message,
                    datetime.now(timezone.utc),
                    template_id,
                    namespace,
                ),
            ).rowcount
        return rowcount == 1

    def delete(self, template_id: str, namespace: str) -> None:
        with self._pool.connection() as conn:
            conn.execute(
                "DELETE FROM templates WHERE template_id = %s AND namespace = %s",
                (template_id, namespace),
            )

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
                CREATE TABLE IF NOT EXISTS templates (
                    template_id      TEXT PRIMARY KEY,
                    namespace        TEXT NOT NULL,
                    crd_name         TEXT NOT NULL,
                    spec_json        JSONB NOT NULL,
                    metadata_json    JSONB NOT NULL DEFAULT '{}'::jsonb,
                    source_image     TEXT NOT NULL,
                    publish          TEXT NOT NULL,
                    format           TEXT NOT NULL DEFAULT 'overlaybd',
                    phase            TEXT NOT NULL,
                    manifest_ref     TEXT,
                    message          TEXT,
                    created_at       TIMESTAMPTZ NOT NULL,
                    updated_at       TIMESTAMPTZ NOT NULL,
                    UNIQUE (namespace, crd_name)
                )
                """
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_templates_namespace"
                " ON templates(namespace)"
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_templates_phase"
                " ON templates(phase)"
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_templates_created_at"
                " ON templates(created_at DESC)"
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_templates_spec_image"
                " ON templates((spec_json ->> 'image'))"
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS idx_templates_metadata"
                " ON templates USING GIN (metadata_json jsonb_path_ops)"
            )

    @staticmethod
    def _to_db_params(record: FsbTemplateRecord) -> dict[str, Any]:
        now = datetime.now(timezone.utc)
        return {
            "template_id": record.template_id,
            "namespace": record.namespace,
            "crd_name": record.crd_name,
            "spec_json": Jsonb(record.spec),
            "metadata_json": Jsonb(record.metadata),
            "source_image": record.source_image,
            "publish": record.publish,
            "format": str(record.spec.get("format", "overlaybd")),
            "phase": record.phase.value,
            "manifest_ref": record.manifest_ref,
            "message": record.message,
            "created_at": record.created_at or now,
            "updated_at": record.updated_at or now,
        }

    @staticmethod
    def _row_to_record(row: dict[str, Any]) -> FsbTemplateRecord:
        spec = dict(row["spec_json"])
        spec["format"] = row["format"]
        return FsbTemplateRecord(
            template_id=row["template_id"],
            namespace=row["namespace"],
            crd_name=row["crd_name"],
            spec=spec,
            metadata=dict(row["metadata_json"]),
            phase=FsbTemplatePhase(row["phase"]),
            manifest_ref=row["manifest_ref"],
            message=row["message"],
            created_at=row["created_at"],
            updated_at=row["updated_at"],
        )


__all__ = ["PostgreSQLFsbTemplateRepository"]
