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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class NetworkPolicyAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var networkPolicyAdapter: NetworkPolicyAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()

        httpClientProvider = HttpClientProvider(config)
        networkPolicyAdapter = NetworkPolicyAdapter(httpClientProvider, "fsb-001")
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `getPolicy should read the policy payload from the lifecycle control plane`() {
        // Mock response
        val responseBody =
            """
            {
                "status": "ok",
                "mode": "deny_all",
                "policy": {
                    "defaultAction": "deny",
                    "egress": [{ "action": "allow", "target": "pypi.org" }]
                }
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        // Execute
        val policy = networkPolicyAdapter.getPolicy()

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("GET", recorded.method)
        assertEquals("/v1/sandboxes/fsb-001/networkpolicy", recorded.path)

        // Verify response
        assertEquals(NetworkPolicy.DefaultAction.DENY, policy.defaultAction)
        assertEquals(1, policy.egress!!.size)
        assertEquals(NetworkRule.Action.ALLOW, policy.egress!![0].action)
        assertEquals("pypi.org", policy.egress!![0].target)
    }

    @Test
    fun `patchRules should send the rule array to the lifecycle control plane`() {
        // Mock response
        mockWebServer.enqueue(
            MockResponse().setBody("""{"status": "ok", "mode": "deny_all"}""").setResponseCode(200),
        )

        // Execute
        val rules =
            listOf(
                NetworkRule.builder().action(NetworkRule.Action.ALLOW).target("pypi.org").build(),
                NetworkRule.builder().action(NetworkRule.Action.DENY).target("evil.example.com").build(),
            )
        networkPolicyAdapter.patchRules(rules)

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("PATCH", recorded.method)
        assertEquals("/v1/sandboxes/fsb-001/networkpolicy", recorded.path)
        val payload = Json.parseToJsonElement(recorded.body.readUtf8()).jsonArray
        assertEquals(2, payload.size)
        assertEquals("allow", payload[0].jsonObject["action"]!!.jsonPrimitive.content)
        assertEquals("pypi.org", payload[0].jsonObject["target"]!!.jsonPrimitive.content)
        assertEquals("deny", payload[1].jsonObject["action"]!!.jsonPrimitive.content)
    }

    @Test
    fun `deleteRules should send the target list to the lifecycle control plane`() {
        // Mock response
        mockWebServer.enqueue(
            MockResponse().setBody("""{"status": "ok", "mode": "deny_all"}""").setResponseCode(200),
        )

        // Execute
        networkPolicyAdapter.deleteRules(listOf("pypi.org", "evil.example.com"))

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("DELETE", recorded.method)
        assertEquals("/v1/sandboxes/fsb-001/networkpolicy", recorded.path)
        val payload = Json.parseToJsonElement(recorded.body.readUtf8()).jsonArray
        assertEquals("pypi.org", payload[0].jsonPrimitive.content)
        assertEquals("evil.example.com", payload[1].jsonPrimitive.content)
    }

    @Test
    fun `credential vault operations should be rejected for template-backed sandboxes`() {
        val exception =
            assertThrows(UnsupportedOperationException::class.java) {
                networkPolicyAdapter.create(CredentialVaultCreateRequest.builder().build())
            }
        assertTrue(
            exception.message!!.contains("requires the sandbox-side egress sidecar"),
            "unexpected message: ${exception.message}",
        )
        assertThrows(UnsupportedOperationException::class.java) { networkPolicyAdapter.get() }
        assertThrows(UnsupportedOperationException::class.java) { networkPolicyAdapter.delete() }
        assertThrows(UnsupportedOperationException::class.java) { networkPolicyAdapter.listCredentials() }
        assertThrows(UnsupportedOperationException::class.java) { networkPolicyAdapter.listBindings() }
    }
}
