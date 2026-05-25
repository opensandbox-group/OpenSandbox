/*
 * Copyright 2025 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.e2e;

import static org.junit.jupiter.api.Assertions.*;
import static org.junit.jupiter.api.Assumptions.assumeTrue;

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.SandboxManager;
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolAcquireFailedException;
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.RedisPoolStateStore;
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.function.BooleanSupplier;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;
import redis.clients.jedis.JedisPooled;

@Tag("e2e")
@DisplayName("SandboxPool E2E Tests (Redis Distributed)")
public class SandboxPoolRedisDistributedE2ETest extends BaseE2ETest {
    private static final Duration RECONCILE_INTERVAL = Duration.ofSeconds(1);
    private static final Duration PRIMARY_LOCK_TTL = Duration.ofSeconds(4);
    private static final Duration DRAIN_TIMEOUT = Duration.ofMillis(200);
    private static final Duration AWAIT_TIMEOUT = Duration.ofMinutes(2);

    private final List<SandboxPool> pools = new ArrayList<>();
    private final List<Sandbox> borrowed = new CopyOnWriteArrayList<>();

    private JedisPooled redis;
    private SandboxManager sandboxManager;
    private String keyPrefix;
    private String tag;

    @BeforeEach
    void setupRedis() {
        String redisUrl = System.getenv("OPENSANDBOX_TEST_REDIS_URL");
        assumeTrue(
                redisUrl != null && !redisUrl.isBlank(),
                "Set OPENSANDBOX_TEST_REDIS_URL to run Redis-backed pool E2E tests");
        redis = new JedisPooled(redisUrl);
        keyPrefix = "opensandbox:e2e:" + UUID.randomUUID();
    }

    @AfterEach
    void teardown() {
        for (Sandbox sandbox : borrowed) {
            killAndCloseQuietly(sandbox);
        }
        borrowed.clear();

        for (SandboxPool pool : pools) {
            try {
                pool.resize(0);
            } catch (Exception ignored) {
            }
            try {
                pool.releaseAllIdle();
            } catch (Exception ignored) {
            }
            try {
                pool.shutdown(false);
            } catch (Exception ignored) {
            }
        }
        pools.clear();

        if (sandboxManager != null && tag != null) {
            cleanupTaggedSandboxes(tag);
        }
        if (sandboxManager != null) {
            try {
                sandboxManager.close();
            } catch (Exception ignored) {
            }
        }
        if (redis != null) {
            cleanupRedisKeys();
            try {
                redis.close();
            } catch (Exception ignored) {
            }
        }
    }

    @Test
    @DisplayName("Redis store supports cross-node acquire, shared resize, and idle drain")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testCrossNodeAcquireResizeAndDrain() throws Exception {
        tag = "e2e-redis-pool-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-pool-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 2);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 2);
        pools.add(poolA);
        pools.add(poolB);

        poolA.start();
        poolB.start();

        eventually(
                "Redis-backed distributed pool warmed idle",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() >= 1);

        Sandbox sandbox = poolB.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
        borrowed.add(sandbox);
        assertTrue(sandbox.isHealthy(), "cross-node acquire should return a healthy sandbox");
        Execution execution =
                sandbox.commands()
                        .run(RunCommandRequest.builder().command("echo redis-dist-ok").build());
        assertNotNull(execution);
        assertNull(execution.getError());

        poolB.resize(0);
        eventually(
                "Redis-backed idle drains after resize(0)",
                Duration.ofSeconds(45),
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() == 0);

        Thread.sleep(RECONCILE_INTERVAL.multipliedBy(3).toMillis());
        assertEquals(
                0, poolA.snapshot().getIdleCount(), "idle should stay at zero after resize(0)");
        assertThrows(
                PoolEmptyException.class,
                () -> poolA.acquire(Duration.ofMinutes(2), AcquirePolicy.FAIL_FAST));

        Sandbox direct = poolA.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
        borrowed.add(direct);
        assertTrue(
                direct.isHealthy(), "direct create should still work when shared maxIdle is zero");
        Execution directExecution =
                direct.commands()
                        .run(RunCommandRequest.builder().command("echo redis-direct-ok").build());
        assertNotNull(directExecution);
        assertNull(directExecution.getError());
        assertEquals(
                0,
                poolA.snapshot().getIdleCount(),
                "DIRECT_CREATE must not repopulate the shared idle store");
    }

    @Test
    @DisplayName("Redis primary lock fails over after leader shutdown")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testPrimaryFailoverAfterLeaderShutdown() throws Exception {
        tag = "e2e-redis-failover-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-failover-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 1);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 1);
        pools.add(poolA);
        pools.add(poolB);

        poolA.start();

        eventually(
                "first Redis-backed node warms idle",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() >= 1);

        int beforeShutdown = poolA.snapshot().getIdleCount();
        poolB.start();
        poolA.shutdown(false);
        poolB.resize(0);
        eventually(
                "remaining node applies shared resize after peer shutdown",
                Duration.ofSeconds(45),
                Duration.ofSeconds(1),
                () -> poolB.snapshot().getIdleCount() == 0);

        poolB.resize(1);
        eventually(
                "remaining node replenishes after failover",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolB.snapshot().getIdleCount() >= 1);
        assertTrue(beforeShutdown >= 1, "poolA should have warmed idle before shutdown");
    }

    @Test
    @DisplayName("Redis start overwrites stale shared maxIdle after restart")
    @Timeout(value = 7, unit = TimeUnit.MINUTES)
    void testStartOverwritesStaleSharedMaxIdleAfterRestart() throws Exception {
        tag = "e2e-redis-restart-config-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-restart-config-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 1);
        pools.add(poolA);

        poolA.start();
        eventually(
                "initial Redis-backed pool warms",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() >= 1);
        poolA.resize(0);
        eventually(
                "initial Redis-backed pool drains to zero",
                Duration.ofSeconds(45),
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() == 0);
        poolA.shutdown(false);

        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 2);
        pools.add(poolB);
        poolB.start();

        eventually(
                "restart with same Redis namespace uses new configured maxIdle",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolB.snapshot().getMaxIdle() == 2 && poolB.snapshot().getIdleCount() >= 2);
    }

    @Test
    @DisplayName("Redis secondary resize is applied by primary periodic reconcile")
    @Timeout(value = 7, unit = TimeUnit.MINUTES)
    void testSecondaryResizeAppliedByPrimaryPeriodicReconcile() throws Exception {
        tag = "e2e-redis-secondary-resize-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-secondary-resize-" + tag;
        String ownerA = "owner-a-" + tag;
        String ownerB = "owner-b-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, ownerA, storeA, 2);
        SandboxPool poolB = createPool(poolName, ownerB, storeB, 2);
        pools.add(poolA);
        pools.add(poolB);
        String lockKey = poolKey(poolName, "lock");

        poolA.start();
        eventually(
                "primary Redis-backed node owns lock and warms",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> ownerA.equals(redis.get(lockKey)) && poolA.snapshot().getIdleCount() >= 2);
        poolB.start();

        poolB.resize(0);
        eventually(
                "secondary resize to zero is applied by primary",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> ownerA.equals(redis.get(lockKey)) && poolA.snapshot().getIdleCount() == 0);
        poolB.resize(2);
        eventually(
                "secondary resize up is applied by primary",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> ownerA.equals(redis.get(lockKey)) && poolA.snapshot().getIdleCount() >= 2);
    }

    @Test
    @DisplayName("Redis idle take is unique under concurrent cross-node acquire")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testConcurrentCrossNodeAcquireDoesNotDuplicateIdle() throws Exception {
        tag = "e2e-redis-concurrent-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-concurrent-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 2);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 2);
        pools.add(poolA);
        pools.add(poolB);

        poolA.start();
        poolB.start();
        eventually(
                "Redis-backed pool warms two idle sandboxes",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() >= 2);

        ExecutorService executor = Executors.newFixedThreadPool(2);
        CountDownLatch start = new CountDownLatch(1);
        Set<String> acquiredIds = ConcurrentHashMap.newKeySet();
        List<Future<Sandbox>> futures = new ArrayList<>();
        futures.add(
                executor.submit(
                        () -> {
                            start.await();
                            return poolA.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
                        }));
        futures.add(
                executor.submit(
                        () -> {
                            start.await();
                            return poolB.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
                        }));
        start.countDown();

        try {
            for (Future<Sandbox> future : futures) {
                Sandbox sandbox = future.get(90, TimeUnit.SECONDS);
                borrowed.add(sandbox);
                assertTrue(sandbox.isHealthy(), "concurrent acquire should return healthy sandbox");
                assertTrue(
                        acquiredIds.add(sandbox.getId()), "sandbox ID must not be acquired twice");
            }
        } finally {
            executor.shutdownNow();
        }

        assertEquals(
                2, acquiredIds.size(), "two concurrent acquires should get two distinct sandboxes");
    }

    @Test
    @DisplayName("Redis store atomic take remains unique under local contention")
    @Timeout(value = 1, unit = TimeUnit.MINUTES)
    void testRedisStoreAtomicTakeUnderContention() throws Exception {
        String poolName = "redis-store-contention-" + UUID.randomUUID();
        RedisPoolStateStore store = new RedisPoolStateStore(redis, keyPrefix);
        int idleCount = 50;
        int workerCount = 16;
        for (int i = 0; i < idleCount; i++) {
            store.putIdle(poolName, "id-" + i);
        }

        ExecutorService executor = Executors.newFixedThreadPool(workerCount);
        CountDownLatch start = new CountDownLatch(1);
        Set<String> taken = ConcurrentHashMap.newKeySet();
        List<Future<?>> futures = new ArrayList<>();
        for (int i = 0; i < workerCount; i++) {
            futures.add(
                    executor.submit(
                            () -> {
                                start.await();
                                while (true) {
                                    String id = store.tryTakeIdle(poolName);
                                    if (id == null) {
                                        return null;
                                    }
                                    assertTrue(taken.add(id), "duplicate idle ID taken: " + id);
                                }
                            }));
        }
        start.countDown();

        try {
            for (Future<?> future : futures) {
                future.get(30, TimeUnit.SECONDS);
            }
        } finally {
            executor.shutdownNow();
        }

        assertEquals(idleCount, taken.size());
        assertEquals(0, store.snapshotCounters(poolName).getIdleCount());
    }

    @Test
    @DisplayName("Redis expired idle is not removed by snapshot but take reaps it")
    @Timeout(value = 1, unit = TimeUnit.MINUTES)
    void testExpiredIdleIsNotRemovedBySnapshotButTakeReapsIt() throws Exception {
        String poolName = "redis-expired-idle-" + UUID.randomUUID();
        RedisPoolStateStore store = new RedisPoolStateStore(redis, keyPrefix);

        store.setIdleEntryTtl(poolName, Duration.ofMillis(50));
        store.putIdle(poolName, "expired-" + UUID.randomUUID());
        Thread.sleep(100);

        assertEquals(1, store.snapshotCounters(poolName).getIdleCount());
        assertNull(store.tryTakeIdle(poolName));
        assertEquals(0, store.snapshotCounters(poolName).getIdleCount());
    }

    @Test
    @DisplayName("Redis concurrent acquire and resize jitter stay bounded")
    @Timeout(value = 7, unit = TimeUnit.MINUTES)
    void testConcurrentAcquireAndResizeJitterStayBounded() throws Exception {
        tag = "e2e-redis-acquire-resize-jitter-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-acquire-resize-jitter-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 2);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 2);
        pools.add(poolA);
        pools.add(poolB);
        poolA.start();
        poolB.start();
        eventually(
                "Redis-backed jitter pool warms two idle sandboxes",
                AWAIT_TIMEOUT,
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() >= 2);

        ExecutorService executor = Executors.newFixedThreadPool(5);
        Set<String> acquiredIds = ConcurrentHashMap.newKeySet();
        List<Future<?>> futures = new ArrayList<>();
        for (int i = 0; i < 4; i++) {
            final int index = i;
            futures.add(
                    executor.submit(
                            () -> {
                                SandboxPool pool = index % 2 == 0 ? poolA : poolB;
                                Sandbox sandbox =
                                        pool.acquire(
                                                Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
                                borrowed.add(sandbox);
                                assertTrue(
                                        acquiredIds.add(sandbox.getId()),
                                        "sandbox ID must not be acquired twice");
                                Execution execution =
                                        sandbox.commands()
                                                .run(
                                                        RunCommandRequest.builder()
                                                                .command(
                                                                        "echo redis-jitter-"
                                                                                + index)
                                                                .build());
                                assertNotNull(execution);
                                assertNull(execution.getError());
                                return null;
                            }));
        }
        futures.add(
                executor.submit(
                        () -> {
                            for (int i = 0; i < 8; i++) {
                                (i % 2 == 0 ? poolA : poolB).resize(i % 3);
                                Thread.sleep(200);
                            }
                            poolB.resize(2);
                            return null;
                        }));

        try {
            for (Future<?> future : futures) {
                future.get(180, TimeUnit.SECONDS);
            }
        } finally {
            executor.shutdownNow();
        }
        assertEquals(4, acquiredIds.size());
        eventually(
                "Redis-backed acquire plus resize jitter converges and stays bounded",
                Duration.ofSeconds(90),
                Duration.ofSeconds(1),
                () -> poolA.snapshot().getIdleCount() <= 2 && countTaggedSandboxes(tag) <= 8);
    }

    @Test
    @DisplayName("Redis stale idle is removed and direct-create fallback works")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testStaleIdleIsRemovedAndDirectCreateFallbackWorks() throws Exception {
        tag = "e2e-redis-stale-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-stale-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        RedisPoolStateStore storeA = new RedisPoolStateStore(redis, keyPrefix);
        RedisPoolStateStore storeB = new RedisPoolStateStore(redis, keyPrefix);
        SandboxPool poolA = createPool(poolName, "owner-a-" + tag, storeA, 0);
        SandboxPool poolB = createPool(poolName, "owner-b-" + tag, storeB, 0);
        pools.add(poolA);
        pools.add(poolB);
        poolA.start();
        poolB.start();

        storeA.putIdle(poolName, "missing-" + UUID.randomUUID());
        assertThrows(
                PoolAcquireFailedException.class,
                () -> poolB.acquire(Duration.ofSeconds(2), AcquirePolicy.FAIL_FAST));
        assertEquals(0, storeA.snapshotCounters(poolName).getIdleCount());

        Sandbox sandbox = poolB.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
        borrowed.add(sandbox);
        assertTrue(sandbox.isHealthy(), "direct create should work after stale idle removal");
    }

    @Test
    @DisplayName("Redis store drops lost-lock warmup orphan and recovers")
    @Timeout(value = 7, unit = TimeUnit.MINUTES)
    void testLostLockWindowDropsWarmupOrphanAndRecovers() throws Exception {
        tag = "e2e-redis-renew-window-" + UUID.randomUUID().toString().substring(0, 8);
        String poolName = "redis-renew-window-" + tag;
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        AtomicBoolean droppedOnce = new AtomicBoolean(false);
        RedisPoolStateStore store = new RedisPoolStateStore(redis, keyPrefix);
        String lockKey = poolKey(poolName, "lock");
        SandboxPool pool =
                createPoolBuilder(poolName, "owner-a-" + tag, store, 1)
                        .warmupSandboxPreparer(
                                sandbox -> {
                                    if (droppedOnce.compareAndSet(false, true)) {
                                        redis.del(lockKey);
                                    }
                                })
                        .build();
        pools.add(pool);

        pool.start();
        eventually(
                "Redis-backed pool recovers after losing primary lock during warmup",
                Duration.ofSeconds(90),
                Duration.ofMillis(500),
                () -> pool.snapshot().getIdleCount() == 1);
        eventually(
                "lost-lock orphan cleanup keeps remote tagged count bounded",
                Duration.ofSeconds(60),
                Duration.ofSeconds(1),
                () -> countTaggedSandboxes(tag) == 1);
    }

    private SandboxPool createPool(
            String poolName, String ownerId, RedisPoolStateStore store, int maxIdle) {
        return createPoolBuilder(poolName, ownerId, store, maxIdle).build();
    }

    private SandboxPool.Builder createPoolBuilder(
            String poolName, String ownerId, RedisPoolStateStore store, int maxIdle) {
        PoolCreationSpec creationSpec =
                PoolCreationSpec.builder()
                        .image(getSandboxImage())
                        .entrypoint(List.of("tail -f /dev/null"))
                        .metadata(Map.of("tag", tag, "suite", "sandbox-pool-redis-e2e"))
                        .env(
                                Map.of(
                                        "E2E_TEST",
                                        "true",
                                        "EXECD_API_GRACE_SHUTDOWN",
                                        "3s",
                                        "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                        "1s"))
                        .build();
        return SandboxPool.builder()
                .poolName(poolName)
                .ownerId(ownerId)
                .maxIdle(maxIdle)
                .warmupConcurrency(1)
                .stateStore(store)
                .connectionConfig(sharedConnectionConfig)
                .creationSpec(creationSpec)
                .reconcileInterval(RECONCILE_INTERVAL)
                .primaryLockTtl(PRIMARY_LOCK_TTL)
                .drainTimeout(DRAIN_TIMEOUT);
    }

    private void cleanupTaggedSandboxes(String cleanupTag) {
        for (int i = 0; i < 5; i++) {
            try {
                PagedSandboxInfos infos =
                        sandboxManager.listSandboxInfos(
                                SandboxFilter.builder()
                                        .metadata(Map.of("tag", cleanupTag))
                                        .pageSize(50)
                                        .build());
                if (infos.getSandboxInfos().isEmpty()) {
                    return;
                }
                infos.getSandboxInfos()
                        .forEach(
                                info -> {
                                    try {
                                        sandboxManager.killSandbox(info.getId());
                                    } catch (Exception ignored) {
                                    }
                                });
            } catch (Exception ignored) {
                return;
            }
        }
    }

    private int countTaggedSandboxes(String queryTag) {
        if (sandboxManager == null || queryTag == null || queryTag.isBlank()) {
            return 0;
        }
        PagedSandboxInfos infos =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", queryTag))
                                .pageSize(50)
                                .build());
        return infos.getSandboxInfos().size();
    }

    private String poolKey(String poolName, String suffix) {
        String tag =
                java.util.Base64.getUrlEncoder()
                        .withoutPadding()
                        .encodeToString(poolName.getBytes(java.nio.charset.StandardCharsets.UTF_8));
        return keyPrefix + ":{" + tag + "}:" + suffix;
    }

    private void cleanupRedisKeys() {
        if (keyPrefix == null) {
            return;
        }
        try {
            Set<String> keys = redis.keys(keyPrefix + "*");
            if (!keys.isEmpty()) {
                redis.del(keys.toArray(String[]::new));
            }
        } catch (Exception ignored) {
        }
    }

    private void eventually(
            String description, Duration timeout, Duration interval, BooleanSupplier condition)
            throws InterruptedException {
        long deadline = System.currentTimeMillis() + timeout.toMillis();
        Throwable lastError = null;
        while (System.currentTimeMillis() < deadline) {
            try {
                if (condition.getAsBoolean()) {
                    return;
                }
            } catch (Throwable t) {
                lastError = t;
            }
            Thread.sleep(interval.toMillis());
        }
        if (lastError != null) {
            fail(
                    "Timed out waiting for "
                            + description
                            + ", last error: "
                            + lastError.getMessage());
        } else {
            fail("Timed out waiting for " + description);
        }
    }

    private static void killAndCloseQuietly(Sandbox sandbox) {
        if (sandbox == null) {
            return;
        }
        try {
            sandbox.kill();
        } catch (Exception ignored) {
        }
        try {
            sandbox.close();
        } catch (Exception ignored) {
        }
    }
}
