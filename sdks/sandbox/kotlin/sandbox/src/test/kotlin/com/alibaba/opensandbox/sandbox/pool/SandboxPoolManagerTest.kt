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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyIncompleteException
import com.alibaba.opensandbox.sandbox.domain.exceptions.PoolDestroyedException
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyOptions
import com.alibaba.opensandbox.sandbox.domain.pool.PoolDestroyState
import com.alibaba.opensandbox.sandbox.domain.pool.PoolStateStore
import com.alibaba.opensandbox.sandbox.infrastructure.pool.InMemoryPoolStateStore
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import java.time.Duration

class SandboxPoolManagerTest {
    @Test
    fun `destroy drains idle state and writes destroyed tombstone`() {
        val store = InMemoryPoolStateStore()
        val manager =
            SandboxPoolManager.builder()
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .ownerId("manager-1")
                .build()

        val result =
            manager.destroy(
                "old-pool",
                PoolDestroyOptions(
                    tombstoneTtl = Duration.ofMinutes(5),
                ),
            )

        assertEquals(PoolDestroyState.DESTROYED, result.state)
        assertEquals(0, result.drainedIdleCount)
        assertEquals(0, result.killedIdleCount)
        assertEquals(true, result.persistentStateCleared)
        assertEquals(PoolDestroyState.DESTROYED, store.getDestroyState("old-pool"))
        assertEquals(0, store.snapshotCounters("old-pool").idleCount)
        assertThrows(PoolDestroyedException::class.java) {
            store.putIdle("old-pool", "id-3")
        }
    }

    @Test
    fun `destroy keeps namespace destroying when clear state fails`() {
        val delegate = InMemoryPoolStateStore()
        val store = ClearFailsStore(delegate)
        val manager =
            SandboxPoolManager.builder()
                .stateStore(store)
                .connectionConfig(ConnectionConfig.builder().build())
                .ownerId("manager-1")
                .build()

        assertThrows(PoolDestroyIncompleteException::class.java) {
            manager.destroy(
                "old-pool",
                PoolDestroyOptions(
                    tombstoneTtl = Duration.ofMinutes(5),
                ),
            )
        }

        assertEquals(PoolDestroyState.DESTROYING, delegate.getDestroyState("old-pool"))
    }

    private class ClearFailsStore(
        private val delegate: InMemoryPoolStateStore,
    ) : PoolStateStore by delegate {
        override fun clearPoolState(poolName: String) {
            throw IllegalStateException("clear failed")
        }
    }
}
