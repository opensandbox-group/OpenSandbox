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
"""Pool namespace management helpers."""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable
from uuid import uuid4

from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import (
    PoolDestroyedException,
    PoolDestroyIncompleteException,
)
from opensandbox.manager import SandboxManager
from opensandbox.pool_types import (
    AsyncPoolStateStore,
    PoolDestroyOptions,
    PoolDestroyResult,
    PoolDestroyState,
    PoolDestroyStrategy,
    PoolStateStore,
)
from opensandbox.sync.manager import SandboxManagerSync

logger = logging.getLogger(__name__)


class SandboxPoolManagerSync:
    """Synchronous manager for shared sandbox pool namespaces."""

    def __init__(
        self,
        *,
        state_store: PoolStateStore,
        connection_config: ConnectionConfigSync | None = None,
        owner_id: str | None = None,
        sandbox_manager_factory: Callable[
            [ConnectionConfigSync], SandboxManagerSync
        ] = SandboxManagerSync.create,
    ) -> None:
        self._state_store = state_store
        self._connection_config = connection_config or ConnectionConfigSync()
        self._owner_id = owner_id or f"pool-manager-{uuid4()}"
        self._sandbox_manager_factory = sandbox_manager_factory

    def destroy(
        self,
        pool_name: str,
        options: PoolDestroyOptions | None = None,
    ) -> PoolDestroyResult:
        options = options or PoolDestroyOptions()
        _validate_destroy_options(options)
        state = self._state_store.get_destroy_state(pool_name)
        if state == PoolDestroyState.DESTROYED:
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=0,
                killed_idle_count=0,
                persistent_state_cleared=False,
            )

        try:
            self._state_store.begin_destroy(pool_name, self._owner_id)
        except PoolDestroyedException:
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=0,
                killed_idle_count=0,
                persistent_state_cleared=False,
            )

        manager = self._sandbox_manager_factory(self._connection_for_pool_resource())
        drained = 0
        killed = 0
        drain_timeout_seconds = options.drain_timeout.total_seconds()
        deadline = time.monotonic() + drain_timeout_seconds
        try:
            while True:
                sandbox_id = self._state_store.try_take_idle(pool_name)
                if sandbox_id is None:
                    break
                drained += 1
                try:
                    manager.kill_sandbox(sandbox_id)
                    killed += 1
                except Exception as exc:
                    logger.warning(
                        "destroy: failed to kill idle sandbox: pool_name=%s sandbox_id=%s error=%s",
                        pool_name,
                        sandbox_id,
                        exc,
                    )
                if drain_timeout_seconds > 0 and time.monotonic() > deadline:
                    raise PoolDestroyIncompleteException(
                        f"Pool destroy drain timed out: pool_name={pool_name}"
                    )
            try:
                self._state_store.clear_pool_state(pool_name)
            except Exception as exc:
                raise PoolDestroyIncompleteException(
                    f"Pool destroy failed to clear persistent state: pool_name={pool_name}",
                    exc,
                ) from exc
            try:
                self._state_store.mark_destroyed(
                    pool_name,
                    self._owner_id,
                    options.tombstone_ttl,
                )
            except Exception as exc:
                raise PoolDestroyIncompleteException(
                    f"Pool destroy failed to write tombstone: pool_name={pool_name}",
                    exc,
                ) from exc
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=drained,
                killed_idle_count=killed,
                persistent_state_cleared=True,
            )
        finally:
            manager.close()

    def _connection_for_pool_resource(self) -> ConnectionConfigSync:
        if (
            self._connection_config.transport is not None
            and not self._connection_config._owns_transport
        ):
            return self._connection_config
        config = self._connection_config.model_copy(update={"transport": None})
        config._owns_transport = True
        return config


class SandboxPoolManagerAsync:
    """Async manager for shared sandbox pool namespaces."""

    def __init__(
        self,
        *,
        state_store: AsyncPoolStateStore,
        connection_config: ConnectionConfig | None = None,
        owner_id: str | None = None,
        sandbox_manager_factory: Callable[
            [ConnectionConfig], Awaitable[SandboxManager]
        ] = SandboxManager.create,
    ) -> None:
        self._state_store = state_store
        self._connection_config = connection_config or ConnectionConfig()
        self._owner_id = owner_id or f"pool-manager-{uuid4()}"
        self._sandbox_manager_factory = sandbox_manager_factory

    async def destroy(
        self,
        pool_name: str,
        options: PoolDestroyOptions | None = None,
    ) -> PoolDestroyResult:
        options = options or PoolDestroyOptions()
        _validate_destroy_options(options)
        state = await self._state_store.get_destroy_state(pool_name)
        if state == PoolDestroyState.DESTROYED:
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=0,
                killed_idle_count=0,
                persistent_state_cleared=False,
            )

        try:
            await self._state_store.begin_destroy(pool_name, self._owner_id)
        except PoolDestroyedException:
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=0,
                killed_idle_count=0,
                persistent_state_cleared=False,
            )

        manager = await self._sandbox_manager_factory(self._connection_for_pool_resource())
        drained = 0
        killed = 0
        drain_timeout_seconds = options.drain_timeout.total_seconds()
        deadline = asyncio.get_running_loop().time() + drain_timeout_seconds
        try:
            while True:
                sandbox_id = await self._state_store.try_take_idle(pool_name)
                if sandbox_id is None:
                    break
                drained += 1
                try:
                    await manager.kill_sandbox(sandbox_id)
                    killed += 1
                except Exception as exc:
                    logger.warning(
                        "destroy: failed to kill idle sandbox: pool_name=%s sandbox_id=%s error=%s",
                        pool_name,
                        sandbox_id,
                        exc,
                    )
                if (
                    drain_timeout_seconds > 0
                    and asyncio.get_running_loop().time() > deadline
                ):
                    raise PoolDestroyIncompleteException(
                        f"Pool destroy drain timed out: pool_name={pool_name}"
                    )
            try:
                await self._state_store.clear_pool_state(pool_name)
            except Exception as exc:
                raise PoolDestroyIncompleteException(
                    f"Pool destroy failed to clear persistent state: pool_name={pool_name}",
                    exc,
                ) from exc
            try:
                await self._state_store.mark_destroyed(
                    pool_name,
                    self._owner_id,
                    options.tombstone_ttl,
                )
            except Exception as exc:
                raise PoolDestroyIncompleteException(
                    f"Pool destroy failed to write tombstone: pool_name={pool_name}",
                    exc,
                ) from exc
            return PoolDestroyResult(
                pool_name=pool_name,
                state=PoolDestroyState.DESTROYED,
                drained_idle_count=drained,
                killed_idle_count=killed,
                persistent_state_cleared=True,
            )
        finally:
            await manager.close()

    def _connection_for_pool_resource(self) -> ConnectionConfig:
        if (
            self._connection_config.transport is not None
            and not self._connection_config._owns_transport
        ):
            return self._connection_config
        config = self._connection_config.model_copy(update={"transport": None})
        config._owns_transport = True
        return config


def _validate_destroy_options(options: PoolDestroyOptions) -> None:
    if options.strategy is not PoolDestroyStrategy.FORCE:
        raise ValueError("Only FORCE pool destroy strategy is supported")


SandboxPoolManager = SandboxPoolManagerSync

__all__ = [
    "SandboxPoolManager",
    "SandboxPoolManagerAsync",
    "SandboxPoolManagerSync",
]
