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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException;
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.*;
import java.time.Duration;
import java.util.*;
import java.util.concurrent.TimeUnit;
import org.junit.jupiter.api.*;

/**
 * E2E tests for SandboxManager list/filter semantics.
 *
 * <p>Focus:
 *
 * <ul>
 *   <li>states filter uses OR logic
 *   <li>metadata filter uses AND logic
 * </ul>
 *
 * <p>We create 3 dedicated sandboxes per run to keep assertions deterministic and avoid impacting
 * the shared sandbox used by other tests.
 */
@Tag("e2e")
@DisplayName("SandboxManager E2E Tests (Java SDK) - List/Filter Semantics")
@TestMethodOrder(MethodOrderer.OrderAnnotation.class)
public class SandboxManagerE2ETest extends BaseE2ETest {

    private SandboxManager sandboxManager;
    private Sandbox s1;
    private Sandbox s2;
    private Sandbox s3;
    private String tag;

    @BeforeAll
    void setup() throws InterruptedException {
        sandboxManager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();
        tag = "e2e-sandbox-manager-" + UUID.randomUUID().toString().substring(0, 8);
        Map<String, String> resourceMap = new HashMap<>();
        resourceMap.put("cpu", "1");
        resourceMap.put("memory", "2Gi");

        s1 =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .image(getSandboxImage())
                        .resource(resourceMap)
                        .timeout(Duration.ofMinutes(5))
                        .readyTimeout(Duration.ofSeconds(60))
                        .metadata(Map.of("tag", tag, "team", "t1", "env", "prod"))
                        .env("E2E_TEST", "true")
                        .env("EXECD_API_GRACE_SHUTDOWN", "3s")
                        .env("EXECD_JUPYTER_IDLE_POLL_INTERVAL", "200ms")
                        .healthCheckPollingInterval(Duration.ofMillis(500))
                        .build();
        s2 =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .image(getSandboxImage())
                        .resource(resourceMap)
                        .timeout(Duration.ofMinutes(5))
                        .readyTimeout(Duration.ofSeconds(60))
                        .metadata(Map.of("tag", tag, "team", "t1", "env", "dev"))
                        .env("E2E_TEST", "true")
                        .env("EXECD_API_GRACE_SHUTDOWN", "3s")
                        .env("EXECD_JUPYTER_IDLE_POLL_INTERVAL", "200ms")
                        .healthCheckPollingInterval(Duration.ofMillis(500))
                        .build();
        s3 =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .image(getSandboxImage())
                        .resource(resourceMap)
                        .timeout(Duration.ofMinutes(5))
                        .readyTimeout(Duration.ofSeconds(60))
                        .metadata(Map.of("tag", tag, "env", "prod"))
                        .env("E2E_TEST", "true")
                        .env("EXECD_API_GRACE_SHUTDOWN", "3s")
                        .env("EXECD_JUPYTER_IDLE_POLL_INTERVAL", "200ms")
                        .healthCheckPollingInterval(Duration.ofMillis(500))
                        .build();

        assertTrue(s1.isHealthy());
        assertTrue(s2.isHealthy());
        assertTrue(s3.isHealthy());

        // Pause s3 to create a deterministic non-Running state.
        sandboxManager.pauseSandbox(s3.getId());
        long deadline = System.currentTimeMillis() + 180_000;
        while (System.currentTimeMillis() < deadline) {
            SandboxInfo info = sandboxManager.getSandboxInfo(s3.getId());
            if ("Paused".equals(info.getStatus().getState())) {
                break;
            }
            Thread.sleep(1000);
        }
        assertEquals("Paused", sandboxManager.getSandboxInfo(s3.getId()).getStatus().getState());
    }

    @AfterAll
    void teardown() {
        for (Sandbox s : List.of(s1, s2, s3)) {
            if (s == null) continue;
            try {
                s.kill();
            } catch (Exception ignored) {
            }
            try {
                s.close();
            } catch (Exception ignored) {
            }
        }
        if (sandboxManager != null) {
            try {
                sandboxManager.close();
            } catch (Exception ignored) {
            }
        }
    }

