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
"""E2E coverage for the asyncio Python sandbox pool."""

from __future__ import annotations

import asyncio
import os
import uuid
from collections.abc import Awaitable, Callable
from datetime import datetime, timedelta, timezone

import httpx
import pytest
from opensandbox import Sandbox, SandboxManager
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import (
    PoolAcquireFailedException,
    PoolEmptyException,
    PoolNotRunningException,
)
from opensandbox.models.sandboxes import SandboxFilter
from opensandbox.pool import (
    AcquirePolicy,
    AsyncPoolStateStore,
    InMemoryAsyncPoolStateStore,
    PoolCreationSpec,
    PoolSnapshot,
    PoolState,
    SandboxPoolAsync,
)
from opensandbox.pool_redis import AsyncRedisPoolStateStore

from tests.base_e2e_test import (
    create_connection_config,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

MAX_IDLE = 2
RECONCILE_INTERVAL = timedelta(seconds=1)
PRIMARY_LOCK_TTL = timedelta(seconds=4)
DRAIN_TIMEOUT = timedelta(milliseconds=300)
AWAIT_TIMEOUT = timedelta(minutes=2)


@pytest.mark.e2e
class TestSandboxPoolSingleNodeE2EAsync:
    """Single-event-loop async in-memory pool E2E scenarios."""

    @pytest.fixture(autouse=True)
    async def _pool_lifecycle(self):
        self.tag = _tag("py-async-pool")
        self.pool_name = f"pool-{self.tag}"
        self.store = InMemoryAsyncPoolStateStore()
        self.manager = await SandboxManager.create(create_connection_config())
        self.borrowed: list[Sandbox] = []
        self.pool = _create_pool(
            pool_name=self.pool_name,
            owner_id=f"owner-{self.tag}",
            state_store=self.store,
            tag=self.tag,
            max_idle=MAX_IDLE,
        )
        await self.pool.start()
        try:
            yield
        finally:
            await _cleanup_borrowed(self.borrowed)
            await _cleanup_pool(self.pool)
            await _cleanup_tagged_sandboxes(self.manager, self.tag)
            await self.manager.close()

    @pytest.mark.timeout(240)
    async def test_async_warmup_acquire_command_resize_and_shutdown(self) -> None:
        await _eventually(
            "async pool becomes healthy with warm idle",
            lambda: _snapshot_matches(
                self.pool,
                lambda snap: snap.state == PoolState.HEALTHY and snap.idle_count >= 1,
            ),
        )

        sandbox = await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.FAIL_FAST)
        self.borrowed.append(sandbox)
        assert await sandbox.is_healthy()
        result = await sandbox.commands.run("echo py-async-pool-ok")
        assert result.error is None
        assert result.logs.stdout[0].text == "py-async-pool-ok"

        await self.pool.resize(0)
        released = await self.pool.release_all_idle()
        assert released >= 0
        await _eventually(
            "async idle drains after resize zero",
            lambda: _snapshot_matches(self.pool, lambda snap: snap.idle_count == 0),
        )
        with pytest.raises(PoolEmptyException):
            await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.FAIL_FAST)

        direct = await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)
        self.borrowed.append(direct)
        assert await direct.is_healthy()

        await self.pool.shutdown(graceful=True)
        with pytest.raises(PoolNotRunningException):
            await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)

    @pytest.mark.timeout(300)
    async def test_async_lifecycle_idempotency_release_remote_and_rewarm(self) -> None:
        await self.pool.start()
        await _eventually(
            "async pool warms before lifecycle checks",
            lambda: _snapshot_matches(self.pool, lambda snap: snap.idle_count >= 1),
        )

        await self.pool.shutdown(False)
        await self.pool.shutdown(False)
        assert (await self.pool.snapshot()).state == PoolState.STOPPED
        with pytest.raises(PoolNotRunningException):
            await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)

        await self.pool.release_all_idle()
        assert (await self.pool.snapshot()).idle_count == 0
        await self.store.put_idle(self.pool_name, f"injected-a-{uuid.uuid4().hex}")
        await self.store.put_idle(self.pool_name, f"injected-b-{uuid.uuid4().hex}")
        assert await self.pool.release_all_idle() == 2
        assert (await self.pool.snapshot()).idle_count == 0

        await self.pool.start()
        await _eventually(
            "async pool rewarms after restart",
            lambda: _snapshot_matches(self.pool, lambda snap: snap.idle_count >= 1),
        )

        await self.pool.resize(0)
        assert await self.pool.release_all_idle() >= 0
        await _eventually(
            "async releaseAllIdle reduces remote tagged sandboxes",
            lambda: _async_release_drained(self.pool, self.manager, self.tag),
            timeout=timedelta(seconds=60),
        )

        await self.pool.resize(1)
        await _eventually(
            "async resize from zero to positive rewarms idle",
            lambda: _snapshot_matches(
                self.pool,
                lambda snap: snap.state == PoolState.HEALTHY and snap.idle_count >= 1,
            ),
        )

    @pytest.mark.timeout(240)
    async def test_async_stale_idle_preparer_snapshot_and_context_manager(self) -> None:
        await self.store.put_idle(self.pool_name, f"missing-{uuid.uuid4().hex}")
        fallback = await self.pool.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)
        self.borrowed.append(fallback)
        assert await fallback.is_healthy()

        await _cleanup_pool(self.pool)
        marker_path = f"/tmp/{self.tag}-prepared.txt"

        async def preparer(sandbox: Sandbox) -> None:
            result = await sandbox.commands.run(f"printf async-prepared > {marker_path}")
            assert result.error is None

        prepared_pool = _create_pool(
            pool_name=f"prepared-{self.pool_name}",
            owner_id=f"prepared-owner-{self.tag}",
            state_store=InMemoryAsyncPoolStateStore(),
            tag=self.tag,
            max_idle=1,
            warmup_sandbox_preparer=preparer,
        )
        async with prepared_pool:
            await _eventually(
                "async prepared pool warms",
                lambda: _snapshot_matches(prepared_pool, lambda snap: snap.idle_count >= 1),
            )
            entries = await prepared_pool.snapshot_idle_entries()
            assert entries
            assert all(entry.expires_at > datetime.now(timezone.utc) for entry in entries)

            sandbox = await prepared_pool.acquire(timedelta(minutes=5), AcquirePolicy.FAIL_FAST)
            self.borrowed.append(sandbox)
            result = await sandbox.commands.run(f"cat {marker_path}")
            assert result.error is None
            assert result.logs.stdout[0].text == "async-prepared"

    @pytest.mark.timeout(300)
    async def test_async_concurrent_shutdown_and_acquire_does_not_deadlock(self) -> None:
        await _eventually(
            "async pool has warm idle before shutdown race",
            lambda: _snapshot_matches(self.pool, lambda snap: snap.idle_count >= 1),
        )

        start = asyncio.Event()
        errors: list[BaseException] = []

        async def acquire_during_shutdown() -> None:
            try:
                await start.wait()
                sandbox = await self.pool.acquire(
                    timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE
                )
                self.borrowed.append(sandbox)
            except PoolNotRunningException:
                return
            except BaseException as exc:
                errors.append(exc)
                raise

        async def shutdown_during_acquire() -> None:
            await start.wait()
            await self.pool.shutdown(graceful=True)

        tasks = [asyncio.create_task(acquire_during_shutdown()) for _ in range(4)]
        tasks.append(asyncio.create_task(shutdown_during_acquire()))
        start.set()
        await asyncio.wait_for(asyncio.gather(*tasks), timeout=180)

        assert not errors

    @pytest.mark.timeout(300)
    async def test_async_warmup_concurrency_above_one_reaches_target_and_stays_bounded(self) -> None:
        await _cleanup_pool(self.pool)
        concurrent_tag = _tag("py-async-pool-warmup-concurrency")
        concurrent_pool = _create_pool(
            pool_name=f"concurrent-{self.pool_name}",
            owner_id=f"concurrent-owner-{self.tag}",
            state_store=InMemoryAsyncPoolStateStore(),
            tag=concurrent_tag,
            max_idle=3,
            warmup_concurrency=2,
        )
        try:
            await concurrent_pool.start()
            await _eventually(
                "async concurrent warmup fills configured idle target",
                lambda: _snapshot_and_remote_count_match(
                    concurrent_pool,
                    self.manager,
                    concurrent_tag,
                    lambda snap, count: snap.idle_count >= 3 and count <= 3,
                ),
                timeout=timedelta(seconds=90),
            )
        finally:
            await _cleanup_pool(concurrent_pool)
            await _cleanup_tagged_sandboxes(self.manager, concurrent_tag)

    @pytest.mark.timeout(240)
    async def test_async_broken_connection_degrades_and_healthy_pool_still_works(self) -> None:
        await _cleanup_pool(self.pool)
        bad_tag = _tag("py-async-pool-bad")
        bad_pool = _create_pool(
            pool_name=f"bad-{self.pool_name}",
            owner_id=f"bad-owner-{self.tag}",
            state_store=InMemoryAsyncPoolStateStore(),
            tag=bad_tag,
            max_idle=1,
            connection_config=_broken_connection_config(),
            degraded_threshold=1,
            warmup_ready_timeout=timedelta(seconds=1),
            acquire_ready_timeout=timedelta(seconds=1),
        )
        try:
            await bad_pool.start()
            await _eventually(
                "async bad pool enters degraded state",
                lambda: _snapshot_matches(
                    bad_pool, lambda snap: snap.state == PoolState.DEGRADED
                ),
                timeout=timedelta(seconds=60),
            )
            snapshot = await bad_pool.snapshot()
            assert snapshot.last_error
            assert snapshot.idle_count == 0
            with pytest.raises(PoolEmptyException):
                await bad_pool.acquire(timedelta(minutes=1), AcquirePolicy.FAIL_FAST)
            with pytest.raises(Exception):
                await bad_pool.acquire(timedelta(minutes=1), AcquirePolicy.DIRECT_CREATE)
        finally:
            await _cleanup_pool(bad_pool)
            await _cleanup_tagged_sandboxes(self.manager, bad_tag)

        healthy_tag = _tag("py-async-pool-good")
        healthy_pool = _create_pool(
            pool_name=f"healthy-{self.pool_name}",
            owner_id=f"healthy-owner-{self.tag}",
            state_store=InMemoryAsyncPoolStateStore(),
            tag=healthy_tag,
            max_idle=1,
        )
        try:
            await healthy_pool.start()
            await _eventually(
                "async healthy pool still works after broken pool path",
                lambda: _snapshot_matches(healthy_pool, lambda snap: snap.idle_count >= 1),
            )
            sandbox = await healthy_pool.acquire(
                timedelta(minutes=5), AcquirePolicy.FAIL_FAST
            )
            self.borrowed.append(sandbox)
            assert await sandbox.is_healthy()
        finally:
            await _cleanup_pool(healthy_pool)
            await _cleanup_tagged_sandboxes(self.manager, healthy_tag)


