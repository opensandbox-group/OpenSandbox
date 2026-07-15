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
from typing import TYPE_CHECKING, Protocol, cast
from uuid import uuid4

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    PlatformSpec,
    SandboxImageSpec,
    Volume,
)

if TYPE_CHECKING:
    from collections.abc import Awaitable, Iterable

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


class PoolDestroyState(Enum):
    """Shared destroy lifecycle for one pool namespace."""

    ACTIVE = "ACTIVE"
    DESTROYING = "DESTROYING"
    DESTROYED = "DESTROYED"


class PoolDestroyStrategy(Enum):
    """Destroy strategy. V1 implements FORCE only."""

    FORCE = "FORCE"


@dataclass(frozen=True)
class PoolDestroyOptions:
    """Options for destroying a pool namespace."""

    strategy: PoolDestroyStrategy = PoolDestroyStrategy.FORCE
    tombstone_ttl: timedelta | None = timedelta(days=7)
    drain_timeout: timedelta = timedelta(seconds=30)

    def __post_init__(self) -> None:
        if self.tombstone_ttl is not None:
            _require_positive(self.tombstone_ttl, "tombstone_ttl must be positive")
        if self.drain_timeout.total_seconds() < 0:
            raise ValueError("drain_timeout must be non-negative")


@dataclass(frozen=True)
class PoolDestroyResult:
    """Result of a successful pool namespace destroy operation."""

    pool_name: str
    state: PoolDestroyState
    drained_idle_count: int
    killed_idle_count: int
    persistent_state_cleared: bool


@dataclass(frozen=True)
class IdleEntry:
    sandbox_id: str
    expires_at: datetime


@dataclass(frozen=True)
class StoreCounters:
    idle_count: int


@dataclass(frozen=True)
class TakeIdleResult:
    """Result of a near-expiry-aware ``try_take_idle``.

    ``sandbox_id`` is the chosen idle sandbox ID, or ``None`` if no entry satisfied the threshold.
    ``discarded_alive_sandbox_ids`` lists IDs that were skipped because their remaining TTL was
    below the configured ``min_remaining_ttl``. Those sandboxes are still **alive on the server**
    (their server-side TTL has not elapsed yet) — callers should best-effort terminate them.

    Already-expired entries (server-side TTL has elapsed) are intentionally excluded: the server
    has already reaped them and a kill call would be a wasted round-trip.
    """

    sandbox_id: str | None
    discarded_alive_sandbox_ids: tuple[str, ...] = ()


class PooledSandboxCreateReason(Enum):
    """Why the pool is creating a sandbox."""

    WARMUP = "WARMUP"
    DIRECT_CREATE = "DIRECT_CREATE"


@dataclass(frozen=True)
class PooledSandboxCreateContext:
    """Context passed to a sandbox creator when the pool needs a new sandbox."""

    pool_name: str
    owner_id: str
    idle_timeout: timedelta
    reason: PooledSandboxCreateReason
    ready_timeout: timedelta
    health_check_polling_interval: timedelta
    skip_health_check: bool
    health_check: (
        Callable[[SandboxSync], bool] | Callable[[Sandbox], Awaitable[bool]] | None
    )
    connection_config: ConnectionConfigSync | ConnectionConfig


class PooledSandboxCreator(Protocol):
    """Creates a sandbox for the pool."""

    def __call__(self, context: PooledSandboxCreateContext) -> SandboxSync: ...


class AsyncPooledSandboxCreator(Protocol):
    """Async counterpart of :class:`PooledSandboxCreator`."""

    async def __call__(self, context: PooledSandboxCreateContext) -> Sandbox: ...


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
    """Coordination state and idle sandbox membership store.

    The ``*_min_ttl`` overloads are optional: callers must use
    :func:`try_take_idle_min_ttl` and :func:`reap_expired_idle_min_ttl` to
    obtain near-expiry filtering, but stores predating those methods continue
    to work via the binary-expiry fallback in the call sites.
    """

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

    def get_destroy_state(self, pool_name: str) -> PoolDestroyState: ...

    def begin_destroy(self, pool_name: str, owner_id: str) -> None: ...

    def clear_pool_state(self, pool_name: str) -> None: ...

    def mark_destroyed(
        self, pool_name: str, owner_id: str, tombstone_ttl: timedelta | None
    ) -> None: ...


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

    async def get_destroy_state(self, pool_name: str) -> PoolDestroyState: ...

    async def begin_destroy(self, pool_name: str, owner_id: str) -> None: ...

    async def clear_pool_state(self, pool_name: str) -> None: ...

    async def mark_destroyed(
        self, pool_name: str, owner_id: str, tombstone_ttl: timedelta | None
    ) -> None: ...


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
    acquire_min_remaining_ttl: timedelta | None = None
    sandbox_creator: PooledSandboxCreator | None = None

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
        _require_positive(
            self.reconcile_interval, "reconcile_interval must be positive"
        )
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
        resolved_min_ttl = self.acquire_min_remaining_ttl
        if resolved_min_ttl is None:
            resolved_min_ttl = _default_acquire_min_remaining_ttl(self.idle_timeout)
        object.__setattr__(self, "acquire_min_remaining_ttl", resolved_min_ttl)
        _require_acquire_min_remaining_ttl(resolved_min_ttl, self.idle_timeout)

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
    acquire_min_remaining_ttl: timedelta | None = None
    sandbox_creator: AsyncPooledSandboxCreator | None = None

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
        _require_positive(
            self.reconcile_interval, "reconcile_interval must be positive"
        )
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
        resolved_min_ttl = self.acquire_min_remaining_ttl
        if resolved_min_ttl is None:
            resolved_min_ttl = _default_acquire_min_remaining_ttl(self.idle_timeout)
        object.__setattr__(self, "acquire_min_remaining_ttl", resolved_min_ttl)
        _require_acquire_min_remaining_ttl(resolved_min_ttl, self.idle_timeout)

    def with_max_idle(self, max_idle: int) -> AsyncPoolConfig:
        return replace(self, max_idle=max_idle)


