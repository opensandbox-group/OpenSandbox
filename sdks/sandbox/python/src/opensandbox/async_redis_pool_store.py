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
"""Async Redis-backed pool state store."""

from __future__ import annotations

import base64
from collections.abc import Awaitable, Callable
from datetime import datetime, timedelta, timezone
from typing import Any, TypeVar, cast

from opensandbox.exceptions import PoolStateStoreUnavailableException
from opensandbox.pool_types import IdleEntry, StoreCounters
from opensandbox.redis_pool_store import (
    _REQUIRED_REDIS_METHODS,
    Redis,
    RedisPoolStateStore,
    _decode,
    _millis,
    _validate_owner_and_ttl,
)

try:
    from redis.asyncio import Redis as AsyncRedis
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        'Install opensandbox[pool-redis] to use opensandbox.pool_redis'
    ) from exc

T = TypeVar("T")


class AsyncRedisPoolStateStore:
    """Distributed async pool store backed by a caller-managed Redis async client."""

    DEFAULT_KEY_PREFIX = RedisPoolStateStore.DEFAULT_KEY_PREFIX

    def __init__(self, redis: AsyncRedis, key_prefix: str = DEFAULT_KEY_PREFIX) -> None:
        _validate_async_redis_client(redis)
        self._redis = redis
        self._key_prefix = key_prefix
        self._default_idle_ttl = timedelta(hours=24)

    async def try_take_idle(self, pool_name: str) -> str | None:
        result = await self._execute(
            "try_take_idle",
            pool_name,
            lambda: cast(
                Awaitable[Any],
                self._redis.eval(
                    RedisPoolStateStore._TAKE_IDLE_SCRIPT,
                    2,
                    self._idle_list_key(pool_name),
                    self._idle_expires_key(pool_name),
                ),
            ),
        )
        return _decode(result) if result is not None else None

    async def put_idle(self, pool_name: str, sandbox_id: str) -> None:
        if not sandbox_id or not sandbox_id.strip():
            raise ValueError("sandbox_id must not be blank")
        ttl_millis = max(1, _millis(await self._resolve_idle_ttl(pool_name)))
        await self._execute(
            "put_idle",
            pool_name,
            lambda: cast(
                Awaitable[Any],
                self._redis.eval(
                    RedisPoolStateStore._PUT_IDLE_SCRIPT,
                    2,
                    self._idle_list_key(pool_name),
                    self._idle_expires_key(pool_name),
                    sandbox_id,
                    str(ttl_millis),
                ),
            ),
        )

    async def remove_idle(self, pool_name: str, sandbox_id: str) -> None:
        await self._execute(
            "remove_idle",
            pool_name,
            lambda: _gather_tuple(
                cast(
                    Awaitable[Any],
                    self._redis.hdel(self._idle_expires_key(pool_name), sandbox_id),
                ),
                cast(
                    Awaitable[Any],
                    self._redis.lrem(self._idle_list_key(pool_name), 0, sandbox_id),
                ),
            ),
        )

    async def try_acquire_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        _validate_owner_and_ttl(owner_id, ttl)

        async def op() -> bool:
            acquired = await self._redis.set(
                self._primary_lock_key(pool_name),
                owner_id,
                nx=True,
                px=max(1, _millis(ttl)),
            )
            return bool(acquired) or await self.renew_primary_lock(
                pool_name, owner_id, ttl
            )

        return await self._execute("try_acquire_primary_lock", pool_name, op)

    async def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        _validate_owner_and_ttl(owner_id, ttl)
        result = await self._execute(
            "renew_primary_lock",
            pool_name,
            lambda: cast(
                Awaitable[Any],
                self._redis.eval(
                    RedisPoolStateStore._RENEW_LOCK_SCRIPT,
                    1,
                    self._primary_lock_key(pool_name),
                    owner_id,
                    str(max(1, _millis(ttl))),
                ),
            ),
        )
        return result == 1 or result == b"1"

    async def release_primary_lock(self, pool_name: str, owner_id: str) -> None:
        await self._execute(
            "release_primary_lock",
            pool_name,
            lambda: cast(
                Awaitable[Any],
                self._redis.eval(
                    RedisPoolStateStore._RELEASE_LOCK_SCRIPT,
                    1,
                    self._primary_lock_key(pool_name),
                    owner_id,
                ),
            ),
        )

    async def reap_expired_idle(self, pool_name: str, now: datetime) -> None:
        await self._execute(
            "reap_expired_idle",
            pool_name,
            lambda: cast(
                Awaitable[Any],
                self._redis.eval(
                    RedisPoolStateStore._REAP_EXPIRED_SCRIPT,
                    2,
                    self._idle_list_key(pool_name),
                    self._idle_expires_key(pool_name),
                ),
            ),
        )

    async def snapshot_counters(self, pool_name: str) -> StoreCounters:
        async def op() -> StoreCounters:
            idle_count = cast(
                int,
                await cast(
                    Awaitable[Any],
                    self._redis.hlen(self._idle_expires_key(pool_name)),
                ),
            )
            return StoreCounters(
                idle_count=idle_count
            )

        return await self._execute("snapshot_counters", pool_name, op)

    async def snapshot_idle_entries(self, pool_name: str) -> list[IdleEntry]:
        async def op() -> list[IdleEntry]:
            raw_ids = cast(
                list[Any],
                await cast(
                    Awaitable[Any],
                    self._redis.lrange(self._idle_list_key(pool_name), 0, -1),
                ),
            )
            ids = [_decode(v) for v in raw_ids]
            raw_expires_by_id = cast(
                dict[Any, Any],
                await cast(
                    Awaitable[Any],
                    self._redis.hgetall(self._idle_expires_key(pool_name)),
                ),
            )
            expires_by_id = {
                _decode(k): _decode(v)
                for k, v in raw_expires_by_id.items()
            }
            entries: list[IdleEntry] = []
            for sandbox_id in ids:
                expires_at = expires_by_id.get(sandbox_id)
                if expires_at is None:
                    continue
                entries.append(
                    IdleEntry(
                        sandbox_id=sandbox_id,
                        expires_at=datetime.fromtimestamp(
                            int(expires_at) / 1000, timezone.utc
                        ),
                    )
                )
            return entries

        return await self._execute("snapshot_idle_entries", pool_name, op)

    async def get_max_idle(self, pool_name: str) -> int | None:
        value = await self._execute(
            "get_max_idle",
            pool_name,
            lambda: self._redis.get(self._max_idle_key(pool_name)),
        )
        if value is None:
            return None
        try:
            return int(_decode(value))
        except ValueError:
            return None

    async def set_max_idle(self, pool_name: str, max_idle: int) -> None:
        if max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        await self._execute(
            "set_max_idle",
            pool_name,
            lambda: self._redis.set(self._max_idle_key(pool_name), str(max_idle)),
        )

    async def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None:
        if idle_ttl.total_seconds() <= 0:
            raise ValueError("idle_ttl must be positive")
        await self._execute(
            "set_idle_entry_ttl",
            pool_name,
            lambda: self._redis.set(
                self._idle_ttl_key(pool_name), str(max(1, _millis(idle_ttl)))
            ),
        )

    async def _resolve_idle_ttl(self, pool_name: str) -> timedelta:
        value = await self._execute(
            "resolve_idle_ttl",
            pool_name,
            lambda: self._redis.get(self._idle_ttl_key(pool_name)),
        )
        if value is None:
            return self._default_idle_ttl
        try:
            return timedelta(milliseconds=max(1, int(_decode(value))))
        except ValueError:
            return self._default_idle_ttl

    def _idle_list_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "idle:list")

    def _idle_expires_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "idle:expires")

    def _primary_lock_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "lock")

    def _max_idle_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "maxIdle")

    def _idle_ttl_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "idleTtlMillis")

    def _pool_key(self, pool_name: str, suffix: str) -> str:
        if not pool_name or not pool_name.strip():
            raise ValueError("pool_name must not be blank")
        tag = base64.urlsafe_b64encode(pool_name.encode()).decode().rstrip("=")
        return f"{self._key_prefix}:{{{tag}}}:{suffix}"

    async def _execute(
        self, operation: str, pool_name: str, block: Callable[[], Awaitable[T]]
    ) -> T:
        try:
            return await block()
        except ValueError:
            raise
        except Exception as exc:
            raise PoolStateStoreUnavailableException(
                f"Redis pool state store operation failed: operation={operation} pool_name={pool_name}",
                exc,
            ) from exc


async def _gather_tuple(*awaitables: Awaitable[Any]) -> tuple[Any, ...]:
    import asyncio

    return tuple(await asyncio.gather(*awaitables))


def _validate_async_redis_client(redis: Any) -> None:
    if isinstance(redis, Redis) or not isinstance(redis, AsyncRedis):
        raise TypeError(
            "AsyncRedisPoolStateStore requires a redis.asyncio.Redis client; "
            "use RedisPoolStateStore for redis.Redis"
        )
    for method_name in _REQUIRED_REDIS_METHODS:
        method = getattr(redis, method_name, None)
        if not callable(method):
            raise TypeError(
                f"AsyncRedisPoolStateStore requires a Redis client with callable {method_name}()"
            )
