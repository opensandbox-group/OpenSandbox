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
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertSame
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
        assertEquals(Duration.ofHours(24), config.idleTimeout)
    }

    @Test
    fun `build keeps configured warmup readiness settings`() {
        val healthCheck: (Sandbox) -> Boolean = { true }
        val preparer = SandboxPreparer {}
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
                .warmupReadyTimeout(Duration.ofSeconds(45))
                .warmupHealthCheckPollingInterval(Duration.ofSeconds(1))
                .warmupHealthCheck(healthCheck)
                .warmupSandboxPreparer(preparer)
                .warmupSkipHealthCheck()
                .idleTimeout(Duration.ofMinutes(10))
                .build()

        assertEquals(Duration.ofSeconds(10), config.acquireReadyTimeout)
        assertEquals(Duration.ofMillis(250), config.acquireHealthCheckPollingInterval)
        assertSame(healthCheck, config.acquireHealthCheck)
        assertEquals(true, config.acquireSkipHealthCheck)
        assertEquals(Duration.ofSeconds(45), config.warmupReadyTimeout)
        assertEquals(Duration.ofSeconds(1), config.warmupHealthCheckPollingInterval)
        assertSame(healthCheck, config.warmupHealthCheck)
        assertSame(preparer, config.warmupSandboxPreparer)
        assertEquals(true, config.warmupSkipHealthCheck)
        assertEquals(Duration.ofMinutes(10), config.idleTimeout)
    }
}
