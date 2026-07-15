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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException;
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution;
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSnapshotInfos;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotFilter;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo;
import java.time.Duration;
import java.time.OffsetDateTime;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.Assumptions;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Tag;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.Timeout;

@Tag("e2e")
@DisplayName("Snapshot E2E Tests (Java SDK)")
public class SnapshotE2ETest extends BaseE2ETest {

    private static final Duration SNAPSHOT_TIMEOUT = Duration.ofMinutes(5);
    private static final Duration DELETE_TIMEOUT = Duration.ofMinutes(3);
    private static final String SNAPSHOT_DIR = "/tmp/opensandbox-snapshot-e2e";
    private static final String SNAPSHOT_FILE = SNAPSHOT_DIR + "/marker.txt";

    @Test
    @DisplayName("snapshot create, poll, list, restore, and delete")
    @Timeout(value = 10, unit = TimeUnit.MINUTES)
    void testSnapshotLifecycleEndToEnd() throws InterruptedException {
        SandboxManager manager =
                SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();
        Sandbox source = null;
        Sandbox restored = null;
        String readySnapshotId = null;
        String tag = "snapshot-e2e-" + UUID.randomUUID().toString().substring(0, 8);
        String marker = "snapshot-marker-" + UUID.randomUUID();

        try {
            source =
                    Sandbox.builder()
                            .connectionConfig(sharedConnectionConfig)
                            .image(getSandboxImage())
                            .timeout(Duration.ofMinutes(5))
                            .readyTimeout(Duration.ofSeconds(60))
                            .metadata(Map.of("tag", tag, "role", "source"))
                            .env("E2E_TEST", "true")
                            .healthCheckPollingInterval(Duration.ofMillis(500))
                            .build();
            assertTrue(source.isHealthy(), "source sandbox should be healthy");

            runOk(
                    source,
                    "mkdir -p "
                            + SNAPSHOT_DIR
                            + " && printf '%s' '"
                            + marker
                            + "' > "
                            + SNAPSHOT_FILE);

            SnapshotInfo accepted = source.createSnapshot(tag + "-ready");
            readySnapshotId = accepted.getId();
            assertSnapshotFields(
                    accepted,
                    readySnapshotId,
                    source.getId(),
                    tag + "-ready",
                    "Creating",
                    "snapshot_accepted",
                    "Snapshot creation accepted.",
                    null);

            SnapshotInfo ready =
                    waitForSnapshotState(manager, readySnapshotId, "Ready", SNAPSHOT_TIMEOUT);
            assertSnapshotFields(
                    ready,
                    readySnapshotId,
                    source.getId(),
                    tag + "-ready",
                    "Ready",
                    null,
                    null,
                    accepted.getCreatedAt());
            assertFalse(
                    ready.getStatus()
                            .getLastTransitionAt()
                            .isBefore(accepted.getStatus().getLastTransitionAt()),
                    "Ready transition timestamp should not be earlier than Creating");

            PagedSnapshotInfos allSnapshots =
                    manager.listSnapshots(
                            SnapshotFilter.builder()
                                    .sandboxId(source.getId())
                                    .page(1)
                                    .pageSize(50)
                                    .build());
            assertSnapshotList(allSnapshots, 1, 50, ready);

            PagedSnapshotInfos readySnapshots =
                    manager.listSnapshots(
                            SnapshotFilter.builder()
                                    .sandboxId(source.getId())
                                    .page(1)
                                    .states("Ready")
                                    .pageSize(50)
                                    .build());
            assertSnapshotList(readySnapshots, 1, 50, ready);

            restored =
                    Sandbox.builder()
                            .connectionConfig(sharedConnectionConfig)
                            .snapshotId(readySnapshotId)
                            .timeout(Duration.ofMinutes(5))
                            .readyTimeout(Duration.ofSeconds(60))
                            .metadata(Map.of("tag", tag, "role", "restored"))
                            .env("E2E_TEST", "true")
                            .healthCheckPollingInterval(Duration.ofMillis(500))
                            .build();
            assertTrue(restored.isHealthy(), "restored sandbox should be healthy");
            Execution restoredRead =
                    runOk(restored, "test -f " + SNAPSHOT_FILE + " && cat " + SNAPSHOT_FILE);
            assertEquals(1, restoredRead.getLogs().getStdout().size());
            assertEquals(marker, restoredRead.getLogs().getStdout().get(0).getText());

            closeSandbox(restored);
            restored = null;

            manager.deleteSnapshot(readySnapshotId);
            waitForSnapshotDeleted(manager, readySnapshotId, DELETE_TIMEOUT);
            readySnapshotId = null;

            PagedSnapshotInfos readySnapshotsAfterDelete =
                    manager.listSnapshots(
                            SnapshotFilter.builder()
                                    .sandboxId(source.getId())
                                    .page(1)
                                    .states("Ready")
                                    .pageSize(50)
                                    .build());
            assertSnapshotList(readySnapshotsAfterDelete, 1, 50);
        } finally {
            closeSandbox(restored);
            closeSandbox(source);
            deleteSnapshotIfExists(manager, readySnapshotId);
            manager.close();
        }
    }

    private static Execution runOk(Sandbox sandbox, String command) {
        Execution execution =
                sandbox.commands().run(RunCommandRequest.builder().command(command).build());
        assertNotNull(execution);
        assertNull(execution.getError(), "command failed: " + command);
        return execution;
    }