@pytest.mark.e2e
class TestSandboxPoolRedisDistributedE2EAsync:
    """Redis-backed async pool E2E scenarios."""

    @pytest.fixture(autouse=True)
    async def _redis_lifecycle(self):
        redis_url = os.getenv("OPENSANDBOX_TEST_REDIS_URL")
        if not redis_url:
            pytest.skip("Set OPENSANDBOX_TEST_REDIS_URL to run Redis-backed pool E2E tests")
        redis_module = pytest.importorskip("redis.asyncio")
        self.redis = redis_module.Redis.from_url(redis_url, decode_responses=True)
        self.key_prefix = f"opensandbox:e2e:{uuid.uuid4()}"
        self.manager = await SandboxManager.create(create_connection_config())
        self.borrowed: list[Sandbox] = []
        self.pools: list[SandboxPoolAsync] = []
        self.tag = _tag("py-async-redis-pool")
        try:
            yield
        finally:
            await _cleanup_borrowed(self.borrowed)
            for pool in self.pools:
                await _cleanup_pool(pool)
            await _cleanup_tagged_sandboxes(self.manager, self.tag)
            await self.manager.close()
            async for key in self.redis.scan_iter(f"{self.key_prefix}:*"):
                await self.redis.delete(key)
            await self.redis.aclose()

    @pytest.mark.timeout(360)
    async def test_async_redis_cross_node_acquire_resize_and_concurrent_uniqueness(self) -> None:
        pool_name = f"async-redis-pool-{self.tag}"
        pool_a = _create_pool(
            pool_name,
            f"owner-a-{self.tag}",
            AsyncRedisPoolStateStore(self.redis, self.key_prefix),
            self.tag,
            2,
        )
        pool_b = _create_pool(
            pool_name,
            f"owner-b-{self.tag}",
            AsyncRedisPoolStateStore(self.redis, self.key_prefix),
            self.tag,
            2,
        )
        self.pools.extend([pool_a, pool_b])
        await pool_a.start()
        await pool_b.start()
        await _eventually(
            "async Redis pool warms two idle",
            lambda: _snapshot_matches(pool_a, lambda snap: snap.idle_count >= 2),
        )

        acquired = await asyncio.gather(
            pool_a.acquire(timedelta(minutes=5), AcquirePolicy.FAIL_FAST),
            pool_b.acquire(timedelta(minutes=5), AcquirePolicy.FAIL_FAST),
        )
        self.borrowed.extend(acquired)
        assert len({sandbox.id for sandbox in acquired}) == 2
        assert all([await sandbox.is_healthy() for sandbox in acquired])

        result = await acquired[0].commands.run("echo py-async-redis-ok")
        assert result.error is None

        await pool_b.resize(0)
        await _eventually(
            "async Redis idle drains after shared resize",
            lambda: _snapshot_matches(pool_a, lambda snap: snap.idle_count == 0),
        )
        await asyncio.sleep(RECONCILE_INTERVAL.total_seconds() * 2)
        assert (await pool_a.snapshot()).idle_count == 0
        with pytest.raises(PoolEmptyException):
            await pool_a.acquire(timedelta(minutes=2), AcquirePolicy.FAIL_FAST)

        direct = await pool_a.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)
        self.borrowed.append(direct)
        assert await direct.is_healthy()
        result = await direct.commands.run("echo py-async-redis-direct-create-ok")
        assert result.error is None
        assert (await pool_a.snapshot()).idle_count == 0

    @pytest.mark.timeout(420)
    async def test_async_redis_primary_failover_and_restart_stay_bounded(self) -> None:
        pool_name = f"async-redis-failover-{self.tag}"
        owner_a = f"owner-a-{self.tag}"
        owner_b = f"owner-b-{self.tag}"
        store_a = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        store_b = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_a = _create_pool(pool_name, owner_a, store_a, self.tag, 1)
        pool_b = _create_pool(pool_name, owner_b, store_b, self.tag, 1)
        self.pools.extend([pool_a, pool_b])
        lock_key = store_a._primary_lock_key(pool_name)

        await pool_a.start()
        await _eventually(
            "async first Redis node owns primary lock and warms",
            lambda: _redis_lock_and_snapshot_match(
                self.redis,
                lock_key,
                owner_a,
                pool_a,
                lambda snap: snap.idle_count >= 1,
            ),
        )

        await pool_b.start()
        await pool_a.shutdown(False)
        await pool_b.resize(1)
        await _eventually(
            "async Redis primary lock fails over",
            lambda: _redis_lock_and_snapshot_match(
                self.redis,
                lock_key,
                owner_b,
                pool_b,
                lambda snap: snap.idle_count >= 1,
            ),
            timeout=timedelta(seconds=60),
        )

        await pool_a.start()
        await pool_b.resize(1)
        await _eventually(
            "async Redis restart stays bounded",
            lambda: _snapshot_and_remote_count_match(
                pool_a,
                self.manager,
                self.tag,
                lambda snap, count: snap.idle_count <= 1 and count <= 2,
            ),
            timeout=timedelta(seconds=60),
        )

    @pytest.mark.timeout(420)
    async def test_async_redis_start_overwrites_stale_shared_max_idle_after_restart(self) -> None:
        pool_name = f"async-redis-restart-config-{self.tag}"
        store_a = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_a = _create_pool(pool_name, f"owner-a-{self.tag}", store_a, self.tag, 1)
        self.pools.append(pool_a)

        await pool_a.start()
        await _eventually(
            "async initial Redis pool warms",
            lambda: _snapshot_matches(pool_a, lambda snap: snap.idle_count >= 1),
        )
        await pool_a.resize(0)
        await _eventually(
            "async initial Redis pool drains to zero",
            lambda: _snapshot_matches(pool_a, lambda snap: snap.idle_count == 0),
        )
        await pool_a.shutdown(False)

        store_b = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_b = _create_pool(pool_name, f"owner-b-{self.tag}", store_b, self.tag, 2)
        self.pools.append(pool_b)
        await pool_b.start()

        await _eventually(
            "async restart with same Redis namespace uses new configured max_idle",
            lambda: _snapshot_matches(
                pool_b,
                lambda snap: snap.max_idle == 2 and snap.idle_count >= 2,
            ),
        )

    @pytest.mark.timeout(420)
    async def test_async_redis_secondary_resize_is_applied_by_primary_periodic_reconcile(self) -> None:
        pool_name = f"async-redis-secondary-resize-{self.tag}"
        owner_a = f"owner-a-{self.tag}"
        owner_b = f"owner-b-{self.tag}"
        store_a = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        store_b = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_a = _create_pool(pool_name, owner_a, store_a, self.tag, 2)
        pool_b = _create_pool(pool_name, owner_b, store_b, self.tag, 2)
        self.pools.extend([pool_a, pool_b])
        lock_key = store_a._primary_lock_key(pool_name)

        await pool_a.start()
        await _eventually(
            "async primary Redis node owns lock and warms",
            lambda: _redis_lock_and_snapshot_match(
                self.redis,
                lock_key,
                owner_a,
                pool_a,
                lambda snap: snap.idle_count >= 2,
            ),
        )
        await pool_b.start()

        await pool_b.resize(0)
        await _eventually(
            "async secondary resize to zero is applied by primary",
            lambda: _redis_lock_and_snapshot_match(
                self.redis,
                lock_key,
                owner_a,
                pool_a,
                lambda snap: snap.idle_count == 0,
            ),
        )
        await pool_b.resize(2)
        await _eventually(
            "async secondary resize up is applied by primary",
            lambda: _redis_lock_and_snapshot_match(
                self.redis,
                lock_key,
                owner_a,
                pool_a,
                lambda snap: snap.idle_count >= 2,
            ),
        )

    @pytest.mark.timeout(420)
    async def test_async_redis_concurrent_acquire_and_resize_jitter_remain_bounded(self) -> None:
        pool_name = f"async-redis-acquire-resize-jitter-{self.tag}"
        store_a = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        store_b = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_a = _create_pool(pool_name, f"owner-a-{self.tag}", store_a, self.tag, 2)
        pool_b = _create_pool(pool_name, f"owner-b-{self.tag}", store_b, self.tag, 2)
        self.pools.extend([pool_a, pool_b])
        await pool_a.start()
        await pool_b.start()
        await _eventually(
            "async Redis jitter pool warms two idle",
            lambda: _snapshot_matches(pool_a, lambda snap: snap.idle_count >= 2),
        )

        async def acquire_once(index: int) -> Sandbox:
            pool = pool_a if index % 2 == 0 else pool_b
            sandbox = await pool.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)
            result = await sandbox.commands.run(f"echo py-async-redis-jitter-{index}")
            assert result.error is None
            return sandbox

        async def resize_jitter() -> None:
            for index in range(8):
                await (pool_a if index % 2 == 0 else pool_b).resize(index % 3)
                await asyncio.sleep(0.2)
            await pool_b.resize(2)

        acquired, _ = await asyncio.gather(
            asyncio.gather(*(acquire_once(i) for i in range(4))),
            resize_jitter(),
        )
        self.borrowed.extend(acquired)
        assert len({sandbox.id for sandbox in acquired}) == 4

        await _eventually(
            "async Redis acquire plus resize jitter converges and stays bounded",
            lambda: _snapshot_and_remote_count_match(
                pool_a,
                self.manager,
                self.tag,
                lambda snap, count: snap.idle_count <= 2 and count <= 8,
            ),
            timeout=timedelta(seconds=90),
        )

    @pytest.mark.timeout(360)
    async def test_async_redis_stale_idle_is_removed_and_direct_create_fallback_works(self) -> None:
        pool_name = f"async-redis-stale-{self.tag}"
        store_a = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        store_b = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_a = _create_pool(pool_name, f"owner-a-{self.tag}", store_a, self.tag, 0)
        pool_b = _create_pool(pool_name, f"owner-b-{self.tag}", store_b, self.tag, 0)
        self.pools.extend([pool_a, pool_b])
        await pool_a.start()
        await pool_b.start()

        await store_a.put_idle(pool_name, f"missing-{uuid.uuid4().hex}")
        with pytest.raises(PoolAcquireFailedException):
            await pool_b.acquire(timedelta(seconds=2), AcquirePolicy.FAIL_FAST)
        assert (await store_a.snapshot_counters(pool_name)).idle_count == 0

        sandbox = await pool_b.acquire(timedelta(minutes=5), AcquirePolicy.DIRECT_CREATE)
        self.borrowed.append(sandbox)
        assert await sandbox.is_healthy()

    @pytest.mark.timeout(60)
    async def test_async_redis_expired_idle_is_not_removed_by_snapshot_but_take_reaps_it(self) -> None:
        store = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        pool_name = f"async-redis-expired-idle-{self.tag}"

        await store.set_idle_entry_ttl(pool_name, timedelta(milliseconds=50))
        await store.put_idle(pool_name, f"expired-{uuid.uuid4().hex}")
        await asyncio.sleep(0.1)

        assert (await store.snapshot_counters(pool_name)).idle_count == 1
        assert await store.try_take_idle(pool_name) is None
        assert (await store.snapshot_counters(pool_name)).idle_count == 0

    @pytest.mark.timeout(420)
    async def test_async_redis_lost_lock_window_discards_orphan_and_recovers(self) -> None:
        pool_name = f"async-redis-renew-window-{self.tag}"
        owner = f"owner-a-{self.tag}"
        store = AsyncRedisPoolStateStore(self.redis, self.key_prefix)
        lock_key = store._primary_lock_key(pool_name)
        dropped_once = asyncio.Event()

        async def drop_lock_once(_: Sandbox) -> None:
            if not dropped_once.is_set():
                dropped_once.set()
                await self.redis.delete(lock_key)

        pool = _create_pool(
            pool_name,
            owner,
            store,
            self.tag,
            1,
            warmup_sandbox_preparer=drop_lock_once,
        )
        self.pools.append(pool)

        await pool.start()
        await _eventually(
            "async Redis pool recovers after losing primary lock during warmup",
            lambda: _snapshot_matches(pool, lambda snap: snap.idle_count == 1),
            timeout=timedelta(seconds=90),
            interval=timedelta(milliseconds=500),
        )
        await _eventually(
            "async lost-lock orphan cleanup keeps remote count bounded",
            lambda: _remote_count_matches(self.manager, self.tag, lambda count: count == 1),
            timeout=timedelta(seconds=60),
        )


