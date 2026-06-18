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
Pool service interface.

Protocol for pool lifecycle management operations.
"""

from typing import Protocol

from opensandbox.models.pools import (
    CreatePoolParams,
    PoolInfo,
    PoolListResponse,
    UpdatePoolParams,
)


class Pools(Protocol):
    """
    Pool management service protocol.

    Abstracts CRUD operations for Pool resources, completely isolating
    business logic from HTTP/API implementation details.
    """

    async def create_pool(self, params: CreatePoolParams) -> PoolInfo:
        """
        Create a new pre-warmed resource pool.

        Args:
            params: Pool creation parameters including name, template, and capacity.

        Returns:
            PoolInfo for the newly created pool.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def get_pool(self, pool_name: str) -> PoolInfo:
        """
        Retrieve a pool by name.

        Args:
            pool_name: Name of the pool to retrieve.

        Returns:
            Current PoolInfo including observed runtime status.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def list_pools(self) -> PoolListResponse:
        """
        List all pools.

        Returns:
            PoolListResponse containing all pools.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def update_pool(self, pool_name: str, params: UpdatePoolParams) -> PoolInfo:
        """
        Update the capacity configuration of an existing pool.

        Only ``capacity_spec`` can be changed after creation.

        Args:
            pool_name: Name of the pool to update.
            params: Update parameters with the new capacity spec.

        Returns:
            Updated PoolInfo.

        Raises:
            SandboxException: if the operation fails.
        """
        ...

    async def delete_pool(self, pool_name: str) -> None:
        """
        Delete a pool.

        Args:
            pool_name: Name of the pool to delete.

        Raises:
            SandboxException: if the operation fails.
        """
        ...
