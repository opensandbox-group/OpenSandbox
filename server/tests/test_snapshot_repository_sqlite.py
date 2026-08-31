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

import sqlite3

import pytest

from opensandbox_server.repositories.snapshots import factory as factory_module
from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
from opensandbox_server.repositories.snapshots.sqlite import (
    SQLITE_BUSY_TIMEOUT_MS,
    SQLiteSnapshotRepository,
)
from opensandbox_server.config import AppConfig, RuntimeConfig, StoreConfig
from tests.snapshot_repository_contract import SnapshotRepositoryContract


class TestSQLiteSnapshotRepositoryContract(SnapshotRepositoryContract):
    @pytest.fixture
    def repository(self, tmp_path) -> SQLiteSnapshotRepository:
        return SQLiteSnapshotRepository(tmp_path / "snapshots.db")


def test_sqlite_snapshot_repository_enables_wal_and_busy_timeout(tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "snapshots.db")

    with repo._connect() as conn:
        journal_mode = conn.execute("PRAGMA journal_mode").fetchone()[0]
        busy_timeout = conn.execute("PRAGMA busy_timeout").fetchone()[0]

    assert journal_mode.lower() == "wal"
    assert busy_timeout == SQLITE_BUSY_TIMEOUT_MS


def test_sqlite_snapshot_repository_indexes_name_queries_after_migration(
    tmp_path,
) -> None:
    db_path = tmp_path / "legacy-snapshots.db"
    with sqlite3.connect(db_path) as conn:
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

    repo = SQLiteSnapshotRepository(db_path)

    with repo._connect() as conn:
        indexes = {row["name"] for row in conn.execute("PRAGMA index_list(snapshots)")}
        name_plan = conn.execute(
            "EXPLAIN QUERY PLAN SELECT COUNT(*) FROM snapshots WHERE name = ?",
            ("cache-key",),
        ).fetchall()
        tenant_plan = conn.execute(
            """
            EXPLAIN QUERY PLAN
            SELECT COUNT(*) FROM snapshots WHERE namespace = ? AND name = ?
            """,
            ("tenant-a", "cache-key"),
        ).fetchall()

    assert "idx_snapshots_name_namespace" in indexes
    assert any("idx_snapshots_name_namespace" in row["detail"] for row in name_plan)
    assert any("idx_snapshots_name_namespace" in row["detail"] for row in tenant_plan)


def test_snapshot_repository_factory_defaults_to_sqlite(tmp_path) -> None:
    db_path = tmp_path / "factory-snapshots.db"
    config = AppConfig(
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"),
        store=StoreConfig(path=str(db_path)),
    )

    repo = create_snapshot_repository(config)

    assert isinstance(repo, SQLiteSnapshotRepository)
    assert repo.db_path == db_path


def test_snapshot_repository_factory_reuses_process_repository(monkeypatch, tmp_path) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "shared-snapshots.db")
    factory_calls = 0

    def create_repository() -> SQLiteSnapshotRepository:
        nonlocal factory_calls
        factory_calls += 1
        return repo

    factory_module.get_snapshot_repository.cache_clear()
    monkeypatch.setattr(factory_module, "create_snapshot_repository", create_repository)

    try:
        assert factory_module.get_snapshot_repository() is repo
        assert factory_module.get_snapshot_repository() is repo
        assert factory_calls == 1
    finally:
        factory_module.get_snapshot_repository.cache_clear()


def test_snapshot_repository_factory_closes_and_discards_process_repository(
    monkeypatch, tmp_path
) -> None:
    repo = SQLiteSnapshotRepository(tmp_path / "shared-snapshots.db")
    close_calls = 0
    factory_calls = 0

    def close_repository() -> None:
        nonlocal close_calls
        close_calls += 1

    def create_repository() -> SQLiteSnapshotRepository:
        nonlocal factory_calls
        factory_calls += 1
        return repo

    monkeypatch.setattr(repo, "close", close_repository)
    monkeypatch.setattr(factory_module, "create_snapshot_repository", create_repository)
    factory_module.get_snapshot_repository.cache_clear()

    try:
        factory_module.get_snapshot_repository()
        factory_module.close_snapshot_repository()
        factory_module.get_snapshot_repository()

        assert close_calls == 1
        assert factory_calls == 2
    finally:
        factory_module.get_snapshot_repository.cache_clear()
