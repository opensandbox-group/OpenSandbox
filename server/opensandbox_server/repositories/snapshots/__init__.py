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
Snapshot persistence backends.

Exports are resolved lazily: importing ``factory`` eagerly imports the
``services`` package, which eagerly imports the Docker and Kubernetes service
stack and transitively returns to this package. Top-level imports here would
fail when the CLI entry point loads this package before ``services``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository
    from opensandbox_server.repositories.snapshots.postgresql import (
        PostgreSQLSnapshotRepository,
    )
    from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository

__all__ = [
    "SQLiteSnapshotRepository",
    "PostgreSQLSnapshotRepository",
    "create_snapshot_repository",
]


def __getattr__(name: str) -> Any:
    if name == "SQLiteSnapshotRepository":
        from opensandbox_server.repositories.snapshots.sqlite import SQLiteSnapshotRepository

        return SQLiteSnapshotRepository
    if name == "PostgreSQLSnapshotRepository":
        from opensandbox_server.repositories.snapshots.postgresql import (
            PostgreSQLSnapshotRepository,
        )

        return PostgreSQLSnapshotRepository
    if name == "create_snapshot_repository":
        from opensandbox_server.repositories.snapshots.factory import create_snapshot_repository

        return create_snapshot_repository
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")
