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
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.alibaba.opensandbox.sandbox.Sandbox;
import com.alibaba.opensandbox.sandbox.SandboxManager;
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolNotRunningException;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter;
import com.alibaba.opensandbox.sandbox.domain.pool.AcquirePolicy;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.BooleanSupplier;
import java.util.function.Function;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("SandboxPool acquire lifecycle E2E Tests")
public class SandboxPoolAcquireLifecycleE2ETest extends BaseE2ETest {
    private SandboxManager sandboxManager;
    private SandboxPool pool;
    private String tag;

    @BeforeEach
    void setup() {
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();
    }

    @AfterEach
    void teardown() {
        shutdownAndRelease(pool);
        cleanupTaggedSandboxes();
        if (sandboxManager != null) {
            sandboxManager.close();
        }
    }

    @Test
    @DisplayName("cancelled acquire stops policy fallback and deletes the popped idle")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testCancelledAcquireStopsFallbackAndDeletesPoppedIdle() throws Exception {
        tag = uniqueTag("cancel");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch healthCheckEntered = new CountDownLatch(1);
        CountDownLatch releaseHealthCheck = new CountDownLatch(1);
        CountDownLatch acquireExited = new CountDownLatch(1);
        AtomicBoolean healthCheckInterrupted = new AtomicBoolean(false);
        AtomicBoolean interruptRestoredAtExit = new AtomicBoolean(false);
        ExecutorService executor = Executors.newSingleThreadExecutor();
        pool =
                createPool(
                        stateStore,
                        1,
                        sandbox -> {
                            healthCheckEntered.countDown();
                            try {
                                releaseHealthCheck.await();
                                return true;
                            } catch (InterruptedException exception) {
                                healthCheckInterrupted.set(true);
                                throw new RuntimeException(
                                        "acquire health check interrupted", exception);
                            }
                        },
                        null);

        try {
            pool.start();
            eventually(
                    "initial idle sandbox is warmed",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(500),
                    () -> pool.snapshot().getIdleCount() == 1);

            Future<Sandbox> acquire =
                    executor.submit(
                            () -> {
                                try {
                                    return pool.acquire(
                                            Duration.ofMinutes(5), AcquirePolicy.DIRECT_CREATE);
                                } finally {
                                    interruptRestoredAtExit.set(
                                            Thread.currentThread().isInterrupted());
                                    acquireExited.countDown();
                                }
                            });
            assertTrue(
                    healthCheckEntered.await(30, TimeUnit.SECONDS),
                    "acquire should enter the custom health check");
            pool.resize(0);

            assertTrue(acquire.cancel(true), "the in-flight acquire should be cancellable");
            assertTrue(acquireExited.await(30, TimeUnit.SECONDS), "cancelled acquire should exit");
            assertTrue(healthCheckInterrupted.get(), "health check should receive interruption");
            assertTrue(
                    interruptRestoredAtExit.get(),
                    "the acquire worker interrupt status should be restored before exit");
            eventually(
                    "cancelled candidate is deleted without direct-create fallback",
                    Duration.ofSeconds(60),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
            assertEquals(0, pool.snapshot().getInFlightOperations());
        } finally {
            releaseHealthCheck.countDown();
            executor.shutdownNow();
        }
    }

    @Test
    @DisplayName("retired acquire cannot consume idle warmed by a restarted run")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testRetiredAcquireCannotConsumeRestartedRunIdle() throws Exception {
        tag = uniqueTag("restart-acquire");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch oldHealthCheckEntered = new CountDownLatch(1);
        CountDownLatch releaseOldHealthCheck = new CountDownLatch(1);
        AtomicReference<String> oldSandboxId = new AtomicReference<>();
        AtomicReference<String> newSandboxId = new AtomicReference<>();
        ExecutorService executor = Executors.newSingleThreadExecutor();
        pool =
                createPool(
                        stateStore,
                        1,
                        sandbox -> {
                            if (sandbox.getId().equals(oldSandboxId.get())) {
                                oldHealthCheckEntered.countDown();
                                awaitLatch(releaseOldHealthCheck);
                                return false;
                            }
                            return true;
                        },
                        null);

        try {
            pool.start();
            eventually(
                    "old run warms one idle",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(500),
                    () -> pool.snapshot().getIdleCount() == 1);
            oldSandboxId.set(pool.snapshotIdleEntries().get(0).getSandboxId());

            Future<Sandbox> oldAcquire =
                    executor.submit(
                            () ->
                                    pool.acquire(
                                            Duration.ofMinutes(5), AcquirePolicy.RETRY_NEXT_IDLE));
            assertTrue(
                    oldHealthCheckEntered.await(30, TimeUnit.SECONDS),
                    "old acquire should block in readiness");

            pool.shutdown(false);
            pool.start();
            eventually(
                    "new run warms a replacement idle",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(500),
                    () -> pool.snapshot().getIdleCount() == 1);
            newSandboxId.set(pool.snapshotIdleEntries().get(0).getSandboxId());
            releaseOldHealthCheck.countDown();

            ExecutionException failure =
                    assertThrows(
                            ExecutionException.class, () -> oldAcquire.get(30, TimeUnit.SECONDS));
            assertTrue(
                    failure.getCause() instanceof PoolNotRunningException,
                    "the retired acquire should fail at the run-generation fence");
            assertEquals(1, pool.snapshot().getIdleCount());
            assertEquals(
                    newSandboxId.get(),
                    pool.snapshotIdleEntries().get(0).getSandboxId(),
                    "the restarted run idle must remain in the store");

            pool.resize(0);
            Sandbox acquired = pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST);
            assertEquals(newSandboxId.get(), acquired.getId());
            acquired.kill();
            acquired.close();
            eventually(
                    "old and new run sandboxes are deleted",
                    Duration.ofSeconds(60),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            releaseOldHealthCheck.countDown();
            executor.shutdownNow();
        }
    }

    @Test
    @DisplayName("acquire health-check Error cleans the popped idle before propagation")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testAcquireHealthCheckErrorCleansPoppedIdle() throws Exception {
        tag = uniqueTag("acquire-error");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        AssertionError expected = new AssertionError("user acquire health check failed");
        pool =
                createPool(
                        stateStore,
                        1,
                        sandbox -> {
                            throw expected;
                        },
                        null);

        pool.start();
        eventually(
                "idle is available before Error injection",
                Duration.ofMinutes(2),
                Duration.ofMillis(500),
                () -> pool.snapshot().getIdleCount() == 1);
        pool.resize(0);

        AssertionError actual =
                assertThrows(
                        AssertionError.class,
                        () -> pool.acquire(Duration.ofMinutes(5), AcquirePolicy.FAIL_FAST));
        assertSame(expected, actual);
        eventually(
                "popped idle is deleted after health-check Error",
                Duration.ofSeconds(60),
                Duration.ofMillis(500),
                () -> countTaggedSandboxes() == 0);
        assertEquals(0, pool.snapshot().getIdleCount());
        assertEquals(0, pool.snapshot().getInFlightOperations());
    }

    @Test
    @DisplayName("warmup health-check Error releases counters and allows replacement")
    @Timeout(value = 5, unit = TimeUnit.MINUTES)
    void testWarmupHealthCheckErrorReleasesCountersAndAllowsReplacement() throws Exception {
        tag = uniqueTag("warmup-error");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        AtomicInteger healthChecks = new AtomicInteger();
        pool =
                createPool(
                        stateStore,
                        1,
                        null,
                        sandbox -> {
                            if (healthChecks.incrementAndGet() == 1) {
                                throw new AssertionError("user warmup health check failed");
                            }
                            return true;
                        });

        pool.start();
        eventually(
                "replacement warmup succeeds after the first Error",
                Duration.ofMinutes(2),
                Duration.ofMillis(500),
                () ->
                        healthChecks.get() >= 2
                                && pool.snapshot().getIdleCount() == 1
                                && pool.snapshot().getInFlightOperations() == 0
                                && countTaggedSandboxes() == 1);
        assertEquals(1, pool.snapshotIdleEntries().size());
    }

    private SandboxPool createPool(
            InMemoryPoolStateStore stateStore,
            int maxIdle,
            Function<Sandbox, Boolean> acquireHealthCheck,
            Function<Sandbox, Boolean> warmupHealthCheck) {
        PoolCreationSpec creationSpec =
                PoolCreationSpec.builder()
                        .image(getSandboxImage())
                        .entrypoint(List.of("tail -f /dev/null"))
                        .metadata(Map.of("tag", tag, "suite", "sandbox-pool-acquire-lifecycle-e2e"))
                        .env(Map.of("E2E_TEST", "true"))
                        .build();
        SandboxPool.Builder builder =
                SandboxPool.builder()
                        .poolName("pool-" + tag)
                        .ownerId("owner-" + tag)
                        .maxIdle(maxIdle)
                        .warmupConcurrency(1)
                        .stateStore(stateStore)
                        .connectionConfig(sharedConnectionConfig)
                        .creationSpec(creationSpec)
                        .acquireReadyTimeout(Duration.ofSeconds(30))
                        .acquireHealthCheckPollingInterval(Duration.ofMillis(50))
                        .warmupReadyTimeout(Duration.ofMinutes(2))
                        .warmupHealthCheckPollingInterval(Duration.ofMillis(100))
                        .reconcileInterval(Duration.ofSeconds(1))
                        .drainTimeout(Duration.ofSeconds(2));
        if (acquireHealthCheck != null) {
            builder.acquireHealthCheck(acquireHealthCheck::apply);
        }
        if (warmupHealthCheck != null) {
            builder.warmupHealthCheck(warmupHealthCheck::apply);
        }
        return builder.build();
    }

    private static void awaitLatch(CountDownLatch latch) {
        try {
            latch.await();
        } catch (InterruptedException exception) {
            Thread.currentThread().interrupt();
            throw new RuntimeException("health check interrupted unexpectedly", exception);
        }
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
                        SandboxFilter.builder().metadata(Map.of("tag", tag)).pageSize(20).build());
        return infos.getSandboxInfos().size();
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