    @Test
    @Order(1)
    @DisplayName("states filter uses OR semantics")
    @Timeout(value = 2, unit = TimeUnit.MINUTES)
    void testStatesFilterOrLogic() {
        SandboxFilter filter =
                SandboxFilter.builder()
                        .states("Running", "Paused")
                        .metadata(Map.of("tag", tag))
                        .pageSize(50)
                        .build();
        PagedSandboxInfos infos = sandboxManager.listSandboxInfos(filter);
        Set<String> ids = new HashSet<>();
        for (SandboxInfo info : infos.getSandboxInfos()) {
            ids.add(info.getId());
        }
        assertTrue(ids.containsAll(Set.of(s1.getId(), s2.getId(), s3.getId())));

        PagedSandboxInfos pausedOnly =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .states("Paused")
                                .metadata(Map.of("tag", tag))
                                .pageSize(50)
                                .build());
        Set<String> pausedIds = new HashSet<>();
        for (SandboxInfo info : pausedOnly.getSandboxInfos()) {
            pausedIds.add(info.getId());
        }
        assertTrue(pausedIds.contains(s3.getId()));
        assertFalse(pausedIds.contains(s1.getId()));
        assertFalse(pausedIds.contains(s2.getId()));

        PagedSandboxInfos runningOnly =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .states("Running")
                                .metadata(Map.of("tag", tag))
                                .pageSize(50)
                                .build());
        Set<String> runningIds = new HashSet<>();
        for (SandboxInfo info : runningOnly.getSandboxInfos()) {
            runningIds.add(info.getId());
        }
        assertTrue(runningIds.contains(s1.getId()));
        assertTrue(runningIds.contains(s2.getId()));
        assertFalse(runningIds.contains(s3.getId()));
    }

    @Test
    @Order(2)
    @DisplayName("metadata filter uses AND semantics")
    @Timeout(value = 2, unit = TimeUnit.MINUTES)
    void testMetadataFilterAndLogic() {
        PagedSandboxInfos tagAndTeam =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", tag, "team", "t1"))
                                .pageSize(50)
                                .build());
        Set<String> ids = new HashSet<>();
        for (SandboxInfo info : tagAndTeam.getSandboxInfos()) {
            ids.add(info.getId());
        }
        assertTrue(ids.contains(s1.getId()));
        assertTrue(ids.contains(s2.getId()));
        assertFalse(ids.contains(s3.getId()));

        PagedSandboxInfos tagTeamEnv =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", tag, "team", "t1", "env", "prod"))
                                .pageSize(50)
                                .build());
        Set<String> ids2 = new HashSet<>();
        for (SandboxInfo info : tagTeamEnv.getSandboxInfos()) {
            ids2.add(info.getId());
        }
        assertTrue(ids2.contains(s1.getId()));
        assertFalse(ids2.contains(s2.getId()));
        assertFalse(ids2.contains(s3.getId()));

        PagedSandboxInfos tagEnv =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", tag, "env", "prod"))
                                .pageSize(50)
                                .build());
        Set<String> ids3 = new HashSet<>();
        for (SandboxInfo info : tagEnv.getSandboxInfos()) {
            ids3.add(info.getId());
        }
        assertTrue(ids3.contains(s1.getId()));
        assertTrue(ids3.contains(s3.getId()));
        assertFalse(ids3.contains(s2.getId()));

        PagedSandboxInfos noneMatch =
                sandboxManager.listSandboxInfos(
                        SandboxFilter.builder()
                                .metadata(Map.of("tag", tag, "team", "t2"))
                                .pageSize(50)
                                .build());
        for (SandboxInfo info : noneMatch.getSandboxInfos()) {
            assertFalse(Set.of(s1.getId(), s2.getId(), s3.getId()).contains(info.getId()));
        }

        Map<String, String> patch = new HashMap<>();
        patch.put("env", "stage");
        patch.put("team", null);
        SandboxInfo patched = sandboxManager.patchSandboxMetadata(s2.getId(), patch);
        assertEquals("stage", patched.getMetadata().get("env"));
        assertFalse(patched.getMetadata().containsKey("team"));

        SandboxInfo refreshed = sandboxManager.getSandboxInfo(s2.getId());
        assertEquals("stage", refreshed.getMetadata().get("env"));
        assertFalse(refreshed.getMetadata().containsKey("team"));
    }

    @Test
    @Order(3)
    @DisplayName("invalid operations raise SandboxException")
    @Timeout(value = 1, unit = TimeUnit.MINUTES)
    void testInvalidOperations() {
        String nonExistentId = "non-existent-" + System.nanoTime();
        assertThrows(SandboxException.class, () -> sandboxManager.getSandboxInfo(nonExistentId));
        assertThrows(SandboxException.class, () -> sandboxManager.pauseSandbox(nonExistentId));
        assertThrows(SandboxException.class, () -> sandboxManager.resumeSandbox(nonExistentId));
        assertThrows(SandboxException.class, () -> sandboxManager.killSandbox(nonExistentId));
        assertThrows(
                SandboxException.class,
                () -> sandboxManager.renewSandbox(nonExistentId, Duration.ofMinutes(5)));
    }
}
