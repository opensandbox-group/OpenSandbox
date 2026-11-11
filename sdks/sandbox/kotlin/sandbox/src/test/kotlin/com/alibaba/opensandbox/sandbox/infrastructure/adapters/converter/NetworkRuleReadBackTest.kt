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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainEgressNetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainNetworkRule
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Test
import com.alibaba.opensandbox.sandbox.api.models.NetworkRule as ApiNetworkRule
import com.alibaba.opensandbox.sandbox.api.models.egress.NetworkRule as ApiEgressNetworkRule

/**
 * Read-back shapes the sidecar actually produces (Go marshals target without
 * omitempty), and the wire shapes the lenient path must tolerate: read-back
 * normalization runs before the lenient factory, so contract drift on the
 * server can never turn a policy read into an uncaught builder error.
 */
class NetworkRuleReadBackTest {
    @Test
    fun `port-only rule reads back with blank target and ports`() {
        val rule = ApiNetworkRule(action = ApiNetworkRule.Action.deny, target = "", ports = setOf(25)).toDomainNetworkRule()

        assertEquals(NetworkRule.Action.DENY, rule.action)
        assertNull(rule.target)
        assertEquals(listOf(25), rule.ports)
    }

    @Test
    fun `empty ports list reads back as not port scoped`() {
        val rule = ApiNetworkRule(action = ApiNetworkRule.Action.allow, target = "example.com", ports = emptySet()).toDomainNetworkRule()

        assertEquals("example.com", rule.target)
        assertNull(rule.ports)
    }

    @Test
    fun `egress read-back shares the lifecycle normalization`() {
        val lifecycle = ApiNetworkRule(action = ApiNetworkRule.Action.deny, target = "", ports = setOf(25)).toDomainNetworkRule()
        val egress =
            ApiEgressNetworkRule(
                action = ApiEgressNetworkRule.Action.deny,
                target = "",
                ports = setOf(25),
            ).toDomainEgressNetworkRule()

        assertEquals(lifecycle.action, egress.action)
        assertNull(egress.target)
        assertEquals(lifecycle.ports, egress.ports)
    }
}
