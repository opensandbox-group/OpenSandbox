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
"""Durable fork intents and database leases in the configured lifecycle store."""

from contextlib import contextmanager
import json
from pathlib import Path
import sqlite3
import time
from typing import Any, Iterator

from psycopg_pool import ConnectionPool

from opensandbox_server.config import AppConfig


class ForkRepository:
    """Both stores use the same conditional SQL transitions and JSON payload."""

    def __init__(self, config: AppConfig) -> None:
        self._pool = None
        self._path = None
        if config.store.type == "sqlite":
            self._path = Path(config.store.path).expanduser()
            self._path.parent.mkdir(parents=True, exist_ok=True)
        else:
            pg = config.store.postgresql
            if pg.dsn is None:
                raise ValueError("PostgreSQL store requires a DSN.")
            self._pool = ConnectionPool(
                pg.dsn.get_secret_value(), min_size=pg.min_pool_size,
                max_size=pg.max_pool_size, timeout=pg.pool_timeout_seconds,
                kwargs={"connect_timeout": pg.connect_timeout_seconds}, open=False,
            )
            self._pool.open(wait=True, timeout=pg.connect_timeout_seconds)
        with self._connection() as conn:
            if self._pool is not None:
                conn.execute("SELECT pg_advisory_xact_lock(hashtext('opensandbox-fork-schema'))")
            conn.execute("""
                CREATE TABLE IF NOT EXISTS sandbox_forks (
                    id TEXT PRIMARY KEY, owner_scope TEXT NOT NULL,
                    idempotency_key TEXT, payload TEXT NOT NULL,
                    active INTEGER NOT NULL, lease_owner TEXT,
                    lease_until DOUBLE PRECISION NOT NULL DEFAULT 0,
                    UNIQUE(owner_scope, idempotency_key)
                )
            """)

    @contextmanager
    def _connection(self) -> Iterator[Any]:
        if self._pool is not None:
            with self._pool.connection() as conn:
                yield conn
        else:
            with sqlite3.connect(str(self._path), timeout=5) as conn:
                yield conn

    def _sql(self, query: str) -> str:
        return query.replace("?", "%s") if self._pool is not None else query

    def create(self, record: dict, scope: str, key: str | None) -> dict:
        with self._connection() as conn:
            row = conn.execute(self._sql("""
                INSERT INTO sandbox_forks(id, owner_scope, idempotency_key, payload, active)
                VALUES (?, ?, ?, ?, 1) ON CONFLICT DO NOTHING RETURNING payload
            """), (record["operation"]["id"], scope, key, json.dumps(record))).fetchone()
            if row is not None:
                return json.loads(row[0])
        if key is not None:
            existing = self.by_key(scope, key)
            if existing is not None:
                return existing
        raise RuntimeError("Fork intent could not be persisted.")

    def by_key(self, scope: str, key: str) -> dict | None:
        with self._connection() as conn:
            row = conn.execute(self._sql(
                "SELECT payload FROM sandbox_forks WHERE owner_scope = ? AND idempotency_key = ?"
            ), (scope, key)).fetchone()
        return json.loads(row[0]) if row else None

    def get(self, fork_id: str, scope: str) -> dict | None:
        with self._connection() as conn:
            row = conn.execute(self._sql(
                "SELECT payload FROM sandbox_forks WHERE id = ? AND owner_scope = ?"
            ), (fork_id, scope)).fetchone()
        return json.loads(row[0]) if row else None

    def claim(self, owner: str) -> dict | None:
        now = time.time()
        with self._connection() as conn:
            rows = conn.execute(self._sql(
            "SELECT id FROM sandbox_forks WHERE active = 1 AND lease_until < ? ORDER BY lease_until LIMIT 20"
            ), (now,)).fetchall()
            for (fork_id,) in rows:
                row = conn.execute(self._sql("""
                    UPDATE sandbox_forks SET lease_owner = ?, lease_until = ?
                    WHERE id = ? AND active = 1 AND lease_until < ? RETURNING payload
                """), (owner, now + 60, fork_id, now)).fetchone()
                if row:
                    return json.loads(row[0])
        return None

    def renew(self, fork_id: str, owner: str) -> bool:
        now = time.time()
        with self._connection() as conn:
            result = conn.execute(self._sql("""
                UPDATE sandbox_forks SET lease_until = ?
                WHERE id = ? AND lease_owner = ? AND lease_until > ?
            """), (now + 60, fork_id, owner, now))
            return result.rowcount == 1

    def save(self, record: dict, owner: str, *, release: bool = False) -> bool:
        operation = record["operation"]
        active = int(operation["status"]["state"] not in {"Succeeded", "Failed"}
                     or operation.get("cleanupPending", False))
        now = time.time()
        with self._connection() as conn:
            result = conn.execute(self._sql("""
                UPDATE sandbox_forks SET payload = ?, active = ?, lease_until = ?
                WHERE id = ? AND lease_owner = ? AND lease_until > ?
            """), (json.dumps(record), active, now + 2 if release else now + 60,
                    operation["id"], owner, now))
            return result.rowcount == 1

    def close(self) -> None:
        if self._pool is not None:
            self._pool.close()
