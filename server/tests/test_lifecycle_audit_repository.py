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

from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone

import psycopg
import pytest

from opensandbox_server.repositories.lifecycle_audit.postgresql import (
    PostgreSQLLifecycleAuditRepository,
)

TEST_POSTGRESQL_DSN_ENV_VAR = "OPENSANDBOX_TEST_POSTGRESQL_DSN"
_HISTORY_TABLE = "sandbox_lifecycle_history"


@pytest.fixture(scope="module")
def postgresql_dsn() -> str:
    dsn = os.environ.get(TEST_POSTGRESQL_DSN_ENV_VAR)
    if not dsn:
        pytest.skip(f"{TEST_POSTGRESQL_DSN_ENV_VAR} is not set")
    return dsn


def _truncate_history(dsn: str) -> None:
    with psycopg.connect(dsn) as conn:
        conn.execute(f"TRUNCATE TABLE {_HISTORY_TABLE}")


def _fetch_row(dsn: str, sandbox_id: str) -> dict | None:
    with psycopg.connect(dsn, row_factory=psycopg.rows.dict_row) as conn:
        row = conn.execute(
            f"SELECT * FROM {_HISTORY_TABLE} WHERE sandbox_id = %s",
            (sandbox_id,),
        ).fetchone()
        return dict(row) if row else None


def _repo(dsn: str) -> PostgreSQLLifecycleAuditRepository:
    return PostgreSQLLifecycleAuditRepository(dsn, min_pool_size=1, max_pool_size=2)


def test_late_create_audit_does_not_revive_terminated_row(
    postgresql_dsn: str,
) -> None:
    _truncate_history(postgresql_dsn)
    repo = _repo(postgresql_dsn)
    try:
        sandbox_id = "sbx-late-create-race"
        created = datetime(2026, 1, 1, 12, 0, 0, tzinfo=timezone.utc)
        expires = created + timedelta(hours=1)

        repo.mark_deleted(
            sandbox_id,
            tenant_name="t1",
            namespace="ns1",
            final_state="Terminated",
            image_uri="img:1",
            lifecycle_created_at=created,
            expires_at=expires,
        )
        repo.upsert_created(
            sandbox_id=sandbox_id,
            tenant_name="t1",
            namespace="ns1",
            state="Running",
            image_uri="img:1",
            lifecycle_created_at=created,
            expires_at=expires,
        )

        row = _fetch_row(postgresql_dsn, sandbox_id)
        assert row is not None
        assert row["state"] == "Terminated"
        assert row["deleted_at"] is not None
    finally:
        repo.close()


def test_create_then_delete_records_terminal_state(postgresql_dsn: str) -> None:
    _truncate_history(postgresql_dsn)
    repo = _repo(postgresql_dsn)
    try:
        sandbox_id = "sbx-create-delete"
        created = datetime(2026, 2, 1, 8, 0, 0, tzinfo=timezone.utc)
        expires = created + timedelta(hours=2)

        repo.upsert_created(
            sandbox_id=sandbox_id,
            tenant_name="t2",
            namespace="ns2",
            state="Pending",
            image_uri="img:2",
            lifecycle_created_at=created,
            expires_at=expires,
        )
        repo.mark_deleted(
            sandbox_id,
            tenant_name="t2",
            namespace="ns2",
            final_state="Failed",
            image_uri="img:2",
            lifecycle_created_at=created,
            expires_at=expires,
        )

        row = _fetch_row(postgresql_dsn, sandbox_id)
        assert row is not None
        assert row["state"] == "Failed"
        assert row["deleted_at"] is not None
    finally:
        repo.close()


def test_upsert_created_preserves_expires_at_when_incoming_null(
    postgresql_dsn: str,
) -> None:
    _truncate_history(postgresql_dsn)
    repo = _repo(postgresql_dsn)
    try:
        sandbox_id = "sbx-expires-coalesce"
        created = datetime(2026, 3, 1, 9, 0, 0, tzinfo=timezone.utc)
        expires = created + timedelta(hours=3)

        repo.upsert_created(
            sandbox_id=sandbox_id,
            tenant_name="t3",
            namespace="ns3",
            state="Running",
            image_uri="img:3",
            lifecycle_created_at=created,
            expires_at=expires,
        )
        repo.upsert_created(
            sandbox_id=sandbox_id,
            tenant_name="t3",
            namespace="ns3",
            state="Running",
            image_uri="img:3",
            lifecycle_created_at=created,
            expires_at=None,
        )

        row = _fetch_row(postgresql_dsn, sandbox_id)
        assert row is not None
        assert row["expires_at"] == expires
    finally:
        repo.close()
