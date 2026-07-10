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

package com.alibaba.opensandbox.sandbox.domain.pool

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import io.mockk.mockk
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import java.time.Duration

class PoolConfigTest {
    @Test
    fun `build uses default warmup readiness settings`() {
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .build()

        assertEquals(Duration.ofSeconds(30), config.warmupReadyTimeout)
        assertEquals(Duration.ofMillis(200), config.warmupHealthCheckPollingInterval)
        assertFalse(config.warmupSkipHealthCheck)
        assertEquals(null, config.warmupHealthCheck)
        assertEquals(Duration.ofSeconds(30), config.acquireReadyTimeout)
        assertEquals(Duration.ofMillis(200), config.acquireHealthCheckPollingInterval)
        assertFalse(config.acquireSkipHealthCheck)
        assertEquals(null, config.acquireHealthCheck)
        // 24h idle, default cap of 60s applies → 60s.
        assertEquals(Duration.ofSeconds(60), config.acquireMinRemainingTtl)
        assertEquals(Duration.ofHours(24), config.idleTimeout)
    }

    @Test
    fun `build keeps configured warmup readiness settings`() {
        val healthCheck: (Sandbox) -> Boolean = { true }
        val preparer = SandboxPreparer {}
        val sandboxCreator = PooledSandboxCreator { mockk<Sandbox>() }
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .acquireReadyTimeout(Duration.ofSeconds(10))
                .acquireHealthCheckPollingInterval(Duration.ofMillis(250))
                .acquireHealthCheck(healthCheck)
                .acquireSkipHealthCheck()
                .acquireMinRemainingTtl(Duration.ofSeconds(90))
                .warmupReadyTimeout(Duration.ofSeconds(45))
                .warmupHealthCheckPollingInterval(Duration.ofSeconds(1))
                .warmupHealthCheck(healthCheck)
                .warmupSandboxPreparer(preparer)
                .warmupSkipHealthCheck()
                .sandboxCreator(sandboxCreator)
                .idleTimeout(Duration.ofMinutes(10))
                .build()

        assertEquals(Duration.ofSeconds(10), config.acquireReadyTimeout)
        assertEquals(Duration.ofMillis(250), config.acquireHealthCheckPollingInterval)
        assertSame(healthCheck, config.acquireHealthCheck)
        assertEquals(true, config.acquireSkipHealthCheck)
        assertEquals(Duration.ofSeconds(90), config.acquireMinRemainingTtl)
        assertEquals(Duration.ofSeconds(45), config.warmupReadyTimeout)
        assertEquals(Duration.ofSeconds(1), config.warmupHealthCheckPollingInterval)
        assertSame(healthCheck, config.warmupHealthCheck)
        assertSame(preparer, config.warmupSandboxPreparer)
        assertEquals(true, config.warmupSkipHealthCheck)
        assertSame(sandboxCreator, config.sandboxCreator)
        assertEquals(Duration.ofMinutes(10), config.idleTimeout)
    }

    @Test
    fun `build rejects negative acquireMinRemainingTtl`() {
        val builder =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .acquireMinRemainingTtl(Duration.ofSeconds(-1))

        assertThrows(IllegalArgumentException::class.java) { builder.build() }
    }

    @Test
    fun `default acquireMinRemainingTtl scales down for short idleTimeout`() {
        // idleTimeout = 30s ⇒ default = min(60s, 30s/2) = 15s. Existing users with short idle
        // timeouts must not get a config-time error from a hidden 60s default.
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .idleTimeout(Duration.ofSeconds(30))
                .build()

        assertEquals(Duration.ofSeconds(15), config.acquireMinRemainingTtl)
    }

    @Test
    fun `default acquireMinRemainingTtl caps at 60s for long idleTimeout`() {
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .idleTimeout(Duration.ofMinutes(10))
                .build()

        assertEquals(Duration.ofSeconds(60), config.acquireMinRemainingTtl)
    }

    @Test
    fun `build rejects explicit acquireMinRemainingTtl greater than or equal to idleTimeout`() {
        // The auto-default protects against this, but if a user explicitly sets a value above
        // idleTimeout the build() guard still fires.
        val builder =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .idleTimeout(Duration.ofSeconds(30))
                .acquireMinRemainingTtl(Duration.ofSeconds(30))

        assertThrows(IllegalArgumentException::class.java) { builder.build() }
    }

    @Test
    fun `build accepts acquireMinRemainingTtl just below idleTimeout`() {
        // Strict-less-than boundary: 9s < 10s passes.
        val config =
            PoolConfig.builder()
                .poolName("test-pool")
                .ownerId("test-owner")
                .maxIdle(2)
                .stateStore(InMemoryPoolStateStore())
                .connectionConfig(ConnectionConfig.builder().build())
                .creationSpec(PoolCreationSpec.builder().image("ubuntu:22.04").build())
                .idleTimeout(Duration.ofSeconds(10))
                .acquireMinRemainingTtl(Duration.ofSeconds(9))
                .build()

        assertEquals(Duration.ofSeconds(9), config.acquireMinRemainingTtl)
    }
}
