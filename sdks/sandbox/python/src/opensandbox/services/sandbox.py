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
Sandbox service interface.

Protocol for sandbox lifecycle management operations.
"""

from datetime import datetime, timedelta
from typing import Protocol

from opensandbox.models.sandboxes import (
    CreateSnapshotRequest,
    NetworkPolicy,
    PagedSandboxInfos,
    PagedSnapshotInfos,
    PlatformSpec,
    SandboxCreateResponse,
    SandboxEndpoint,
    SandboxFilter,
    SandboxImageSpec,
    SandboxInfo,
    SandboxRenewResponse,
    SnapshotFilter,
    SnapshotInfo,
    Volume,
)


class Sandboxes(Protocol):
    """
    Core sandbox lifecycle management service.

    This service provides a clean abstraction over sandbox creation, management,
    and termination operations, completely isolating business logic from API implementation details.
    """

    async def create_sandbox(
        self,
        spec: SandboxImageSpec | None,
        entrypoint: list[str] | None,
        env: dict[str, str],
        metadata: dict[str, str],
        timeout: timedelta | None,
        resource: dict[str, str],
        network_policy: NetworkPolicy | None,
        extensions: dict[str, str],
        volumes: list[Volume] | None,
        platform: PlatformSpec | None = None,
        secure_access: bool = False,
        snapshot_id: str | None = None,
    ) -> SandboxCreateResponse:
        """
        Create a new sandbox with the specified configuration.

        Args:
            spec: Container image specification for provisioning the sandbox.
            entrypoint: Command to run as the sandbox's main process.
            env: Environment variables injected into the sandbox runtime.
            metadata: User-defined metadata used for management and filtering.
            timeout: Sandbox lifetime. Pass None to create a sandbox that requires explicit cleanup.
            resource: Runtime resource limits (e.g. cpu/memory). Exact semantics are server-defined.
            network_policy: Optional outbound network policy (egress).
            extensions: Opaque extension parameters passed through to the server as-is.
                Prefer namespaced keys (e.g. ``storage.id``).
            volumes: Optional list of volume mounts for persistent storage.
            secure_access: Whether to enable secured access for sandbox endpoints.

        Returns:
            Sandbox create response

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def get_sandbox_info(self, sandbox_id: str) -> SandboxInfo:
        """
        Retrieve information about an existing sandbox.

        Args:
            sandbox_id: Unique identifier of the sandbox

        Returns:
            Current sandbox information

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def list_sandboxes(self, filter: SandboxFilter) -> PagedSandboxInfos:
        """
        List sandboxes with optional filtering.

        Args:
            filter: Optional filter criteria

        Returns:
            List of sandbox information matching the filter

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def patch_sandbox_metadata(
        self, sandbox_id: str, patch: dict[str, str | None]
    ) -> SandboxInfo:
        """
        Patch sandbox metadata.

        Args:
            sandbox_id: Unique identifier of the sandbox
            patch: Metadata merge patch. String values add or replace keys; None deletes keys.

        Returns:
            Current sandbox information after applying the patch

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def get_sandbox_endpoint(
        self, sandbox_id: str, port: int, use_server_proxy: bool = False
    ) -> SandboxEndpoint:
        """
        Get sandbox endpoint.

        Args:
            sandbox_id: Sandbox ID
            port: Endpoint port number
            use_server_proxy: Whether to use server proxy for endpoint

        Returns:
            Target sandbox endpoint

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def get_signed_sandbox_endpoint(
        self, sandbox_id: str, port: int, expires: int,
        use_server_proxy: bool = False,
    ) -> SandboxEndpoint:
        """
        Get signed sandbox endpoint with an OSEP-0011 route token.

        Args:
            sandbox_id: Sandbox ID
            port: Endpoint port number
            expires: Unix epoch seconds for the signed route token expiry
            use_server_proxy: Whether to use server proxy for endpoint

        Returns:
            Target sandbox endpoint

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def pause_sandbox(self, sandbox_id: str) -> None:
        """
        Pause a running sandbox, preserving its state.

        Args:
            sandbox_id: Unique identifier of the sandbox

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def resume_sandbox(self, sandbox_id: str) -> None:
        """
        Resume a paused sandbox.

        Args:
            sandbox_id: Unique identifier of the sandbox

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def renew_sandbox_expiration(
        self, sandbox_id: str, new_expiration_time: datetime
    ) -> SandboxRenewResponse:
        """
        Renew the expiration time of a sandbox.

        Args:
            sandbox_id: Unique identifier of the sandbox
            new_expiration_time: New expiration timestamp

        Returns:
            Renew response including the new expiration time.

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def kill_sandbox(self, sandbox_id: str) -> None:
        """
        Terminate a sandbox and release all associated resources.

        Args:
            sandbox_id: Unique identifier of the sandbox

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def create_snapshot(
        self, sandbox_id: str, request: CreateSnapshotRequest | None = None
    ) -> SnapshotInfo:
        """Create a persistent snapshot from a sandbox."""
        ...

    async def get_snapshot(self, snapshot_id: str) -> SnapshotInfo:
        """Retrieve information about an existing snapshot."""
        ...

    async def list_snapshots(self, filter: SnapshotFilter) -> PagedSnapshotInfos:
        """List snapshots with optional filtering."""
        ...

    async def delete_snapshot(self, snapshot_id: str) -> None:
        """Delete a snapshot."""
        ...
