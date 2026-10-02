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

from functools import cache
from typing import Optional

from opensandbox_server.config import AppConfig, get_config
from opensandbox_server.repositories.lifecycle_audit.postgresql import (
    PostgreSQLLifecycleAuditRepository,
)


def _audit_enabled(config: AppConfig) -> bool:
    return (
        config.store.type == "postgresql"
        and config.store.lifecycle_audit.enabled
    )


def create_lifecycle_audit_repository(
    config: Optional[AppConfig] = None,
) -> PostgreSQLLifecycleAuditRepository | None:
    active = config or get_config()
    if not _audit_enabled(active):
        return None
    postgresql_config = active.store.postgresql
    dsn = postgresql_config.dsn
    if dsn is None:
        raise ValueError("Lifecycle audit requires store.postgresql.dsn.")
    return PostgreSQLLifecycleAuditRepository(
        dsn.get_secret_value(),
        min_pool_size=postgresql_config.min_pool_size,
        max_pool_size=min(5, postgresql_config.max_pool_size),
        connect_timeout_seconds=postgresql_config.connect_timeout_seconds,
        pool_timeout_seconds=postgresql_config.pool_timeout_seconds,
    )


@cache
def get_lifecycle_audit_repository() -> PostgreSQLLifecycleAuditRepository | None:
    return create_lifecycle_audit_repository()


def close_lifecycle_audit_repository() -> None:
    if get_lifecycle_audit_repository.cache_info().currsize == 0:
        return
    repository = get_lifecycle_audit_repository()
    get_lifecycle_audit_repository.cache_clear()
    if repository is not None:
        repository.close()


__all__ = [
    "close_lifecycle_audit_repository",
    "create_lifecycle_audit_repository",
    "get_lifecycle_audit_repository",
]
