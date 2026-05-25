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
"""Redis-backed pool state store."""

from __future__ import annotations

import base64
from datetime import datetime, timedelta, timezone
from typing import Any, cast

from opensandbox.exceptions import PoolStateStoreUnavailableException
from opensandbox.pool_types import IdleEntry, StoreCounters

try:
    from redis import Redis
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        'Install opensandbox[pool-redis] to use opensandbox.pool_redis'
    ) from exc

_REQUIRED_REDIS_METHODS = (
    "eval",
    "get",
    "set",
    "hdel",
    "lrem",
    "hlen",
    "lrange",
    "hgetall",
)


class RedisPoolStateStore:
    """Distributed pool store backed by a caller-managed Redis client."""

    DEFAULT_KEY_PREFIX = "opensandbox:pool"

    _TAKE_IDLE_SCRIPT = """
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
while true do
  local sandbox_id = redis.call('LPOP', KEYS[1])
  if not sandbox_id then
    return nil
  end
  local expires_at = redis.call('HGET', KEYS[2], sandbox_id)
  if expires_at then
    redis.call('HDEL', KEYS[2], sandbox_id)
    if tonumber(expires_at) > now_ms then
      return sandbox_id
    end
  end
end
"""

    _PUT_IDLE_SCRIPT = """
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local expires_at = now_ms + tonumber(ARGV[2])
local current_expires_at = redis.call('HGET', KEYS[2], ARGV[1])
if current_expires_at and tonumber(current_expires_at) > now_ms then
  return 0
end
if not current_expires_at then
  redis.call('RPUSH', KEYS[1], ARGV[1])
end
redis.call('HSET', KEYS[2], ARGV[1], expires_at)
return 1
"""

    _RENEW_LOCK_SCRIPT = """
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
"""

    _RELEASE_LOCK_SCRIPT = """
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
"""

    _REAP_EXPIRED_SCRIPT = """
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local entries = redis.call('HGETALL', KEYS[2])
for i = 1, #entries, 2 do
  if tonumber(entries[i + 1]) <= now_ms then
    redis.call('HDEL', KEYS[2], entries[i])
    redis.call('LREM', KEYS[1], 0, entries[i])
  end
end
return 1
"""

    def __init__(self, redis: Redis, key_prefix: str = DEFAULT_KEY_PREFIX) -> None:
        _validate_sync_redis_client(redis)
        self._redis = redis
        self._key_prefix = key_prefix
        self._default_idle_ttl = timedelta(hours=24)

    def try_take_idle(self, pool_name: str) -> str | None:
        result = self._execute(
            "try_take_idle",
            pool_name,
            lambda: self._redis.eval(
                self._TAKE_IDLE_SCRIPT,
                2,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
            ),
        )
        return _decode(result) if result is not None else None

    def put_idle(self, pool_name: str, sandbox_id: str) -> None:
        if not sandbox_id or not sandbox_id.strip():
            raise ValueError("sandbox_id must not be blank")
        ttl_millis = max(1, _millis(self._resolve_idle_ttl(pool_name)))
        self._execute(
            "put_idle",
            pool_name,
            lambda: self._redis.eval(
                self._PUT_IDLE_SCRIPT,
                2,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
                sandbox_id,
                str(ttl_millis),
            ),
        )

    def remove_idle(self, pool_name: str, sandbox_id: str) -> None:
        self._execute(
            "remove_idle",
            pool_name,
            lambda: (
                self._redis.hdel(self._idle_expires_key(pool_name), sandbox_id),
                self._redis.lrem(self._idle_list_key(pool_name), 0, sandbox_id),
            ),
        )

    def try_acquire_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        _validate_owner_and_ttl(owner_id, ttl)

        def op() -> bool:
            acquired = self._redis.set(
                self._primary_lock_key(pool_name),
                owner_id,
                nx=True,
                px=max(1, _millis(ttl)),
            )
            return bool(acquired) or self.renew_primary_lock(pool_name, owner_id, ttl)

        return self._execute("try_acquire_primary_lock", pool_name, op)

    def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        _validate_owner_and_ttl(owner_id, ttl)
        result = self._execute(
            "renew_primary_lock",
            pool_name,
            lambda: self._redis.eval(
                self._RENEW_LOCK_SCRIPT,
                1,
                self._primary_lock_key(pool_name),
                owner_id,
                str(max(1, _millis(ttl))),
            ),
        )
        return result == 1 or result == b"1"

    def release_primary_lock(self, pool_name: str, owner_id: str) -> None:
        self._execute(
            "release_primary_lock",
            pool_name,
            lambda: self._redis.eval(
                self._RELEASE_LOCK_SCRIPT,
                1,
                self._primary_lock_key(pool_name),
                owner_id,
            ),
        )

    def reap_expired_idle(self, pool_name: str, now: datetime) -> None:
        self._execute(
            "reap_expired_idle",
            pool_name,
            lambda: self._redis.eval(
                self._REAP_EXPIRED_SCRIPT,
                2,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
            ),
        )

    def snapshot_counters(self, pool_name: str) -> StoreCounters:
        def op() -> StoreCounters:
            idle_count = cast(int, self._redis.hlen(self._idle_expires_key(pool_name)))
            return StoreCounters(idle_count=idle_count)

        return self._execute("snapshot_counters", pool_name, op)

    def snapshot_idle_entries(self, pool_name: str) -> list[IdleEntry]:
        def op() -> list[IdleEntry]:
            raw_ids = cast(
                list[Any],
                self._redis.lrange(self._idle_list_key(pool_name), 0, -1),
            )
            ids = [_decode(v) for v in raw_ids]
            raw_expires_by_id = cast(
                dict[Any, Any],
                self._redis.hgetall(self._idle_expires_key(pool_name)),
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

        return self._execute("snapshot_idle_entries", pool_name, op)

    def get_max_idle(self, pool_name: str) -> int | None:
        value = self._execute(
            "get_max_idle", pool_name, lambda: self._redis.get(self._max_idle_key(pool_name))
        )
        if value is None:
            return None
        try:
            return int(_decode(value))
        except ValueError:
            return None

    def set_max_idle(self, pool_name: str, max_idle: int) -> None:
        if max_idle < 0:
            raise ValueError("max_idle must be >= 0")
        self._execute(
            "set_max_idle",
            pool_name,
            lambda: self._redis.set(self._max_idle_key(pool_name), str(max_idle)),
        )

    def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None:
        if idle_ttl.total_seconds() <= 0:
            raise ValueError("idle_ttl must be positive")
        self._execute(
            "set_idle_entry_ttl",
            pool_name,
            lambda: self._redis.set(
                self._idle_ttl_key(pool_name), str(max(1, _millis(idle_ttl)))
            ),
        )

    def _resolve_idle_ttl(self, pool_name: str) -> timedelta:
        value = self._execute(
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

    def _execute(self, operation: str, pool_name: str, block: Any) -> Any:
        try:
            return block()
        except ValueError:
            raise
        except Exception as exc:
            raise PoolStateStoreUnavailableException(
                f"Redis pool state store operation failed: operation={operation} pool_name={pool_name}",
                exc,
            ) from exc


def _millis(value: timedelta) -> int:
    return int(value.total_seconds() * 1000)


def _decode(value: Any) -> str:
    if isinstance(value, bytes):
        return value.decode()
    return str(value)


def _validate_owner_and_ttl(owner_id: str, ttl: timedelta) -> None:
    if not owner_id or not owner_id.strip():
        raise ValueError("owner_id must not be blank")
    if ttl.total_seconds() <= 0:
        raise ValueError("ttl must be positive")


def _validate_sync_redis_client(redis: Any) -> None:
    if not isinstance(redis, Redis):
        raise TypeError(
            "RedisPoolStateStore requires a redis.Redis client; "
            "use AsyncRedisPoolStateStore for redis.asyncio.Redis"
        )
    for method_name in _REQUIRED_REDIS_METHODS:
        method = getattr(redis, method_name, None)
        if not callable(method):
            raise TypeError(
                f"RedisPoolStateStore requires a Redis client with callable {method_name}()"
            )
