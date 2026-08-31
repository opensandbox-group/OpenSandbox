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
Factory for selecting the configured snapshot repository backend.
"""

from __future__ import annotations

from functools import cache
from typing import Optional

from opensandbox_server.config import AppConfig, get_config
from opensandbox_server.repositories.snapshots.postgresql import PostgreSQLSnapshotRepository
from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository
from opensandbox_server.services.snapshot_repository import SnapshotRepository


def create_snapshot_repository(
    config: Optional[AppConfig] = None,
) -> SnapshotRepository:
    """
    Create the configured snapshot repository.
    """

    active_config = config or get_config()
    store_config = active_config.store

    if store_config.type == "sqlite":
        return SQLiteSnapshotRepository(store_config.path)
    if store_config.type == "postgresql":
        postgresql_config = store_config.postgresql
        dsn = postgresql_config.dsn
        if dsn is None:
            raise ValueError("PostgreSQL snapshot store requires a DSN.")
        return PostgreSQLSnapshotRepository(
            dsn.get_secret_value(),
            min_pool_size=postgresql_config.min_pool_size,
            max_pool_size=postgresql_config.max_pool_size,
            connect_timeout_seconds=postgresql_config.connect_timeout_seconds,
            pool_timeout_seconds=postgresql_config.pool_timeout_seconds,
        )

    raise ValueError(
        f"Unsupported snapshot store type: {store_config.type}"
    )


@cache
def get_snapshot_repository() -> SnapshotRepository:
    """Return the repository shared by the current server process."""
    return create_snapshot_repository()


def close_snapshot_repository() -> None:
    """Close and discard the repository shared by the current server process."""
    if get_snapshot_repository.cache_info().currsize == 0:
        return
    repository = get_snapshot_repository()
    get_snapshot_repository.cache_clear()
    repository.close()


__all__ = [
    "close_snapshot_repository",
    "create_snapshot_repository",
    "get_snapshot_repository",
]
