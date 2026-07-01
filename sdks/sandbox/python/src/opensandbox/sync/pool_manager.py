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
Synchronous PoolManager implementation.

Provides a high-level **blocking** interface for creating and managing
pre-warmed sandbox resource pools.  Use PoolManagerSync when you prefer
synchronous code or when running outside of an async event loop.

Usage::

    from opensandbox.sync.pool_manager import PoolManagerSync

    with PoolManagerSync.create() as manager:
        pool = manager.create_pool(
            name="my-pool",
            template={"spec": {"containers": [{"name": "sbx", "image": "python:3.11"}]}},
            buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
        )
        print(pool.name, pool.status)
"""

from __future__ import annotations

import logging
from typing import Any

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.pools import (
    CreatePoolParams,
    PoolCapacitySpec,
    PoolInfo,
    PoolListResponse,
    UpdatePoolParams,
)
from opensandbox.sync.adapters.factory import AdapterFactorySync
from opensandbox.sync.services.pool import PoolsSync

logger = logging.getLogger(__name__)


class PoolManagerSync:
    """
    High-level synchronous interface for managing pre-warmed sandbox resource pools.

    Pools are Kubernetes CRD resources that keep a set of pods pre-warmed,
    reducing sandbox cold-start latency.  This manager exposes simple CRUD
    methods that map to the server's ``/pools`` API.  All methods **block**
    until the underlying HTTP call completes.

    **Creating a manager**::

        manager = PoolManagerSync.create(ConnectionConfigSync(api_key="..."))

    **Using as a context manager** (recommended for automatic cleanup)::

        with PoolManagerSync.create() as manager:
            pool = manager.create_pool(
                name="my-pool",
                template={...},
                buffer_max=3, buffer_min=1, pool_max=10, pool_min=0,
            )

    **Cleanup**: Call ``manager.close()`` (or use the context manager) to
    release the internal httpx transport.

    **Note**: Pool management requires the server to be running with
    ``runtime.type = 'kubernetes'``.  Non-Kubernetes deployments return a
    ``SandboxApiException`` with status 501.
    """

    def __init__(
        self,
        pool_service: PoolsSync,
        connection_config: ConnectionConfigSync,
    ) -> None:
        """
        Internal constructor.  Use :meth:`create` instead.

        Args:
            pool_service: Synchronous pool service implementation.
            connection_config: Connection configuration (shared transport, headers, timeouts).
        """
        self._pool_service = pool_service
        self._connection_config = connection_config

    @property
    def connection_config(self) -> ConnectionConfigSync:
        """Connection configuration used by this manager."""
        return self._connection_config

    @classmethod
    def create(
        cls, connection_config: ConnectionConfigSync | None = None
    ) -> "PoolManagerSync":
        """
        Create a PoolManagerSync with the provided (or default) connection config.

        Args:
            connection_config: Connection configuration.  If ``None``, the default
                               configuration (env vars / defaults) is used.

        Returns:
            Configured PoolManagerSync instance.
        """
        config = (connection_config or ConnectionConfigSync()).with_transport_if_missing()
        factory = AdapterFactorySync(config)
        pool_service = factory.create_pool_service()
        return cls(pool_service, config)

    # ------------------------------------------------------------------
    # Pool CRUD (blocking)
    # ------------------------------------------------------------------

    def create_pool(
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
        Create a new pre-warmed resource pool (blocking).

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
            SandboxException: If the operation fails.
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
        logger.info("Creating pool (sync): %s", name)
        return self._pool_service.create_pool(params)

    def get_pool(self, pool_name: str) -> PoolInfo:
        """
        Retrieve a pool by name (blocking).

        Args:
            pool_name: Name of the pool to retrieve.

        Returns:
            Current PoolInfo including observed runtime status.

        Raises:
            SandboxException: If the operation fails.
        """
        logger.debug("Getting pool (sync): %s", pool_name)
        return self._pool_service.get_pool(pool_name)

    def list_pools(self) -> PoolListResponse:
        """
        List all pools (blocking).

        Returns:
            PoolListResponse containing all pools in the namespace.

        Raises:
            SandboxException: If the operation fails.
        """
        logger.debug("Listing pools (sync)")
        return self._pool_service.list_pools()

    def update_pool(
        self,
        pool_name: str,
        *,
        buffer_max: int,
        buffer_min: int,
        pool_max: int,
        pool_min: int,
    ) -> PoolInfo:
        """
        Update the capacity configuration of an existing pool (blocking).

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
            SandboxException: If the operation fails.
        """
        params = UpdatePoolParams(
            capacitySpec=PoolCapacitySpec(
                bufferMax=buffer_max,
                bufferMin=buffer_min,
                poolMax=pool_max,
                poolMin=pool_min,
            )
        )
        logger.info("Updating pool capacity (sync): %s", pool_name)
        return self._pool_service.update_pool(pool_name, params)

    def delete_pool(self, pool_name: str) -> None:
        """
        Delete a pool (blocking).

        Args:
            pool_name: Name of the pool to delete.

        Raises:
            SandboxException: If the operation fails.
        """
        logger.info("Deleting pool (sync): %s", pool_name)
        self._pool_service.delete_pool(pool_name)

    # ------------------------------------------------------------------
    # Lifecycle
    # ------------------------------------------------------------------

    def close(self) -> None:
        """
        Release HTTP client resources owned by this manager.

        Logs but does not re-raise errors so ``__exit__`` is always safe.
        """
        try:
            self._connection_config.close_transport_if_owned()
        except Exception as e:
            logger.warning(
                "Error closing PoolManagerSync resources: %s", e, exc_info=True
            )

    def __enter__(self) -> "PoolManagerSync":
        """Sync context manager entry."""
        return self

    def __exit__(self, exc_type: Any, exc_val: Any, exc_tb: Any) -> None:
        """Sync context manager exit."""
        self.close()
