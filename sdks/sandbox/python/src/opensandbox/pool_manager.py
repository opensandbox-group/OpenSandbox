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
Pool management interface for administrative pool operations.

Provides a high-level async interface for creating and managing pre-warmed
sandbox resource pools.  Use PoolManager when you want to manage the pool
infrastructure independently of individual sandbox instances.

Usage::

    async with await PoolManager.create() as manager:
        pool = await manager.create_pool(
            name="my-pool",
            template={"spec": {"containers": [{"name": "sbx", "image": "python:3.11"}]}},
            buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
        )
        print(pool.name, pool.status)
"""

from __future__ import annotations

import logging
from typing import Any

from opensandbox.adapters.factory import AdapterFactory
from opensandbox.config import ConnectionConfig
from opensandbox.models.pools import (
    CreatePoolParams,
    PoolCapacitySpec,
    PoolInfo,
    PoolListResponse,
    UpdatePoolParams,
)
from opensandbox.services.pool import Pools

logger = logging.getLogger(__name__)


class PoolManager:
    """
    High-level async interface for managing pre-warmed sandbox resource pools.

    Pools are Kubernetes CRD resources that keep a set of pods pre-warmed,
    reducing sandbox cold-start latency.  This manager exposes simple CRUD
    methods that map to the server's ``/pools`` API.

    **Creating a manager**::

        manager = await PoolManager.create(connection_config=config)

    **Using as an async context manager** (recommended)::

        async with await PoolManager.create() as manager:
            pool = await manager.create_pool(
                name="my-pool",
                template={...},
                buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
            )

    **Cleanup**: Call ``await manager.close()`` (or use the async context manager)
    to release HTTP client resources.

    **Note**: Pool management requires the server to be running with
    ``runtime.type = 'kubernetes'``.  Non-Kubernetes deployments return a
    ``SandboxApiException`` with status 501.
    """

    def __init__(
        self,
        pool_service: Pools,
        connection_config: ConnectionConfig,
    ) -> None:
        self._pool_service = pool_service
        self._connection_config = connection_config

    @property
    def connection_config(self) -> ConnectionConfig:
        """Connection configuration used by this manager."""
        return self._connection_config

    @classmethod
    async def create(
        cls, connection_config: ConnectionConfig | None = None
    ) -> "PoolManager":
        """
        Create a PoolManager with the provided (or default) connection config.

        Args:
            connection_config: Connection configuration. If ``None``, the default
                               configuration (env vars / defaults) is used.

        Returns:
            Configured PoolManager instance.
        """
        config = (connection_config or ConnectionConfig()).with_transport_if_missing()
        factory = AdapterFactory(config)
        pool_service = factory.create_pool_service()
        return cls(pool_service, config)

    # ------------------------------------------------------------------
    # Pool CRUD
    # ------------------------------------------------------------------

    async def create_pool(
        self,
        name: str,
        template: dict[str, Any],
        *,
        buffer_max: int,
        buffer_min: int,
        pool_max: int,
        pool_min: int,
    ) -> PoolInfo:
        """
        Create a new pre-warmed resource pool.

        Args:
            name: Unique pool name (must be a valid Kubernetes resource name).
            template: Kubernetes PodTemplateSpec dict for the pre-warmed pods.
            buffer_max: Maximum number of pods in the warm buffer.
            buffer_min: Minimum number of pods in the warm buffer.
            pool_max: Maximum total pool size.
            pool_min: Minimum total pool size.

        Returns:
            PoolInfo representing the newly created pool.

        Raises:
            SandboxException: if the operation fails.
        """
        params = CreatePoolParams(
            name=name,
            template=template,
            capacitySpec=PoolCapacitySpec(
                bufferMax=buffer_max,
                bufferMin=buffer_min,
                poolMax=pool_max,
                poolMin=pool_min,
            ),
        )
        logger.info("Creating pool: %s", name)
        return await self._pool_service.create_pool(params)

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
        logger.debug("Getting pool: %s", pool_name)
        return await self._pool_service.get_pool(pool_name)

    async def list_pools(self) -> PoolListResponse:
        """
        List all pools.

        Returns:
            PoolListResponse containing all pools in the namespace.

        Raises:
            SandboxException: if the operation fails.
        """
        logger.debug("Listing pools")
        return await self._pool_service.list_pools()

    async def update_pool(
        self,
        pool_name: str,
        *,
        buffer_max: int,
        buffer_min: int,
        pool_max: int,
        pool_min: int,
    ) -> PoolInfo:
        """
        Update the capacity configuration of an existing pool.

        Only capacity values can be changed after creation.  To change the
        pod template, delete and recreate the pool.

        Args:
            pool_name: Name of the pool to update.
            buffer_max: New maximum warm-buffer size.
            buffer_min: New minimum warm-buffer size.
            pool_max: New maximum total pool size.
            pool_min: New minimum total pool size.

        Returns:
            Updated PoolInfo.

        Raises:
            SandboxException: if the operation fails.
        """
        params = UpdatePoolParams(
            capacitySpec=PoolCapacitySpec(
                bufferMax=buffer_max,
                bufferMin=buffer_min,
                poolMax=pool_max,
                poolMin=pool_min,
            )
        )
        logger.info("Updating pool capacity: %s", pool_name)
        return await self._pool_service.update_pool(pool_name, params)

    async def delete_pool(self, pool_name: str) -> None:
        """
        Delete a pool.

        Args:
            pool_name: Name of the pool to delete.

        Raises:
            SandboxException: if the operation fails.
        """
        logger.info("Deleting pool: %s", pool_name)
        await self._pool_service.delete_pool(pool_name)

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    async def close(self) -> None:
        """Release HTTP client resources owned by this manager."""
        try:
            await self._connection_config.close_transport_if_owned()
        except Exception as e:
            logger.warning("Error closing PoolManager resources: %s", e, exc_info=True)

    async def __aenter__(self) -> "PoolManager":
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: object,
    ) -> None:
        await self.close()
