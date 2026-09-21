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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CreateTemplateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSnapshotInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxCreateResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateFormat
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplatePhase
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.net.URLDecoder
import java.time.Duration
import java.time.OffsetDateTime

class TemplatesAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var sandboxesAdapter: SandboxesAdapter
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
        sandboxesAdapter = SandboxesAdapter(httpClientProvider)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `createTemplate should send correct request and parse response`() {
        // Mock response
        val responseBody =
            """
            {
                "templateId": "tpl_new",
                "image": "ubuntu:22.04",
                "publish": "s3://bucket/publish",
                "format": "overlaybd",
                "status": { "phase": "Pending" },
                "resourceLimits": { "cpu": "2", "memory": "512Mi", "disk": "10Gi" },
                "readiness": { "probe": "tcp://127.0.0.1:44772", "warmupSeconds": 30 },
                "createdAt": "2026-09-17T10:00:00Z",
                "updatedAt": "2026-09-17T10:00:00Z"
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        // Execute
        val request =
            CreateTemplateRequest
                .builder()
                .image("ubuntu:22.04")
                .publish("s3://bucket/publish")
                .resourceLimits(mapOf("cpu" to "2", "memory" to "512Mi", "disk" to "10Gi"))
                .entrypoint("tail", "-f", "/dev/null")
                .metadata(mapOf("team" to "backend"))
                .readiness {
                    probe("tcp://127.0.0.1:44772")
                    warmupSeconds(30)
                }
                .build()
        val result = sandboxesAdapter.createTemplate(request)

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("/v1/templates", recorded.path)
        val payload = Json.parseToJsonElement(recorded.body.readUtf8()).jsonObject
        assertEquals("ubuntu:22.04", payload["image"]!!.jsonPrimitive.content)
        assertEquals("s3://bucket/publish", payload["publish"]!!.jsonPrimitive.content)
        assertEquals("2", payload["resourceLimits"]!!.jsonObject["cpu"]!!.jsonPrimitive.content)
        assertEquals("tail", payload["entrypoint"]!!.jsonArray[0].jsonPrimitive.content)
        assertEquals("backend", payload["metadata"]!!.jsonObject["team"]!!.jsonPrimitive.content)
        val readiness = payload["readiness"]!!.jsonObject
        assertEquals("tcp://127.0.0.1:44772", readiness["probe"]!!.jsonPrimitive.content)
        assertEquals(30, readiness["warmupSeconds"]!!.jsonPrimitive.int)
        // The generated serializer always encodes format; an unset domain format is
        // pinned to the spec default instead of an explicit null.
        assertEquals("overlaybd", payload["format"]!!.jsonPrimitive.content)

        // Verify response
        assertEquals("tpl_new", result.templateId)
        assertEquals(TemplateFormat.OVERLAYBD, result.format)
        assertEquals(TemplatePhase.PENDING, result.status.phase)
        assertNull(result.status.manifestRef)
        assertEquals(mapOf("cpu" to "2", "memory" to "512Mi", "disk" to "10Gi"), result.resourceLimits)
        assertEquals(30, result.readiness?.warmupSeconds)
    }

    @Test
    fun `getTemplate should convert succeeded status`() {
        // Mock response
        val responseBody =
            """
            {
                "templateId": "tpl_1",
                "image": "alpine:3.19",
                "publish": "s3://bucket/publish",
                "format": "native",
                "status": {
                    "phase": "Succeeded",
                    "manifestRef": "s3://bucket/publish/tpl_1"
                },
                "metadata": { "team": "backend" },
                "createdAt": "2026-09-17T10:00:00Z",
                "updatedAt": "2026-09-17T10:05:00Z"
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        // Execute
        val result = sandboxesAdapter.getTemplate("tpl_1")

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("GET", recorded.method)
        assertEquals("/v1/templates/tpl_1", recorded.path)

        // Verify response
        assertEquals("tpl_1", result.templateId)
        assertEquals(TemplateFormat.NATIVE, result.format)
        assertEquals(TemplatePhase.SUCCEEDED, result.status.phase)
        assertEquals("s3://bucket/publish/tpl_1", result.status.manifestRef)
        assertEquals(OffsetDateTime.parse("2026-09-17T10:00:00Z"), result.createdAt)
    }

    @Test
    fun `listTemplates should join metadata filters and convert pagination`() {
        // Mock response
        val responseBody =
            """
            {
                "items": [
                    {
                        "templateId": "tpl_1",
                        "image": "alpine:3.19",
                        "publish": "s3://bucket/publish",
                        "format": "overlaybd",
                        "status": { "phase": "Succeeded" },
                        "createdAt": "2026-09-17T10:00:00Z",
                        "updatedAt": "2026-09-17T10:00:00Z"
                    },
                    {
                        "templateId": "tpl_2",
                        "image": "alpine:3.19",
                        "publish": "s3://bucket/publish",
                        "format": "overlaybd",
                        "status": { "phase": "Pending" },
                        "createdAt": "2026-09-17T10:00:00Z",
                        "updatedAt": "2026-09-17T10:00:00Z"
                    }
                ],
                "pagination": { "page": 1, "pageSize": 20, "totalItems": 2, "totalPages": 1, "hasNextPage": false }
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        // Execute
        val filter =
            TemplateFilter
                .builder()
                .metadata(mapOf("env" to "prod"))
                .page(1)
                .pageSize(20)
                .build()
        val result = sandboxesAdapter.listTemplates(filter)

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("GET", recorded.method)
        assertEquals("env=prod", recorded.requestUrl!!.queryParameter("metadata"))
        assertEquals("1", recorded.requestUrl!!.queryParameter("page"))
        assertEquals("20", recorded.requestUrl!!.queryParameter("pageSize"))

        // Verify response
        assertEquals(2, result.templateInfos.size)
        assertEquals("tpl_1", result.templateInfos[0].templateId)
        assertEquals(TemplatePhase.PENDING, result.templateInfos[1].status.phase)
        assertEquals(2, result.pagination.totalItems)
    }

    @Test
    fun `listTemplates should percent-encode metadata filters once`() {
        // Mock response
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"items":[],"pagination":{"page":1,"pageSize":20,"totalItems":0,"totalPages":0,"hasNextPage":false}}""")
                .setResponseCode(200),
        )

        // Execute
        val filter =
            TemplateFilter
                .builder()
                .metadata(mapOf("a" to "x&y=b", "p" to "50%"))
                .build()
        sandboxesAdapter.listTemplates(filter)

        // Verify request: queryParameter decodes the transport layer, modelling the
        // server decode order; the remaining single layer must round-trip losslessly.
        val recorded = mockWebServer.takeRequest()
        val metadataParam = recorded.requestUrl!!.queryParameter("metadata")
        val decoded =
            metadataParam!!.split("&").associate { pair ->
                val (key, value) = pair.split("=", limit = 2)
                URLDecoder.decode(key, "UTF-8") to URLDecoder.decode(value, "UTF-8")
            }
        assertEquals(mapOf("a" to "x&y=b", "p" to "50%"), decoded)
    }

    @Test
    fun `listTemplates should omit unset filters`() {
        // Mock response
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"items":[],"pagination":{"page":1,"pageSize":20,"totalItems":0,"totalPages":0,"hasNextPage":false}}""")
                .setResponseCode(200),
        )

        // Execute
        sandboxesAdapter.listTemplates(TemplateFilter.builder().build())

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertNull(recorded.requestUrl!!.queryParameter("metadata"))
        assertNull(recorded.requestUrl!!.queryParameter("page"))
        assertNull(recorded.requestUrl!!.queryParameter("pageSize"))
    }

    @Test
    fun `deleteTemplate should call the delete endpoint`() {
        // Mock response
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        // Execute
        sandboxesAdapter.deleteTemplate("tpl_1")

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("DELETE", recorded.method)
        assertEquals("/v1/templates/tpl_1", recorded.path)
    }

    @Test
    fun `createTemplate should map conflict to sandbox api exception`() {
        // Mock response
        mockWebServer.enqueue(
            MockResponse()
                .setBody("""{"code": "TEMPLATE_ALREADY_EXISTS", "message": "template already exists"}""")
                .setResponseCode(409),
        )

        // Execute
        val request =
            CreateTemplateRequest
                .builder()
                .image("ubuntu:22.04")
                .publish("s3://bucket/publish")
                .build()
        val exception =
            assertThrows(SandboxApiException::class.java) {
                sandboxesAdapter.createTemplate(request)
            }

        // Verify response
        assertEquals(409, exception.statusCode)
        assertEquals("TEMPLATE_ALREADY_EXISTS", exception.error.code)
    }

    @Test
    fun `createSandboxFromTemplate should send template mode wire body only`() {
        // Mock response
        val responseBody =
            """
            {
                "id": "fsb-001",
                "status": { "state": "Pending" },
                "createdAt": "2026-09-17T10:00:00Z",
                "entrypoint": ["tail", "-f", "/dev/null"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        // Execute
        val result =
            sandboxesAdapter.createSandboxFromTemplate(
                templateId = "tpl_1",
                timeout = Duration.ofSeconds(300),
                metadata = mapOf("team" to "backend"),
                networkPolicy =
                    NetworkPolicy
                        .builder()
                        .defaultAction(NetworkPolicy.DefaultAction.DENY)
                        .addEgress(
                            NetworkRule
                                .builder()
                                .action(NetworkRule.Action.ALLOW)
                                .target("pypi.org")
                                .build(),
                        )
                        .build(),
                extensions = mapOf("storage.id" to "abc123"),
            )

        // Verify request
        val recorded = mockWebServer.takeRequest()
        assertEquals("POST", recorded.method)
        assertEquals("/v1/sandboxes", recorded.path)
        val payload = Json.parseToJsonElement(recorded.body.readUtf8()).jsonObject
        assertEquals("tpl_1", payload["templateId"]!!.jsonPrimitive.content)
        assertEquals(300, payload["timeout"]!!.jsonPrimitive.int)
        assertEquals("backend", payload["metadata"]!!.jsonObject["team"]!!.jsonPrimitive.content)
        assertEquals("abc123", payload["extensions"]!!.jsonObject["storage.id"]!!.jsonPrimitive.content)
        val networkPolicy = payload["networkPolicy"]!!.jsonObject
        assertEquals("deny", networkPolicy["defaultAction"]!!.jsonPrimitive.content)
        assertEquals("pypi.org", networkPolicy["egress"]!!.jsonArray[0].jsonObject["target"]!!.jsonPrimitive.content)
        // Workload-shaping fields must stay unset: the server rejects them in template mode.
        val forbiddenFields =
            listOf(
                "image",
                "snapshotId",
                "entrypoint",
                "env",
                "resourceLimits",
                "resourceRequests",
                "volumes",
                "platform",
                "credentialProxy",
                "lifecycle",
            )
        for (forbidden in forbiddenFields) {
            val value = payload[forbidden]
            assertTrue(
                value == null || value.toString() == "null",
                "$forbidden must not be set in template mode, got: $value",
            )
        }

        // Verify response
        assertEquals("fsb-001", result.id)
    }

    @Test
    fun `default interface template operations should be unsupported`() {
        // A bare Sandboxes implementation that does not override the template
        // operations must keep compiling: the interface defaults throw instead.
        val bare = BareSandboxes()

        val createFromTemplate =
            assertThrows(UnsupportedOperationException::class.java) {
                bare.createSandboxFromTemplate("tpl_1", Duration.ofSeconds(300))
            }
        assertTrue(createFromTemplate.message!!.contains("Template-based sandbox creation"))
        assertTrue(
            assertThrows(UnsupportedOperationException::class.java) { bare.createTemplate(minimalRequest()) }
                .message!!.contains("Template management"),
        )
        assertTrue(
            assertThrows(UnsupportedOperationException::class.java) { bare.getTemplate("tpl_1") }
                .message!!.contains("Template management"),
        )
        assertTrue(
            assertThrows(UnsupportedOperationException::class.java) { bare.listTemplates(TemplateFilter.builder().build()) }
                .message!!.contains("Template management"),
        )
        assertTrue(
            assertThrows(UnsupportedOperationException::class.java) { bare.deleteTemplate("tpl_1") }
                .message!!.contains("Template management"),
        )
    }

    private fun minimalRequest(): CreateTemplateRequest =
        CreateTemplateRequest
            .builder()
            .image("ubuntu:22.04")
            .publish("s3://bucket/publish")
            .build()

    private class BareSandboxes : Sandboxes {
        override fun createSandbox(
            spec: SandboxImageSpec?,
            entrypoint: List<String>?,
            env: Map<String, String>,
            metadata: Map<String, String>,
            timeout: Duration?,
            resource: Map<String, String>,
            networkPolicy: NetworkPolicy?,
            extensions: Map<String, String>,
            volumes: List<Volume>?,
            platform: PlatformSpec?,
            secureAccess: Boolean,
            snapshotId: String?,
            resourceRequests: Map<String, String>?,
        ): SandboxCreateResponse = throw UnsupportedOperationException()

        override fun getSandboxInfo(sandboxId: String): SandboxInfo = throw UnsupportedOperationException()

        override fun listSandboxes(filter: SandboxFilter): PagedSandboxInfos = throw UnsupportedOperationException()

        override fun patchSandboxMetadata(
            sandboxId: String,
            patch: Map<String, String?>,
        ): SandboxInfo = throw UnsupportedOperationException()

        override fun createSnapshot(
            sandboxId: String,
            name: String?,
        ): SnapshotInfo = throw UnsupportedOperationException()

        override fun getSnapshot(snapshotId: String): SnapshotInfo = throw UnsupportedOperationException()

        override fun listSnapshots(filter: SnapshotFilter): PagedSnapshotInfos = throw UnsupportedOperationException()

        override fun deleteSnapshot(snapshotId: String) = throw UnsupportedOperationException()

        override fun getSandboxEndpoint(
            sandboxId: String,
            port: Int,
        ): SandboxEndpoint = throw UnsupportedOperationException()

        override fun getSandboxEndpoint(
            sandboxId: String,
            port: Int,
            useServerProxy: Boolean,
        ): SandboxEndpoint = throw UnsupportedOperationException()

        override fun getSignedSandboxEndpoint(
            sandboxId: String,
            port: Int,
            expires: Long,
            useServerProxy: Boolean,
        ): SandboxEndpoint = throw UnsupportedOperationException()

        override fun pauseSandbox(sandboxId: String) = throw UnsupportedOperationException()

        override fun resumeSandbox(sandboxId: String) = throw UnsupportedOperationException()

        override fun renewSandboxExpiration(
            sandboxId: String,
            newExpirationTime: OffsetDateTime,
        ): SandboxRenewResponse = throw UnsupportedOperationException()

        override fun killSandbox(sandboxId: String) = throw UnsupportedOperationException()
    }
}
