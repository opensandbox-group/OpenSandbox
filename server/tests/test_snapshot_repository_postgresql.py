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

from concurrent.futures import ThreadPoolExecutor
from collections.abc import Iterator
from datetime import datetime, timedelta, timezone
import os
from threading import Barrier

import psycopg
import pytest
from pydantic import SecretStr

from opensandbox_server.config import (
    AppConfig,
    PostgreSQLStoreConfig,
    RuntimeConfig,
    StoreConfig,
)
from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
from opensandbox_server.repositories.snapshots.postgresql import PostgreSQLSnapshotRepository
from opensandbox_server.services.snapshot_models import SnapshotState
from tests.snapshot_repository_contract import (
    SnapshotRepositoryContract,
    snapshot_record,
)

TEST_POSTGRESQL_DSN_ENV_VAR = "OPENSANDBOX_TEST_POSTGRESQL_DSN"


@pytest.fixture(scope="module")
def postgresql_dsn() -> str:
    dsn = os.environ.get(TEST_POSTGRESQL_DSN_ENV_VAR)
    if not dsn:
        pytest.skip(f"{TEST_POSTGRESQL_DSN_ENV_VAR} is not set")
    return dsn


def _repository(dsn: str) -> PostgreSQLSnapshotRepository:
    return PostgreSQLSnapshotRepository(
        dsn,
        min_pool_size=0,
        max_pool_size=4,
        connect_timeout_seconds=5,
        pool_timeout_seconds=5,
    )


class TestPostgreSQLSnapshotRepositoryContract(SnapshotRepositoryContract):
    @pytest.fixture
    def repository(self, postgresql_dsn: str) -> Iterator[PostgreSQLSnapshotRepository]:
        repo = _repository(postgresql_dsn)
        try:
            with psycopg.connect(postgresql_dsn) as conn:
                conn.execute("TRUNCATE TABLE snapshots")
            yield repo
        finally:
            repo.close()


def test_postgresql_factory_selects_backend(postgresql_dsn: str) -> None:
    config = AppConfig(
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:test"),
        store=StoreConfig(
            type="postgresql",
            postgresql=PostgreSQLStoreConfig(
                dsn=SecretStr(postgresql_dsn),
                min_pool_size=0,
                max_pool_size=2,
            ),
        ),
    )

    repo = create_snapshot_repository(config)
    try:
        assert isinstance(repo, PostgreSQLSnapshotRepository)
    finally:
        repo.close()


def test_postgresql_schema_initialization_is_concurrent_safe(
    postgresql_dsn: str,
    monkeypatch,
) -> None:
    with psycopg.connect(postgresql_dsn) as conn:
        conn.execute("DROP TABLE IF EXISTS snapshots")

    barrier = Barrier(2)
    initialize_schema = PostgreSQLSnapshotRepository._initialize_schema

    def initialize_schema_concurrently(repo: PostgreSQLSnapshotRepository) -> None:
        barrier.wait(timeout=5)
        initialize_schema(repo)

    monkeypatch.setattr(
        PostgreSQLSnapshotRepository,
        "_initialize_schema",
        initialize_schema_concurrently,
    )

    repositories: list[PostgreSQLSnapshotRepository] = []
    try:
        with ThreadPoolExecutor(max_workers=2) as executor:
            futures = [executor.submit(_repository, postgresql_dsn) for _ in range(2)]
            errors: list[BaseException] = []
            for future in futures:
                try:
                    repositories.append(future.result())
                except BaseException as exc:
                    errors.append(exc)
            if errors:
                raise errors[0]

        with psycopg.connect(postgresql_dsn) as conn:
            row = conn.execute("SELECT to_regclass('snapshots')").fetchone()
        assert row is not None
        table_name = row[0]
        assert table_name == "snapshots"
    finally:
        for repo in repositories:
            repo.close()


def test_postgresql_compare_and_swap_has_one_winner(postgresql_dsn: str) -> None:
    repositories: list[PostgreSQLSnapshotRepository] = []
    try:
        first_repo = _repository(postgresql_dsn)
        repositories.append(first_repo)
        second_repo = _repository(postgresql_dsn)
        repositories.append(second_repo)
        with psycopg.connect(postgresql_dsn) as conn:
            conn.execute("TRUNCATE TABLE snapshots")

        now = datetime.now(timezone.utc)
        original = snapshot_record("snap-race", "sbx-001", now)
        ready = snapshot_record(
            original.id,
            original.source_sandbox_id,
            original.created_at,
            SnapshotState.READY,
        )
        failed = snapshot_record(
            original.id,
            original.source_sandbox_id,
            original.created_at,
            SnapshotState.FAILED,
        )
        first_repo.create(original)
        barrier = Barrier(2)

        def update(repo, record) -> bool:
            barrier.wait(timeout=5)
            return repo.update_if_state(record, SnapshotState.CREATING)

        with ThreadPoolExecutor(max_workers=2) as executor:
            ready_future = executor.submit(update, first_repo, ready)
            failed_future = executor.submit(update, second_repo, failed)
            results = [ready_future.result(), failed_future.result()]

        assert sorted(results) == [False, True]
        winning_state = SnapshotState.READY if results[0] else SnapshotState.FAILED
        stored = first_repo.get(original.id)
        assert stored is not None
        assert stored.status.state == winning_state
    finally:
        for repo in repositories:
            repo.close()


def test_postgresql_close_releases_pool(postgresql_dsn: str) -> None:
    repo = _repository(postgresql_dsn)

    repo.close()

    assert repo._pool.closed is True


def test_postgresql_row_timestamps_are_normalized_to_utc() -> None:
    local_timezone = timezone(timedelta(hours=8))
    local_time = datetime(2026, 1, 2, 8, 0, tzinfo=local_timezone)

    record = PostgreSQLSnapshotRepository._row_to_record(
        {
            "id": "snap-timezone",
            "source_sandbox_id": "sbx-001",
            "namespace": None,
            "name": None,
            "description": None,
            "restore_config": {"image": None},
            "state": SnapshotState.CREATING.value,
            "reason": None,
            "message": None,
            "last_transition_at": local_time,
            "created_at": local_time,
            "updated_at": local_time,
        }
    )

    expected = datetime(2026, 1, 2, 0, 0, tzinfo=timezone.utc)
    last_transition_at = record.status.last_transition_at
    assert last_transition_at is not None
    assert last_transition_at == expected
    assert last_transition_at.tzinfo is timezone.utc
    assert record.created_at == expected
    assert record.created_at.tzinfo is timezone.utc
    assert record.updated_at == expected
    assert record.updated_at.tzinfo is timezone.utc
