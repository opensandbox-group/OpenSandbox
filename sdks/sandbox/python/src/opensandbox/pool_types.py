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
"""Shared sandbox pool types aligned with the Kotlin SDK."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, replace
from datetime import datetime, timedelta
from enum import Enum
from math import ceil
from typing import TYPE_CHECKING, Protocol
from uuid import uuid4

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    PlatformSpec,
    SandboxImageSpec,
    Volume,
)

if TYPE_CHECKING:
    from collections.abc import Awaitable

    from opensandbox.config.connection import ConnectionConfig
    from opensandbox.sandbox import Sandbox
    from opensandbox.sync.sandbox import SandboxSync


class AcquirePolicy(Enum):
    """Policy for acquire when the idle buffer is empty."""

    FAIL_FAST = "FAIL_FAST"
    DIRECT_CREATE = "DIRECT_CREATE"


class PoolState(Enum):
    """High-level state of the sandbox pool."""

    HEALTHY = "HEALTHY"
    DEGRADED = "DEGRADED"
    DRAINING = "DRAINING"
    STOPPED = "STOPPED"


class PoolLifecycleState(Enum):
    """Detailed lifecycle state of one sandbox pool instance."""

    NOT_STARTED = "NOT_STARTED"
    STARTING = "STARTING"
    RUNNING = "RUNNING"
    DRAINING = "DRAINING"
    STOPPED = "STOPPED"


@dataclass(frozen=True)
class IdleEntry:
    sandbox_id: str
    expires_at: datetime


@dataclass(frozen=True)
class StoreCounters:
    idle_count: int


@dataclass(frozen=True)
class PoolSnapshot:
    state: PoolState
    lifecycle_state: PoolLifecycleState
    idle_count: int
    max_idle: int
    failure_count: int
    backoff_active: bool
    last_error: str | None
    in_flight_operations: int


class PoolStateStore(Protocol):
    """Coordination state and idle sandbox membership store."""

    def try_take_idle(self, pool_name: str) -> str | None: ...

    def put_idle(self, pool_name: str, sandbox_id: str) -> None: ...

    def remove_idle(self, pool_name: str, sandbox_id: str) -> None: ...

    def try_acquire_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool: ...

    def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool: ...

    def release_primary_lock(self, pool_name: str, owner_id: str) -> None: ...

    def reap_expired_idle(self, pool_name: str, now: datetime) -> None: ...

    def snapshot_counters(self, pool_name: str) -> StoreCounters: ...

    def snapshot_idle_entries(self, pool_name: str) -> list[IdleEntry]: ...

    def get_max_idle(self, pool_name: str) -> int | None: ...

    def set_max_idle(self, pool_name: str, max_idle: int) -> None: ...

    def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None: ...


class AsyncPoolStateStore(Protocol):
    """Async coordination state and idle sandbox membership store."""

    async def try_take_idle(self, pool_name: str) -> str | None: ...

    async def put_idle(self, pool_name: str, sandbox_id: str) -> None: ...

    async def remove_idle(self, pool_name: str, sandbox_id: str) -> None: ...

    async def try_acquire_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool: ...

    async def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool: ...

    async def release_primary_lock(self, pool_name: str, owner_id: str) -> None: ...

    async def reap_expired_idle(self, pool_name: str, now: datetime) -> None: ...

    async def snapshot_counters(self, pool_name: str) -> StoreCounters: ...

    async def snapshot_idle_entries(self, pool_name: str) -> list[IdleEntry]: ...

    async def get_max_idle(self, pool_name: str) -> int | None: ...

    async def set_max_idle(self, pool_name: str, max_idle: int) -> None: ...

    async def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None: ...


@dataclass(frozen=True)
class PoolCreationSpec:
    """Template for creating sandboxes in the pool."""

    image: SandboxImageSpec | str
    entrypoint: list[str] | None = None
    resource: dict[str, str] | None = None
    env: dict[str, str] | None = None
    metadata: dict[str, str] | None = None
    extensions: dict[str, str] | None = None
    network_policy: NetworkPolicy | None = None
    platform: PlatformSpec | None = None
    secure_access: bool = False
    volumes: list[Volume] | None = None


@dataclass(frozen=True)
class PoolConfig:
    """Configuration for a client-side sandbox pool."""

    pool_name: str
    max_idle: int
    state_store: PoolStateStore
    connection_config: ConnectionConfigSync
    creation_spec: PoolCreationSpec
    owner_id: str | None = None
    warmup_concurrency: int | None = None
    primary_lock_ttl: timedelta = timedelta(seconds=60)
    reconcile_interval: timedelta = timedelta(seconds=30)
    degraded_threshold: int = 3
    acquire_ready_timeout: timedelta = timedelta(seconds=30)
    acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200)
    acquire_health_check: Callable[[SandboxSync], bool] | None = None
    acquire_skip_health_check: bool = False
    warmup_ready_timeout: timedelta = timedelta(seconds=30)
    warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=200)
    warmup_health_check: Callable[[SandboxSync], bool] | None = None
    warmup_sandbox_preparer: Callable[[SandboxSync], None] | None = None
    warmup_skip_health_check: bool = False
    idle_timeout: timedelta = timedelta(hours=24)
    drain_timeout: timedelta = timedelta(seconds=30)

    def __post_init__(self) -> None:
        owner_id = self.owner_id or f"pool-owner-{uuid4()}"
        warmup_concurrency = self.warmup_concurrency
        if warmup_concurrency is None:
            warmup_concurrency = max(1, ceil(self.max_idle * 0.2))
        object.__setattr__(self, "owner_id", owner_id)
        object.__setattr__(self, "warmup_concurrency", warmup_concurrency)

        _require_text(self.pool_name, "pool_name must not be blank")
        _require_text(owner_id, "owner_id must not be blank")
        if self.max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        if warmup_concurrency <= 0:
            raise ValueError("warmup_concurrency must be positive")
        if self.degraded_threshold <= 0:
            raise ValueError("degraded_threshold must be positive")
        _require_positive(self.primary_lock_ttl, "primary_lock_ttl must be positive")
        _require_positive(self.reconcile_interval, "reconcile_interval must be positive")
        _require_positive(
            self.acquire_ready_timeout, "acquire_ready_timeout must be positive"
        )
        _require_positive(
            self.acquire_health_check_polling_interval,
            "acquire_health_check_polling_interval must be positive",
        )
        _require_positive(
            self.warmup_ready_timeout, "warmup_ready_timeout must be positive"
        )
        _require_positive(
            self.warmup_health_check_polling_interval,
            "warmup_health_check_polling_interval must be positive",
        )
        _require_positive(self.idle_timeout, "idle_timeout must be positive")
        if self.drain_timeout.total_seconds() < 0:
            raise ValueError("drain_timeout must be non-negative")

    def with_max_idle(self, max_idle: int) -> PoolConfig:
        return replace(self, max_idle=max_idle)


@dataclass(frozen=True)
class AsyncPoolConfig:
    """Configuration for an asyncio client-side sandbox pool."""

    pool_name: str
    max_idle: int
    state_store: AsyncPoolStateStore
    connection_config: ConnectionConfig
    creation_spec: PoolCreationSpec
    owner_id: str | None = None
    warmup_concurrency: int | None = None
    primary_lock_ttl: timedelta = timedelta(seconds=60)
    reconcile_interval: timedelta = timedelta(seconds=30)
    degraded_threshold: int = 3
    acquire_ready_timeout: timedelta = timedelta(seconds=30)
    acquire_health_check_polling_interval: timedelta = timedelta(milliseconds=200)
    acquire_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None
    acquire_skip_health_check: bool = False
    warmup_ready_timeout: timedelta = timedelta(seconds=30)
    warmup_health_check_polling_interval: timedelta = timedelta(milliseconds=200)
    warmup_health_check: Callable[[Sandbox], Awaitable[bool]] | None = None
    warmup_sandbox_preparer: Callable[[Sandbox], Awaitable[None]] | None = None
    warmup_skip_health_check: bool = False
    idle_timeout: timedelta = timedelta(hours=24)
    drain_timeout: timedelta = timedelta(seconds=30)

    def __post_init__(self) -> None:
        owner_id = self.owner_id or f"pool-owner-{uuid4()}"
        warmup_concurrency = self.warmup_concurrency
        if warmup_concurrency is None:
            warmup_concurrency = max(1, ceil(self.max_idle * 0.2))
        object.__setattr__(self, "owner_id", owner_id)
        object.__setattr__(self, "warmup_concurrency", warmup_concurrency)

        _require_text(self.pool_name, "pool_name must not be blank")
        _require_text(owner_id, "owner_id must not be blank")
        if self.max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        if warmup_concurrency <= 0:
            raise ValueError("warmup_concurrency must be positive")
        if self.degraded_threshold <= 0:
            raise ValueError("degraded_threshold must be positive")
        _require_positive(self.primary_lock_ttl, "primary_lock_ttl must be positive")
        _require_positive(self.reconcile_interval, "reconcile_interval must be positive")
        _require_positive(
            self.acquire_ready_timeout, "acquire_ready_timeout must be positive"
        )
        _require_positive(
            self.acquire_health_check_polling_interval,
            "acquire_health_check_polling_interval must be positive",
        )
        _require_positive(
            self.warmup_ready_timeout, "warmup_ready_timeout must be positive"
        )
        _require_positive(
            self.warmup_health_check_polling_interval,
            "warmup_health_check_polling_interval must be positive",
        )
        _require_positive(self.idle_timeout, "idle_timeout must be positive")
        if self.drain_timeout.total_seconds() < 0:
            raise ValueError("drain_timeout must be non-negative")

    def with_max_idle(self, max_idle: int) -> AsyncPoolConfig:
        return replace(self, max_idle=max_idle)


def _require_text(value: str, message: str) -> None:
    if not value or not value.strip():
        raise ValueError(message)


def _require_positive(value: timedelta, message: str) -> None:
    if value.total_seconds() <= 0:
        raise ValueError(message)
