from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from typing import Any

import pytest
from redis import Redis
from redis.asyncio import Redis as AsyncRedis
from test_redis_pool_store import _FakeRedis

from opensandbox.exceptions import PoolStateStoreUnavailableException
from opensandbox.pool_redis import AsyncRedisPoolStateStore


@pytest.fixture()
async def async_redis_store() -> tuple[AsyncRedisPoolStateStore, Any, str]:
    redis_client = _FakeAsyncRedis()
    key_prefix = "opensandbox:test:async"
    store = AsyncRedisPoolStateStore(redis_client, key_prefix=key_prefix)
    return store, redis_client, key_prefix


@pytest.mark.asyncio
async def test_async_redis_store_put_and_take_idle_fifo(
    async_redis_store: tuple[AsyncRedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = async_redis_store

    await store.put_idle("pool", "id-1")
    await store.put_idle("pool", "id-2")
    await store.put_idle("pool", "id-3")

    assert await store.try_take_idle("pool") == "id-1"
    assert await store.try_take_idle("pool") == "id-2"
    assert await store.try_take_idle("pool") == "id-3"
    assert await store.try_take_idle("pool") is None


@pytest.mark.asyncio
async def test_async_redis_store_put_idle_is_idempotent(
    async_redis_store: tuple[AsyncRedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = async_redis_store

    await store.put_idle("pool", "id-1")
    await store.put_idle("pool", "id-1")

    assert (await store.snapshot_counters("pool")).idle_count == 1
    assert await store.try_take_idle("pool") == "id-1"
    assert await store.try_take_idle("pool") is None


@pytest.mark.asyncio
async def test_async_redis_store_reap_expired_idle(
    async_redis_store: tuple[AsyncRedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = async_redis_store

    await store.set_idle_entry_ttl("pool", timedelta(milliseconds=50))
    await store.put_idle("pool", "id-1")
    await asyncio.sleep(0.1)
    assert (await store.snapshot_counters("pool")).idle_count == 1

    await store.reap_expired_idle("pool", datetime.now(timezone.utc))

    assert (await store.snapshot_counters("pool")).idle_count == 0
    assert await store.try_take_idle("pool") is None


@pytest.mark.asyncio
async def test_async_redis_store_primary_lock_owner_semantics(
    async_redis_store: tuple[AsyncRedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = async_redis_store

    assert await store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert await store.try_acquire_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert await store.renew_primary_lock("pool", "owner-1", timedelta(seconds=60))
    assert not await store.try_acquire_primary_lock(
        "pool", "owner-2", timedelta(seconds=60)
    )
    assert not await store.renew_primary_lock("pool", "owner-2", timedelta(seconds=60))

    await store.release_primary_lock("pool", "owner-2")
    assert not await store.try_acquire_primary_lock(
        "pool", "owner-2", timedelta(seconds=60)
    )

    await store.release_primary_lock("pool", "owner-1")
    assert await store.try_acquire_primary_lock("pool", "owner-2", timedelta(seconds=60))


@pytest.mark.asyncio
async def test_async_redis_store_max_idle_is_shared(
    async_redis_store: tuple[AsyncRedisPoolStateStore, Any, str],
) -> None:
    store, _, _ = async_redis_store

    assert await store.get_max_idle("pool") is None
    await store.set_max_idle("pool", 7)
    assert await store.get_max_idle("pool") == 7
    await store.set_max_idle("pool", 0)
    assert await store.get_max_idle("pool") == 0


@pytest.mark.asyncio
async def test_async_redis_store_wraps_client_failures() -> None:
    store = AsyncRedisPoolStateStore(_BrokenAsyncRedis())

    with pytest.raises(PoolStateStoreUnavailableException):
        await store.get_max_idle("pool")


def test_async_redis_store_rejects_sync_client_shape() -> None:
    with pytest.raises(TypeError, match="redis.asyncio.Redis"):
        AsyncRedisPoolStateStore(_FakeSyncRedis())


class _BrokenAsyncRedis(AsyncRedis):
    def __init__(self) -> None:
        pass

    async def eval(self, script: str, numkeys: int, *args: str) -> str | int | None:
        raise RuntimeError("redis unavailable")

    async def get(self, key: str) -> str | None:
        raise RuntimeError("redis unavailable")

    async def set(
        self,
        key: str,
        value: str,
        *,
        nx: bool = False,
        px: int | None = None,
    ) -> bool:
        raise RuntimeError("redis unavailable")

    async def hdel(self, key: str, field: str) -> int:
        raise RuntimeError("redis unavailable")

    async def lrem(self, key: str, count: int, value: str) -> int:
        raise RuntimeError("redis unavailable")

    async def hlen(self, key: str) -> int:
        raise RuntimeError("redis unavailable")

    async def lrange(self, key: str, start: int, stop: int) -> list[str]:
        raise RuntimeError("redis unavailable")

    async def hgetall(self, key: str) -> dict[str, str]:
        raise RuntimeError("redis unavailable")


class _FakeSyncRedis(Redis):
    def __init__(self) -> None:
        pass


class _FakeAsyncRedis(AsyncRedis):
    def __init__(self) -> None:
        self._sync = _FakeRedis()

    async def eval(self, script: str, numkeys: int, *args: str) -> str | int | None:
        return self._sync.eval(script, numkeys, *args)

    async def get(self, key: str) -> str | None:
        return self._sync.get(key)

    async def set(
        self,
        key: str,
        value: str,
        *,
        nx: bool = False,
        px: int | None = None,
    ) -> bool:
        return self._sync.set(key, value, nx=nx, px=px)

    async def hdel(self, key: str, field: str) -> int:
        return self._sync.hdel(key, field)

    async def lrem(self, key: str, count: int, value: str) -> int:
        return self._sync.lrem(key, count, value)

    async def hlen(self, key: str) -> int:
        return self._sync.hlen(key)

    async def lrange(self, key: str, start: int, stop: int) -> list[str]:
        return self._sync.lrange(key, start, stop)

    async def hgetall(self, key: str) -> dict[str, str]:
        return self._sync.hgetall(key)
