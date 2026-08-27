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

from opensandbox.exceptions import (
    PoolDestroyedException,
    PoolStateStoreUnavailableException,
)
from opensandbox.pool_types import (
    IdleEntry,
    PoolDestroyState,
    StoreCounters,
    TakeIdleResult,
)

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
    "delete",
)


class RedisPoolStateStore:
    """Distributed pool store backed by a caller-managed Redis client."""

    DEFAULT_KEY_PREFIX = "opensandbox:pool"

    _TAKE_IDLE_SCRIPT = """
local redis_time = redis.call('TIME')
local now_ms = tonumber(redis_time[1]) * 1000 + math.floor(tonumber(redis_time[2]) / 1000)
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
while true do
  local sandbox_id = redis.call('LPOP', KEYS[1])
  if not sandbox_id then
    if #discarded_alive == 0 then
      return nil
    end
    return {'', discarded_alive}
  end
  local expires_at = redis.call('HGET', KEYS[2], sandbox_id)
  if expires_at then
    redis.call('HDEL', KEYS[2], sandbox_id)
    local exp = tonumber(expires_at)
    if exp > cutoff_ms then
      return {sandbox_id, discarded_alive}
    end
    if exp > now_ms then
      table.insert(discarded_alive, sandbox_id)
    end
  end
end
"""

    _PUT_IDLE_SCRIPT = """
local destroy_state = redis.call('GET', KEYS[3])
if destroy_state then
  return -1
end
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

    _ACQUIRE_LOCK_SCRIPT = """
local destroy_state = redis.call('GET', KEYS[2])
if destroy_state then
  return 0
end
if redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2]) then
  return 1
end
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
"""

    _RENEW_LOCK_SCRIPT = """
local destroy_state = redis.call('GET', KEYS[2])
if destroy_state then
  return 0
end
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
local min_remaining_ttl_ms = tonumber(ARGV[1]) or 0
local cutoff_ms = now_ms + min_remaining_ttl_ms
local discarded_alive = {}
local entries = redis.call('HGETALL', KEYS[2])
for i = 1, #entries, 2 do
  local sandbox_id = entries[i]
  local exp = tonumber(entries[i + 1])
  if exp <= cutoff_ms then
    redis.call('HDEL', KEYS[2], sandbox_id)
    redis.call('LREM', KEYS[1], 0, sandbox_id)
    if exp > now_ms then
      table.insert(discarded_alive, sandbox_id)
    end
  end
end
return discarded_alive
"""

    _SET_VALUE_SCRIPT = """
local destroy_state = redis.call('GET', KEYS[2])
if destroy_state then
  return -1
end
redis.call('SET', KEYS[1], ARGV[1])
return 1
"""

    _BEGIN_DESTROY_SCRIPT = """
local destroy_state = redis.call('GET', KEYS[1])
if destroy_state == ARGV[2] then
  return -1
