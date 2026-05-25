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
Sandbox management interface for administrative operations.

This module provides a centralized interface for managing sandbox instances,
enabling administrative operations and sandbox discovery following the Kotlin SDK pattern.
"""

import logging
from datetime import datetime, timedelta, timezone

from opensandbox.adapters.factory import AdapterFactory
from opensandbox.config import ConnectionConfig
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
from opensandbox.services.diagnostics import Diagnostics
from opensandbox.services.sandbox import Sandboxes

logger = logging.getLogger(__name__)


class SandboxManager:
    """
    Sandbox management interface for administrative operations and monitoring sandbox instances.

    This class provides a centralized interface for managing sandbox instances,
    enabling administrative operations and sandbox discovery.
    It focuses on high-level management operations rather than individual sandbox interactions.

    Key Features:

    - **Sandbox Discovery**: List and filter sandbox instances by various criteria
    - **Administrative Operations**: Individual sandbox management operations
    - **Connection Pool Management**: Efficient HTTP client reuse for multiple operations

    Usage Example:

    ```python
    # Create manager
    manager = await SandboxManager.create(connection_config=connection_config)

    # List all running sandboxes
    running_sandboxes = await manager.list_sandbox_infos(
        SandboxFilter(states=["RUNNING"])
    )

    # Individual operations
    sandbox_id = "sandbox-id"
    await manager.get_sandbox_info(sandbox_id)
    await manager.pause_sandbox(sandbox_id)
    await manager.resume_sandbox(sandbox_id)
    await manager.kill_sandbox(sandbox_id)

    # Cleanup
    await manager.close()
    ```

    **Note**: This class is designed for administrative operations.
    For individual sandbox interactions, use the Sandbox class directly.
    """

    def __init__(
        self,
        sandbox_service: Sandboxes,
        connection_config: ConnectionConfig,
        diagnostics_service: Diagnostics | None = None,
    ) -> None:
        """
        Internal constructor for SandboxManager.

        Note: Use SandboxManager.create() instead.

        Args:
            sandbox_service: Service for sandbox operations
            connection_config: Connection configuration (shared transport, headers, timeouts)
            diagnostics_service: Optional service for sandbox diagnostics
        """
        self._sandbox_service = sandbox_service
        self._connection_config = connection_config
        self._diagnostics_service = diagnostics_service or AdapterFactory(
            connection_config
        ).create_diagnostics_service()

    @property
    def connection_config(self) -> ConnectionConfig:
        """Provides access to the connection configuration (including shared transport)."""
        return self._connection_config

    @classmethod
    async def create(
        cls, connection_config: ConnectionConfig | None = None
    ) -> "SandboxManager":
        """
        Creates a SandboxManager instance with the provided configuration.

        Args:
            connection_config: Connection configuration for the manager.
                             If None, default configuration will be used.

        Returns:
            SandboxManager: Configured sandbox manager instance
        """
        config = (connection_config or ConnectionConfig()).with_transport_if_missing()
        factory = AdapterFactory(config)
        sandbox_service = factory.create_sandbox_service()
        diagnostics_service = factory.create_diagnostics_service()
        return cls(sandbox_service, config, diagnostics_service)

    async def list_sandbox_infos(self, filter: SandboxFilter) -> PagedSandboxInfos:
        """
        List sandboxes with filtering options.

        Args:
            filter: Filter criteria for sandbox listing

        Returns:
            Paged sandbox information matching the filter criteria

        Raises:
            SandboxException: if the operation fails
        """
        return await self._sandbox_service.list_sandboxes(filter)

    async def get_sandbox_info(self, sandbox_id: str) -> SandboxInfo:
        """
        Get information for a single sandbox by its ID.

        Args:
            sandbox_id: Sandbox ID to retrieve information for

        Returns:
            SandboxInfo for the specified sandbox

        Raises:
            SandboxException: if the operation fails
        """
        logger.debug(f"Getting info for sandbox: {sandbox_id}")
        return await self._sandbox_service.get_sandbox_info(sandbox_id)

    async def get_diagnostic_logs(
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
        return await self._diagnostics_service.get_logs(sandbox_id, scope)

    async def get_diagnostic_events(
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
        return await self._diagnostics_service.get_events(sandbox_id, scope)

    async def patch_sandbox_metadata(
        self, sandbox_id: str, patch: dict[str, str | None]
    ) -> SandboxInfo:
        """
        Patch metadata for a sandbox.

        String values add or replace keys; None deletes keys.
        """
        logger.info("Patching metadata for sandbox: %s", sandbox_id)
        return await self._sandbox_service.patch_sandbox_metadata(sandbox_id, patch)

    async def kill_sandbox(self, sandbox_id: str) -> None:
        """
        Terminate a single sandbox.

        Args:
            sandbox_id: Sandbox ID to terminate

        Raises:
            SandboxException: if the operation fails
        """
        logger.info(f"Terminating sandbox: {sandbox_id}")
        await self._sandbox_service.kill_sandbox(sandbox_id)
        logger.info(f"Successfully terminated sandbox: {sandbox_id}")

    async def renew_sandbox(self, sandbox_id: str, timeout: timedelta) -> SandboxRenewResponse:
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
        logger.info(f"Renew expiration for sandbox {sandbox_id} to {new_expiration}")
        return await self._sandbox_service.renew_sandbox_expiration(
            sandbox_id, new_expiration
        )

    async def pause_sandbox(self, sandbox_id: str) -> None:
        """
        Pause a single sandbox while preserving its state.

        Args:
            sandbox_id: Sandbox ID to pause

        Raises:
            SandboxException: if the operation fails
        """
        logger.info(f"Pausing sandbox: {sandbox_id}")
        await self._sandbox_service.pause_sandbox(sandbox_id)

    async def resume_sandbox(self, sandbox_id: str) -> None:
        """
        Resume a previously paused sandbox.

        Args:
            sandbox_id: Sandbox ID to resume

        Raises:
            SandboxException: if the operation fails
        """
        logger.info(f"Resuming sandbox: {sandbox_id}")
        await self._sandbox_service.resume_sandbox(sandbox_id)

    async def create_snapshot(
        self, sandbox_id: str, name: str | None = None
    ) -> SnapshotInfo:
        """Create a snapshot from a sandbox."""
        return await self._sandbox_service.create_snapshot(
            sandbox_id, CreateSnapshotRequest(name=name)
        )

    async def get_snapshot(self, snapshot_id: str) -> SnapshotInfo:
        """Get information for a snapshot by id."""
        return await self._sandbox_service.get_snapshot(snapshot_id)

    async def list_snapshots(self, filter: SnapshotFilter) -> PagedSnapshotInfos:
        """List snapshots with filtering options."""
        return await self._sandbox_service.list_snapshots(filter)

    async def delete_snapshot(self, snapshot_id: str) -> None:
        """Delete a snapshot by id."""
        await self._sandbox_service.delete_snapshot(snapshot_id)

    async def close(self) -> None:
        """
        Close local resources associated with this sandbox manager.

        This method closes HTTP client resources and other local resources.

        Note: This method logs errors but does not raise exceptions to avoid
        issues in context manager cleanup.
        """
        try:
            # Close transport only when SDK owns it (default transport).
            await self._connection_config.close_transport_if_owned()
        except Exception as e:
            logger.warning(
                f"Error closing resources for sandbox manager: {e}",
                exc_info=True
            )

    async def __aenter__(self) -> "SandboxManager":
        """Async context manager entry."""
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: object,
    ) -> None:
        """Async context manager exit."""
        await self.close()