def _create_pool(
    pool_name: str,
    owner_id: str,
    state_store: AsyncPoolStateStore,
    tag: str,
    max_idle: int,
    warmup_sandbox_preparer: Callable[[Sandbox], Awaitable[None]] | None = None,
    connection_config: ConnectionConfig | None = None,
    degraded_threshold: int = 3,
    warmup_ready_timeout: timedelta = timedelta(seconds=30),
    acquire_ready_timeout: timedelta = timedelta(seconds=30),
    warmup_concurrency: int = 1,
) -> SandboxPoolAsync:
    return SandboxPoolAsync(
        pool_name=pool_name,
        owner_id=owner_id,
        max_idle=max_idle,
        warmup_concurrency=warmup_concurrency,
        state_store=state_store,
        connection_config=connection_config or create_connection_config(),
        creation_spec=PoolCreationSpec(
            image=get_sandbox_image(),
            entrypoint=["tail", "-f", "/dev/null"],
            metadata={"tag": tag, "suite": "sandbox-pool-python-async-e2e"},
            env={
                "E2E_TEST": "true",
                "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "1s",
            },
            resource=get_e2e_sandbox_resource(),
        ),
        reconcile_interval=RECONCILE_INTERVAL,
        primary_lock_ttl=PRIMARY_LOCK_TTL,
        drain_timeout=DRAIN_TIMEOUT,
        warmup_sandbox_preparer=warmup_sandbox_preparer,
        degraded_threshold=degraded_threshold,
        warmup_ready_timeout=warmup_ready_timeout,
        acquire_ready_timeout=acquire_ready_timeout,
    )


