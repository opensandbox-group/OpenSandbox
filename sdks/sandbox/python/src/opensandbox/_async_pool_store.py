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
"""Async pool state store implementations."""

from __future__ import annotations

import asyncio
from collections import deque
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone

from opensandbox.exceptions import PoolDestroyedException
from opensandbox.pool_types import (
    IdleEntry,
    PoolDestroyState,
    StoreCounters,
    TakeIdleResult,
)


class InMemoryAsyncPoolStateStore:
    """Single-process asyncio in-memory pool store."""

    def __init__(self) -> None:
        self._default_idle_ttl = timedelta(hours=24)
        self._idle_ttl_by_pool: dict[str, timedelta] = {}
        self._destroy_state_by_pool: dict[str, _DestroyStateEntry] = {}
        self._pools: dict[str, _PoolIdleState] = {}
        self._lock = asyncio.Lock()

    async def try_take_idle(self, pool_name: str) -> str | None:
        async with self._lock:
            state = self._pools.get(pool_name)
            if state is None:
                return None
            now = _now()
            while state.queue:
                sandbox_id = state.queue.popleft()
                entry = state.entries.pop(sandbox_id, None)
                if entry is None:
                    continue
                if entry.expires_at > now:
                    return sandbox_id
            return None

    async def try_take_idle_min_ttl(
        self, pool_name: str, min_remaining_ttl: timedelta
    ) -> TakeIdleResult:
        """Variant of :meth:`try_take_idle` that skips entries with insufficient remaining TTL.

        See :meth:`InMemoryPoolStateStore.try_take_idle_min_ttl` for behavior details.
        """
        if min_remaining_ttl.total_seconds() <= 0:
            return TakeIdleResult(sandbox_id=await self.try_take_idle(pool_name))
        async with self._lock:
            state = self._pools.get(pool_name)
            if state is None:
                return TakeIdleResult(sandbox_id=None)
            now = _now()
            cutoff = now + min_remaining_ttl
            discarded_alive: list[str] = []
            while state.queue:
                sandbox_id = state.queue.popleft()
                entry = state.entries.pop(sandbox_id, None)
                if entry is None:
                    continue
                if entry.expires_at > cutoff:
                    return TakeIdleResult(
                        sandbox_id=sandbox_id,
                        discarded_alive_sandbox_ids=tuple(discarded_alive),
                    )
                if entry.expires_at > now:
                    discarded_alive.append(sandbox_id)
            return TakeIdleResult(
                sandbox_id=None,
                discarded_alive_sandbox_ids=tuple(discarded_alive),
            )

    async def put_idle(self, pool_name: str, sandbox_id: str) -> None:
        if not sandbox_id or not sandbox_id.strip():
            raise ValueError("sandbox_id must not be blank")
        async with self._lock:
            self._reject_if_destroyed_locked(pool_name)
            state = self._pools.setdefault(pool_name, _PoolIdleState())
            expires_at = _now() + self._resolve_idle_ttl(pool_name)
            if sandbox_id not in state.entries:
                state.queue.append(sandbox_id)
            state.entries.setdefault(sandbox_id, IdleEntry(sandbox_id, expires_at))

    async def remove_idle(self, pool_name: str, sandbox_id: str) -> None:
        async with self._lock:
            state = self._pools.get(pool_name)
            if state is not None:
                state.entries.pop(sandbox_id, None)

    async def try_acquire_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        if await self.get_destroy_state(pool_name) is not PoolDestroyState.ACTIVE:
            return False
        return True

    async def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        if await self.get_destroy_state(pool_name) is not PoolDestroyState.ACTIVE:
            return False
        return True

    async def release_primary_lock(self, pool_name: str, owner_id: str) -> None:
        return None

    async def reap_expired_idle(self, pool_name: str, now: datetime) -> None:
        async with self._lock:
            self._reap_locked(pool_name, now)

    async def reap_expired_idle_min_ttl(
        self, pool_name: str, now: datetime, min_remaining_ttl: timedelta
    ) -> tuple[str, ...]:
        """Variant of :meth:`reap_expired_idle` that also evicts near-expiry entries.

        Returns IDs of **alive** entries (server-side TTL has not elapsed) that were evicted
        because their remaining TTL fell below ``min_remaining_ttl``. Already-expired entries
        are still evicted but excluded from the return value — the server has reaped them.
        """
        if min_remaining_ttl.total_seconds() <= 0:
            await self.reap_expired_idle(pool_name, now)
            return ()
        cutoff = now + min_remaining_ttl
        discarded_alive: list[str] = []
        async with self._lock:
            state = self._pools.get(pool_name)
            if state is None:
                return ()
            for sandbox_id, entry in list(state.entries.items()):
                if entry.expires_at > cutoff:
                    continue
                state.entries.pop(sandbox_id, None)
                if entry.expires_at > now:
                    discarded_alive.append(sandbox_id)
            state.queue = deque(
                sandbox_id for sandbox_id in state.queue if sandbox_id in state.entries
            )
        return tuple(discarded_alive)

    async def snapshot_counters(self, pool_name: str) -> StoreCounters:
        async with self._lock:
            self._reap_locked(pool_name, _now())
            state = self._pools.get(pool_name)
            return StoreCounters(idle_count=0 if state is None else len(state.entries))

    async def snapshot_idle_entries(self, pool_name: str) -> list[IdleEntry]:
        async with self._lock:
            self._reap_locked(pool_name, _now())
            state = self._pools.get(pool_name)
            if state is None:
                return []
            return [
                entry
                for sandbox_id in state.queue
                if (entry := state.entries.get(sandbox_id)) is not None
            ]

    async def get_max_idle(self, pool_name: str) -> int | None:
        return None

    async def set_max_idle(self, pool_name: str, max_idle: int) -> None:
        async with self._lock:
            self._reject_if_destroyed_locked(pool_name)
        return None

    async def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None:
        if idle_ttl.total_seconds() <= 0:
            raise ValueError("idle_ttl must be positive")
        async with self._lock:
            self._reject_if_destroyed_locked(pool_name)
            self._idle_ttl_by_pool[pool_name] = idle_ttl

    async def get_destroy_state(self, pool_name: str) -> PoolDestroyState:
        async with self._lock:
            entry = self._destroy_state_by_pool.get(pool_name)
            if entry is None:
                return PoolDestroyState.ACTIVE
            if entry.expires_at is not None and entry.expires_at <= _now():
                self._destroy_state_by_pool.pop(pool_name, None)
                return PoolDestroyState.ACTIVE
            return entry.state

    async def begin_destroy(self, pool_name: str, owner_id: str) -> None:
        if not owner_id or not owner_id.strip():
            raise ValueError("owner_id must not be blank")
        async with self._lock:
            existing = self._get_destroy_state_locked(pool_name)
            if existing is PoolDestroyState.DESTROYED:
                raise PoolDestroyedException(
                    f"Pool namespace is already DESTROYED: pool_name={pool_name}"
                )
            self._destroy_state_by_pool[pool_name] = _DestroyStateEntry(
                state=PoolDestroyState.DESTROYING,
                expires_at=None,
                owner_id=owner_id,
            )

    async def clear_pool_state(self, pool_name: str) -> None:
        async with self._lock:
            self._pools.pop(pool_name, None)
            self._idle_ttl_by_pool.pop(pool_name, None)

    async def mark_destroyed(
        self, pool_name: str, owner_id: str, tombstone_ttl: timedelta | None
    ) -> None:
        if not owner_id or not owner_id.strip():
            raise ValueError("owner_id must not be blank")
        if tombstone_ttl is not None and tombstone_ttl.total_seconds() <= 0:
            raise ValueError("tombstone_ttl must be positive")
        async with self._lock:
            self._destroy_state_by_pool[pool_name] = _DestroyStateEntry(
                state=PoolDestroyState.DESTROYED,
                expires_at=None if tombstone_ttl is None else _now() + tombstone_ttl,
                owner_id=owner_id,
            )

    def _resolve_idle_ttl(self, pool_name: str) -> timedelta:
        return self._idle_ttl_by_pool.get(pool_name, self._default_idle_ttl)

    def _reap_locked(self, pool_name: str, now: datetime) -> None:
        state = self._pools.get(pool_name)
        if state is None:
            return
        expired = [
            sandbox_id
            for sandbox_id, entry in state.entries.items()
            if entry.expires_at <= now
        ]
        for sandbox_id in expired:
            state.entries.pop(sandbox_id, None)
        if expired:
            state.queue = deque(
                sandbox_id for sandbox_id in state.queue if sandbox_id in state.entries
            )

    def _get_destroy_state_locked(self, pool_name: str) -> PoolDestroyState:
        entry = self._destroy_state_by_pool.get(pool_name)
        if entry is None:
            return PoolDestroyState.ACTIVE
        if entry.expires_at is not None and entry.expires_at <= _now():
            self._destroy_state_by_pool.pop(pool_name, None)
            return PoolDestroyState.ACTIVE
        return entry.state

    def _reject_if_destroyed_locked(self, pool_name: str) -> None:
        state = self._get_destroy_state_locked(pool_name)
        if state is not PoolDestroyState.ACTIVE:
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={pool_name}"
            )


@dataclass
class _PoolIdleState:
    entries: dict[str, IdleEntry] = field(default_factory=dict)
    queue: deque[str] = field(default_factory=deque)


@dataclass(frozen=True)
class _DestroyStateEntry:
    state: PoolDestroyState
    expires_at: datetime | None
    owner_id: str


def _now() -> datetime:
    return datetime.now(timezone.utc)
