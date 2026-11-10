# Copyright 2025 The OpenSandbox Authors
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
    # Backend marker persisted into restore_config on READY (e.g. "fsb") so
    # create-time routing can send restores to the owning backend.
    backend: Optional[str] = None
    # Source sandbox's per-sandbox limits, persisted and replayed on restore.
    resource_limits: Optional[dict] = None
    # Artifact class the runtime confirmed for the produced snapshot
    # ("full" | "rootfsOnly"); None when the runtime cannot report one and
    # the persisted request mode stays authoritative.
    mode: Optional[str] = None


# Public snapshot mode values (wire names; see CreateSnapshotRequest).
SNAPSHOT_MODE_FULL = "full"
SNAPSHOT_MODE_ROOTFS_ONLY = "rootfsOnly"
# Only "rootfsOnly" is stored; absent (None) means the default "full", so
# legacy rows and explicit full requests persist identically.
PERSISTED_SNAPSHOT_MODES = frozenset({SNAPSHOT_MODE_ROOTFS_ONLY})


def normalize_snapshot_mode(mode: Optional[str]) -> Optional[str]:
    """Collapse the public mode to its persisted form (None = full)."""
    if mode == SNAPSHOT_MODE_ROOTFS_ONLY:
        return SNAPSHOT_MODE_ROOTFS_ONLY
    return None


def reject_rootfs_only_mode(backend_description: str, mode: Optional[str]) -> None:
    """Fail closed when a runtime without rootfs-only support is asked for it.

    Runtimes other than fast-sandbox cannot produce a rootfs-only artifact;
    rejecting in preflight keeps the failure a client-side 409 before any
    side effect instead of a failed row after one.
    """
    if mode == SNAPSHOT_MODE_ROOTFS_ONLY:
        raise SnapshotRuntimeUnsupportedError(
            "Snapshot mode 'rootfsOnly' is only supported on the fast-sandbox "
            f"backend; {backend_description}."
        )


# Reason on a FAILED SnapshotRuntimeStatus when the runtime refused the create
# because the source sandbox is already held by another snapshot (re-entry
# fence). Surfaced as 409 instead of 500 by the synchronous create path.
SNAPSHOT_CREATE_CONFLICT_REASON = "snapshot_runtime_create_conflict"


class SnapshotRuntimePreflightError(RuntimeError):
    """Snapshot creation cannot safely start for the current source runtime."""


class SnapshotRuntimeUnsupportedError(SnapshotRuntimePreflightError):
    """The source runtime is known to be incompatible with snapshot creation."""


class SnapshotRuntime(Protocol):
    def supports_create_snapshot(self) -> bool:
        """
        Whether this runtime supports creating snapshots.
        """

    def supports_synchronous_create(self) -> bool:
        """
        Whether ``create_snapshot`` is a fast, idempotent submit that can run
        inline before the snapshot row is persisted.
        """

    def create_snapshot_unsupported_message(self) -> str:
        """
        Human-readable message used when snapshot creation is unsupported.
        """

    def preflight_create_snapshot(
        self,
        sandbox_id: str,
        *,
        namespace: str | None = None,
        mode: str | None = None,
    ) -> None:
        """Validate source-specific compatibility before persisting a snapshot."""

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
        mode: str | None = None,
    ) -> Optional[SnapshotRuntimeStatus]:
        """
        Create a snapshot for a sandbox and return the final runtime status.
        """

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        """
        Return the most recent runtime view for a snapshot if known.
        """

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        """
        Delete runtime-managed artifacts for a snapshot.

        ``source_sandbox_id`` identifies the owning backend for composite
        dispatch; runtimes that serve one backend only ignore it.
        """

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        """
        Inspect runtime-managed artifacts for startup recovery.
        """


def is_snapshot_source_state_supported(
    runtime: SnapshotRuntime,
    sandbox_id: str,
    state: str,
    *,
    namespace: str | None = None,
) -> bool:
    """Return whether a runtime accepts snapshot creation from the source state.

    Existing runtime adapters that do not expose a source-state capability
    retain the historical behavior and only accept Running. Backends may opt
    into additional safe states by implementing supports_snapshot_source_state.
    """
    checker = getattr(runtime, "supports_snapshot_source_state", None)
    if checker is None:
        return state == "Running"
    return checker(sandbox_id, state, namespace=namespace)


class NoopSnapshotRuntime:
    """
    Placeholder runtime used when snapshot execution is not yet wired.
    """

    def supports_create_snapshot(self) -> bool:
        return False

    def supports_synchronous_create(self) -> bool:
        return False

    def create_snapshot_unsupported_message(self) -> str:
        return "Snapshot management is not implemented for this runtime."

    def preflight_create_snapshot(
        self,
        sandbox_id: str,
        *,
        namespace: str | None = None,
        mode: str | None = None,
    ) -> None:
        raise SnapshotRuntimeUnsupportedError(
            self.create_snapshot_unsupported_message()
        )

    def create_snapshot(
        self,
        snapshot_id: str,
        sandbox_id: str,
        *,
        namespace: str | None = None,
        mode: str | None = None,
    ) -> Optional[SnapshotRuntimeStatus]:
        raise NotImplementedError(self.create_snapshot_unsupported_message())

    def get_snapshot_status(self, snapshot_id: str) -> Optional[SnapshotRuntimeStatus]:
        return None

    def delete_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> None:
        return None

    def inspect_snapshot(
        self,
        snapshot_id: str,
        image: Optional[str] = None,
        *,
        namespace: str | None = None,
        source_sandbox_id: str | None = None,
    ) -> SnapshotRuntimeStatus:
        return SnapshotRuntimeStatus(
            state=SnapshotState.FAILED,
            reason="snapshot_recovery_not_supported",
            message="Snapshot recovery is not implemented for this runtime.",
        )


__all__ = [
    "SNAPSHOT_CREATE_CONFLICT_REASON",
    "SnapshotRuntime",
    "SnapshotRuntimePreflightError",
    "SnapshotRuntimeStatus",
    "SnapshotRuntimeUnsupportedError",
    "NoopSnapshotRuntime",
    "is_snapshot_source_state_supported",
]
