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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.pool.PoolCreationSpec
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import okhttp3.ConnectionPool
import okhttp3.mockwebserver.Dispatcher
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.RecordedRequest
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Duration
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Verifies the pool-level shared connection pool: created by default,
 * reused by warmup and acquire paths, evicted on shutdown, and never created
 * when the user provides their own pool.
 */
class SandboxPoolSharedConnectionTest {
    private lateinit var lifecycle: MockWebServer
    private lateinit var execd: MockWebServer
    private val sandboxSeq = AtomicInteger()

    @BeforeEach
    fun setUp() {
        lifecycle = MockWebServer()
        execd = MockWebServer()
        lifecycle.start()
        execd.start()
        lifecycle.dispatcher =
            object : Dispatcher() {
                override fun dispatch(request: RecordedRequest): MockResponse {
                    val path = request.path.orEmpty()
                    return when {
                        request.method == "POST" && path == "/v1/sandboxes" ->
                            MockResponse().setResponseCode(201).setBody(
                                """{"id":"sbx-${sandboxSeq.incrementAndGet()}","status":{"state":"Running"},""" +
                                    """"createdAt":"2026-01-01T00:00:00Z","entrypoint":["tail"]}""",
                            )
                        request.method == "GET" && path.contains("/endpoints/") ->
                            MockResponse().setResponseCode(200).setBody(
                                """{"endpoint":"${execd.hostName}:${execd.port}","headers":{"X-EXECD-ACCESS-TOKEN":"t"}}""",
                            )
                        request.method == "POST" && path.endsWith("/renew-expiration") ->
                            MockResponse().setResponseCode(200).setBody("""{"expiresAt":"2026-12-31T00:00:00Z"}""")
                        request.method == "DELETE" -> MockResponse().setResponseCode(204)
                        else -> MockResponse().setResponseCode(404)
                    }
                }
            }
        execd.dispatcher =
            object : Dispatcher() {
                override fun dispatch(request: RecordedRequest): MockResponse =
                    MockResponse().setResponseCode(200).setBody("""{"status":"ok"}""")
            }
    }

    @AfterEach
    fun tearDown() {
        lifecycle.shutdown()
        execd.shutdown()
    }

    private fun connectionConfig(userPool: ConnectionPool? = null): ConnectionConfig {
        val builder =
            ConnectionConfig.builder()
                .domain(lifecycle.hostName + ":" + lifecycle.port)
                .protocol("http")
                .requestTimeout(Duration.ofSeconds(10))
                .disableMetrics()
        userPool?.let { builder.connectionPool(it) }
        return builder.build()
    }

    private fun buildPool(
        userPool: ConnectionPool? = null,
        maxIdle: Int = 4,
        warmupConcurrency: Int = 2,
    ): SandboxPool =
        SandboxPool.builder()
            .poolName("shared-conn-test")
            .ownerId("owner")
            .maxIdle(maxIdle)
            .warmupConcurrency(warmupConcurrency)
            .stateStore(InMemoryPoolStateStore())
            .connectionConfig(connectionConfig(userPool))
            .creationSpec(
                PoolCreationSpec.builder()
                    .image("test:latest")
                    .entrypoint("tail", "-f", "/dev/null")
                    .build(),
            )
            .reconcileInterval(Duration.ofMillis(100))
            .idleTimeout(Duration.ofMinutes(30))
            .acquireSkipHealthCheck(false)
            .build()

    private fun waitForIdle(
        pool: SandboxPool,
        target: Int,
        timeoutMs: Long = 15000,
    ) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (pool.snapshot().idleCount < target) {
            assertTrue(System.currentTimeMillis() < deadline, "idle never reached $target")
            Thread.sleep(50)
        }
    }

    @Test
    fun `pool creates a default shared pool when none provided`() {
        val pool = buildPool(warmupConcurrency = 5)
        assertNotNull(pool.sharedConnectionPoolForTests(), "expected a pool-created shared connection pool")
        pool.shutdown(graceful = false)
    }

    @Test
    fun `user provided pool is used as-is and no default pool is created`() {
        val userPool = ConnectionPool(1, 1, TimeUnit.MINUTES)
        val pool = buildPool(userPool = userPool)
        assertNull(pool.sharedConnectionPoolForTests(), "user pool must not be shadowed by a default pool")
        pool.shutdown(graceful = false)
    }

    @Test
    fun `warmup and acquire reuse the shared pool instead of fresh connections`() {
        val pool = buildPool(maxIdle = 4, warmupConcurrency = 2)
        val shared = pool.sharedConnectionPoolForTests()!!
        pool.start()
        try {
            waitForIdle(pool, 4)
            // 4 sandboxes were created over HTTP; with a shared pool the
            // connections stay in the pool instead of being opened fresh per
            // sandbox. The bound is loose to stay robust on all platforms.
            assertTrue(shared.idleConnectionCount() > 0, "shared pool should hold idle connections after warmup")
            assertTrue(
                shared.connectionCount() <= 6,
                "expected connection reuse, got ${shared.connectionCount()} connections",
            )

            // Acquire reuses the same shared pool.
            val sb = pool.acquire(Duration.ofMinutes(10))
            assertTrue(sb.id.startsWith("sbx-"))
            sb.kill()
        } finally {
            pool.shutdown(graceful = false)
        }
    }

    @Test
    fun `pool-owned shared pool is evicted on shutdown`() {
        val pool = buildPool(maxIdle = 2, warmupConcurrency = 1)
        val shared = pool.sharedConnectionPoolForTests()!!
        pool.start()
        try {
            waitForIdle(pool, 2)
            assertTrue(shared.idleConnectionCount() > 0)
        } finally {
            pool.shutdown(graceful = false)
        }
        assertEquals(0, shared.idleConnectionCount(), "pool-owned connections must be evicted on shutdown")
    }
}
