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

"""Factory for selecting the configured fsb template repository backend."""

from __future__ import annotations

from typing import Optional, Protocol

from functools import cache

from opensandbox_server.config import AppConfig, get_config


class FastSandboxTemplateRepository(Protocol):
    """Storage contract for the fsb template catalog."""

    def create(self, record): ...

    def get(self, template_id: str, namespace: str): ...

    def get_by_crd_name(self, namespace: str, crd_name: str): ...

    def list(self, query): ...

    def namespaces(self): ...

    def update_status(self, template_id: str, namespace: str, *, phase, manifest_ref, message) -> bool: ...

    def delete(self, template_id: str, namespace: str) -> None: ...

    def close(self) -> None: ...


def create_fsb_template_repository(
    config: Optional[AppConfig] = None,
) -> FastSandboxTemplateRepository:
    """Create the configured fsb template repository (mirrors snapshots)."""
    from opensandbox_server.repositories.templates.postgresql import (
        PostgreSQLFastSandboxTemplateRepository,
    )
    from opensandbox_server.repositories.templates.sqlite import SQLiteFastSandboxTemplateRepository

    active_config = config or get_config()
    store_config = active_config.store

    if store_config.type == "sqlite":
        return SQLiteFastSandboxTemplateRepository(store_config.path)  # type: ignore[return-value]
    if store_config.type == "postgresql":
        postgresql_config = store_config.postgresql
        dsn = postgresql_config.dsn
        if dsn is None:
            raise ValueError("PostgreSQL fsb template store requires a DSN.")
        return PostgreSQLFastSandboxTemplateRepository(
            dsn.get_secret_value(),
            min_pool_size=postgresql_config.min_pool_size,
            max_pool_size=postgresql_config.max_pool_size,
            connect_timeout_seconds=postgresql_config.connect_timeout_seconds,
            pool_timeout_seconds=postgresql_config.pool_timeout_seconds,
        )  # type: ignore[return-value]

    raise ValueError(f"Unsupported fsb template store type: {store_config.type}")


@cache
def get_fsb_template_repository() -> FastSandboxTemplateRepository:
    """Return the repository shared by the current server process."""
    return create_fsb_template_repository()


def close_fsb_template_repository() -> None:
    """Close and discard the repository shared by the current server process."""
    if get_fsb_template_repository.cache_info().currsize == 0:
        return
    repository = get_fsb_template_repository()
    get_fsb_template_repository.cache_clear()
    repository.close()


__all__ = [
    "FastSandboxTemplateRepository",
    "close_fsb_template_repository",
    "create_fsb_template_repository",
    "get_fsb_template_repository",
]