    private static SnapshotInfo waitForSnapshotState(
            SandboxManager manager, String snapshotId, String expectedState, Duration timeout)
            throws InterruptedException {
        long deadline = System.nanoTime() + timeout.toNanos();
        SnapshotInfo last = null;
        while (System.nanoTime() < deadline) {
            last = manager.getSnapshot(snapshotId);
            String state = last.getStatus().getState();
            if (expectedState.equals(state)) {
                return last;
            }
            if ("Failed".equals(state)) {
                fail(
                        "snapshot failed: reason="
                                + last.getStatus().getReason()
                                + " message="
                                + last.getStatus().getMessage());
            }
            Thread.sleep(1000);
        }
        fail(
                "snapshot did not become "
                        + expectedState
                        + " within "
                        + timeout
                        + ", lastState="
                        + (last == null ? "<none>" : last.getStatus().getState()));
        throw new IllegalStateException("unreachable");
    }

    private static void waitForSnapshotDeleted(
            SandboxManager manager, String snapshotId, Duration timeout)
            throws InterruptedException {
        long deadline = System.nanoTime() + timeout.toNanos();
        while (System.nanoTime() < deadline) {
            try {
                manager.getSnapshot(snapshotId);
            } catch (SandboxApiException exception) {
                if (Integer.valueOf(404).equals(exception.getStatusCode())) {
                    return;
                }
                throw exception;
            }
            Thread.sleep(1000);
        }
        fail("snapshot was not deleted within " + timeout + ": " + snapshotId);
    }

    private static void deleteSnapshotIfExists(SandboxManager manager, String snapshotId) {
        if (snapshotId == null) {
            return;
        }
        try {
            manager.deleteSnapshot(snapshotId);
        } catch (SandboxException ignored) {
        }
    }

    private static void assertSnapshotFields(
            SnapshotInfo snapshot,
            String expectedId,
            String expectedSandboxId,
            String expectedName,
            String expectedState,
            String expectedReason,
            String expectedMessage,
            OffsetDateTime expectedCreatedAt) {
        assertNotNull(snapshot);
        assertEquals(expectedId, snapshot.getId());
        assertEquals(expectedSandboxId, snapshot.getSandboxId());
        assertEquals(expectedName, snapshot.getName());
        assertNotNull(snapshot.getStatus());
        assertEquals(expectedState, snapshot.getStatus().getState());
        assertNotNull(snapshot.getCreatedAt(), "snapshot.createdAt should be present");
        assertNotNull(
                snapshot.getStatus().getLastTransitionAt(),
                "snapshot.status.lastTransitionAt should be present");
        assertFalse(
                snapshot.getStatus().getLastTransitionAt().isBefore(snapshot.getCreatedAt()),
                "snapshot.status.lastTransitionAt should not be earlier than createdAt");

        if (expectedCreatedAt != null) {
            assertEquals(expectedCreatedAt, snapshot.getCreatedAt());
        }

        if (expectedReason != null) {
            assertEquals(expectedReason, snapshot.getStatus().getReason());
        } else {
            assertNotNull(
                    snapshot.getStatus().getReason(), "snapshot.status.reason should be present");
            assertFalse(
                    snapshot.getStatus().getReason().isBlank(),
                    "snapshot.status.reason should not be blank");
        }

        if (expectedMessage != null) {
            assertEquals(expectedMessage, snapshot.getStatus().getMessage());
        } else {
            assertNotNull(
                    snapshot.getStatus().getMessage(), "snapshot.status.message should be present");
            assertFalse(
                    snapshot.getStatus().getMessage().isBlank(),
                    "snapshot.status.message should not be blank");
        }
    }

    private static void assertSnapshotList(
            PagedSnapshotInfos snapshotPage,
            int expectedPage,
            int expectedPageSize,
            SnapshotInfo... expected) {
        assertNotNull(snapshotPage);
        assertNotNull(snapshotPage.getSnapshotInfos());
        assertNotNull(snapshotPage.getPagination());
        assertEquals(expectedPage, snapshotPage.getPagination().getPage());
        assertEquals(expectedPageSize, snapshotPage.getPagination().getPageSize());
        assertEquals(expected.length, snapshotPage.getSnapshotInfos().size());
        assertEquals(expected.length, snapshotPage.getPagination().getTotalItems());
        assertEquals(expected.length == 0 ? 0 : 1, snapshotPage.getPagination().getTotalPages());
        assertFalse(snapshotPage.getPagination().getHasNextPage());

        List<SnapshotInfo> expectedSnapshots = Arrays.asList(expected);
        for (SnapshotInfo expectedSnapshot : expectedSnapshots) {
            SnapshotInfo listedSnapshot =
                    snapshotPage.getSnapshotInfos().stream()
                            .filter(snapshot -> expectedSnapshot.getId().equals(snapshot.getId()))
                            .findFirst()
                            .orElseThrow(
                                    () ->
                                            new AssertionError(
                                                    "snapshot "
                                                            + expectedSnapshot.getId()
                                                            + " missing from listSnapshots response"));
            assertSnapshotEquals(expectedSnapshot, listedSnapshot);
        }
    }

    private static void assertSnapshotEquals(SnapshotInfo expected, SnapshotInfo actual) {
        assertNotNull(expected);
        assertNotNull(actual);
        assertEquals(expected.getId(), actual.getId());
        assertEquals(expected.getSandboxId(), actual.getSandboxId());
        assertEquals(expected.getName(), actual.getName());
        assertEquals(expected.getCreatedAt(), actual.getCreatedAt());
        assertNotNull(actual.getStatus());
        assertEquals(expected.getStatus().getState(), actual.getStatus().getState());
        assertEquals(expected.getStatus().getReason(), actual.getStatus().getReason());
        assertEquals(expected.getStatus().getMessage(), actual.getStatus().getMessage());
        assertEquals(
                expected.getStatus().getLastTransitionAt(),
                actual.getStatus().getLastTransitionAt());
    }

    private static void closeSandbox(Sandbox sandbox) {
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
