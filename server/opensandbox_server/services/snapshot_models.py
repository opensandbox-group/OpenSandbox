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
Runtime-agnostic persistent models for server-managed snapshots.

These models define the server-side source of truth for snapshot metadata,
restore configuration, and lifecycle status. They are intentionally decoupled
from both API schemas and runtime-specific objects.
"""

from dataclasses import dataclass, field
from datetime import datetime
from enum import Enum


class SnapshotState(str, Enum):
    """
    Canonical server-side lifecycle states for persisted snapshots.
    """

    CREATING = "Creating"
    DELETING = "Deleting"
    READY = "Ready"
    FAILED = "Failed"


@dataclass(slots=True)
class SnapshotRestoreConfig:
    """
    Runtime-agnostic restore configuration for a snapshot.

    Phase 1 stores only the image reference needed to restore a sandbox from the
    snapshot. Keep this as an object so future fields can be added without
    changing the top-level snapshot record shape.
    """

    image: str | None = None


@dataclass(slots=True)
class SnapshotStatusRecord:
    """
    Server-observed lifecycle status for a snapshot.
    """

    state: SnapshotState
    reason: str | None = None
    message: str | None = None
    last_transition_at: datetime | None = None


@dataclass(slots=True)
class SnapshotRecord:
    """
    Persisted snapshot resource managed by the lifecycle server.
    """

    id: str
    source_sandbox_id: str
    name: str | None = None
    description: str | None = None
    restore_config: SnapshotRestoreConfig = field(default_factory=SnapshotRestoreConfig)
    status: SnapshotStatusRecord = field(
        default_factory=lambda: SnapshotStatusRecord(state=SnapshotState.CREATING)
    )
    created_at: datetime = field(default_factory=datetime.utcnow)
    updated_at: datetime = field(default_factory=datetime.utcnow)


__all__ = [
    "SnapshotState",
    "SnapshotRestoreConfig",
    "SnapshotStatusRecord",
    "SnapshotRecord",
]
