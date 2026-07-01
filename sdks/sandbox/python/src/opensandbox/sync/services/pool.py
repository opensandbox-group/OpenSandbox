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
Synchronous pool service interface.

Defines the contract for **blocking** pool lifecycle operations.
This is the sync counterpart of :mod:`opensandbox.services.pool`.
"""

from typing import Protocol

from opensandbox.models.pools import (
    CreatePoolParams,
    PoolInfo,
    PoolListResponse,
    UpdatePoolParams,
)


class PoolsSync(Protocol):
    """
    Core pool lifecycle management service (sync).

    Provides a clean abstraction over pool creation, management and deletion,
    isolating business logic from API implementation details.
    """

    def create_pool(self, params: CreatePoolParams) -> PoolInfo:
        """
        Create a new pre-warmed resource pool (blocking).

        Args:
            params: Pool creation parameters.

        Returns:
            PoolInfo representing the newly created pool.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def get_pool(self, pool_name: str) -> PoolInfo:
        """
        Retrieve a pool by name (blocking).

        Args:
            pool_name: Name of the pool to retrieve.

        Returns:
            Current PoolInfo including observed runtime status.

        Raises:
            SandboxException: If the operation fails (404 if not found).
        """
        ...

    def list_pools(self) -> PoolListResponse:
        """
        List all pools in the namespace (blocking).

        Returns:
            PoolListResponse containing all pools.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def update_pool(self, pool_name: str, params: UpdatePoolParams) -> PoolInfo:
        """
        Update the capacity configuration of an existing pool (blocking).

        Args:
            pool_name: Name of the pool to update.
            params: New capacity configuration.

        Returns:
            Updated PoolInfo.

        Raises:
            SandboxException: If the operation fails (404 if not found).
        """
        ...

    def delete_pool(self, pool_name: str) -> None:
        """
        Delete a pool (blocking).

        Args:
            pool_name: Name of the pool to delete.

        Raises:
            SandboxException: If the operation fails (404 if not found).
        """
        ...