def _broken_connection_config() -> ConnectionConfig:
    return ConnectionConfig(
        domain="127.0.0.1:9",
        api_key="broken-e2e-test",
        request_timeout=timedelta(seconds=1),
        transport=httpx.AsyncHTTPTransport(
            limits=httpx.Limits(max_connections=2, max_keepalive_connections=0)
        ),
    )


async def _eventually(
    description: str,
    condition: Callable[[], Awaitable[bool]],
    timeout: timedelta = AWAIT_TIMEOUT,
    interval: timedelta = timedelta(seconds=1),
) -> None:
    deadline = asyncio.get_running_loop().time() + timeout.total_seconds()
    last_error: BaseException | None = None
    while asyncio.get_running_loop().time() < deadline:
        try:
            if await condition():
                return
        except BaseException as exc:
            last_error = exc
        await asyncio.sleep(interval.total_seconds())
    if last_error is not None:
        raise AssertionError(f"Timed out waiting for {description}") from last_error
    raise AssertionError(f"Timed out waiting for {description}")


async def _snapshot_matches(
    pool: SandboxPoolAsync,
    predicate: Callable[[PoolSnapshot], bool],
) -> bool:
    return predicate(await pool.snapshot())


async def _cleanup_pool(pool: SandboxPoolAsync) -> None:
    try:
        await pool.resize(0)
    except Exception:
        pass
    try:
        await pool.release_all_idle()
    except Exception:
        pass
    try:
        await pool.shutdown(False)
    except Exception:
        pass


