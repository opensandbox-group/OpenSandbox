#
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
#
"""
Synchronous SandboxManager implementation.
"""

import logging
from datetime import datetime, timedelta, timezone
from typing import Any

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.sandboxes import (
    CreateSnapshotRequest,
    PagedSandboxInfos,
    PagedSnapshotInfos,
    SandboxFilter,
    SandboxInfo,
    SandboxRenewResponse,
    SnapshotFilter,
    SnapshotInfo,
)
from opensandbox.sync.adapters.factory import AdapterFactorySync
from opensandbox.sync.services.diagnostics import DiagnosticsSync
from opensandbox.sync.services.sandbox import SandboxesSync

logger = logging.getLogger(__name__)


class SandboxManagerSync:
    """
    Synchronous sandbox management interface for administrative operations.

    This class mirrors the async :class:`opensandbox.manager.SandboxManager`, but all
    operations are **blocking** and executed in the current thread.

    It is designed for *fleet* / admin workflows (listing, filtering, controlling sandboxes).
    For interacting with a single sandbox instance (files/commands/metrics), prefer
    :class:`opensandbox.sync.sandbox.SandboxSync`.

    Usage Example:

    ```python
    from opensandbox.models.sandboxes import SandboxFilter
    from opensandbox.sync.manager import SandboxManagerSync

    manager = SandboxManagerSync.create()
    infos = manager.list_sandbox_infos(SandboxFilter(states=["RUNNING"]))
    manager.close()
    ```
    """

    def __init__(
        self,
        sandbox_service: SandboxesSync,
        connection_config: ConnectionConfigSync,
        diagnostics_service: DiagnosticsSync | None = None,
    ) -> None:
        """
        Internal constructor for SandboxManagerSync.

        Note: Use :meth:`create` instead.

        Args:
            sandbox_service: Service for sandbox operations
            connection_config: Connection configuration (shared transport, headers, timeouts)
            diagnostics_service: Optional service for sandbox diagnostics
        """
        self._sandbox_service = sandbox_service
        self._connection_config = connection_config
        self._diagnostics_service = diagnostics_service or AdapterFactorySync(
            connection_config
        ).create_diagnostics_service()

    @property
    def connection_config(self) -> ConnectionConfigSync:
        """Provides access to the connection configuration (including shared transport)."""
        return self._connection_config

    @classmethod
    def create(cls, connection_config: ConnectionConfigSync | None = None) -> "SandboxManagerSync":
        """
        Create a SandboxManagerSync instance with the provided configuration (blocking).

        Args:
            connection_config: Connection configuration for the manager.
                If None, default configuration will be used.

        Returns:
            Configured sandbox manager instance
        """
        config = (connection_config or ConnectionConfigSync()).with_transport_if_missing()
        factory = AdapterFactorySync(config)
        sandbox_service = factory.create_sandbox_service()
        diagnostics_service = factory.create_diagnostics_service()
        return cls(sandbox_service, config, diagnostics_service)

    def list_sandbox_infos(self, filter: SandboxFilter) -> PagedSandboxInfos:
        """
        List sandboxes with filtering options.

        Args:
            filter: Filter criteria for sandbox listing

        Returns:
            Paged sandbox information matching the filter criteria

        Raises:
            SandboxException: if the operation fails
        """
        return self._sandbox_service.list_sandboxes(filter)

    def get_sandbox_info(self, sandbox_id: str) -> SandboxInfo:
        """
        Get information for a single sandbox by its ID.

        Args:
            sandbox_id: Sandbox ID to retrieve information for

        Returns:
            SandboxInfo for the specified sandbox

        Raises:
            SandboxException: if the operation fails
        """
        logger.debug("Getting info for sandbox: %s", sandbox_id)
        return self._sandbox_service.get_sandbox_info(sandbox_id)

    def get_diagnostic_logs(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        """
        Get diagnostic log content for a sandbox by ID.

        Args:
            sandbox_id: Sandbox ID to retrieve diagnostics for
            scope: Required diagnostic scope such as "container", "lifecycle", or "all".
        """
        return self._diagnostics_service.get_logs(sandbox_id, scope)

    def get_diagnostic_events(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        """
        Get diagnostic event content for a sandbox by ID.

        Args:
            sandbox_id: Sandbox ID to retrieve diagnostics for
            scope: Required diagnostic scope such as "runtime", "lifecycle", or "all".
        """
        return self._diagnostics_service.get_events(sandbox_id, scope)

    def patch_sandbox_metadata(
        self, sandbox_id: str, patch: dict[str, str | None]
    ) -> SandboxInfo:
        """
        Patch metadata for a sandbox.

        String values add or replace keys; None deletes keys.
        """
        logger.info("Patching metadata for sandbox: %s", sandbox_id)
        return self._sandbox_service.patch_sandbox_metadata(sandbox_id, patch)

    def kill_sandbox(self, sandbox_id: str) -> None:
        """
        Terminate a single sandbox.

        Args:
            sandbox_id: Sandbox ID to terminate

        Raises:
            SandboxException: if the operation fails
        """
        logger.info("Terminating sandbox: %s", sandbox_id)
        self._sandbox_service.kill_sandbox(sandbox_id)
        logger.info("Successfully terminated sandbox: %s", sandbox_id)

    def renew_sandbox(self, sandbox_id: str, timeout: timedelta) -> SandboxRenewResponse:
        """
        Renew expiration time for a single sandbox.

        The new expiration time will be set to the current time plus the provided duration.

        Args:
            sandbox_id: Sandbox ID to renew
            timeout: Duration to add to the current time to set the new expiration

        Raises:
            SandboxException: if the operation fails
        """
        # Use timezone-aware UTC datetime to avoid cross-timezone ambiguity.
        new_expiration = datetime.now(timezone.utc) + timeout
        logger.info("Renew expiration for sandbox %s to %s", sandbox_id, new_expiration)
        return self._sandbox_service.renew_sandbox_expiration(sandbox_id, new_expiration)

    def pause_sandbox(self, sandbox_id: str) -> None:
        """
        Pause a single sandbox while preserving its state.

        Args:
            sandbox_id: Sandbox ID to pause

        Raises:
            SandboxException: if the operation fails
        """
        logger.info("Pausing sandbox: %s", sandbox_id)
        self._sandbox_service.pause_sandbox(sandbox_id)

    def resume_sandbox(self, sandbox_id: str) -> None:
        """
        Resume a previously paused sandbox.

        Args:
            sandbox_id: Sandbox ID to resume

        Raises:
            SandboxException: if the operation fails
        """
        logger.info("Resuming sandbox: %s", sandbox_id)
        self._sandbox_service.resume_sandbox(sandbox_id)

    def create_snapshot(self, sandbox_id: str, name: str | None = None) -> SnapshotInfo:
        """Create a snapshot from a sandbox (blocking)."""
        return self._sandbox_service.create_snapshot(
            sandbox_id, CreateSnapshotRequest(name=name)
        )

    def get_snapshot(self, snapshot_id: str) -> SnapshotInfo:
        """Get information for a snapshot by id (blocking)."""
        return self._sandbox_service.get_snapshot(snapshot_id)

    def list_snapshots(self, filter: SnapshotFilter) -> PagedSnapshotInfos:
        """List snapshots with filtering options (blocking)."""
        return self._sandbox_service.list_snapshots(filter)

    def delete_snapshot(self, snapshot_id: str) -> None:
        """Delete a snapshot by id (blocking)."""
        self._sandbox_service.delete_snapshot(snapshot_id)

    def close(self) -> None:
        """
        Close local resources associated with this sandbox manager.

        This method closes HTTP client resources and other local resources.

        Note: This method logs errors but does not raise exceptions to avoid
        issues in context manager cleanup.
        """
        try:
            self._connection_config.close_transport_if_owned()
        except Exception as e:
            logger.warning("Error closing resources for sandbox manager: %s", e, exc_info=True)

    def __enter__(self) -> "SandboxManagerSync":
        """Sync context manager entry."""
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        """Sync context manager exit."""
        self.close()
