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

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig;
import java.io.IOException;
import java.io.InputStream;
import java.time.Duration;
import java.time.OffsetDateTime;
import java.util.*;
import org.junit.jupiter.api.*;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/** Base class for all E2E tests providing common setup and teardown functionality. */
@TestInstance(TestInstance.Lifecycle.PER_CLASS)
public abstract class BaseE2ETest {

    protected static final Logger logger = LoggerFactory.getLogger(BaseE2ETest.class);

    // ==========================================
    // Configuration Keys
    // ==========================================
    private static final String PROP_API_KEY = "opensandbox.test.api.key";
    private static final String PROP_DOMAIN = "opensandbox.test.domain";
    private static final String PROP_PROTOCOL = "opensandbox.test.protocol";
    private static final String PROP_IMG_DEFAULT = "opensandbox.sandbox.default.image";
    private static final String ENV_API_KEY = "OPENSANDBOX_TEST_API_KEY";
    private static final String ENV_DOMAIN = "OPENSANDBOX_TEST_DOMAIN";
    private static final String ENV_PROTOCOL = "OPENSANDBOX_TEST_PROTOCOL";
    private static final String ENV_IMG_DEFAULT = "OPENSANDBOX_SANDBOX_DEFAULT_IMAGE";
    private static final String ENV_USE_SERVER_PROXY = "OPENSANDBOX_TEST_USE_SERVER_PROXY";

    // ==========================================
    // Shared State (Static)
    // ==========================================
    protected static final Properties testProperties = new Properties();
    protected static ConnectionConfig sharedConnectionConfig;

    static {
        loadTestProperties();
        initializeSharedConfig();
    }

    protected static String getSandboxImage() {
        return configValue(PROP_IMG_DEFAULT, ENV_IMG_DEFAULT, "opensandbox/code-interpreter:latest");
    }

    protected static ConnectionConfig createConnectionConfig(boolean useServerProxy) {
        String protocol = configValue(PROP_PROTOCOL, ENV_PROTOCOL, "http");
        return ConnectionConfig.builder()
                .apiKey(configValue(PROP_API_KEY, ENV_API_KEY, "e2e-test"))
                .domain(configValue(PROP_DOMAIN, ENV_DOMAIN, "localhost:8080"))
                .requestTimeout(Duration.ofMinutes(3))
                .protocol(protocol)
                .useServerProxy(useServerProxy || shouldUseServerProxy())
                .build();
    }

    private static void loadTestProperties() {
        try (InputStream input =
                BaseE2ETest.class.getClassLoader().getResourceAsStream("test.properties")) {
            if (input != null) {
                testProperties.load(input);
            } else {
                logger.warn("test.properties file not found, using default values.");
            }
        } catch (IOException e) {
            throw new RuntimeException("Failed to load test properties", e);
        }
    }

    private static void initializeSharedConfig() {
        String protocol = configValue(PROP_PROTOCOL, ENV_PROTOCOL, "http");
        sharedConnectionConfig =
                ConnectionConfig.builder()
                        .apiKey(configValue(PROP_API_KEY, ENV_API_KEY, "e2e-test"))
                        .domain(configValue(PROP_DOMAIN, ENV_DOMAIN, "localhost:8080"))
                        .requestTimeout(Duration.ofMinutes(3))
                        .protocol(protocol)
                        .useServerProxy(shouldUseServerProxy())
                        .build();
    }

    private static String configValue(String propertyKey, String envKey, String defaultValue) {
        String envValue = System.getenv(envKey);
        if (envValue != null && !envValue.isBlank()) {
            return envValue;
        }
        return testProperties.getProperty(propertyKey, defaultValue);
    }

    private static boolean shouldUseServerProxy() {
        String envValue = System.getenv(ENV_USE_SERVER_PROXY);
        return envValue != null && Boolean.parseBoolean(envValue);
    }

    @BeforeEach
    void beforeEach(TestInfo testInfo) {
        logger.info("=== Starting test: {} ===", testInfo.getDisplayName());
    }

    // ==========================================
    // Shared assertion helpers (ported from python e2e style)
    // ==========================================
    protected static long nowMs() {
        return System.currentTimeMillis();
    }

    protected static void assertRecentTimestampMs(long ts, long toleranceMs) {
        assertTrue(ts > 0, "timestamp must be > 0");
        long delta = Math.abs(nowMs() - ts);
        assertTrue(
                delta <= toleranceMs,
                "timestamp too far from now: delta=" + delta + "ms (ts=" + ts + ")");
    }

    protected static void assertEndpointHasPort(String endpoint, int expectedPort) {
        assertNotNull(endpoint);
        assertFalse(endpoint.contains("://"), "unexpected scheme in endpoint: " + endpoint);
        if (endpoint.contains("/")) {
            assertTrue(
                    endpoint.endsWith("/" + expectedPort),
                    "endpoint route must end with /" + expectedPort + ": " + endpoint);
            String prefix = endpoint.split("/", 2)[0];
            assertFalse(prefix.isBlank(), "missing domain in endpoint: " + endpoint);
            return;
        }
        int idx = endpoint.lastIndexOf(':');
        assertTrue(idx > 0, "missing host:port in endpoint: " + endpoint);
        String host = endpoint.substring(0, idx);
        String port = endpoint.substring(idx + 1);
        assertFalse(host.isBlank(), "missing host in endpoint: " + endpoint);
        assertTrue(port.matches("\\d+"), "non-numeric port in endpoint: " + endpoint);
        assertEquals(expectedPort, Integer.parseInt(port), "endpoint port mismatch: " + endpoint);
    }

    protected static void assertTimesClose(
            OffsetDateTime createdAt, OffsetDateTime modifiedAt, long toleranceSeconds) {
        long delta = Math.abs(Duration.between(createdAt, modifiedAt).getSeconds());
        assertTrue(delta <= toleranceSeconds, "created/modified skew too large: " + delta + "s");
    }
}
