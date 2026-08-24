/*
 * Copyright 2026 Alibaba Group Holding Ltd.
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

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.SandboxManager;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState;
import com.alibaba.opensandbox.sandbox.domain.pool.PooledSandboxCreateContext;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import java.time.Duration;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.BooleanSupplier;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("SandboxPool staged warmup E2E Tests")
public class SandboxPoolStagedWarmupE2ETest extends BaseE2ETest {
    private SandboxManager sandboxManager;
    private String tag;

    @BeforeEach
    void setup() {
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();
    }

    @AfterEach
    void teardown() {
        cleanupTaggedSandboxes();
        if (sandboxManager != null) {
            sandboxManager.close();
        }
    }

    @Test
    @DisplayName("readiness final attempt, prepare, and post-prepare retry run in order")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testStagedWarmupRunsInOrderAndPreparesExactlyOnce() throws Exception {
        tag = uniqueTag("staged-order");
        String markerPath = "/tmp/staged-warmup-marker.txt";
        String markerContent = "prepared-" + tag;
        List<String> events = Collections.synchronizedList(new java.util.ArrayList<>());
        AtomicInteger readinessCalls = new AtomicInteger();
        AtomicInteger preparerCalls = new AtomicInteger();
        AtomicInteger postPrepareCalls = new AtomicInteger();
        AtomicInteger readinessCallsAtPrepare = new AtomicInteger();
        SandboxPool pool =
                basePoolBuilder(1)
                        // The first requested check is beyond the soft deadline, so the task must
                        // still receive its one final readiness attempt at the deadline.
                        .warmupReadyTimeout(Duration.ofSeconds(5))
                        .warmupHealthCheckInitialDelay(Duration.ofSeconds(30))
                        .warmupHealthCheckPollingInterval(Duration.ofMillis(250))
                        .warmupHealthCheck(
                                sandbox -> {
                                    events.add("readiness");
                                    readinessCalls.incrementAndGet();
                                    return true;
                                })
                        .warmupSandboxPreparer(
                                sandbox -> {
                                    events.add("prepare");
                                    readinessCallsAtPrepare.set(readinessCalls.get());
                                    preparerCalls.incrementAndGet();
                                    writeMarkerWhenReady(
                                            sandbox,
                                            markerPath,
                                            markerContent,
                                            Duration.ofSeconds(30));
                                })
                        .warmupPostPrepareHealthCheck(
                                sandbox -> {
                                    events.add("post-prepare");
                                    int attempt = postPrepareCalls.incrementAndGet();
                                    if (attempt == 1) {
                                        return false;
                                    }
                                    return markerContent.equals(
                                                    sandbox.files().readFile(markerPath).trim())
                                            && sandbox.ping();
                                })
                        .warmupPostPrepareHealthCheckTimeout(Duration.ofSeconds(10))
                        .build();

        try {
            pool.start();
            eventually(
                    "staged warmup becomes idle",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(250),
                    () ->
                            pool.snapshot().getIdleCount() == 1
                                    && pool.snapshot().getInFlightOperations() == 0);

            assertEquals(1, readinessCalls.get(), "soft deadline should allow one final check");
            assertEquals(1, readinessCallsAtPrepare.get());
            assertEquals(1, preparerCalls.get(), "preparer must execute exactly once");
            assertEquals(2, postPrepareCalls.get(), "post-prepare check should retry once");
            assertEquals(List.of("readiness", "prepare", "post-prepare", "post-prepare"), events);
            assertEquals(1, countTaggedSandboxes());

            Sandbox sandbox = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
            try {
                assertTrue(sandbox.isHealthy());
                assertEquals(markerContent, sandbox.files().readFile(markerPath).trim());
            } finally {
                try {
                    sandbox.kill();
                } finally {
                    sandbox.close();
                }
            }
            eventually(
                    "acquired staged sandbox is deleted",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("readiness timeout deletes the failed sandbox and replenishes")
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void testReadinessTimeoutDeletesAndReplenishes() throws Exception {
        tag = uniqueTag("readiness-timeout");
        AtomicReference<String> failedSandboxId = new AtomicReference<>();
        AtomicReference<String> preparedSandboxId = new AtomicReference<>();
        AtomicInteger preparerCalls = new AtomicInteger();
        SandboxPool pool =
                basePoolBuilder(1)
                        .warmupReadyTimeout(Duration.ofSeconds(2))
                        .warmupHealthCheckPollingInterval(Duration.ofMillis(250))
                        .warmupHealthCheck(
                                sandbox -> {
                                    failedSandboxId.compareAndSet(null, sandbox.getId());
                                    if (sandbox.getId().equals(failedSandboxId.get())) {
                                        return false;
                                    }
                                    return sandbox.ping();
                                })
                        .warmupSandboxPreparer(
                                sandbox -> {
                                    preparerCalls.incrementAndGet();
                                    preparedSandboxId.set(sandbox.getId());
                                })
                        .build();

        try {
            pool.start();
            eventually(
                    "replacement succeeds after readiness timeout",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(250),
                    () ->
                            failedSandboxId.get() != null
                                    && preparedSandboxId.get() != null
                                    && !failedSandboxId.get().equals(preparedSandboxId.get())
                                    && pool.snapshot().getIdleCount() == 1
                                    && pool.snapshot().getInFlightOperations() == 0
                                    && !taggedSandboxExists(failedSandboxId.get())
                                    && countTaggedSandboxes() == 1);

            assertEquals(1, preparerCalls.get());
            assertTrue(
                    pool.snapshotIdleEntries().stream()
                            .allMatch(
                                    entry -> preparedSandboxId.get().equals(entry.getSandboxId())));
        } finally {
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("post-prepare timeout deletes the failed sandbox and replenishes")
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void testPostPrepareTimeoutDeletesAndReplenishes() throws Exception {
        tag = uniqueTag("post-prepare-timeout");
        AtomicReference<String> failedSandboxId = new AtomicReference<>();
        Map<String, AtomicInteger> preparerCallsBySandbox = new ConcurrentHashMap<>();
        AtomicInteger totalPreparerCalls = new AtomicInteger();
        SandboxPool pool =
                basePoolBuilder(1)
                        .warmupReadyTimeout(Duration.ofSeconds(30))
                        .warmupHealthCheckPollingInterval(Duration.ofMillis(250))
                        .warmupHealthCheck(Sandbox::ping)
                        .warmupSandboxPreparer(
                                sandbox -> {
                                    totalPreparerCalls.incrementAndGet();
                                    preparerCallsBySandbox
                                            .computeIfAbsent(
                                                    sandbox.getId(), ignored -> new AtomicInteger())
                                            .incrementAndGet();
                                })
                        .warmupPostPrepareHealthCheck(
                                sandbox -> {
                                    failedSandboxId.compareAndSet(null, sandbox.getId());
                                    if (sandbox.getId().equals(failedSandboxId.get())) {
                                        return false;
                                    }
                                    return sandbox.ping();
                                })
                        .warmupPostPrepareHealthCheckTimeout(Duration.ofSeconds(2))
                        .build();

        try {
            pool.start();
            eventually(
                    "replacement succeeds after post-prepare timeout",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(250),
                    () ->
                            failedSandboxId.get() != null
                                    && pool.snapshot().getIdleCount() == 1
                                    && pool.snapshot().getInFlightOperations() == 0
                                    && !taggedSandboxExists(failedSandboxId.get())
                                    && countTaggedSandboxes() == 1);

            assertEquals(2, totalPreparerCalls.get());
            assertEquals(1, preparerCallsBySandbox.get(failedSandboxId.get()).get());
            assertTrue(
                    preparerCallsBySandbox.values().stream().allMatch(calls -> calls.get() == 1),
                    "no sandbox may run the preparer more than once");
        } finally {
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("forced shutdown deletes a sandbox waiting in the delay queue")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testForcedShutdownDeletesDelayedWarmup() throws Exception {
        tag = uniqueTag("delayed-shutdown");
        AtomicInteger healthCheckCalls = new AtomicInteger();
        AtomicInteger preparerCalls = new AtomicInteger();
        SandboxPool pool =
                basePoolBuilder(1)
                        .warmupReadyTimeout(Duration.ofMinutes(1))
                        .warmupHealthCheckInitialDelay(Duration.ofSeconds(30))
                        .warmupHealthCheck(
                                sandbox -> {
                                    healthCheckCalls.incrementAndGet();
                                    return true;
                                })
                        .warmupSandboxPreparer(sandbox -> preparerCalls.incrementAndGet())
                        .drainTimeout(Duration.ofMillis(200))
                        .build();

        try {
            pool.start();
            eventually(
                    "created sandbox waits in the delay queue",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(200),
                    () ->
                            countTaggedSandboxes() == 1
                                    && pool.snapshot().getInFlightOperations() == 1
                                    && healthCheckCalls.get() == 0);

            long startedAt = System.nanoTime();
            pool.shutdown(false);
            Duration shutdownElapsed = Duration.ofNanos(System.nanoTime() - startedAt);

            assertTrue(
                    shutdownElapsed.compareTo(Duration.ofSeconds(10)) < 0,
                    "forced shutdown should not wait for the delay: " + shutdownElapsed);
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
            assertEquals(0, pool.snapshot().getIdleCount());
            assertEquals(0, pool.snapshot().getInFlightOperations());
            assertEquals(0, healthCheckCalls.get());
            assertEquals(0, preparerCalls.get());
            eventually(
                    "delayed warmup sandbox is deleted",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("warmupCreateQps admits real sandbox creation across fixed reconcile batches")
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void testWarmupCreateQpsAdmitsAcrossFixedReconcileBatches() throws Exception {
        tag = uniqueTag("create-qps");
        CountDownLatch firstAdmission = new CountDownLatch(1);
        CountDownLatch secondAdmission = new CountDownLatch(1);
        AtomicInteger admissions = new AtomicInteger();
        AtomicLong firstAdmissionAt = new AtomicLong();
        AtomicLong secondAdmissionAt = new AtomicLong();
        SandboxPool pool =
                basePoolBuilder(3)
                        .warmupCreateQps(1)
                        .warmupConcurrency(3)
                        .warmupSkipHealthCheck(true)
                        .sandboxCreator(
                                context -> {
                                    int admission = admissions.incrementAndGet();
                                    if (admission == 1) {
                                        firstAdmissionAt.set(System.nanoTime());
                                        firstAdmission.countDown();
                                    } else if (admission == 2) {
                                        secondAdmissionAt.set(System.nanoTime());
                                        secondAdmission.countDown();
                                    }
                                    return createRealSandbox(context);
                                })
                        .build();

        try {
            pool.start();
            assertTrue(
                    firstAdmission.await(2, TimeUnit.MINUTES),
                    "the first fixed reconcile should admit one create");
            assertFalse(
                    secondAdmission.await(400, TimeUnit.MILLISECONDS),
                    "create completion must not trigger immediate admission");

            eventually(
                    "QPS-limited warmup converges across reconcile batches",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(250),
                    () ->
                            admissions.get() == 3
                                    && pool.snapshot().getIdleCount() == 3
                                    && pool.snapshot().getInFlightOperations() == 0
                                    && countTaggedSandboxes() == 3);

            assertTrue(secondAdmissionAt.get() > firstAdmissionAt.get());
            assertTrue(
                    Duration.ofNanos(secondAdmissionAt.get() - firstAdmissionAt.get())
                                    .compareTo(Duration.ofMillis(500))
                            >= 0,
                    "the second admission should wait for a later fixed reconcile");
            assertEquals(3, pool.snapshotIdleEntries().size());
        } finally {
            shutdownAndRelease(pool);
        }
    }

    private SandboxPool.Builder basePoolBuilder(int maxIdle) {
        PoolCreationSpec creationSpec =
                PoolCreationSpec.builder()
                        .image(getSandboxImage())
                        .entrypoint(List.of("tail -f /dev/null"))
                        .metadata(Map.of("tag", tag, "suite", "sandbox-pool-staged-warmup-e2e"))
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
                .poolName("pool-" + tag)
                .ownerId("owner-" + tag)
                .maxIdle(maxIdle)
                .warmupCreateQps(1)
                .warmupConcurrency(1)
                .stateStore(new InMemoryPoolStateStore())
                .connectionConfig(sharedConnectionConfig)
                .creationSpec(creationSpec)
                .drainTimeout(Duration.ofMillis(500));
    }

    private Sandbox createRealSandbox(PooledSandboxCreateContext context) {
        return Sandbox.builder()
                .connectionConfig(context.getConnectionConfig())
                .image(getSandboxImage())
                .entrypoint(List.of("tail -f /dev/null"))
                .metadata(Map.of("tag", tag, "suite", "sandbox-pool-staged-warmup-e2e"))
                .env(
                        Map.of(
                                "E2E_TEST",
                                "true",
                                "EXECD_API_GRACE_SHUTDOWN",
                                "3s",
                                "EXECD_JUPYTER_IDLE_POLL_INTERVAL",
                                "1s"))
                .timeout(context.getIdleTimeout())
                .readyTimeout(context.getReadyTimeout())
                .healthCheckPollingInterval(context.getHealthCheckPollingInterval())
                .skipHealthCheck(context.getSkipHealthCheck())
                .initializationConnectionConfig(context.getCreateConnectionConfig())
                .build();
    }

    private static void writeMarkerWhenReady(
            Sandbox sandbox, String path, String content, Duration timeout) {
        long deadline = System.nanoTime() + timeout.toNanos();
        Throwable lastError = null;
        while (System.nanoTime() < deadline) {
            try {
                sandbox.files().writeFile(path, content);
                return;
            } catch (Throwable throwable) {
                lastError = throwable;
            }
            try {
                Thread.sleep(200);
            } catch (InterruptedException exception) {
                Thread.currentThread().interrupt();
                throw new RuntimeException("marker preparation interrupted", exception);
            }
        }
        throw new AssertionError("sandbox did not become ready for marker preparation", lastError);
    }

    private void eventually(
            String description, Duration timeout, Duration interval, BooleanSupplier condition)
            throws InterruptedException {
        long deadline = System.nanoTime() + timeout.toNanos();
        Throwable lastError = null;
        while (System.nanoTime() < deadline) {
            try {
                if (condition.getAsBoolean()) {
                    return;
                }
            } catch (Throwable throwable) {
                lastError = throwable;
            }
            Thread.sleep(interval.toMillis());
        }
        if (lastError != null) {
            throw new AssertionError(
                    "Timed out waiting for "
                            + description
                            + ", last error: "
                            + lastError.getMessage(),
                    lastError);
        }
        throw new AssertionError("Timed out waiting for " + description);
    }

    private int countTaggedSandboxes() {
        PagedSandboxInfos infos =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder().metadata(Map.of("tag", tag)).pageSize(50).build());
        return infos.getSandboxInfos().size();
    }

    private boolean taggedSandboxExists(String sandboxId) {
        if (sandboxId == null) {
            return false;
        }
        PagedSandboxInfos infos =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder().metadata(Map.of("tag", tag)).pageSize(50).build());
        return infos.getSandboxInfos().stream().anyMatch(info -> sandboxId.equals(info.getId()));
    }

    private void cleanupTaggedSandboxes() {
        if (sandboxManager == null || tag == null || tag.isBlank()) {
            return;
        }
        try {
            PagedSandboxInfos infos =
                    sandboxManager.listSandboxInfos(
                            SandboxFilter.builder()
                                    .metadata(Map.of("tag", tag))
                                    .pageSize(50)
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

    private static void shutdownAndRelease(SandboxPool pool) {
        if (pool == null) {
            return;
        }
        try {
            pool.shutdown(false);
        } catch (Exception ignored) {
        }
        try {
            pool.releaseAllIdle();
        } catch (Exception ignored) {
        }
    }

    private static String uniqueTag(String scenario) {
        return "e2e-pool-" + scenario + "-" + UUID.randomUUID().toString().substring(0, 8);
    }
}
