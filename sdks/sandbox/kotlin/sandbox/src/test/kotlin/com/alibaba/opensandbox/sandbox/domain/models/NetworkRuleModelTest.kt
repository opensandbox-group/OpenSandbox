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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test

class NetworkRuleModelTest {
    @Test
    fun `builder requires either target or ports`() {
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().action(NetworkRule.Action.ALLOW).build()
        }
    }

    @Test
    fun `builder accepts a port-only rule with no target`() {
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.DENY)
                .ports(listOf(25))
                .build()

        assertNull(rule.target)
        assertEquals(listOf(25), rule.ports)
    }

    @Test
    fun `builder accepts a target with ports`() {
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .target("10.0.0.5")
                .ports(listOf(22, 80))
                .build()

        assertEquals("10.0.0.5", rule.target)
        assertEquals(listOf(22, 80), rule.ports)
    }

    @Test
    fun `builder still accepts a plain target with no ports`() {
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .target("example.com")
                .build()

        assertEquals("example.com", rule.target)
        assertNull(rule.ports)
    }

    @Test
    fun `builder rejects an empty ports list`() {
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().ports(emptyList())
        }
    }

    @Test
    fun `builder rejects out-of-range ports`() {
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().ports(listOf(0))
        }
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().ports(listOf(65536))
        }
    }

    @Test
    fun `builder rejects duplicate ports`() {
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().ports(listOf(443, 443))
        }
    }

    @Test
    fun `builder rejects more than 256 ports`() {
        assertThrows(IllegalArgumentException::class.java) {
            NetworkRule.builder().ports((1..257).toList())
        }
    }

    @Test
    fun `builder accepts exactly 256 ports`() {
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .ports((1..256).toList())
                .build()

        assertEquals(256, rule.ports?.size)
    }
}
