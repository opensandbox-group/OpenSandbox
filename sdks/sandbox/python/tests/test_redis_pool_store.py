from __future__ import annotations

import time
from datetime import datetime, timedelta, timezone
from typing import Any

import pytest
from redis import Redis
from redis.asyncio import Redis as AsyncRedis

from opensandbox.exceptions import PoolStateStoreUnavailableException
from opensandbox.pool_redis import RedisPoolStateStore


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


class _FakeRedis(Redis):
    """Small Redis double for RedisPoolStateStore unit tests."""

    def __init__(self) -> None:
        self._strings: dict[str, tuple[str, int | None]] = {}
        self._lists: dict[str, list[str]] = {}
        self._hashes: dict[str, dict[str, str]] = {}

    def eval(self, script: str, numkeys: int, *args: str) -> str | int | None:
        del numkeys
        if "LPOP" in script:
            return self._take_idle(args[0], args[1])
        if "RPUSH" in script:
            return self._put_idle(args[0], args[1], args[2], int(args[3]))
        if "PEXPIRE" in script:
            return int(self._renew_lock(args[0], args[1], int(args[2])))
        if "HGETALL" in script:
            self._reap_expired(args[0], args[1])
            return 1
        if "DEL" in script and "GET" in script:
            return self._release_lock(args[0], args[1])
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

    def _take_idle(self, list_key: str, expires_key: str) -> str | None:
        queue = self._lists.setdefault(list_key, [])
        expires = self._hashes.setdefault(expires_key, {})
        now = _now_ms()
        while queue:
            sandbox_id = queue.pop(0)
            expires_at = expires.pop(sandbox_id, None)
            if expires_at is not None and int(expires_at) > now:
                return sandbox_id
        return None

    def _put_idle(
        self,
        list_key: str,
        expires_key: str,
        sandbox_id: str,
        ttl_ms: int,
    ) -> int:
        now = _now_ms()
        expires = self._hashes.setdefault(expires_key, {})
        current = expires.get(sandbox_id)
        if current is not None and int(current) > now:
            return 0
        if current is None:
            self._lists.setdefault(list_key, []).append(sandbox_id)
        expires[sandbox_id] = str(now + ttl_ms)
        return 1

    def _renew_lock(self, key: str, owner_id: str, ttl_ms: int) -> bool:
        if self.get(key) != owner_id:
            return False
        self._strings[key] = (owner_id, _now_ms() + ttl_ms)
        return True

    def _release_lock(self, key: str, owner_id: str) -> int:
        if self.get(key) != owner_id:
            return 0
        self._strings.pop(key, None)
        return 1

    def _reap_expired(self, list_key: str, expires_key: str) -> None:
        expires = self._hashes.setdefault(expires_key, {})
        expired = {sandbox_id for sandbox_id, expiry in expires.items() if int(expiry) <= _now_ms()}
        for sandbox_id in expired:
            expires.pop(sandbox_id, None)
        if expired:
            self._lists[list_key] = [
                sandbox_id
                for sandbox_id in self._lists.get(list_key, [])
                if sandbox_id not in expired
            ]

    def _expire_string_if_needed(self, key: str) -> None:
        value = self._strings.get(key)
        if value is not None and value[1] is not None and value[1] <= _now_ms():
            self._strings.pop(key, None)


def _now_ms() -> int:
    return int(time.time() * 1000)
