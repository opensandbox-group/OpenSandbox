/*
 * Copyright 2026 The OpenSandbox Authors
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
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent;
import java.time.Duration;
import java.util.List;
import java.util.UUID;
import java.util.function.Supplier;
import org.junit.jupiter.api.*;

@Tag("e2e")
@DisplayName("Diagnostics E2E Tests")
public class DiagnosticsE2ETest extends BaseE2ETest {
    private static final String MARKER = "opensandbox-diagnostics-e2e";
    private SandboxManager manager;
    private Sandbox sandbox;

    @BeforeAll
    void setup() {
        manager = SandboxManager.builder().connectionConfig(sharedConnectionConfig).build();
        sandbox =
                Sandbox.builder()
                        .connectionConfig(sharedConnectionConfig)
                        .image(getSandboxImage())
                        .timeout(Duration.ofMinutes(5))
                        .readyTimeout(Duration.ofSeconds(90))
                        .entrypoint("sh", "-c", "echo " + MARKER + "; sleep 300")
                        .env("EXECD_API_GRACE_SHUTDOWN", "1s")
                        .build();
    }

    @AfterAll
    void cleanup() {
        try {
            if (sandbox != null) {
                try {
                    sandbox.kill();
                } finally {
                    sandbox.close();
                }
            }
        } finally {
            if (manager != null) manager.close();
        }
    }

    @Test
    void contextIdentifiesTheSameBackendThroughBothFacades() {
        var context = manager.getDiagnosticContext(sandbox.getId());
        var current = sandbox.getDiagnosticContext();
        assertEquals(sandbox.getId(), context.getSandboxId());
        assertEquals(context.getBackendType(), current.getBackendType());
        assertEquals(context.getBackendIdentifiers(), current.getBackendIdentifiers());
        assertNotNull(context.getState());
        if ("docker".equals(context.getBackendType())) {
            assertFalse(context.getBackendIdentifiers().get("containerId").isBlank());
            assertFalse(context.getBackendIdentifiers().get("containerName").isBlank());
        } else {
            assertEquals("kubernetes", context.getBackendType());
            assertFalse(context.getBackendIdentifiers().get("namespace").isBlank());
            assertFalse(context.getBackendIdentifiers().get("workloadName").isBlank());
        }
        logger.info(
                "diagnostic context: type={}, identifiers={}",
                context.getBackendType(),
                context.getBackendIdentifiers());
    }

    @Test
    void logsContainEntrypointOutputThroughBothFacades() throws InterruptedException {
        for (String scope : List.of("container", "all")) {
            List<Supplier<DiagnosticContent>> calls =
                    List.of(
                            () -> manager.getDiagnosticLogs(sandbox.getId(), scope),
                            () -> sandbox.getDiagnosticLogs(scope));
            for (var getLogs : calls) {
                long deadline = System.nanoTime() + Duration.ofSeconds(30).toNanos();
                while (true) {
                    var result = getLogs.get();
                    assertContent(result, "logs", scope);
                    if (result.getContent().contains(MARKER)) break;
                    assertTrue(
                            System.nanoTime() < deadline,
                            "entrypoint log missing: " + result.getContent());
                    Thread.sleep(500);
                }
            }
        }
    }

    @Test
    void eventsReturnDescriptorsThroughBothFacades() {
        for (String scope : List.of("runtime", "all")) {
            assertContent(manager.getDiagnosticEvents(sandbox.getId(), scope), "events", scope);
            assertContent(sandbox.getDiagnosticEvents(scope), "events", scope);
        }
    }

    @Test
    void unsupportedScopePreservesApiErrors() {
        for (var call :
                List.<Runnable>of(
                        () -> manager.getDiagnosticLogs(sandbox.getId(), "unsupported-e2e-scope"),
                        () ->
                                manager.getDiagnosticEvents(
                                        sandbox.getId(), "unsupported-e2e-scope"))) {
            var error = assertThrows(SandboxApiException.class, call::run);
            assertEquals(400, error.getStatusCode());
            assertEquals("DIAGNOSTICS_SCOPE_UNSUPPORTED", error.getError().getCode());
        }
    }

    @Test
    void missingSandboxPreservesNotFound() {
        String id = UUID.randomUUID().toString();
        for (var call :
                List.<Runnable>of(
                        () -> manager.getDiagnosticContext(id),
                        () -> manager.getDiagnosticLogs(id, "container"),
                        () -> manager.getDiagnosticEvents(id, "runtime"))) {
            var error = assertThrows(SandboxApiException.class, call::run);
            assertEquals(404, error.getStatusCode());
        }
    }

    private void assertContent(DiagnosticContent result, String kind, String scope) {
        assertEquals(sandbox.getId(), result.getSandboxId());
        assertEquals(kind, result.getKind());
        assertEquals(scope, result.getScope());
        assertEquals("inline", result.getDelivery());
        assertTrue(result.getContentType().startsWith("text/plain"));
        assertNotNull(result.getContent());
    }
}