end
redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], ARGV[3])
return 1
"""

    def __init__(self, redis: Redis, key_prefix: str = DEFAULT_KEY_PREFIX) -> None:
        _validate_sync_redis_client(redis)
        self._redis = redis
        self._key_prefix = key_prefix
        self._default_idle_ttl = timedelta(hours=24)

    def try_take_idle(self, pool_name: str) -> str | None:
        result = self._eval_take_idle(pool_name, "0")
        return result.sandbox_id

    def try_take_idle_min_ttl(
        self, pool_name: str, min_remaining_ttl: timedelta
    ) -> TakeIdleResult:
        """Variant of :meth:`try_take_idle` that skips entries with insufficient remaining TTL.

        See :class:`TakeIdleResult` for the return shape.
        """
        if min_remaining_ttl.total_seconds() <= 0:
            return TakeIdleResult(sandbox_id=self.try_take_idle(pool_name))
        return self._eval_take_idle(pool_name, str(max(0, _millis(min_remaining_ttl))))

    def _eval_take_idle(self, pool_name: str, min_remaining_ttl_ms: str) -> TakeIdleResult:
        result = self._execute(
            "try_take_idle",
            pool_name,
            lambda: self._redis.eval(
                self._TAKE_IDLE_SCRIPT,
                2,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
                min_remaining_ttl_ms,
            ),
        )
        return _decode_take_idle_result(result)

    def put_idle(self, pool_name: str, sandbox_id: str) -> None:
        if not sandbox_id or not sandbox_id.strip():
            raise ValueError("sandbox_id must not be blank")
        ttl_millis = max(1, _millis(self._resolve_idle_ttl(pool_name)))
        self._execute(
            "put_idle",
            pool_name,
            lambda: self._eval_fenced_write(
                pool_name,
                3,
                self._PUT_IDLE_SCRIPT,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
                self._destroy_state_key(pool_name),
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

        result = self._execute(
            "try_acquire_primary_lock",
            pool_name,
            lambda: self._redis.eval(
                self._ACQUIRE_LOCK_SCRIPT,
                2,
                self._primary_lock_key(pool_name),
                self._destroy_state_key(pool_name),
                owner_id,
                str(max(1, _millis(ttl))),
            ),
        )
        return result == 1 or result == b"1"

    def renew_primary_lock(
        self, pool_name: str, owner_id: str, ttl: timedelta
    ) -> bool:
        _validate_owner_and_ttl(owner_id, ttl)
        result = self._execute(
            "renew_primary_lock",
            pool_name,
            lambda: self._redis.eval(
                self._RENEW_LOCK_SCRIPT,
                2,
                self._primary_lock_key(pool_name),
                self._destroy_state_key(pool_name),
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
        self._reap_idle(pool_name, "0")

    def reap_expired_idle_min_ttl(
        self, pool_name: str, now: datetime, min_remaining_ttl: timedelta
    ) -> tuple[str, ...]:
        """Variant of :meth:`reap_expired_idle` that also evicts near-expiry entries.

        Returns IDs of alive evicted sandboxes (excluding fully-expired entries).
        """
        if min_remaining_ttl.total_seconds() <= 0:
            self.reap_expired_idle(pool_name, now)
            return ()
        return self._reap_idle(pool_name, str(max(0, _millis(min_remaining_ttl))))

    def _reap_idle(self, pool_name: str, min_remaining_ttl_ms: str) -> tuple[str, ...]:
        result = self._execute(
            "reap_expired_idle",
            pool_name,
            lambda: self._redis.eval(
                self._REAP_EXPIRED_SCRIPT,
                2,
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
                min_remaining_ttl_ms,
            ),
        )
        if not result:
            return ()
        return tuple(_decode(item) for item in result)

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
            lambda: self._eval_fenced_write(
                pool_name,
                2,
                self._SET_VALUE_SCRIPT,
                self._max_idle_key(pool_name),
                self._destroy_state_key(pool_name),
                str(max_idle),
            ),
        )

    def set_idle_entry_ttl(self, pool_name: str, idle_ttl: timedelta) -> None:
        if idle_ttl.total_seconds() <= 0:
            raise ValueError("idle_ttl must be positive")
        self._execute(
            "set_idle_entry_ttl",
            pool_name,
            lambda: self._eval_fenced_write(
                pool_name,
                2,
                self._SET_VALUE_SCRIPT,
                self._idle_ttl_key(pool_name),
                self._destroy_state_key(pool_name),
                str(max(1, _millis(idle_ttl))),
            ),
        )

    def get_destroy_state(self, pool_name: str) -> PoolDestroyState:
        value = self._execute(
            "get_destroy_state",
            pool_name,
            lambda: self._redis.get(self._destroy_state_key(pool_name)),
        )
        if value is None:
            return PoolDestroyState.ACTIVE
        decoded = _decode(value)
        if decoded == PoolDestroyState.DESTROYING.value:
            return PoolDestroyState.DESTROYING
        if decoded == PoolDestroyState.DESTROYED.value:
            return PoolDestroyState.DESTROYED
        return PoolDestroyState.ACTIVE

    def begin_destroy(self, pool_name: str, owner_id: str) -> None:
        if not owner_id or not owner_id.strip():
            raise ValueError("owner_id must not be blank")

        def op() -> None:
            result = self._redis.eval(
                self._BEGIN_DESTROY_SCRIPT,
                2,
                self._destroy_state_key(pool_name),
                self._destroy_owner_key(pool_name),
                PoolDestroyState.DESTROYING.value,
                PoolDestroyState.DESTROYED.value,
                owner_id,
            )
            if result == -1 or result == b"-1":
                raise PoolDestroyedException(
                    f"Pool namespace is already DESTROYED: pool_name={pool_name}"
                )

        self._execute("begin_destroy", pool_name, op)

    def clear_pool_state(self, pool_name: str) -> None:
        self._execute(
            "clear_pool_state",
            pool_name,
            lambda: self._redis.delete(
                self._idle_list_key(pool_name),
                self._idle_expires_key(pool_name),
                self._primary_lock_key(pool_name),
                self._max_idle_key(pool_name),
                self._idle_ttl_key(pool_name),
            ),
        )

    def mark_destroyed(
        self, pool_name: str, owner_id: str, tombstone_ttl: timedelta | None
    ) -> None:
        if not owner_id or not owner_id.strip():
            raise ValueError("owner_id must not be blank")
        if tombstone_ttl is not None and tombstone_ttl.total_seconds() <= 0:
            raise ValueError("tombstone_ttl must be positive")

        def op() -> None:
            if tombstone_ttl is None:
                self._redis.set(
                    self._destroy_state_key(pool_name),
                    PoolDestroyState.DESTROYED.value,
                )
                self._redis.set(self._destroy_owner_key(pool_name), owner_id)
            else:
                px = max(1, _millis(tombstone_ttl))
                self._redis.set(
                    self._destroy_state_key(pool_name),
                    PoolDestroyState.DESTROYED.value,
                    px=px,
                )
                self._redis.set(self._destroy_owner_key(pool_name), owner_id, px=px)

        self._execute("mark_destroyed", pool_name, op)

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

    def _destroy_state_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "destroy:state")

    def _destroy_owner_key(self, pool_name: str) -> str:
        return self._pool_key(pool_name, "destroy:owner")

    def _eval_fenced_write(
        self, pool_name: str, numkeys: int, script: str, *args: str
    ) -> Any:
        result = self._redis.eval(script, numkeys, *args)
        if result == -1 or result == b"-1":
            state = self.get_destroy_state(pool_name)
            raise PoolDestroyedException(
                f"Pool namespace is {state.value}: pool_name={pool_name}"
            )
        return result

    def _pool_key(self, pool_name: str, suffix: str) -> str:
        if not pool_name or not pool_name.strip():
            raise ValueError("pool_name must not be blank")
        tag = base64.urlsafe_b64encode(pool_name.encode()).decode().rstrip("=")
        return f"{self._key_prefix}:{{{tag}}}:{suffix}"

    def _execute(self, operation: str, pool_name: str, block: Any) -> Any:
        try:
            return block()
        except (ValueError, PoolDestroyedException):
            raise
        except Exception as exc:
            raise PoolStateStoreUnavailableException(
                f"Redis pool state store operation failed: operation={operation} pool_name={pool_name}",
                exc,
            ) from exc


def _decode_take_idle_result(result: Any) -> TakeIdleResult:
    """Decode the Lua return value into :class:`TakeIdleResult`.

    The script returns either nil (empty pool with no discarded entries) or a two-element
    array ``[takenSandboxId | "", [discardedAliveIds...]]``. The empty string in slot 0
    signals "no eligible entry but there were discarded-alive entries to report".
    """
    if result is None:
        return TakeIdleResult(sandbox_id=None)
    if isinstance(result, (str, bytes)):
        return TakeIdleResult(sandbox_id=_decode(result))
    if isinstance(result, list) and len(result) >= 1:
        taken_raw = result[0]
        taken = _decode(taken_raw) if taken_raw is not None else None
        if taken == "":
            taken = None
        discarded_raw = result[1] if len(result) >= 2 else []
        discarded = tuple(_decode(item) for item in (discarded_raw or []))
        return TakeIdleResult(sandbox_id=taken, discarded_alive_sandbox_ids=discarded)
    return TakeIdleResult(sandbox_id=None)


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
