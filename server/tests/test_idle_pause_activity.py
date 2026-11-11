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

"""Activity registry and pause lock behavior (Lua emulated in Python)."""

from typing import Optional

import pytest
from redis.exceptions import RedisError

from opensandbox_server.integrations.idle_pause.activity import (
    ActivityRegistry,
    activity_key,
    unknown_tombstone_key,
)
from opensandbox_server.integrations.idle_pause.locking import PauseLockManager, lock_key


class FakeAsyncRedis:
    """Minimal async redis stand-in: strings, NX/EX set, registered scripts."""

    def __init__(self, *, fail: bool = False):
        self.store: dict[str, str] = {}
        self.ttls: dict[str, int] = {}
        self.fail = fail
        self.scripts: dict[str, object] = {}

    async def ping(self) -> bool:
        if self.fail:
            raise RedisError("down")
        return True

    async def get(self, key: str) -> Optional[str]:
        if self.fail:
            raise RedisError("down")
        return self.store.get(key)

    async def mget(self, keys: list[str]) -> list[Optional[str]]:
        if self.fail:
            raise RedisError("down")
        return [self.store.get(k) for k in keys]

    async def set(
        self,
        key: str,
        value: object,
        nx: bool = False,
        ex: Optional[int] = None,
    ) -> bool:
        if self.fail:
            raise RedisError("down")
        if nx and key in self.store:
            return False
        self.store[key] = str(value)
        if ex is not None:
            self.ttls[key] = ex
        return True

    async def delete(self, key: str) -> int:
        if self.fail:
            raise RedisError("down")
        return self.store.pop(key, None) is not None and 1 or 0

    def register_script(self, script: str):  # noqa: ANN201
        self.scripts[script] = True
        if "tonumber(cur) < new" in script:

            async def monotonic_max(keys: list[str], args: list[int]) -> int:
                key, new, ttl = keys[0], int(args[0]), int(args[1])
                cur = self.store.get(key)
                if cur is None or int(cur) < new:
                    self.store[key] = str(new)
                    self.ttls[key] = ttl
                return 1

            return monotonic_max

        async def release_if_owner(keys: list[str], args: list[str]) -> int:
            key, owner = keys[0], str(args[0])
            if self.store.get(key) == owner:
                self.store.pop(key)
                return 1
            return 0

        return release_if_owner

    async def aclose(self) -> None:
        self.store.clear()


NOW_MS = 1_000_000_000_000


@pytest.fixture
def redis_client() -> FakeAsyncRedis:
    return FakeAsyncRedis()


@pytest.fixture
def registry(redis_client: FakeAsyncRedis) -> ActivityRegistry:
    return ActivityRegistry(redis_client, activity_ttl_seconds=1800)  # type: ignore[arg-type]


@pytest.mark.asyncio
async def test_record_writes_monotonic_max_with_ttl(registry: ActivityRegistry, redis_client: FakeAsyncRedis):
    await registry.record("sb_1", now_ms=NOW_MS)
    await registry.record("sb_1", now_ms=NOW_MS - 5000)  # stale write loses
    assert await registry.read("sb_1") == NOW_MS
    assert redis_client.ttls[activity_key("sb_1")] == 1800
    await registry.record("sb_1", now_ms=NOW_MS + 1)  # newer write wins
    assert await registry.read("sb_1") == NOW_MS + 1


@pytest.mark.asyncio
async def test_read_many_with_tombstones(registry: ActivityRegistry):
    await registry.record("sb_a", now_ms=NOW_MS)
    await registry.ensure_tombstone("sb_b", now_ms=NOW_MS - 1000)
    activity, tombstones = await registry.read_many_with_tombstones(["sb_a", "sb_b", "sb_c"])
    assert activity == {"sb_a": NOW_MS, "sb_b": None, "sb_c": None}
    assert tombstones == {"sb_a": None, "sb_b": NOW_MS - 1000, "sb_c": None}
    assert await registry.read_many_with_tombstones([]) == ({}, {})


@pytest.mark.asyncio
async def test_tombstone_nx_never_overwrites(registry: ActivityRegistry, redis_client: FakeAsyncRedis):
    await registry.ensure_tombstone("sb_1", now_ms=NOW_MS, extra_ttl_seconds=15)
    await registry.ensure_tombstone("sb_1", now_ms=NOW_MS + 999999, extra_ttl_seconds=15)
    assert redis_client.store[unknown_tombstone_key("sb_1")] == str(NOW_MS)
    assert redis_client.ttls[unknown_tombstone_key("sb_1")] == 1800 + 15
    await registry.clear_tombstone("sb_1")
    assert unknown_tombstone_key("sb_1") not in redis_client.store


@pytest.mark.asyncio
async def test_ping_failure_raises_for_sweeper():
    registry = ActivityRegistry(FakeAsyncRedis(fail=True), activity_ttl_seconds=1800)  # type: ignore[arg-type]
    with pytest.raises(RedisError):
        await registry.ping()


@pytest.mark.asyncio
async def test_lock_admits_one_and_release_is_owner_checked(redis_client: FakeAsyncRedis):
    locks = PauseLockManager(redis_client, lock_ttl_seconds=30)  # type: ignore[arg-type]
    assert await locks.try_acquire("sb_1", "replica-a") is True
    assert await locks.try_acquire("sb_1", "replica-b") is False
    assert redis_client.ttls[lock_key("sb_1")] == 30
    await locks.release("sb_1", "replica-b")  # wrong owner: no-op
    assert await locks.try_acquire("sb_1", "replica-b") is False
    await locks.release("sb_1", "replica-a")
    assert await locks.try_acquire("sb_1", "replica-b") is True
