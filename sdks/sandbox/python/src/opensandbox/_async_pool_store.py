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

from opensandbox.pool_types import IdleEntry, StoreCounters


class InMemoryAsyncPoolStateStore:
    """Single-process asyncio in-memory pool store."""

    def __init__(self) -> None:
        self._default_idle_ttl = timedelta(hours=24)
        self._idle_ttl_by_pool: dict[str, timedelta] = {}
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

    async def put_idle(self, pool_name: str, sandbox_id: str) -> None:
        if not sandbox_id or not sandbox_id.strip():
            raise ValueError("sandbox_id must not be blank")
        async with self._lock:
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
        return True

    async def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        return True

    async def release_primary_lock(self, pool_name: str, owner_id: str) -> None:
        return None

    async def reap_expired_idle(self, pool_name: str, now: datetime) -> None:
        async with self._lock:
            self._reap_locked(pool_name, now)

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
        return None

    async def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None:
        if idle_ttl.total_seconds() <= 0:
            raise ValueError("idle_ttl must be positive")
        async with self._lock:
            self._idle_ttl_by_pool[pool_name] = idle_ttl

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


@dataclass
class _PoolIdleState:
    entries: dict[str, IdleEntry] = field(default_factory=dict)
    queue: deque[str] = field(default_factory=deque)


def _now() -> datetime:
    return datetime.now(timezone.utc)
