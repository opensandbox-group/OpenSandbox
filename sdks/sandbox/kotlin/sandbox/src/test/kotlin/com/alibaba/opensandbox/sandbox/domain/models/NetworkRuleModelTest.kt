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

package com.alibaba.opensandbox.sandbox.domain.models

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainNetworkRule
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Test
import com.alibaba.opensandbox.sandbox.api.models.NetworkRule as ApiNetworkRule

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

    @Test
    fun `built rule ports are a defensive copy`() {
        val ports = mutableListOf(22, 80)
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .ports(ports)
                .build()

        ports.add(25)
        assertEquals(listOf(22, 80), rule.ports)
    }

    @Test
    fun `mutating the list between ports and build does not change the rule`() {
        // ports() snapshots a validated copy at set time, so caller-side
        // mutation afterwards can neither smuggle unvalidated values into
        // nor alter the built rule.
        val ports = mutableListOf(22)
        val builder =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .ports(ports)

        ports.clear()
        ports.addAll(listOf(1, 1, 70000))

        assertEquals(listOf(22), builder.build().ports)
    }

    @Test
    fun `builder rejects domain targets combined with ports`() {
        // Leading zeros, unbounded prefixes, and malformed IPv6 literals are
        // all classified as domains (or invalid), matching the server.
        for (target in listOf(
            "example.com",
            "*.example.com",
            "10.0.0.0/255.0.0.0",
            "fe80::1%eth0/64",
            "192.168.001.1",
            "10.0.0.0/33",
            "2001:db8::/129",
            "1:2",
        )) {
            assertThrows(IllegalArgumentException::class.java) {
                NetworkRule
                    .builder()
                    .action(NetworkRule.Action.ALLOW)
                    .target(target)
                    .ports(listOf(443))
                    .build()
            }
        }
    }

    @Test
    fun `builder trims the target before classification`() {
        val rule =
            NetworkRule
                .builder()
                .action(NetworkRule.Action.ALLOW)
                .target(" 10.0.0.5 ")
                .ports(listOf(22))
                .build()

        assertEquals(" 10.0.0.5 ", rule.target)
    }

    @Test
    fun `builder accepts ip and cidr targets combined with ports`() {
        for (target in listOf(
            "10.0.0.5",
            "10.0.0.0/8",
            "0.0.0.0/0",
            "2001:db8::1",
            "::",
            "::ffff:192.168.0.1",
            "fe80::1%eth0",
        )) {
            val rule =
                NetworkRule
                    .builder()
                    .action(NetworkRule.Action.ALLOW)
                    .target(target)
                    .ports(listOf(443))
                    .build()
            assertEquals(target, rule.target)
        }
    }

    @Test
    fun `blank targets fail fast`() {
        for (target in listOf("", "   ")) {
            assertThrows(IllegalArgumentException::class.java) {
                NetworkRule
                    .builder()
                    .action(NetworkRule.Action.ALLOW)
                    .target(target)
                    .build()
            }
        }
    }

    @Test
    fun `read-back tolerates the sidecar wire shape for port-only rules`() {
        // The sidecar marshals target without omitempty, so a port-only rule
        // comes back as {"target": "", "ports": [25]}; parsing must not throw.
        val apiRule =
            ApiNetworkRule(
                action = ApiNetworkRule.Action.deny,
                target = "",
                ports = setOf(25),
            )
        val rule = apiRule.toDomainNetworkRule()

        assertEquals(NetworkRule.Action.DENY, rule.action)
        assertNull(rule.target)
        assertEquals(listOf(25), rule.ports)
    }
}
