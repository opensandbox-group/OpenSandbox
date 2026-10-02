# Copyright 2026 The OpenSandbox Authors
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

"""PostgreSQL sandbox_lifecycle_history (Server lifecycle writes)."""

from __future__ import annotations

from datetime import datetime, timezone

from psycopg.rows import dict_row
from psycopg_pool import ConnectionPool

_SCHEMA_LOCK_NAME = "opensandbox-server-lifecycle-history-schema"
_HISTORY_TABLE = "sandbox_lifecycle_history"


class PostgreSQLLifecycleAuditRepository:
    """Upsert lifecycle rows for sandboxes created via the Lifecycle API."""

    def __init__(
        self,
        dsn: str,
        *,
        min_pool_size: int = 1,
        max_pool_size: int = 5,
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

    def close(self) -> None:
        self._pool.close()

    def _initialize_schema(self) -> None:
        with self._pool.connection() as conn:
            conn.execute(
                "SELECT pg_advisory_xact_lock(hashtext(%s))",
                (_SCHEMA_LOCK_NAME,),
            )
            conn.execute(
                f"""
                CREATE TABLE IF NOT EXISTS {_HISTORY_TABLE} (
                    sandbox_id TEXT PRIMARY KEY,
                    tenant_name TEXT NOT NULL DEFAULT '',
                    namespace TEXT NOT NULL DEFAULT '',
                    state TEXT,
                    image_uri TEXT,
                    lifecycle_created_at TIMESTAMPTZ,
                    expires_at TIMESTAMPTZ,
                    ended_at TIMESTAMPTZ,
                    deleted_at TIMESTAMPTZ,
                    wall_clock_seconds INTEGER,
                    source TEXT NOT NULL DEFAULT 'server-lifecycle',
                    create_request JSONB,
                    cpu_limit TEXT,
                    memory_limit TEXT,
                    cpu_cores DOUBLE PRECISION,
                    memory_gi DOUBLE PRECISION,
                    first_recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
                    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
                )
                """
            )
            conn.execute(
                f"""
                CREATE INDEX IF NOT EXISTS idx_sandbox_lifecycle_history_tenant_created
                    ON {_HISTORY_TABLE} (tenant_name, lifecycle_created_at DESC NULLS LAST)
                """
            )
            conn.execute(
                f"ALTER TABLE {_HISTORY_TABLE} ADD COLUMN IF NOT EXISTS create_request JSONB"
            )

    def upsert_created(
        self,
        *,
        sandbox_id: str,
        tenant_name: str,
        namespace: str,
        state: str | None,
        image_uri: str | None,
        lifecycle_created_at: datetime | None,
        expires_at: datetime | None,
        cpu_limit: str | None = None,
        memory_limit: str | None = None,
        cpu_cores: float | None = None,
        memory_gi: float | None = None,
    ) -> None:
        now = datetime.now(timezone.utc)
        t = _HISTORY_TABLE
        with self._pool.connection() as conn:
            conn.execute(
                f"""
                INSERT INTO {t} (
                    sandbox_id, tenant_name, namespace, state, image_uri,
                    lifecycle_created_at, expires_at, source,
                    cpu_limit, memory_limit, cpu_cores, memory_gi,
                    first_recorded_at, last_seen_at
                ) VALUES (
                    %s, %s, %s, %s, %s,
                    %s, %s, 'server-lifecycle',
                    %s, %s, %s, %s,
                    %s, %s
                )
                ON CONFLICT (sandbox_id) DO UPDATE SET
                    tenant_name = EXCLUDED.tenant_name,
                    namespace = EXCLUDED.namespace,
                    state = CASE
                        WHEN {t}.deleted_at IS NOT NULL THEN {t}.state
                        WHEN {t}.state IN ('Terminated', 'Failed') THEN {t}.state
                        ELSE EXCLUDED.state
                    END,
                    image_uri = COALESCE(EXCLUDED.image_uri, {t}.image_uri),
                    lifecycle_created_at = CASE
                        WHEN {t}.deleted_at IS NOT NULL
                            OR {t}.state IN ('Terminated', 'Failed')
                        THEN {t}.lifecycle_created_at
                        ELSE COALESCE(
                            EXCLUDED.lifecycle_created_at, {t}.lifecycle_created_at
                        )
                    END,
                    expires_at = COALESCE(EXCLUDED.expires_at, {t}.expires_at),
                    cpu_limit = COALESCE(EXCLUDED.cpu_limit, {t}.cpu_limit),
                    memory_limit = COALESCE(
                        EXCLUDED.memory_limit, {t}.memory_limit
                    ),
                    cpu_cores = COALESCE(EXCLUDED.cpu_cores, {t}.cpu_cores),
                    memory_gi = COALESCE(EXCLUDED.memory_gi, {t}.memory_gi),
                    last_seen_at = EXCLUDED.last_seen_at,
                    source = CASE
                        WHEN {t}.source IS NOT NULL AND {t}.source <> 'server-lifecycle'
                        THEN {t}.source
                        ELSE EXCLUDED.source
                    END
                """,
                (
                    sandbox_id,
                    tenant_name,
                    namespace,
                    state,
                    image_uri,
                    lifecycle_created_at,
                    expires_at,
                    cpu_limit,
                    memory_limit,
                    cpu_cores,
                    memory_gi,
                    now,
                    now,
                ),
            )

    def mark_deleted(
        self,
        sandbox_id: str,
        *,
        tenant_name: str,
        namespace: str,
        final_state: str | None,
        image_uri: str | None,
        lifecycle_created_at: datetime | None,
        expires_at: datetime | None,
    ) -> None:
        now = datetime.now(timezone.utc)
        t = _HISTORY_TABLE
        with self._pool.connection() as conn:
            conn.execute(
                f"""
                INSERT INTO {t} (
                    sandbox_id, tenant_name, namespace, state, image_uri,
                    lifecycle_created_at, expires_at, ended_at, deleted_at,
                    source, first_recorded_at, last_seen_at
                ) VALUES (
                    %s, %s, %s, %s, %s,
                    %s, %s, %s, %s,
                    'server-lifecycle', %s, %s
                )
                ON CONFLICT (sandbox_id) DO UPDATE SET
                    tenant_name = COALESCE(NULLIF(EXCLUDED.tenant_name, ''), {t}.tenant_name),
                    namespace = COALESCE(NULLIF(EXCLUDED.namespace, ''), {t}.namespace),
                    state = 'Terminated',
                    image_uri = COALESCE(EXCLUDED.image_uri, {t}.image_uri),
                    lifecycle_created_at = COALESCE(
                        EXCLUDED.lifecycle_created_at, {t}.lifecycle_created_at
                    ),
                    expires_at = COALESCE(EXCLUDED.expires_at, {t}.expires_at),
                    ended_at = COALESCE({t}.ended_at, EXCLUDED.ended_at),
                    deleted_at = COALESCE({t}.deleted_at, EXCLUDED.deleted_at),
                    last_seen_at = EXCLUDED.last_seen_at,
                    wall_clock_seconds = COALESCE(
                        {t}.wall_clock_seconds,
                        GREATEST(
                            0,
                            FLOOR(
                                EXTRACT(
                                    EPOCH FROM (
                                        COALESCE({t}.ended_at, EXCLUDED.ended_at)
                                        - COALESCE(
                                            {t}.lifecycle_created_at,
                                            EXCLUDED.lifecycle_created_at,
                                            {t}.first_recorded_at
                                        )
                                    )
                                )
                            )::int
                        )
                    )
                """,
                (
                    sandbox_id,
                    tenant_name,
                    namespace,
                    final_state if final_state in ("Terminated", "Failed") else "Terminated",
                    image_uri,
                    lifecycle_created_at,
                    expires_at,
                    now,
                    now,
                    now,
                    now,
                ),
            )
