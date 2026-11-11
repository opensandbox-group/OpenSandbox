# Copyright 2026 The OpenSandbox Authors
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

"""Async Redis access to the shared activity keys and unknown-state tombstones.

The ingress (and later the server proxy path) are the push producers; the
server reads here and writes on lifecycle transitions (create, resume).
Failures raise to the caller, which fails toward *not pausing*.
"""

from __future__ import annotations

import asyncio
import logging
import time
from typing import TYPE_CHECKING, Any, Optional

from opensandbox_server.integrations.idle_pause.constants import (
    ACTIVITY_KEY_PREFIX,
    UNKNOWN_TOMBSTONE_PREFIX,
)

if TYPE_CHECKING:
    from redis.asyncio import Redis

logger = logging.getLogger(__name__)

# Monotonic max: out-of-order writes across replicas never regress the key.
_MONOTONIC_MAX_SCRIPT = """
local cur = redis.call('GET', KEYS[1])
local new = tonumber(ARGV[1])
if cur == false or tonumber(cur) < new then
  redis.call('SET', KEYS[1], ARGV[1], 'EX', tonumber(ARGV[2]))
end
return 1
"""


def activity_key(sandbox_id: str) -> str:
    return f"{ACTIVITY_KEY_PREFIX}:{sandbox_id}"


def unknown_tombstone_key(sandbox_id: str) -> str:
    return f"{UNKNOWN_TOMBSTONE_PREFIX}:{sandbox_id}"


class ActivityRegistry:
    """Async Redis access to activity observations and unknown-state tombstones."""

    def __init__(self, client: "Redis", *, activity_ttl_seconds: int):
        self._client = client
        self._ttl = int(activity_ttl_seconds)
        self._max_script = self._client.register_script(_MONOTONIC_MAX_SCRIPT)

    @property
    def activity_ttl_seconds(self) -> int:
        return self._ttl

    async def ping(self) -> None:
        """Raise when Redis is unreachable (the sweep skips entirely)."""
        await self._client.ping()

    async def record(
        self, sandbox_id: str, *, now_ms: Optional[int] = None
    ) -> None:
        """Write one observation (monotonic max, EX ttl)."""
        timestamp = now_ms if now_ms is not None else _now_ms()
        await self._max_script(keys=[activity_key(sandbox_id)], args=[timestamp, self._ttl])

    async def read(self, sandbox_id: str) -> Optional[int]:
        """Last observation in Unix ms, or ``None`` when missing."""
        value = await self._client.get(activity_key(sandbox_id))
        return _parse_ms(value)

    async def read_many_with_tombstones(
        self, sandbox_ids: list[str]
    ) -> tuple[dict[str, Optional[int]], dict[str, Optional[int]]]:
        """Batch-read activity keys and their tombstones in one MGET."""
        if not sandbox_ids:
            return {}, {}
        n = len(sandbox_ids)
        keys = [activity_key(sid) for sid in sandbox_ids]
        keys += [unknown_tombstone_key(sid) for sid in sandbox_ids]
        values = await self._client.mget(keys)
        activity = {sid: _parse_ms(v) for sid, v in zip(sandbox_ids, values[:n])}
        tombstones = {sid: _parse_ms(v) for sid, v in zip(sandbox_ids, values[n:])}
        return activity, tombstones

    async def ensure_tombstone(
        self, sandbox_id: str, *, now_ms: Optional[int] = None, extra_ttl_seconds: int = 0
    ) -> None:
        """Record the first observation of unknown state (NX; never overwrites)."""
        timestamp = now_ms if now_ms is not None else _now_ms()
        ttl = self._ttl + max(0, extra_ttl_seconds)
        await self._client.set(
            unknown_tombstone_key(sandbox_id),
            timestamp,
            nx=True,
            ex=ttl,
        )

    async def clear_tombstone(self, sandbox_id: str) -> None:
        """Drop the tombstone once the activity key reappeared."""
        await self._client.delete(unknown_tombstone_key(sandbox_id))


class ThreadSafeActivityRecorder:
    """Schedules the async registry write on the captured loop.

    For sync lifecycle code running in worker threads; never raises and never
    blocks the caller — recording must not add latency or failure modes.
    """

    def __init__(self, registry: ActivityRegistry, loop: asyncio.AbstractEventLoop) -> None:
        self._registry = registry
        self._loop = loop

    def record(self, sandbox_id: str) -> None:
        try:
            future = asyncio.run_coroutine_threadsafe(
                self._registry.record(sandbox_id), self._loop
            )
            future.add_done_callback(_swallow_future_error)
        except Exception as exc:  # noqa: BLE001 - never break the caller
            logger.debug(f"idle_pause activity record dropped error={exc!s}")


def _swallow_future_error(future: Any) -> None:
    try:
        exc = future.exception()
    except Exception:  # noqa: BLE001 - shutdown races
        return
    if exc is not None:
        logger.debug(f"idle_pause activity record failed error={exc!s}")


def _parse_ms(value: object) -> Optional[int]:
    if value is None:
        return None
    try:
        return int(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        return None


def _now_ms() -> int:
    return time.time_ns() // 1_000_000
