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
Runtime-facing snapshot creation interfaces.

The server owns snapshot resources and lifecycle persistence. Runtime
implementations are responsible for performing concrete snapshot operations and
reporting whether snapshot creation is supported for the active backend.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Optional, Protocol

from opensandbox_server.services.snapshot_models import SnapshotState


@dataclass(frozen=True)
class SnapshotRuntimeStatus:
    state: SnapshotState
    image: Optional[str] = None
    reason: Optional[str] = None
    message: Optional[str] = None


class SnapshotRuntime(Protocol):
    def supports_create_snapshot(self) -> bool:
        """
        Whether this runtime supports creating snapshots.
        """

    def create_snapshot_unsupported_message(self) -> str:
        """
        Human-readable message used when snapshot creation is unsupported.
        """

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
    ) -> Optional[SnapshotRuntimeStatus]:
        """
        Create a snapshot for a sandbox and return the final runtime status.
        """

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        """
        Return the most recent runtime view for a snapshot if known.
        """

    def delete_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> None:
        """
        Delete runtime-managed artifacts for a snapshot.
        """

    def inspect_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> SnapshotRuntimeStatus:
        """
        Inspect runtime-managed artifacts for startup recovery.
        """


class NoopSnapshotRuntime:
    """
    Placeholder runtime used when snapshot execution is not yet wired.
    """

    def supports_create_snapshot(self) -> bool:
        return False

    def create_snapshot_unsupported_message(self) -> str:
        return "Snapshot management is not implemented for this runtime."

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
    ) -> Optional[SnapshotRuntimeStatus]:
        raise NotImplementedError(self.create_snapshot_unsupported_message())

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return None

    def delete_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> None:
        return None

    def inspect_snapshot(self, snapshot_id: str, image: Optional[str] = None) -> SnapshotRuntimeStatus:
        return SnapshotRuntimeStatus(
            state=SnapshotState.FAILED,
            reason="snapshot_recovery_not_supported",
            message="Snapshot recovery is not implemented for this runtime.",
        )


__all__ = [
    "SnapshotRuntime",
    "SnapshotRuntimeStatus",
    "NoopSnapshotRuntime",
]
