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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
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
    fun `credential vault create routes through the lifecycle control plane`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                        "revision": 1,
                        "credentials": [{ "name": "tok", "sourceType": "inline", "revision": 1 }],
                        "bindings": []
                    }
                    """.trimIndent(),
                ).setResponseCode(201),
        )

        val state =
            networkPolicyAdapter.create(
                CredentialVaultCreateRequest
                    .builder()
                    .credentials(listOf(Credential.builder().name("tok").inlineSource("sekrit").build()))
                    .bindings(emptyList())
                    .build(),
            )

        val recorded = mockWebServer.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("/v1/sandboxes/fsb-001/credential-vault", recorded.path)
        assertEquals(1, state.revision)
        val payload = Json.parseToJsonElement(recorded.body.readUtf8()).jsonObject
        assertEquals("sekrit", payload["credentials"]!!.jsonArray[0].jsonObject["source"]!!.jsonObject["value"]!!.jsonPrimitive.content)
    }

    @Test
    fun `credential vault subresource reads and delete hit the encoded lifecycle paths`() {
        // credential metadata read with a name that needs percent-encoding
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"name":"tok/one","sourceType":"inline","revision":2}""")
                .setResponseCode(200),
        )
        val credential = networkPolicyAdapter.getCredential("tok/one")
        val credentialRequest = mockWebServer.takeRequest()
        assertEquals("GET", credentialRequest.method)
        assertEquals(
            "/v1/sandboxes/fsb-001/credential-vault/credentials/tok%2Fone",
            credentialRequest.path,
        )
        assertEquals("tok/one", credential.name)

        // credential list
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"revision":2,"credentials":[]}""")
                .setResponseCode(200),
        )
        val credentials = networkPolicyAdapter.listCredentials()
        val listRequest = mockWebServer.takeRequest()
        assertEquals("/v1/sandboxes/fsb-001/credential-vault/credentials", listRequest.path)
        assertEquals(0, credentials.size)

        // binding metadata read with a space in the name
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"name":"b two","revision":2}""")
                .setResponseCode(200),
        )
        val binding = networkPolicyAdapter.getBinding("b two")
        val bindingRequest = mockWebServer.takeRequest()
        assertEquals(
            "/v1/sandboxes/fsb-001/credential-vault/bindings/b%20two",
            bindingRequest.path,
        )
        assertEquals("b two", binding.name)

        // binding list
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"revision":2,"bindings":[]}""")
                .setResponseCode(200),
        )
        networkPolicyAdapter.listBindings()
        assertEquals(
            "/v1/sandboxes/fsb-001/credential-vault/bindings",
            mockWebServer.takeRequest().path,
        )

        // delete: 204 with no body
        mockWebServer.enqueue(MockResponse().setResponseCode(204))
        networkPolicyAdapter.delete()
        val deleteRequest = mockWebServer.takeRequest()
        assertEquals("DELETE", deleteRequest.method)
        assertEquals("/v1/sandboxes/fsb-001/credential-vault", deleteRequest.path)
    }

    @Test
    fun `credential vault get parses state and patch surfaces 412`() {
        val stateBody =
            """
            {
                "revision": 2,
                "credentials": [{ "name": "tok", "sourceType": "inline", "revision": 2 }],
                "bindings": [
                    {
                        "name": "b",
                        "revision": 2,
                        "match": { "hosts": ["api.test"] },
                        "auth": { "type": "apiKey", "name": "X-Api-Key" }
                    }
                ]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(stateBody).setResponseCode(200))
        val state = networkPolicyAdapter.get()

        val recorded = mockWebServer.takeRequest()
        assertEquals("GET", recorded.method)
        assertEquals("/v1/sandboxes/fsb-001/credential-vault", recorded.path)
        assertEquals(2, state.revision)
        assertEquals("apiKey", state.bindings.single().auth?.type)
        assertEquals("X-Api-Key", state.bindings.single().auth?.name)

        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"code": "FSB::API_ERROR", "message": "credential vault revision conflict"}""")
                .setResponseCode(412),
        )
        val exception =
            assertThrows(SandboxApiException::class.java) {
                networkPolicyAdapter.patch(CredentialVaultPatchRequest.builder().expectedRevision(1).build())
            }
        // The exception message carries the operation and status only; the raw
        // server body stays on responseBody for programmatic use.
        assertEquals(412, exception.statusCode)
        assertTrue(exception.responseBody!!.contains("revision conflict"))

        val patchRecorded = mockWebServer.takeRequest()
        assertEquals("PATCH", patchRecorded.method)
        assertEquals("/v1/sandboxes/fsb-001/credential-vault", patchRecorded.path)
        assertTrue(patchRecorded.body.readUtf8().contains("\"expectedRevision\":1"))
    }
}
