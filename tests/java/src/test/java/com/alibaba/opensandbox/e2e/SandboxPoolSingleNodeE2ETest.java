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

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.SandboxManager;
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig;
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolEmptyException;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.domain.pool.IdleEntry;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolState;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CompletionException;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.function.BooleanSupplier;
import java.util.stream.Collectors;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.MethodOrderer;
import org.junit.jupiter.api.Order;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestMethodOrder;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("SandboxPool E2E Tests (Single-Node)")
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class SandboxPoolSingleNodeE2ETest extends BaseE2ETest {
    private static final int MAX_IDLE = 3;
    private static final int CONCURRENT_BORROW = 3;
    private static final int MAX_TOTAL_SANDBOX_TOLERANCE = 7;

    private SandboxPool pool;
    private InMemoryPoolStateStore stateStore;
    private SandboxManager sandboxManager;
    private String poolName;
    private String tag;
    private final List<Sandbox> borrowed = new CopyOnWriteArrayList<>();

    @BeforeEach
    void setup() {
        tag = "e2e-pool-" + UUID.randomUUID().toString().substring(0, 8);
        poolName = "pool-" + tag;
        stateStore = new InMemoryPoolStateStore();
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();

        PoolCreationSpec creationSpec =
                PoolCreationSpec.builder()
                        .image(getSandboxImage())
                        .entrypoint(List.of("tail -f /dev/null"))
                        .metadata(Map.of("tag", tag, "suite", "sandbox-pool-e2e"))
                        .env(
                                Map.of(
                                        "E2E_TEST",
                                        "true",
                                        "EXECD_API_GRACE_SHUTDOWN",
                                        "3s",
                                        "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                        "1s"))
                        .build();
        pool =
                SandboxPool.builder()
                        .poolName(poolName)
                        .ownerId("owner-" + tag)
                        .maxIdle(MAX_IDLE)
                        .warmupConcurrency(1)
                        .stateStore(stateStore)
                        .connectionConfig(sharedConnectionConfig)
                        .creationSpec(creationSpec)
                        .reconcileInterval(Duration.ofSeconds(2))
                        .drainTimeout(Duration.ofMillis(200))
                        .build();
        pool.start();
    }

    @AfterEach
    void teardown() {
        for (Sandbox sandbox : borrowed) {
            killAndCloseQuietly(sandbox);
        }
        borrowed.clear();

        if (pool != null) {
            try {
                pool.releaseAllIdle();
            } catch (Exception ignored) {
            }
            try {
                pool.shutdown(false);
            } catch (Exception ignored) {
            }
        }

        cleanupTaggedSandboxes();

        if (sandboxManager != null) {
            try {
                sandboxManager.close();
            } catch (Exception ignored) {
            }
        }
    }

    @Test
    @Order(1)
    @DisplayName("warmup + acquire(FAIL_FAST) + basic command run")
    @Timeout(value = 3, unit = TimeUnit.MINUTES)
    void testBasicPoolFlow() throws InterruptedException {
        eventually(
                "pool becomes healthy with warm idle",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () ->
                        pool.snapshot().getState() == PoolState.HEALTHY
                                && pool.snapshot().getIdleCount() >= 1);

        Sandbox sandbox = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
        borrowed.add(sandbox);
        assertTrue(sandbox.isHealthy(), "acquired sandbox should be healthy");

        Execution result =
                sandbox.commands()
                        .run(RunCommandRequest.builder().command("echo pool-basic-ok").build());
        assertNotNull(result);
        assertNull(result.getError());
        assertFalse(result.getLogs().getStdout().isEmpty());
        assertEquals("pool-basic-ok", result.getLogs().getStdout().get(0).getText());
    }

    @Test
    @Order(2)
    @DisplayName("resize/release supports FAIL_FAST on empty and DIRECT_CREATE fallback")
    @Timeout(value = 3, unit = TimeUnit.MINUTES)
    void testResizeReleaseAndFallbackPolicy() throws InterruptedException {
        eventually(
                "pool has warmed idle sandboxes",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getIdleCount() >= 1);

        pool.resize(0);
        int released = pool.releaseAllIdle();
        assertTrue(released >= 0, "releaseAllIdle should return non-negative count");

        eventually(
                "idle is drained",
                Duration.ofSeconds(30),
                Duration.ofSeconds(1),
                () -> pool.snapshot().getIdleCount() == 0);

        assertThrows(
                PoolEmptyException.class,
                () -> pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST));

        Sandbox direct = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
        borrowed.add(direct);
        assertTrue(direct.isHealthy(), "direct-created sandbox should be healthy");
    }

    @Test
    @Order(3)
    @DisplayName("stale idle id fallback works and acquire is rejected after graceful shutdown")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testStaleIdleAndShutdownAcquire() throws InterruptedException {
        String staleId = "non-existent-" + System.nanoTime();
        stateStore.putIdle(poolName, staleId);

        Sandbox staleFallback = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
        borrowed.add(staleFallback);
        assertTrue(staleFallback.isHealthy(), "stale-id fallback should return healthy sandbox");

        pool.shutdown(true);
        SandboxException afterShutdownError =
                assertThrows(
                        SandboxException.class,
                        () -> pool.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE));
        assertEquals("POOL_NOT_RUNNING", afterShutdownError.getError().getCode());
    }

    @Test
    @Order(4)
    @DisplayName("concurrent acquire keeps total pool sandboxes under tolerance")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testConcurrentAcquireWithinTolerance() throws Exception {
        eventually(
                "pool reaches target idle",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getIdleCount() >= MAX_IDLE);

        CountDownLatch startLatch = new CountDownLatch(1);
        CountDownLatch acquiredLatch = new CountDownLatch(CONCURRENT_BORROW);
        CountDownLatch releaseLatch = new CountDownLatch(1);
        ExecutorService executor = Executors.newFixedThreadPool(CONCURRENT_BORROW);
        try {
            List<CompletableFuture<Void>> futures = new ArrayList<>();
            for (int i = 0; i < CONCURRENT_BORROW; i++) {
                futures.add(
                        CompletableFuture.runAsync(
                                () -> {
                                    try {
                                        startLatch.await();
                                        Sandbox sandbox =
                                                pool.acquire(
                                                        Duration.ofMinutes(5),
                                                        AcquirePolicy.DIRECT_CREATE);
                                        borrowed.add(sandbox);
                                        acquiredLatch.countDown();
                                        sandbox.commands()
                                                .run(
                                                        RunCommandRequest.builder()
                                                                .command(
                                                                        "sh -c 'sleep 2; echo pool-concurrency-ok'")
                                                                .build());
                                        releaseLatch.await();
                                    } catch (InterruptedException e) {
                                        Thread.currentThread().interrupt();
                                        throw new CompletionException(e);
                                    } catch (Exception e) {
                                        throw new CompletionException(e);
                                    }
                                },
                                executor));
            }

            startLatch.countDown();
            assertTrue(
                    acquiredLatch.await(2, TimeUnit.MINUTES),
                    "concurrent acquires should complete in time");

            int observedMax = 0;
            for (int i = 0; i < 10; i++) {
                observedMax = Math.max(observedMax, countTaggedSandboxes());
                Thread.sleep(500);
            }
            assertTrue(
                    observedMax <= MAX_TOTAL_SANDBOX_TOLERANCE,
                    "observed tagged sandbox count exceeded tolerance, max=" + observedMax);

            releaseLatch.countDown();
            for (CompletableFuture<Void> future : futures) {
                future.get(2, TimeUnit.MINUTES);
            }
        } finally {
            releaseLatch.countDown();
            executor.shutdownNow();
        }
    }

    @Test
    @Order(5)
    @DisplayName("pool start/shutdown is idempotent and acquire is rejected after stop")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testLifecycleIdempotencyAndAcquireAfterStop() throws InterruptedException {
        pool.start();
        pool.start();

        eventually(
                "pool reaches healthy state after repeated start",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getState() == PoolState.HEALTHY);

        pool.shutdown(true);
        pool.shutdown(true);
        pool.shutdown(false);

        assertEquals(PoolState.STOPPED, pool.snapshot().getState(), "pool should remain STOPPED");
        SandboxException stoppedError =
                assertThrows(
                        SandboxException.class,
                        () -> pool.acquire(Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE));
        assertEquals("POOL_NOT_RUNNING", stoppedError.getError().getCode());
    }

    @Test
    @Order(6)
    @DisplayName("releaseAllIdle drains injected idle ids and stopped pool rejects acquire")
    @Timeout(value = 3, unit = TimeUnit.MINUTES)
    void testReleaseAllIdleAfterStopAndFailFastAcquireFallback() {
        pool.shutdown(false);
        assertEquals(PoolState.STOPPED, pool.snapshot().getState(), "pool should be STOPPED");
        pool.releaseAllIdle();
        assertEquals(
                0,
                stateStore.snapshotCounters(poolName).getIdleCount(),
                "pre-existing idle should be drained before injection");

        String fake1 = "fake-id-1-" + System.nanoTime();
        String fake2 = "fake-id-2-" + System.nanoTime();
        stateStore.putIdle(poolName, fake1);
        stateStore.putIdle(poolName, fake2);
        assertEquals(2, stateStore.snapshotCounters(poolName).getIdleCount());

        int released = pool.releaseAllIdle();
        assertEquals(2, released, "releaseAllIdle should drain all injected idle ids");
        assertEquals(0, stateStore.snapshotCounters(poolName).getIdleCount());

        SandboxException stoppedFailFastError =
                assertThrows(
                        SandboxException.class,
                        () -> pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST));
        assertEquals("POOL_NOT_RUNNING", stoppedFailFastError.getError().getCode());
    }

    @Test
    @Order(7)
    @DisplayName("pool can restart after stop and rewarm idle sandboxes")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testRestartAfterStopRewarmsIdle() throws InterruptedException {
        pool.shutdown(false);
        assertEquals(PoolState.STOPPED, pool.snapshot().getState(), "pool should be STOPPED");

        pool.start();
        eventually(
                "pool restarts and rewarm idle sandboxes",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () ->
                        pool.snapshot().getState() == PoolState.HEALTHY
                                && pool.snapshot().getIdleCount() >= 1);

        Sandbox fromIdle = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
        borrowed.add(fromIdle);
        assertTrue(fromIdle.isHealthy(), "acquire after restart should return healthy sandbox");
    }

    @Test
    @Order(8)
    @DisplayName("concurrent shutdown and acquire does not deadlock (POOL_NOT_RUNNING is allowed)")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testConcurrentShutdownAndAcquireDoesNotDeadlock() throws Exception {
        eventually(
                "pool has warm idle before concurrency scenario",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getIdleCount() >= 1);

        int acquireWorkers = 4;
        CountDownLatch startLatch = new CountDownLatch(1);
        ExecutorService executor = Executors.newFixedThreadPool(acquireWorkers + 1);
        List<CompletableFuture<Void>> futures = new ArrayList<>();
        List<Throwable> errors = new CopyOnWriteArrayList<>();
        try {
            for (int i = 0; i < acquireWorkers; i++) {
                futures.add(
                        CompletableFuture.runAsync(
                                () -> {
                                    try {
                                        startLatch.await();
                                        Sandbox sandbox =
                                                pool.acquire(
                                                        Duration.ofMinutes(5),
                                                        AcquirePolicy.DIRECT_CREATE);
                                        borrowed.add(sandbox);
                                        Execution result =
                                                sandbox.commands()
                                                        .run(
                                                                RunCommandRequest.builder()
                                                                        .command(
                                                                                "echo acquire-concurrent-ok")
                                                                        .build());
                                        assertNotNull(result);
                                    } catch (Throwable t) {
                                        if (isPoolNotRunningError(t)) {
                                            return;
                                        }
                                        errors.add(t);
                                        throw new CompletionException(t);
                                    }
                                },
                                executor));
            }

            futures.add(
                    CompletableFuture.runAsync(
                            () -> {
                                try {
                                    startLatch.await();
                                    pool.shutdown(true);
                                } catch (Throwable t) {
                                    errors.add(t);
                                    throw new CompletionException(t);
                                }
                            },
                            executor));

            startLatch.countDown();
            for (CompletableFuture<Void> future : futures) {
                future.get(2, TimeUnit.MINUTES);
            }
        } finally {
            executor.shutdownNow();
        }

        assertTrue(
                errors.isEmpty(), "concurrent shutdown/acquire should not raise errors: " + errors);
    }

    @Test
    @Order(9)
    @DisplayName("concurrent start/shutdown stress remains stable")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testConcurrentStartShutdownStressSingleNode() throws Exception {
        pool.resize(0);
        pool.releaseAllIdle();
        pool.shutdown(false);

        int workers = 4;
        int loopsPerWorker = 8;
        ExecutorService executor = Executors.newFixedThreadPool(workers);
        List<CompletableFuture<Void>> futures = new ArrayList<>();
        List<Throwable> errors = new CopyOnWriteArrayList<>();
        try {
            for (int i = 0; i < workers; i++) {
                futures.add(
                        CompletableFuture.runAsync(
                                () -> {
                                    for (int j = 0; j < loopsPerWorker; j++) {
                                        try {
                                            pool.start();
                                            Thread.sleep(20);
                                            pool.shutdown(false);
                                        } catch (InterruptedException e) {
                                            Thread.currentThread().interrupt();
                                            errors.add(e);
                                            throw new CompletionException(e);
                                        } catch (Throwable t) {
                                            errors.add(t);
                                            throw new CompletionException(t);
                                        }
                                    }
                                },
                                executor));
            }

            for (CompletableFuture<Void> future : futures) {
                future.get(2, TimeUnit.MINUTES);
            }
        } finally {
            executor.shutdownNow();
        }
        assertTrue(errors.isEmpty(), "concurrent start/shutdown stress should not fail: " + errors);

        pool.resize(2);
        pool.start();
        eventually(
                "pool recovers to healthy after stress",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getState() == PoolState.HEALTHY);
    }

    @Test
    @Order(10)
    @DisplayName("resize from zero to positive rewarm idle target")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testResizeFromZeroToPositiveRewarmsIdleTarget() throws InterruptedException {
        eventually(
                "pool has warmed idle before resize test",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getIdleCount() >= 1);

        pool.resize(0);
        pool.releaseAllIdle();
        eventually(
                "idle drained after resize to zero",
                Duration.ofSeconds(30),
                Duration.ofSeconds(1),
                () -> pool.snapshot().getIdleCount() == 0);

        pool.resize(2);
        eventually(
                "idle rewarmed to resized target",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () ->
                        pool.snapshot().getState() == PoolState.HEALTHY
                                && pool.snapshot().getIdleCount() >= 2);
    }

    @Test
    @Order(11)
    @DisplayName("two pools stay isolated in serial low-footprint mode")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testTwoPoolsIsolationSerialLowFootprint() throws Exception {
        String tagA = "e2e-pool-a-" + UUID.randomUUID().toString().substring(0, 8);
        String tagB = "e2e-pool-b-" + UUID.randomUUID().toString().substring(0, 8);
        String poolNameA = "pool-a-" + tagA;
        String poolNameB = "pool-b-" + tagB;

        SandboxPool poolA = null;
        SandboxPool poolB = null;
        try {
            poolA =
                    SandboxPool.builder()
                            .poolName(poolNameA)
                            .ownerId("owner-a-" + tagA)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(sharedConnectionConfig)
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            tagA,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .reconcileInterval(Duration.ofSeconds(2))
                            .drainTimeout(Duration.ofMillis(200))
                            .build();

            poolA.start();
            SandboxPool finalPoolA = poolA;
            eventually(
                    "poolA warmed to idle target",
                    Duration.ofMinutes(2),
                    Duration.ofSeconds(2),
                    () ->
                            finalPoolA.snapshot().getState() == PoolState.HEALTHY
                                    && finalPoolA.snapshot().getIdleCount() >= 1);

            Sandbox a = poolA.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
            borrowed.add(a);
            assertTrue(a.isHealthy(), "poolA acquire should return healthy sandbox");
            releaseAndRemoveBorrowed(a);

            poolB =
                    SandboxPool.builder()
                            .poolName(poolNameB)
                            .ownerId("owner-b-" + tagB)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(sharedConnectionConfig)
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            tagB,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .reconcileInterval(Duration.ofSeconds(2))
                            .drainTimeout(Duration.ofMillis(200))
                            .build();

            poolB.start();
            SandboxPool finalPoolB = poolB;
            eventually(
                    "poolB warmed to idle target",
                    Duration.ofMinutes(2),
                    Duration.ofSeconds(2),
                    () ->
                            finalPoolB.snapshot().getState() == PoolState.HEALTHY
                                    && finalPoolB.snapshot().getIdleCount() >= 1);

            int taggedA = countTaggedSandboxes(tagA);
            int taggedB = countTaggedSandboxes(tagB);
            assertTrue(taggedA >= 1, "poolA should have at least one tagged sandbox");
            assertTrue(taggedB >= 1, "poolB should have at least one tagged sandbox");

            poolA.resize(0);
            poolA.releaseAllIdle();
            eventually(
                    "poolA idle drains to zero",
                    Duration.ofSeconds(30),
                    Duration.ofSeconds(1),
                    () -> finalPoolA.snapshot().getIdleCount() == 0);

            assertTrue(
                    poolB.snapshot().getIdleCount() >= 1,
                    "poolB idle should remain unaffected by poolA operations");
            assertTrue(
                    countTaggedSandboxes(tagB) >= 1,
                    "poolB tagged sandboxes should remain after poolA drain");
        } finally {
            if (poolA != null) {
                try {
                    poolA.resize(0);
                    poolA.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    poolA.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            if (poolB != null) {
                try {
                    poolB.resize(0);
                    poolB.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    poolB.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(tagA);
            cleanupTaggedSandboxes(tagB);
        }
    }

    @Test
    @Order(12)
    @DisplayName("releaseAllIdle drains store and remote tagged sandboxes in serial mode")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testReleaseAllIdleReducesRemoteTaggedSandboxesSerial() throws InterruptedException {
        eventually(
                "pool has warmed idle before releaseAllIdle remote validation",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () -> pool.snapshot().getIdleCount() >= 1);

        int before = countTaggedSandboxes();
        assertTrue(before >= 1, "expected at least one tagged sandbox before releaseAllIdle");

        pool.resize(0);
        int released = pool.releaseAllIdle();
        assertTrue(released >= 0, "releaseAllIdle should return non-negative count");

        eventually(
                "idle count reaches zero after releaseAllIdle",
                Duration.ofSeconds(30),
                Duration.ofSeconds(1),
                () -> pool.snapshot().getIdleCount() == 0);

        eventually(
                "remote tagged sandbox count decreases after releaseAllIdle",
                Duration.ofSeconds(60),
                Duration.ofSeconds(2),
                () -> countTaggedSandboxes() == 0);
    }

    @Test
    @Order(13)
    @DisplayName("broken connection triggers DEGRADED state with low resource footprint")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testBrokenConnectionTriggersDegradedState() throws InterruptedException {
        String badTag = "e2e-pool-bad-" + UUID.randomUUID().toString().substring(0, 8);
        SandboxPool badPool = null;
        try {
            badPool =
                    SandboxPool.builder()
                            .poolName("pool-bad-" + badTag)
                            .ownerId("owner-bad-" + badTag)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(buildBrokenConnectionConfig())
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            badTag,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .degradedThreshold(1)
                            .reconcileInterval(Duration.ofSeconds(1))
                            .drainTimeout(Duration.ofMillis(100))
                            .build();
            badPool.start();

            SandboxPool finalBadPool = badPool;
            eventually(
                    "bad pool enters DEGRADED state",
                    Duration.ofSeconds(90),
                    Duration.ofSeconds(2),
                    () -> finalBadPool.snapshot().getState() == PoolState.DEGRADED);

            assertNotNull(
                    finalBadPool.snapshot().getLastError(),
                    "degraded snapshot should contain lastError");
            assertEquals(
                    0,
                    finalBadPool.snapshot().getIdleCount(),
                    "broken pool should not create idle sandboxes");
        } finally {
            if (badPool != null) {
                try {
                    badPool.resize(0);
                    badPool.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    badPool.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(badTag);
        }
    }

    @Test
    @Order(14)
    @DisplayName("broken pool enforces FAIL_FAST empty and direct-create failure semantics")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testBrokenPoolAcquireSemantics() throws InterruptedException {
        String badTag = "e2e-pool-bad-acquire-" + UUID.randomUUID().toString().substring(0, 8);
        SandboxPool badPool = null;
        try {
            badPool =
                    SandboxPool.builder()
                            .poolName("pool-bad-acquire-" + badTag)
                            .ownerId("owner-bad-acquire-" + badTag)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(buildBrokenConnectionConfig())
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            badTag,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .degradedThreshold(1)
                            .reconcileInterval(Duration.ofSeconds(1))
                            .drainTimeout(Duration.ofMillis(100))
                            .build();
            badPool.start();
            SandboxPool finalBadPool = badPool;
            eventually(
                    "broken pool enters DEGRADED before acquire semantic checks",
                    Duration.ofSeconds(90),
                    Duration.ofSeconds(2),
                    () -> finalBadPool.snapshot().getState() == PoolState.DEGRADED);

            assertThrows(
                    PoolEmptyException.class,
                    () -> finalBadPool.acquire(Duration.ofMinutes(1), AcquirePolicy.FAIL_FAST));
            assertThrows(
                    Exception.class,
                    () -> finalBadPool.acquire(Duration.ofMinutes(1), AcquirePolicy.DIRECT_CREATE));
        } finally {
            if (badPool != null) {
                try {
                    badPool.resize(0);
                    badPool.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    badPool.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(badTag);
        }
    }

    @Test
    @Order(15)
    @DisplayName("healthy pool still works after broken pool failure path")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testHealthyPoolWorksAfterBrokenPoolPath() throws InterruptedException {
        String goodTag = "e2e-pool-good-after-bad-" + UUID.randomUUID().toString().substring(0, 8);
        SandboxPool goodPool = null;
        try {
            goodPool =
                    SandboxPool.builder()
                            .poolName("pool-good-after-bad-" + goodTag)
                            .ownerId("owner-good-after-bad-" + goodTag)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(sharedConnectionConfig)
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            goodTag,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .reconcileInterval(Duration.ofSeconds(2))
                            .drainTimeout(Duration.ofMillis(100))
                            .build();
            goodPool.start();

            SandboxPool finalGoodPool = goodPool;
            eventually(
                    "healthy pool reaches warm idle",
                    Duration.ofMinutes(2),
                    Duration.ofSeconds(2),
                    () ->
                            finalGoodPool.snapshot().getState() == PoolState.HEALTHY
                                    && finalGoodPool.snapshot().getIdleCount() >= 1);

            Sandbox sandbox = finalGoodPool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
            borrowed.add(sandbox);
            Execution execution =
                    sandbox.commands()
                            .run(RunCommandRequest.builder().command("echo recovery-ok").build());
            assertNotNull(execution);
            assertNull(execution.getError());
            releaseAndRemoveBorrowed(sandbox);
        } finally {
            if (goodPool != null) {
                try {
                    goodPool.resize(0);
                    goodPool.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    goodPool.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(goodTag);
        }
    }

    @Test
    @Order(16)
    @DisplayName("warmupSandboxPreparer prepares idle sandboxes before acquire")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testWarmupSandboxPreparerAppliesBeforeAcquire() throws InterruptedException {
        pool.resize(0);
        pool.releaseAllIdle();
        pool.shutdown(false);

        String preparedTag = "e2e-pool-prepared-" + UUID.randomUUID().toString().substring(0, 8);
        String preparedPoolName = "pool-prepared-" + preparedTag;
        String markerPath = "/tmp/pool-prepared-marker.txt";
        SandboxPool preparedPool = null;
        try {
            preparedPool =
                    SandboxPool.builder()
                            .poolName(preparedPoolName)
                            .ownerId("owner-prepared-" + preparedTag)
                            .maxIdle(1)
                            .warmupConcurrency(1)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(sharedConnectionConfig)
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            preparedTag,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .warmupSandboxPreparer(
                                    sandbox ->
                                            sandbox.files()
                                                    .writeFile(
                                                            markerPath,
                                                            "prepared-by-warmup-" + preparedTag))
                            .reconcileInterval(Duration.ofSeconds(2))
                            .drainTimeout(Duration.ofMillis(200))
                            .build();
            preparedPool.start();

            SandboxPool finalPreparedPool = preparedPool;
            eventually(
                    "prepared pool reaches healthy idle state",
                    Duration.ofMinutes(2),
                    Duration.ofSeconds(2),
                    () ->
                            finalPreparedPool.snapshot().getState() == PoolState.HEALTHY
                                    && finalPreparedPool.snapshot().getIdleCount() >= 1);

            Sandbox sandbox = preparedPool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
            borrowed.add(sandbox);
            assertTrue(sandbox.isHealthy(), "prepared idle sandbox should be healthy");
            assertEquals(
                    "prepared-by-warmup-" + preparedTag,
                    sandbox.files().readFile(markerPath).trim(),
                    "warmup preparer should materialize marker before acquire");
        } finally {
            if (preparedPool != null) {
                try {
                    preparedPool.resize(0);
                    preparedPool.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    preparedPool.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(preparedTag);
        }
    }

    @Test
    @Order(17)
    @DisplayName("warmupConcurrency above one fills target and stays bounded")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testWarmupConcurrencyAboveOneReachesTargetAndStaysBounded() throws InterruptedException {
        pool.resize(0);
        pool.releaseAllIdle();
        pool.shutdown(false);

        String concurrentTag =
                "e2e-pool-warmup-concurrency-" + UUID.randomUUID().toString().substring(0, 8);
        SandboxPool concurrentPool = null;
        try {
            concurrentPool =
                    SandboxPool.builder()
                            .poolName("pool-warmup-concurrency-" + concurrentTag)
                            .ownerId("owner-warmup-concurrency-" + concurrentTag)
                            .maxIdle(3)
                            .warmupConcurrency(2)
                            .stateStore(new InMemoryPoolStateStore())
                            .connectionConfig(sharedConnectionConfig)
                            .creationSpec(
                                    PoolCreationSpec.builder()
                                            .image(getSandboxImage())
                                            .entrypoint(List.of("tail -f /dev/null"))
                                            .metadata(
                                                    Map.of(
                                                            "tag",
                                                            concurrentTag,
                                                            "suite",
                                                            "sandbox-pool-e2e"))
                                            .env(
                                                    Map.of(
                                                            "E2E_TEST",
                                                            "true",
                                                            "EXECD_API_GRACE_SHUTDOWN",
                                                            "3s",
                                                            "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                                            "1s"))
                                            .build())
                            .reconcileInterval(Duration.ofSeconds(2))
                            .drainTimeout(Duration.ofMillis(200))
                            .build();
            concurrentPool.start();

            SandboxPool finalConcurrentPool = concurrentPool;
            eventually(
                    "concurrent warmup reaches configured idle target without overshoot",
                    Duration.ofMinutes(2),
                    Duration.ofSeconds(1),
                    () ->
                            finalConcurrentPool.snapshot().getIdleCount() >= 3
                                    && countTaggedSandboxes(concurrentTag) <= 3);
        } finally {
            if (concurrentPool != null) {
                try {
                    concurrentPool.resize(0);
                    concurrentPool.releaseAllIdle();
                } catch (Exception ignored) {
                }
                try {
                    concurrentPool.shutdown(false);
                } catch (Exception ignored) {
                }
            }
            cleanupTaggedSandboxes(concurrentTag);
        }
    }

    @Test
    @Order(18)
    @DisplayName("snapshot exposes lifecycle maxIdle and idle entry details")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testSnapshotExposesLifecycleAndIdleEntries() throws InterruptedException {
        eventually(
                "pool reaches healthy state for snapshot validation",
                Duration.ofMinutes(2),
                Duration.ofSeconds(2),
                () ->
                        pool.snapshot().getState() == PoolState.HEALTHY
                                && pool.snapshot().getIdleCount() >= 1);

        assertEquals(PoolLifecycleState.RUNNING, pool.snapshot().getLifecycleState());
        assertEquals(MAX_IDLE, pool.snapshot().getMaxIdle());
        assertTrue(pool.snapshot().getInFlightOperations() >= 0);
        assertTrue(pool.snapshot().getFailureCount() >= 0);

        List<IdleEntry> idleEntries = pool.snapshotIdleEntries();
        assertFalse(
                idleEntries.isEmpty(), "snapshotIdleEntries should expose warmed idle sandboxes");
        assertTrue(
                idleEntries.stream().allMatch(entry -> entry.getExpiresAt() != null),
                "idle entry expiration should be populated");

        Set<String> idleIds =
                idleEntries.stream().map(IdleEntry::getSandboxId).collect(Collectors.toSet());

        Sandbox sandbox = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
        borrowed.add(sandbox);
        assertTrue(
                idleIds.contains(sandbox.getId()),
                "acquired sandbox should come from idle snapshot");

        pool.resize(1);
        eventually(
                "snapshot maxIdle reflects resize",
                Duration.ofSeconds(30),
                Duration.ofSeconds(1),
                () -> pool.snapshot().getMaxIdle() == 1);

        pool.shutdown(false);
        assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
        assertEquals(PoolState.STOPPED, pool.snapshot().getState());
    }

    private void cleanupTaggedSandboxes() {
        cleanupTaggedSandboxes(tag);
    }

    private void cleanupTaggedSandboxes(String cleanupTag) {
        if (cleanupTag == null || cleanupTag.isBlank()) {
            return;
        }
        if (sandboxManager == null) {
            return;
        }
        try {
            PagedSandboxInfos infos =
                    sandboxManager.listSandboxInfos(
                            SandboxFilter.builder()
                                    .metadata(Map.of("tag", cleanupTag))
                                    .pageSize(20)
                                    .build());
            infos.getSandboxInfos()
                    .forEach(
                            info -> {
                                try {
                                    sandboxManager.killSandbox(info.getId());
                                } catch (Exception ignored) {
                                }
                            });
        } catch (Exception ignored) {
        }
    }

    private int countTaggedSandboxes() {
        return countTaggedSandboxes(tag);
    }

    private int countTaggedSandboxes(String queryTag) {
        if (queryTag == null || queryTag.isBlank()) {
            return 0;
        }
        PagedSandboxInfos infos =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", queryTag))
                                .pageSize(20)
                                .build());
        return infos.getSandboxInfos().size();
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

    private void releaseAndRemoveBorrowed(Sandbox sandbox) {
        if (sandbox == null) {
            return;
        }
        killAndCloseQuietly(sandbox);
        borrowed.remove(sandbox);
    }

    private ConnectionConfig buildBrokenConnectionConfig() {
        return ConnectionConfig.builder()
                .apiKey(sharedConnectionConfig.getApiKey())
                .domain("127.0.0.1:1")
                .protocol("http")
                .requestTimeout(Duration.ofSeconds(1))
                .build();
    }

    private static boolean isPoolNotRunningError(Throwable throwable) {
        Throwable current = throwable;
        while (current != null) {
            if (current instanceof SandboxException) {
                SandboxException se = (SandboxException) current;
                if (se.getError() != null && "POOL_NOT_RUNNING".equals(se.getError().getCode())) {
                    return true;
                }
            }
            String message = current.getMessage();
            if (message != null && message.contains("POOL_NOT_RUNNING")) {
                return true;
            }
            current = current.getCause();
        }
        return false;
    }
}
