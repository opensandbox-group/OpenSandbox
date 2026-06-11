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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuth
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBinding
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMatch
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CustomHeaderEntry
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class EgressAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var egressAdapter: EgressAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpoint =
            SandboxEndpoint(
                endpoint = "$host:$port",
                headers = mapOf("X-Egress-Token" to "route-token"),
            )

        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .headers(mapOf("X-Client-Trace" to "trace-1"))
                .build()

        httpClientProvider = HttpClientProvider(config)
        egressAdapter = EgressAdapter(httpClientProvider, endpoint)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `create sends credential vault payload with endpoint headers and parses sanitized state`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(201)
                .setBody(
                    """
                    {
                      "revision": 1,
                      "credentials": [
                        {"name": "bearer-token", "sourceType": "inline", "revision": 1}
                      ],
                      "bindings": [
                        {
                          "name": "github-api",
                          "revision": 1,
                          "match": {
                            "schemes": ["https"],
                            "ports": [443],
                            "hosts": ["api.github.com"],
                            "methods": ["GET"],
                            "paths": ["/repos/*"]
                          },
                          "auth": {"type": "bearer"}
                        }
                      ]
                    }
                    """.trimIndent(),
                ),
        )

        val match =
            CredentialMatch.builder()
                .schemes(CredentialMatch.Scheme.HTTPS)
                .ports(443)
                .hosts("api.github.com")
                .methods("GET")
                .paths("/repos/*")
                .build()
        val result =
            egressAdapter.create(
                credentials =
                    listOf(
                        credential("bearer-token"),
                        credential("basic-token"),
                        credential("api-key-token"),
                        credential("custom-header-token"),
                    ),
                bindings =
                    listOf(
                        CredentialBinding.builder()
                            .name("github-api")
                            .match(match)
                            .auth(CredentialAuth.bearer("bearer-token"))
                            .build(),
                        CredentialBinding.builder()
                            .name("basic-api")
                            .match(match)
                            .auth(CredentialAuth.basic("basic-token"))
                            .build(),
                        CredentialBinding.builder()
                            .name("api-key-api")
                            .match(match)
                            .auth(CredentialAuth.apiKey("X-Api-Key", "api-key-token"))
                            .build(),
                        CredentialBinding.builder()
                            .name("custom-header-api")
                            .match(match)
                            .auth(
                                CredentialAuth.customHeaders(
                                    listOf(
                                        CustomHeaderEntry.builder()
                                            .name("X-Custom-Token")
                                            .credential("custom-header-token")
                                            .build(),
                                    ),
                                ),
                            )
                            .build(),
                    ),
            )

        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/credential-vault", request.path)
        assertEquals("route-token", request.getHeader("X-Egress-Token"))
        assertEquals("trace-1", request.getHeader("X-Client-Trace"))

        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        val credentials = payload["credentials"]!!.jsonArray
        assertEquals(4, credentials.size)
        val firstCredential = credentials[0].jsonObject
        assertEquals("bearer-token", firstCredential["name"]!!.jsonPrimitive.content)
        assertEquals("inline", firstCredential["source"]!!.jsonObject["type"]!!.jsonPrimitive.content)
        assertEquals("dummy-bearer-token", firstCredential["source"]!!.jsonObject["value"]!!.jsonPrimitive.content)

        val bindings = payload["bindings"]!!.jsonArray
        assertEquals("bearer", bindings[0].jsonObject["auth"]!!.jsonObject["type"]!!.jsonPrimitive.content)
        assertEquals("basic", bindings[1].jsonObject["auth"]!!.jsonObject["type"]!!.jsonPrimitive.content)
        val apiKeyAuth = bindings[2].jsonObject["auth"]!!.jsonObject
        assertEquals("apiKey", apiKeyAuth["type"]!!.jsonPrimitive.content)
        assertEquals("X-Api-Key", apiKeyAuth["name"]!!.jsonPrimitive.content)
        val customHeadersAuth = bindings[3].jsonObject["auth"]!!.jsonObject
        assertEquals("customHeaders", customHeadersAuth["type"]!!.jsonPrimitive.content)
        assertEquals(
            "X-Custom-Token",
            customHeadersAuth["headers"]!!.jsonArray[0].jsonObject["name"]!!.jsonPrimitive.content,
        )

        assertEquals(1, result.revision)
        assertEquals("bearer-token", result.credentials.single().name)
        assertEquals("inline", result.credentials.single().sourceType)
        assertEquals("github-api", result.bindings.single().name)
        assertEquals("bearer", result.bindings.single().auth?.type)
        assertEquals(listOf("api.github.com"), result.bindings.single().match?.hosts)
    }

    @Test
    fun `patch sends expected revision and mutation sets`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(
                    """
                    {
                      "revision": 2,
                      "credentials": [
                        {"name": "new-token", "sourceType": "inline", "revision": 2}
                      ],
                      "bindings": []
                    }
                    """.trimIndent(),
                ),
        )

        val patch =
            CredentialVaultPatchRequest.builder()
                .expectedRevision(1)
                .credentials(
                    CredentialMutationSet.builder()
                        .add(listOf(credential("new-token")))
                        .delete("old-token")
                        .build(),
                )
                .bindings(
                    CredentialBindingMutationSet.builder()
                        .delete("old-binding")
                        .build(),
                )
                .build()

        val result = egressAdapter.patch(patch)

        val request = mockWebServer.takeRequest()
        assertEquals("PATCH", request.method)
        assertEquals("/credential-vault", request.path)
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("1", payload["expectedRevision"]!!.jsonPrimitive.content)
        val credentialMutations = payload["credentials"]!!.jsonObject
        assertEquals("new-token", credentialMutations["add"]!!.jsonArray[0].jsonObject["name"]!!.jsonPrimitive.content)
        assertEquals("old-token", credentialMutations["delete"]!!.jsonArray[0].jsonPrimitive.content)
        val bindingMutations = payload["bindings"]!!.jsonObject
        assertEquals("old-binding", bindingMutations["delete"]!!.jsonArray[0].jsonPrimitive.content)
        assertEquals(2, result.revision)
    }

    @Test
    fun `get list and delete use credential vault endpoints`() {
        mockWebServer.enqueue(MockResponse().setBody(vaultStateResponse()))
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "revision": 3,
                      "credentials": [
                        {"name": "token-one", "sourceType": "inline", "revision": 3}
                      ]
                    }
                    """.trimIndent(),
                ),
        )
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"name": "token/with space", "sourceType": "inline", "revision": 3}"""),
        )
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "revision": 3,
                      "bindings": [
                        {"name": "binding-one", "revision": 3, "auth": {"type": "apiKey", "name": "X-Api-Key"}}
                      ]
                    }
                    """.trimIndent(),
                ),
        )
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"name": "binding/with space", "revision": 3, "auth": {"type": "basic"}}"""),
        )
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        val state = egressAdapter.get()
        val credentials = egressAdapter.listCredentials()
        val credential = egressAdapter.getCredential("token/with space")
        val bindings = egressAdapter.listBindings()
        val binding = egressAdapter.getBinding("binding/with space")
        egressAdapter.delete()

        assertEquals(3, state.revision)
        assertEquals("token-one", credentials.single().name)
        assertEquals("token/with space", credential.name)
        assertEquals("binding-one", bindings.single().name)
        assertEquals("apiKey", bindings.single().auth?.type)
        assertEquals("X-Api-Key", bindings.single().auth?.name)
        assertEquals("binding/with space", binding.name)

        val getStateRequest = mockWebServer.takeRequest()
        assertEquals("GET", getStateRequest.method)
        assertEquals("/credential-vault", getStateRequest.path)
        assertEquals("/credential-vault/credentials", mockWebServer.takeRequest().path)
        assertEquals("/credential-vault/credentials/token%2Fwith%20space", mockWebServer.takeRequest().path)
        assertEquals("/credential-vault/bindings", mockWebServer.takeRequest().path)
        assertEquals("/credential-vault/bindings/binding%2Fwith%20space", mockWebServer.takeRequest().path)
        val deleteRequest = mockWebServer.takeRequest()
        assertEquals("DELETE", deleteRequest.method)
        assertEquals("/credential-vault", deleteRequest.path)
    }

    @Test
    fun `get credential maps error response`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(404)
                .setHeader("X-Request-ID", "req-404")
                .setBody("""{"code":"VAULT_NOT_FOUND","message":"missing"}"""),
        )

        val exception =
            assertThrows(SandboxApiException::class.java) {
                egressAdapter.getCredential("missing")
            }

        assertEquals(404, exception.statusCode)
        assertEquals("VAULT_NOT_FOUND", exception.error.code)
        assertEquals("req-404", exception.requestId)
    }

    @Test
    fun `credential vault state does not expose credential values`() {
        mockWebServer.enqueue(MockResponse().setBody(vaultStateResponse()))

        val state = egressAdapter.get()

        assertEquals(1, state.credentials.size)
        assertFalse(state.credentials.single().javaClass.declaredFields.any { it.name == "value" })
        assertTrue(state.bindings.single().auth?.name == null)
    }

    private fun credential(name: String): Credential =
        Credential.builder()
            .name(name)
            .inlineSource("dummy-$name")
            .build()

    private fun vaultStateResponse(): String =
        """
        {
          "revision": 3,
          "credentials": [
            {"name": "token-one", "sourceType": "inline", "revision": 3}
          ],
          "bindings": [
            {"name": "binding-one", "revision": 3, "auth": {"type": "bearer"}}
          ]
        }
        """.trimIndent()
}