def _require_text(value: str, message: str) -> None:
    if not value or not value.strip():
        raise ValueError(message)


def _require_positive(value: timedelta, message: str) -> None:
    if value.total_seconds() <= 0:
        raise ValueError(message)


def try_take_idle_with_min_ttl(
    store: PoolStateStore, pool_name: str, min_remaining_ttl: timedelta | None
) -> TakeIdleResult:
    """Call ``store.try_take_idle_min_ttl`` if available, else fall back to ``try_take_idle``.

    Pool stores added before #983 only implement :meth:`PoolStateStore.try_take_idle`.
    This helper preserves source compatibility for those stores: when the threshold is
    ``None``, zero, or negative — or the store does not implement the variant — the
    binary-expiry path is used and the returned result has an empty
    ``discarded_alive_sandbox_ids``.
    """
    if min_remaining_ttl is None or min_remaining_ttl.total_seconds() <= 0:
        return TakeIdleResult(sandbox_id=store.try_take_idle(pool_name))
    method = getattr(store, "try_take_idle_min_ttl", None)
    if callable(method):
        return cast(TakeIdleResult, method(pool_name, min_remaining_ttl))
    return TakeIdleResult(sandbox_id=store.try_take_idle(pool_name))


async def try_take_idle_with_min_ttl_async(
    store: AsyncPoolStateStore, pool_name: str, min_remaining_ttl: timedelta | None
) -> TakeIdleResult:
    """Async counterpart of :func:`try_take_idle_with_min_ttl`."""
    if min_remaining_ttl is None or min_remaining_ttl.total_seconds() <= 0:
        return TakeIdleResult(sandbox_id=await store.try_take_idle(pool_name))
    method = getattr(store, "try_take_idle_min_ttl", None)
    if callable(method):
        coro = cast("Awaitable[TakeIdleResult]", method(pool_name, min_remaining_ttl))
        return await coro
    return TakeIdleResult(sandbox_id=await store.try_take_idle(pool_name))


def reap_expired_idle_with_min_ttl(
    store: PoolStateStore,
    pool_name: str,
    now: datetime,
    min_remaining_ttl: timedelta | None,
) -> tuple[str, ...]:
    """Call ``store.reap_expired_idle_min_ttl`` if available, else fall back.

    Returns the IDs of alive sandboxes the store dropped because their remaining TTL fell
    below the threshold, so callers can kill them. Stores predating this method, or callers
    passing ``None`` / zero / negative, get an empty tuple.
    """
    if min_remaining_ttl is None or min_remaining_ttl.total_seconds() <= 0:
        store.reap_expired_idle(pool_name, now)
        return ()
    method = getattr(store, "reap_expired_idle_min_ttl", None)
    if callable(method):
        result = cast("Iterable[str] | None", method(pool_name, now, min_remaining_ttl))
        return tuple(result) if result else ()
    store.reap_expired_idle(pool_name, now)
    return ()


async def reap_expired_idle_with_min_ttl_async(
    store: AsyncPoolStateStore,
    pool_name: str,
    now: datetime,
    min_remaining_ttl: timedelta | None,
) -> tuple[str, ...]:
    """Async counterpart of :func:`reap_expired_idle_with_min_ttl`."""
    if min_remaining_ttl is None or min_remaining_ttl.total_seconds() <= 0:
        await store.reap_expired_idle(pool_name, now)
        return ()
    method = getattr(store, "reap_expired_idle_min_ttl", None)
    if callable(method):
        coro = cast(
            "Awaitable[Iterable[str] | None]", method(pool_name, now, min_remaining_ttl)
        )
        result = await coro
        return tuple(result) if result else ()
    await store.reap_expired_idle(pool_name, now)
    return ()


_DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP = timedelta(seconds=60)


def _default_acquire_min_remaining_ttl(idle_timeout: timedelta) -> timedelta:
    """Resolve the default ``acquire_min_remaining_ttl`` from ``idle_timeout``.

    Returns ``min(60s, idle_timeout / 2)``. Always strictly less than ``idle_timeout``,
    so existing users with short idle timeouts get a scaled-down threshold instead of
    a config-time error from a hidden 60s default.
    """
    half = idle_timeout / 2
    return (
        _DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP
        if _DEFAULT_ACQUIRE_MIN_REMAINING_TTL_CAP < half
        else half
    )


def _require_acquire_min_remaining_ttl(
    acquire_min_remaining_ttl: timedelta, idle_timeout: timedelta
) -> None:
    """Validate ``acquire_min_remaining_ttl``.

    Must be non-negative and strictly less than ``idle_timeout``; otherwise every
    freshly warmed idle entry would fail the threshold and the pool would discard
    its entire idle buffer on each acquire.
    """
    if acquire_min_remaining_ttl.total_seconds() < 0:
        raise ValueError("acquire_min_remaining_ttl must be non-negative")
    if acquire_min_remaining_ttl >= idle_timeout:
        raise ValueError(
            "acquire_min_remaining_ttl "
            f"({acquire_min_remaining_ttl}) must be strictly less than "
            f"idle_timeout ({idle_timeout}); otherwise every warmed idle entry "
            "would be rejected"
        )
