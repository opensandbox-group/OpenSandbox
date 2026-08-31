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
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.alibaba.opensandbox.sandbox.SandboxManager;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec;
import com.alibaba.opensandbox.sandbox.domain.pool.PoolLifecycleState;
import com.alibaba.opensandbox.sandbox.domain.pool.SandboxPreparer;
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore;
import com.alibaba.opensandbox.sandbox.pool.SandboxPool;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.BooleanSupplier;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("SandboxPool graceful shutdown leak E2E Tests")
public class SandboxPoolGracefulShutdownE2ETest extends BaseE2ETest {
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
    @DisplayName("graceful shutdown publishes warmup completed before drain timeout")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testGracefulShutdownPublishesWarmupCompletedBeforeDrainTimeout() throws Exception {
        tag = uniqueTag("graceful-complete");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch warmupEntered = new CountDownLatch(1);
        CountDownLatch releaseWarmup = new CountDownLatch(1);
        AtomicReference<String> createdSandboxId = new AtomicReference<>();
        SandboxPool pool =
                createPool(
                        stateStore,
                        Duration.ofSeconds(5),
                        sandbox -> {
                            createdSandboxId.set(sandbox.getId());
                            warmupEntered.countDown();
                            awaitLatch(releaseWarmup);
                        });
        Thread releaser = null;

        try {
            pool.start();
            assertTrue(
                    warmupEntered.await(2, TimeUnit.MINUTES),
                    "warmup should create a remote sandbox and enter the preparer");
            assertNotNull(createdSandboxId.get());
            eventually(
                    "created warmup sandbox is visible remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 1);

            releaser =
                    new Thread(
                            () -> {
                                try {
                                    Thread.sleep(500);
                                } catch (InterruptedException exception) {
                                    Thread.currentThread().interrupt();
                                }
                                releaseWarmup.countDown();
                            },
                            "sandbox-pool-e2e-warmup-releaser");
            releaser.start();

            long shutdownStartedAt = System.nanoTime();
            pool.shutdown(true);
            long shutdownElapsedMillis =
                    Duration.ofNanos(System.nanoTime() - shutdownStartedAt).toMillis();

            assertTrue(
                    shutdownElapsedMillis >= 300,
                    "graceful shutdown should wait for the in-flight warmup");
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
            assertEquals(1, pool.snapshot().getIdleCount());
            assertTrue(
                    pool.snapshotIdleEntries().stream()
                            .anyMatch(entry -> createdSandboxId.get().equals(entry.getSandboxId())),
                    "the completed warmup sandbox must be persisted in the idle store");

            pool.releaseAllIdle();
            eventually(
                    "released idle sandbox is deleted remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            releaseWarmup.countDown();
            if (releaser != null) {
                releaser.join(5_000);
            }
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("graceful shutdown drains all admitted warmups without new admission")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testGracefulShutdownDrainsAllAdmittedWarmupsWithoutNewAdmission() throws Exception {
        tag = uniqueTag("graceful-rolling");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch admittedWarmupsEntered = new CountDownLatch(2);
        CountDownLatch releaseWarmups = new CountDownLatch(1);
        AtomicInteger preparerCalls = new AtomicInteger();
        AtomicReference<Throwable> shutdownFailure = new AtomicReference<>();
        SandboxPool pool =
                createPool(
                        stateStore,
                        Duration.ofSeconds(20),
                        sandbox -> {
                            preparerCalls.incrementAndGet();
                            admittedWarmupsEntered.countDown();
                            awaitLatch(releaseWarmups);
                        },
                        3,
                        2);
        Thread shutdownThread = null;

        try {
            pool.start();
            assertTrue(
                    admittedWarmupsEntered.await(2, TimeUnit.MINUTES),
                    "both rolling warmup slots should be admitted before shutdown");
            assertEquals(2, preparerCalls.get());

            shutdownThread =
                    new Thread(
                            () -> {
                                try {
                                    pool.shutdown(true);
                                } catch (Throwable throwable) {
                                    shutdownFailure.set(throwable);
                                }
                            },
                            "sandbox-pool-e2e-graceful-rolling-shutdown");
            shutdownThread.start();

            eventually(
                    "pool enters draining before warmups are released",
                    Duration.ofSeconds(10),
                    Duration.ofMillis(100),
                    () -> pool.snapshot().getLifecycleState() == PoolLifecycleState.DRAINING);
            releaseWarmups.countDown();

            shutdownThread.join(30_000);
            assertFalse(shutdownThread.isAlive(), "graceful shutdown should finish after drain");
            assertNull(shutdownFailure.get(), "graceful shutdown should not fail");
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
            assertEquals(
                    3,
                    pool.snapshot().getIdleCount(),
                    "all pre-admitted warmups should be persisted");
            assertEquals(
                    3,
                    preparerCalls.get(),
                    "shutdown should drain admitted work without admitting beyond maxIdle");
            eventually(
                    "only the three pre-admitted sandboxes remain remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 3);

            pool.releaseAllIdle();
            eventually(
                    "drained rolling warmups are deleted remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            releaseWarmups.countDown();
            if (shutdownThread != null) {
                shutdownThread.join(5_000);
            }
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("forced shutdown deletes interrupted warmup sandbox not persisted as idle")
    @Timeout(value = 4, unit = TimeUnit.MINUTES)
    void testForcedShutdownDeletesInterruptedWarmupSandboxNotPersistedAsIdle() throws Exception {
        tag = uniqueTag("forced-cleanup");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch warmupEntered = new CountDownLatch(1);
        CountDownLatch blockWarmup = new CountDownLatch(1);
        AtomicReference<String> createdSandboxId = new AtomicReference<>();
        AtomicBoolean warmupInterrupted = new AtomicBoolean(false);
        SandboxPool pool =
                createPool(
                        stateStore,
                        Duration.ofMillis(500),
                        sandbox -> {
                            createdSandboxId.set(sandbox.getId());
                            warmupEntered.countDown();
                            try {
                                blockWarmup.await();
                            } catch (InterruptedException exception) {
                                warmupInterrupted.set(true);
                                Thread.currentThread().interrupt();
                                throw new RuntimeException(
                                        "warmup interrupted during forced shutdown", exception);
                            }
                        });

        try {
            pool.start();
            assertTrue(
                    warmupEntered.await(2, TimeUnit.MINUTES),
                    "warmup should create a remote sandbox and enter the preparer");
            assertNotNull(createdSandboxId.get());
            eventually(
                    "unpersisted warmup sandbox is visible remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 1);

            long shutdownStartedAt = System.nanoTime();
            pool.shutdown(true);
            long shutdownElapsedMillis =
                    Duration.ofNanos(System.nanoTime() - shutdownStartedAt).toMillis();

            assertTrue(
                    shutdownElapsedMillis >= 350,
                    "graceful shutdown should wait for drain timeout before forcing warmup stop");
            assertTrue(warmupInterrupted.get(), "warmup should be interrupted after drain timeout");
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
            assertEquals(
                    0,
                    pool.snapshot().getIdleCount(),
                    "failed warmup sandbox must not be persisted as idle");
            assertFalse(
                    pool.snapshotIdleEntries().stream()
                            .anyMatch(entry -> createdSandboxId.get().equals(entry.getSandboxId())),
                    "failed warmup sandbox ID must not remain in the idle store");
            eventually(
                    "interrupted warmup sandbox is deleted remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 0);
        } finally {
            blockWarmup.countDown();
            shutdownAndRelease(pool);
        }
    }

    @Test
    @DisplayName("restart fences and deletes a late warmup from the retired run")
    @Timeout(value = 6, unit = TimeUnit.MINUTES)
    void testRestartFencesLateWarmupFromRetiredRun() throws Exception {
        tag = uniqueTag("restart-late-warmup");
        InMemoryPoolStateStore stateStore = new InMemoryPoolStateStore();
        CountDownLatch oldWarmupEntered = new CountDownLatch(1);
        CountDownLatch releaseOldWarmup = new CountDownLatch(1);
        CountDownLatch newWarmupEntered = new CountDownLatch(1);
        AtomicInteger warmupSequence = new AtomicInteger();
        AtomicBoolean oldWarmupInterrupted = new AtomicBoolean(false);
        AtomicReference<String> oldSandboxId = new AtomicReference<>();
        AtomicReference<String> newSandboxId = new AtomicReference<>();
        SandboxPool pool =
                createPool(
                        stateStore,
                        Duration.ofMillis(300),
                        sandbox -> {
                            if (warmupSequence.incrementAndGet() == 1) {
                                oldSandboxId.set(sandbox.getId());
                                oldWarmupEntered.countDown();
                                awaitIgnoringInterrupt(releaseOldWarmup, oldWarmupInterrupted);
                            } else {
                                newSandboxId.set(sandbox.getId());
                                newWarmupEntered.countDown();
                            }
                        });

        try {
            pool.start();
            assertTrue(
                    oldWarmupEntered.await(2, TimeUnit.MINUTES),
                    "the first lifecycle should enter its deliberately uncooperative warmup");
            assertNotNull(oldSandboxId.get());
            eventually(
                    "old lifecycle warmup is visible remotely",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> taggedSandboxExists(oldSandboxId.get()));

            pool.shutdown(true);

            assertTrue(
                    oldWarmupInterrupted.get(),
                    "forced shutdown should interrupt the old preparer even though it keeps waiting");
            assertEquals(PoolLifecycleState.STOPPED, pool.snapshot().getLifecycleState());
            assertEquals(
                    1L,
                    releaseOldWarmup.getCount(),
                    "old warmup must still be blocked after its lifecycle has stopped");
            assertEquals(0, pool.snapshot().getInFlightOperations());

            pool.start();
            assertTrue(
                    newWarmupEntered.await(2, TimeUnit.MINUTES),
                    "the restarted lifecycle should admit a new warmup independently");
            eventually(
                    "restarted lifecycle publishes only its new warmup",
                    Duration.ofMinutes(2),
                    Duration.ofMillis(500),
                    () ->
                            newSandboxId.get() != null
                                    && pool.snapshot().getIdleCount() == 1
                                    && pool.snapshot().getInFlightOperations() == 0
                                    && pool.snapshotIdleEntries().stream()
                                            .allMatch(
                                                    entry ->
                                                            newSandboxId
                                                                    .get()
                                                                    .equals(entry.getSandboxId())));
            eventually(
                    "old and restarted sandboxes overlap before the old warmup is released",
                    Duration.ofSeconds(30),
                    Duration.ofMillis(500),
                    () -> countTaggedSandboxes() == 2);

            releaseOldWarmup.countDown();

            eventually(
                    "retired lifecycle sandbox is deleted after its late completion",
                    Duration.ofSeconds(60),
                    Duration.ofMillis(500),
                    () ->
                            !taggedSandboxExists(oldSandboxId.get())
                                    && taggedSandboxExists(newSandboxId.get())
                                    && countTaggedSandboxes() == 1);
            assertEquals(1, pool.snapshot().getIdleCount());
            assertEquals(0, pool.snapshot().getInFlightOperations());
            assertTrue(
                    pool.snapshotIdleEntries().stream()
                            .allMatch(entry -> newSandboxId.get().equals(entry.getSandboxId())),
                    "late old-run completion must never enter the restarted idle pool");
        } finally {
            releaseOldWarmup.countDown();
            shutdownAndRelease(pool);
        }
    }

    private SandboxPool createPool(
            InMemoryPoolStateStore stateStore, Duration drainTimeout, SandboxPreparer preparer) {
        return createPool(stateStore, drainTimeout, preparer, 1, 1);
    }

    private SandboxPool createPool(
            InMemoryPoolStateStore stateStore,
            Duration drainTimeout,
            SandboxPreparer preparer,
            int maxIdle,
            int warmupConcurrency) {
        PoolCreationSpec creationSpec =
                PoolCreationSpec.builder()
                        .image(getSandboxImage())
                        .entrypoint(List.of("tail -f /dev/null"))
                        .metadata(Map.of("tag", tag, "suite", "sandbox-pool-graceful-shutdown-e2e"))
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
                .warmupConcurrency(warmupConcurrency)
                .stateStore(stateStore)
                .connectionConfig(sharedConnectionConfig)
                .creationSpec(creationSpec)
                .warmupSandboxPreparer(preparer)
                .drainTimeout(drainTimeout)
                .build();
    }

    private static void awaitLatch(CountDownLatch latch) {
        try {
            latch.await();
        } catch (InterruptedException exception) {
            Thread.currentThread().interrupt();
            throw new RuntimeException("warmup interrupted unexpectedly", exception);
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

    private boolean taggedSandboxExists(String sandboxId) {
        if (sandboxId == null) {
            return false;
        }
        PagedSandboxInfos infos =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder().metadata(Map.of("tag", tag)).pageSize(20).build());
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

    private static void awaitIgnoringInterrupt(CountDownLatch latch, AtomicBoolean interrupted) {
        while (true) {
            try {
                latch.await();
                return;
            } catch (InterruptedException exception) {
                interrupted.set(true);
            }
        }
    }

    private static String uniqueTag(String scenario) {
        return "e2e-pool-" + scenario + "-" + UUID.randomUUID().toString().substring(0, 8);
    }
}
