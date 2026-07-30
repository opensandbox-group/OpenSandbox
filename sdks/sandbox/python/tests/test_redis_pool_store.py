# Copyright 2026 Alibaba Group Holding Ltd.
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

from __future__ import annotations

import time
from datetime import datetime, timedelta, timezone
from typing import Any

import pytest
from redis import Redis
from redis.asyncio import Redis as AsyncRedis

from opensandbox.exceptions import (
    PoolDestroyedException,
    PoolStateStoreUnavailableException,
)
from opensandbox.pool_redis import RedisPoolStateStore
from opensandbox.pool_types import PoolDestroyState


@pytest.fixture()
def redis_store() -> tuple[RedisPoolStateStore, Any, str]:
    redis_client = _FakeRedis()
    key_prefix = "opensandbox:test"
    store = RedisPoolStateStore(redis_client, key_prefix=key_prefix)
    return store, redis_client, key_prefix


def test_redis_store_put_and_take_idle_fifo(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store

    store.put_idle("pool", "id-1")
    store.put_idle("pool", "id-2")
    store.put_idle("pool", "id-3")

    assert store.try_take_idle("pool") == "id-1"
    assert store.try_take_idle("pool") == "id-2"
    assert store.try_take_idle("pool") == "id-3"
    assert store.try_take_idle("pool") is None


def test_redis_store_put_idle_is_idempotent(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store

    store.put_idle("pool", "id-1")
    store.put_idle("pool", "id-1")

    assert store.snapshot_counters("pool").idle_count == 1
    assert store.try_take_idle("pool") == "id-1"
    assert store.try_take_idle("pool") is None


def test_redis_store_reap_expired_idle(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store

    store.set_idle_entry_ttl("pool", timedelta(milliseconds=50))
    store.put_idle("pool", "id-1")
    time.sleep(0.1)
    assert store.snapshot_counters("pool").idle_count == 1

    store.reap_expired_idle("pool", datetime.now(timezone.utc))

    assert store.snapshot_counters("pool").idle_count == 0
    assert store.try_take_idle("pool") is None


def test_redis_store_try_take_idle_min_ttl_surfaces_alive_below_threshold(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store
    store.set_idle_entry_ttl("pool", timedelta(seconds=5))
    store.put_idle("pool", "id-1")
    store.put_idle("pool", "id-2")

    result = store.try_take_idle_min_ttl("pool", timedelta(seconds=60))
    assert result.sandbox_id is None
    assert set(result.discarded_alive_sandbox_ids) == {"id-1", "id-2"}
    assert store.snapshot_counters("pool").idle_count == 0


def test_redis_store_try_take_idle_min_ttl_returns_entries_above_threshold(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store
    store.set_idle_entry_ttl("pool", timedelta(minutes=10))
    store.put_idle("pool", "id-1")

    result = store.try_take_idle_min_ttl("pool", timedelta(seconds=60))
    assert result.sandbox_id == "id-1"
    assert result.discarded_alive_sandbox_ids == ()


def test_redis_store_try_take_idle_min_ttl_zero_falls_back_to_base(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store
    store.put_idle("pool", "id-1")

    taken = store.try_take_idle_min_ttl("pool", timedelta(0))
    assert taken.sandbox_id == "id-1"
    assert taken.discarded_alive_sandbox_ids == ()

    empty = store.try_take_idle_min_ttl("pool", timedelta(0))
    assert empty.sandbox_id is None
    assert empty.discarded_alive_sandbox_ids == ()


def test_redis_store_reap_expired_idle_min_ttl_returns_alive_evicted(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store
    store.set_idle_entry_ttl("pool", timedelta(seconds=5))
    store.put_idle("pool", "id-1")
    store.put_idle("pool", "id-2")

    discarded_alive = store.reap_expired_idle_min_ttl(
        "pool", datetime.now(timezone.utc), timedelta(seconds=60)
    )

    assert set(discarded_alive) == {"id-1", "id-2"}
    assert store.snapshot_counters("pool").idle_count == 0


def test_redis_store_primary_lock_owner_semantics(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store

    assert store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert store.renew_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert not store.try_acquire_primary_lock("pool", "owner-2", timedelta(seconds=60))
    assert not store.renew_primary_lock("pool", "owner-2", timedelta(seconds=60))

    store.release_primary_lock("pool", "owner-2")
    assert not store.try_acquire_primary_lock("pool", "owner-2", timedelta(seconds=60))

    store.release_primary_lock("pool", "owner-1")
    assert store.try_acquire_primary_lock("pool", "owner-2", timedelta(seconds=60))


def test_redis_store_max_idle_is_shared(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store

    assert store.get_max_idle("pool") is None
    store.set_max_idle("pool", 7)
    assert store.get_max_idle("pool") == 7
    store.set_max_idle("pool", 0)
    assert store.get_max_idle("pool") == 0


def test_redis_store_destroy_fences_writes_and_preserves_tombstone(
    redis_store: tuple[RedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = redis_store
    store.put_idle("pool", "id-1")
    assert store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))

    store.begin_destroy("pool", "destroyer")

    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYING
    assert not store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert not store.renew_primary_lock("pool", "owner-1", timedelta(seconds=60))
    with pytest.raises(PoolDestroyedException):
        store.put_idle("pool", "id-2")
    with pytest.raises(PoolDestroyedException):
        store.set_max_idle("pool", 2)

    store.clear_pool_state("pool")
    assert store.snapshot_counters("pool").idle_count == 0
    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYING

    store.mark_destroyed("pool", "destroyer", timedelta(seconds=60))
    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYED
    with pytest.raises(PoolDestroyedException):
        store.begin_destroy("pool", "destroyer-2")
    assert store.get_destroy_state("pool") == PoolDestroyState.DESTROYED


def test_redis_store_wraps_client_failures() -> None:
    store = RedisPoolStateStore(_BrokenRedis())

    with pytest.raises(PoolStateStoreUnavailableException):
        store.get_max_idle("pool")


def test_redis_store_rejects_async_client_shape() -> None:
    with pytest.raises(TypeError, match="redis.Redis"):
        RedisPoolStateStore(_FakeAsyncRedis())


class _BrokenRedis(Redis):
    def __init__(self) -> None:
        pass

    def eval(self, script: str, numkeys: int, *args: str) -> str | int | None:
        raise RuntimeError("redis unavailable")

    def get(self, key: str) -> str | None:
        raise RuntimeError("redis unavailable")

    def set(
        self,
        key: str,
        value: str,
        *,
        nx: bool = False,
        px: int | None = None,
    ) -> bool:
        raise RuntimeError("redis unavailable")

    def hdel(self, key: str, field: str) -> int:
        raise RuntimeError("redis unavailable")

    def lrem(self, key: str, count: int, value: str) -> int:
        raise RuntimeError("redis unavailable")

    def hlen(self, key: str) -> int:
        raise RuntimeError("redis unavailable")

    def lrange(self, key: str, start: int, stop: int) -> list[str]:
        raise RuntimeError("redis unavailable")

    def hgetall(self, key: str) -> dict[str, str]:
        raise RuntimeError("redis unavailable")

    def delete(self, *keys: str) -> int:
        raise RuntimeError("redis unavailable")


class _FakeAsyncRedis(AsyncRedis):
    def __init__(self) -> None:
        pass

    async def eval(self, script: str, numkeys: int, *args: str) -> str | int | None:
        return None

    async def get(self, key: str) -> str | None:
        return None

    async def set(
        self,
        key: str,
        value: str,
        *,
        nx: bool = False,
        px: int | None = None,
    ) -> bool:
        return True

    async def hdel(self, key: str, field: str) -> int:
        return 0

    async def lrem(self, key: str, count: int, value: str) -> int:
        return 0

    async def hlen(self, key: str) -> int:
        return 0

    async def lrange(self, key: str, start: int, stop: int) -> list[str]:
        return []

    async def hgetall(self, key: str) -> dict[str, str]:
        return {}

    async def delete(self, *keys: str) -> int:
        return 0


class _FakeRedis(Redis):
    """Small Redis double for RedisPoolStateStore unit tests."""

    def __init__(self) -> None:
        self._strings: dict[str, tuple[str, int | None]] = {}
        self._lists: dict[str, list[str]] = {}
        self._hashes: dict[str, dict[str, str]] = {}

    def eval(self, script: str, numkeys: int, *args: Any) -> Any:
        del numkeys
        if "LPOP" in script:
            min_remaining_ttl_ms = int(args[2]) if len(args) >= 3 else 0
            return self._take_idle(args[0], args[1], min_remaining_ttl_ms)
        if "RPUSH" in script:
            return self._put_idle(args[0], args[1], args[2], args[3], int(args[4]))
        if "SET" in script and "NX" in script:
            return int(self._acquire_lock(args[0], args[1], args[2], int(args[3])))
        if "PEXPIRE" in script:
            return int(self._renew_lock(args[0], args[1], args[2], int(args[3])))
        if "HGETALL" in script:
            min_remaining_ttl_ms = int(args[2]) if len(args) >= 3 else 0
            return self._reap_expired(args[0], args[1], min_remaining_ttl_ms)
        if "DEL" in script and "GET" in script:
            return self._release_lock(args[0], args[1])
        if "destroy_state == ARGV[2]" in script:
            return self._begin_destroy(args[0], args[1], args[2], args[3], args[4])
        if "SET" in script:
            return self._set_value(args[0], args[1], args[2])
        raise NotImplementedError("unsupported Redis script")

    def get(self, key: str) -> str | None:
        self._expire_string_if_needed(key)
        value = self._strings.get(key)
        return value[0] if value is not None else None

    def set(
        self,
        key: str,
        value: str,
        *,
        nx: bool = False,
        px: int | None = None,
    ) -> bool:
        self._expire_string_if_needed(key)
        if nx and key in self._strings:
            return False
        expires_at = _now_ms() + px if px is not None else None
        self._strings[key] = (value, expires_at)
        return True

    def hdel(self, key: str, field: str) -> int:
        existed = field in self._hashes.get(key, {})
        self._hashes.get(key, {}).pop(field, None)
        return int(existed)

    def lrem(self, key: str, count: int, value: str) -> int:
        del count
        values = self._lists.get(key, [])
        removed = values.count(value)
        self._lists[key] = [item for item in values if item != value]
        return removed

    def hlen(self, key: str) -> int:
        return len(self._hashes.get(key, {}))

    def lrange(self, key: str, start: int, stop: int) -> list[str]:
        values = self._lists.get(key, [])
        if stop == -1:
            return values[start:]
        return values[start : stop + 1]

    def hgetall(self, key: str) -> dict[str, str]:
        return dict(self._hashes.get(key, {}))

    def delete(self, *keys: str) -> int:
        removed = 0
        for key in keys:
            if key in self._strings:
                self._strings.pop(key, None)
                removed += 1
            if key in self._lists:
                self._lists.pop(key, None)
                removed += 1
            if key in self._hashes:
                self._hashes.pop(key, None)
                removed += 1
        return removed

    def _take_idle(
        self, list_key: str, expires_key: str, min_remaining_ttl_ms: int = 0
    ) -> Any:
        queue = self._lists.setdefault(list_key, [])
        expires = self._hashes.setdefault(expires_key, {})
        now_ms = _now_ms()
        cutoff = now_ms + max(0, min_remaining_ttl_ms)
        discarded_alive: list[str] = []
        while queue:
            sandbox_id = queue.pop(0)
            expires_at = expires.pop(sandbox_id, None)
            if expires_at is None:
                continue
            exp = int(expires_at)
            if exp > cutoff:
                return [sandbox_id, discarded_alive]
            if exp > now_ms:
                discarded_alive.append(sandbox_id)
        if not discarded_alive:
            return None
        return ["", discarded_alive]

    def _put_idle(
        self,
        list_key: str,
        expires_key: str,
        destroy_state_key: str,
        sandbox_id: str,
        ttl_ms: int,
    ) -> int:
        if self.get(destroy_state_key) is not None:
            return -1
        now = _now_ms()
        expires = self._hashes.setdefault(expires_key, {})
        current = expires.get(sandbox_id)
        if current is not None and int(current) > now:
            return 0
        if current is None:
            self._lists.setdefault(list_key, []).append(sandbox_id)
        expires[sandbox_id] = str(now + ttl_ms)
        return 1

    def _acquire_lock(
        self, key: str, destroy_state_key: str, owner_id: str, ttl_ms: int
    ) -> bool:
        if self.get(destroy_state_key) is not None:
            return False
        if self.set(key, owner_id, nx=True, px=ttl_ms):
            return True
        return self._renew_lock(key, destroy_state_key, owner_id, ttl_ms)

    def _renew_lock(
        self, key: str, destroy_state_key: str, owner_id: str, ttl_ms: int
    ) -> bool:
        if self.get(destroy_state_key) is not None:
            return False
        if self.get(key) != owner_id:
            return False
        self._strings[key] = (owner_id, _now_ms() + ttl_ms)
        return True

    def _set_value(self, key: str, destroy_state_key: str, value: str) -> int:
        if self.get(destroy_state_key) is not None:
            return -1
        self.set(key, value)
        return 1

    def _begin_destroy(
        self,
        state_key: str,
        owner_key: str,
        destroying: str,
        destroyed: str,
        owner_id: str,
    ) -> int:
        if self.get(state_key) == destroyed:
            return -1
        self.set(state_key, destroying)
        self.set(owner_key, owner_id)
        return 1

    def _release_lock(self, key: str, owner_id: str) -> int:
        if self.get(key) != owner_id:
            return 0
        self._strings.pop(key, None)
        return 1

    def _reap_expired(
        self, list_key: str, expires_key: str, min_remaining_ttl_ms: int = 0
    ) -> list[str]:
        expires = self._hashes.setdefault(expires_key, {})
        now_ms = _now_ms()
        cutoff = now_ms + max(0, min_remaining_ttl_ms)
        evicted: list[tuple[str, int]] = []
        for sandbox_id, expiry in list(expires.items()):
            exp = int(expiry)
            if exp <= cutoff:
                evicted.append((sandbox_id, exp))
                expires.pop(sandbox_id, None)
        if evicted:
            evicted_ids = {sandbox_id for sandbox_id, _ in evicted}
            self._lists[list_key] = [
                sandbox_id
                for sandbox_id in self._lists.get(list_key, [])
                if sandbox_id not in evicted_ids
            ]
        return [sandbox_id for sandbox_id, exp in evicted if exp > now_ms]

    def _expire_string_if_needed(self, key: str) -> None:
        value = self._strings.get(key)
        if value is not None and value[1] is not None and value[1] <= _now_ms():
            self._strings.pop(key, None)


def _now_ms() -> int:
    return int(time.time() * 1000)