async def _cleanup_borrowed(sandboxes: list[Sandbox]) -> None:
    for sandbox in sandboxes:
        try:
            await sandbox.kill()
        except Exception:
            pass
        try:
            await sandbox.close()
        except Exception:
            pass
    sandboxes.clear()


async def _cleanup_tagged_sandboxes(manager: SandboxManager, tag: str) -> None:
    for _ in range(5):
        try:
            infos = await manager.list_sandbox_infos(
                SandboxFilter(metadata={"tag": tag}, page_size=50)
            )
            if not infos.sandbox_infos:
                return
            for info in infos.sandbox_infos:
                try:
                    await manager.kill_sandbox(info.id)
                except Exception:
                    pass
        except Exception:
            return


async def _count_tagged_sandboxes(manager: SandboxManager, tag: str) -> int:
    infos = await manager.list_sandbox_infos(SandboxFilter(metadata={"tag": tag}, page_size=50))
    return len(infos.sandbox_infos)


async def _async_release_drained(
    pool: SandboxPoolAsync,
    manager: SandboxManager,
    tag: str,
) -> bool:
    snapshot = await pool.snapshot()
    return snapshot.idle_count == 0 and await _count_tagged_sandboxes(manager, tag) == 0


async def _redis_lock_and_snapshot_match(
    redis: object,
    lock_key: str,
    owner_id: str,
    pool: SandboxPoolAsync,
    predicate: Callable[[PoolSnapshot], bool],
) -> bool:
    owner = await redis.get(lock_key)  # type: ignore[attr-defined]
    return owner == owner_id and predicate(await pool.snapshot())


async def _snapshot_and_remote_count_match(
    pool: SandboxPoolAsync,
    manager: SandboxManager,
    tag: str,
    predicate: Callable[[PoolSnapshot, int], bool],
) -> bool:
    return predicate(await pool.snapshot(), await _count_tagged_sandboxes(manager, tag))


async def _remote_count_matches(
    manager: SandboxManager,
    tag: str,
    predicate: Callable[[int], bool],
) -> bool:
    return predicate(await _count_tagged_sandboxes(manager, tag))


def _tag(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:8]}"
