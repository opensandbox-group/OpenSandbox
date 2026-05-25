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

from typing import Optional

from opensandbox_server.config import AppConfig, get_config
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

    raise ValueError(
        f"Unsupported snapshot store type: {store_config.type}"
    )


__all__ = [
    "create_snapshot_repository",
]
