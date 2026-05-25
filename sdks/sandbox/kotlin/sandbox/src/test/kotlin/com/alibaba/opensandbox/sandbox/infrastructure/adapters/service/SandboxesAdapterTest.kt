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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.OSSFS
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.time.Duration

class SandboxesAdapterTest {
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
    fun `createSandbox should send correct request and parse response`() {
        // Mock response
        val responseBody =
            """
            {
                "id": "550e8400-e29b-41d4-a716-446655440000",
                "status": { "state": "Running" },
                "platform": { "os": "linux", "arch": "amd64" },
                "expiresAt": "2023-01-01T11:00:00Z",
                "createdAt": "2023-01-01T10:00:00Z",
                "entrypoint": ["bash"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        // Execute
        val spec = SandboxImageSpec.builder().image("ubuntu:latest").build()
        val extensions = mapOf("storage.id" to "abc123", "debug" to "true")
        val networkPolicy =
            NetworkPolicy.builder()
                .defaultAction(NetworkPolicy.DefaultAction.DENY)
                .addEgress(
                    NetworkRule.builder()
                        .action(NetworkRule.Action.ALLOW)
                        .target("pypi.org")
                        .build(),
                )
                .build()
        val result =
            sandboxesAdapter.createSandbox(
                spec = spec,
                entrypoint = listOf("bash"),
                env = mapOf("KEY" to "VALUE"),
                metadata = mapOf("meta" to "data"),
                timeout = Duration.ofSeconds(600),
                resource = mapOf("cpu" to "1"),
                platform =
                    PlatformSpec.builder()
                        .os("linux")
                        .arch("arm64")
                        .build(),
                networkPolicy = networkPolicy,
                extensions = extensions,
                volumes = null,
                secureAccess = true,
                snapshotId = null,
            )

        // Verify request
        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/v1/sandboxes", request.path)
        val requestBody = request.body.readUtf8()
        assertTrue(requestBody.isNotBlank(), "request body should not be blank")

        val payload = Json.parseToJsonElement(requestBody).jsonObject
        val gotExtensions = payload["extensions"]?.jsonObject
        assertNotNull(gotExtensions, "extensions should be present in createSandbox request")
        assertEquals("abc123", gotExtensions!!["storage.id"]!!.jsonPrimitive.content)
        assertEquals("true", gotExtensions["debug"]!!.jsonPrimitive.content)
        val gotNetworkPolicy = payload["networkPolicy"]?.jsonObject
        assertNotNull(gotNetworkPolicy, "networkPolicy should be present in createSandbox request")
        val gotDefaultAction = gotNetworkPolicy!!["defaultAction"]
        assertNotNull(gotDefaultAction, "defaultAction should be present in networkPolicy")
        assertEquals("deny", gotDefaultAction!!.jsonPrimitive.content)
        val egressArray = gotNetworkPolicy["egress"]!!.jsonArray
        assertEquals(1, egressArray.size)
        val rule = egressArray[0].jsonObject
        assertEquals("allow", rule["action"]!!.jsonPrimitive.content)
        assertEquals("pypi.org", rule["target"]!!.jsonPrimitive.content)
        val gotPlatform = payload["platform"]?.jsonObject
        assertNotNull(gotPlatform, "platform should be present in createSandbox request")
        assertEquals("linux", gotPlatform!!["os"]!!.jsonPrimitive.content)
        assertEquals("arm64", gotPlatform["arch"]!!.jsonPrimitive.content)
        assertEquals("true", payload["secureAccess"]!!.jsonPrimitive.content)

        // Verify response
        assertEquals("550e8400-e29b-41d4-a716-446655440000", result.id)
        assertEquals("amd64", result.platform?.arch)
    }

    @Test
    fun `createSandbox should forward windows platform in request`() {
        val responseBody =
            """
            {
                "id": "windows-sandbox-id",
                "status": { "state": "Running" },
                "platform": { "os": "windows", "arch": "amd64" },
                "expiresAt": null,
                "createdAt": "2023-01-01T10:00:00Z",
                "entrypoint": ["cmd"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        val spec = SandboxImageSpec.builder().image("dockurr/windows:latest").build()
        val result =
            sandboxesAdapter.createSandbox(
                spec = spec,
                entrypoint = listOf("cmd"),
                env = emptyMap(),
                metadata = emptyMap(),
                timeout = Duration.ofSeconds(600),
                resource = mapOf("cpu" to "2", "memory" to "4G"),
                platform =
                    PlatformSpec.builder()
                        .os("windows")
                        .arch("amd64")
                        .build(),
                networkPolicy = null,
                extensions = emptyMap(),
                volumes = null,
                secureAccess = false,
            )

        val request = mockWebServer.takeRequest()
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        val gotPlatform = payload["platform"]?.jsonObject
        assertNotNull(gotPlatform, "platform should be present in createSandbox request")
        assertEquals("windows", gotPlatform!!["os"]!!.jsonPrimitive.content)
        assertEquals("amd64", gotPlatform["arch"]!!.jsonPrimitive.content)
        assertEquals("windows", result.platform?.os)
    }

    @Test
    fun `createSandbox should accept null expiresAt for manual cleanup response`() {
        val responseBody =
            """
            {
                "id": "manual-sbx",
                "status": { "state": "Running" },
                "expiresAt": null,
                "createdAt": "2023-01-01T10:00:00Z",
                "entrypoint": ["bash"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        val spec = SandboxImageSpec.builder().image("ubuntu:latest").build()
        val result =
            sandboxesAdapter.createSandbox(
                spec = spec,
                entrypoint = listOf("bash"),
                env = emptyMap(),
                metadata = emptyMap(),
                timeout = null,
                resource = mapOf("cpu" to "1"),
                platform = null,
                networkPolicy = null,
                extensions = emptyMap(),
                volumes = null,
                secureAccess = false,
                snapshotId = null,
            )

        assertEquals("manual-sbx", result.id)

        val request = mockWebServer.takeRequest()
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("false", payload["secureAccess"]!!.jsonPrimitive.content)
    }

    @Test
    fun `createSandbox should support snapshot restore request`() {
        val responseBody =
            """
            {
                "id": "snapshot-sbx",
                "status": { "state": "Running" },
                "createdAt": "2023-01-01T10:00:00Z",
                "entrypoint": ["bash"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        sandboxesAdapter.createSandbox(
            spec = null,
            entrypoint = null,
            env = emptyMap(),
            metadata = emptyMap(),
            timeout = null,
            resource = mapOf("cpu" to "1"),
            platform = null,
            networkPolicy = null,
            extensions = emptyMap(),
            volumes = null,
            secureAccess = false,
            snapshotId = "snap-123",
        )

        val request = mockWebServer.takeRequest()
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("snap-123", payload["snapshotId"]!!.jsonPrimitive.content)
        assertEquals(JsonNull, payload["image"])
        assertEquals(JsonNull, payload["entrypoint"])
    }

    @Test
    fun `createSandbox should preserve explicit snapshot entrypoint`() {
        mockWebServer.enqueue(
            MockResponse()
                .setBody(
                    """
                    {
                      "id": "sbx-123",
                      "status": { "state": "Pending" },
                      "metadata": {},
                      "createdAt": "2025-01-01T00:00:00Z",
                      "entrypoint": ["python", "app.py"]
                    }
                    """.trimIndent(),
                ).setResponseCode(202),
        )

        sandboxesAdapter.createSandbox(
            spec = null,
            entrypoint = listOf("python", "app.py"),
            env = emptyMap(),
            metadata = emptyMap(),
            timeout = null,
            resource = mapOf("cpu" to "1"),
            platform = null,
            networkPolicy = null,
            extensions = emptyMap(),
            volumes = null,
            secureAccess = false,
            snapshotId = "snap-123",
        )

        val request = mockWebServer.takeRequest()
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("snap-123", payload["snapshotId"]!!.jsonPrimitive.content)
        assertEquals(
            Json.parseToJsonElement("""["python","app.py"]"""),
            payload["entrypoint"],
        )
    }

    @Test
    fun `createSnapshot should send request to snapshots api and parse response`() {
        val sandboxId = "sandbox-123"
        val responseBody =
            """
            {
                "id": "snap-123",
                "sandboxId": "$sandboxId",
                "name": "baseline",
                "status": {
                    "state": "Ready",
                    "reason": "snapshot_ready",
                    "message": null,
                    "lastTransitionAt": "2023-01-01T10:00:00Z"
                },
                "createdAt": "2023-01-01T10:00:00Z"
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        val result = sandboxesAdapter.createSnapshot(sandboxId, "baseline")

        assertEquals("snap-123", result.id)
        assertEquals(sandboxId, result.sandboxId)
        assertEquals("baseline", result.name)

        val request = mockWebServer.takeRequest()
        assertEquals("POST", request.method)
        assertEquals("/v1/sandboxes/$sandboxId/snapshots", request.path)
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals("baseline", payload["name"]!!.jsonPrimitive.content)
    }

    @Test
    fun `getSnapshot should send request to snapshots api and parse response`() {
        val snapshotId = "snap-123"
        val responseBody =
            """
            {
                "id": "$snapshotId",
                "sandboxId": "sandbox-123",
                "name": "baseline",
                "status": {
                    "state": "Ready",
                    "reason": "snapshot_ready",
                    "message": null,
                    "lastTransitionAt": "2023-01-01T10:00:00Z"
                },
                "createdAt": "2023-01-01T10:00:00Z"
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        val result = sandboxesAdapter.getSnapshot(snapshotId)

        assertEquals(snapshotId, result.id)
        assertEquals("sandbox-123", result.sandboxId)

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        assertEquals("/v1/snapshots/$snapshotId", request.path)
    }

    @Test
    fun `listSnapshots should send request to snapshots api with filters`() {
        val responseBody =
            """
            {
                "items": [
                    {
                        "id": "snap-123",
                        "sandboxId": "sandbox-123",
                        "name": "baseline",
                        "status": {
                            "state": "Ready",
                            "reason": "snapshot_ready",
                            "message": null,
                            "lastTransitionAt": "2023-01-01T10:00:00Z"
                        },
                        "createdAt": "2023-01-01T10:00:00Z"
                    }
                ],
                "pagination": {
                    "page": 1,
                    "pageSize": 20,
                    "totalItems": 1,
                    "totalPages": 1,
                    "hasNextPage": false
                }
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        val filter =
            SnapshotFilter.builder()
                .sandboxId("sandbox-123")
                .states("ready", "pending")
                .page(1)
                .pageSize(20)
                .build()

        val result = sandboxesAdapter.listSnapshots(filter)

        assertEquals(1, result.snapshotInfos.size)
        assertEquals("snap-123", result.snapshotInfos.first().id)

        val request = mockWebServer.takeRequest()
        assertEquals("GET", request.method)
        val url = request.requestUrl
        assertNotNull(url)
        assertEquals("sandbox-123", url!!.queryParameter("sandboxId"))
        assertEquals(listOf("ready", "pending"), url.queryParameterValues("state"))
        assertEquals("1", url.queryParameter("page"))
        assertEquals("20", url.queryParameter("pageSize"))
    }

    @Test
    fun `deleteSnapshot should send request to snapshots api`() {
        val snapshotId = "snap-123"
        mockWebServer.enqueue(MockResponse().setResponseCode(204))

        sandboxesAdapter.deleteSnapshot(snapshotId)

        val request = mockWebServer.takeRequest()
        assertEquals("DELETE", request.method)
        assertEquals("/v1/snapshots/$snapshotId", request.path)
    }

    @Test
    fun `deleteSnapshot should convert conflict response into sandbox api exception`() {
        val snapshotId = "snap-123"
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(409)
                .setBody("""{"code":"SNAPSHOT::IN_USE","message":"image is being used by running container"}"""),
        )

        val ex =
            assertThrows(SandboxApiException::class.java) {
                sandboxesAdapter.deleteSnapshot(snapshotId)
            }

        assertEquals(409, ex.statusCode)
        assertEquals("SNAPSHOT::IN_USE", ex.error.code)
        assertEquals("image is being used by running container", ex.error.message)

        val request = mockWebServer.takeRequest()
        assertEquals("DELETE", request.method)
        assertEquals("/v1/snapshots/$snapshotId", request.path)
    }

    @Test
    fun `createSandbox should serialize OSSFS volume`() {
        val responseBody =
            """
            {
                "id": "ossfs-sbx",
                "status": { "state": "Running" },
                "expiresAt": null,
                "createdAt": "2023-01-01T10:00:00Z",
                "entrypoint": ["bash"]
            }
            """.trimIndent()
        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(201))

        val spec = SandboxImageSpec.builder().image("ubuntu:latest").build()
        val volumes =
            listOf(
                Volume.builder()
                    .name("oss-data")
                    .ossfs(
                        OSSFS.builder()
                            .bucket("bucket-a")
                            .endpoint("oss-cn-hangzhou.aliyuncs.com")
                            .accessKeyId("ak")
                            .accessKeySecret("sk")
                            .options("allow_other", "max_stat_cache_size=0")
                            .build(),
                    )
                    .mountPath("/mnt/oss")
                    .subPath("prefix")
                    .build(),
            )

        sandboxesAdapter.createSandbox(
            spec = spec,
            entrypoint = listOf("bash"),
            env = emptyMap(),
            metadata = emptyMap(),
            timeout = null,
            resource = mapOf("cpu" to "1"),
            platform = null,
            networkPolicy = null,
            extensions = emptyMap(),
            volumes = volumes,
            secureAccess = false,
            snapshotId = null,
        )

        val request = mockWebServer.takeRequest()
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        val serializedVolume = payload["volumes"]!!.jsonArray[0].jsonObject
        val ossfs = serializedVolume["ossfs"]!!.jsonObject

        assertEquals("bucket-a", ossfs["bucket"]!!.jsonPrimitive.content)
        assertEquals("oss-cn-hangzhou.aliyuncs.com", ossfs["endpoint"]!!.jsonPrimitive.content)
        assertEquals("ak", ossfs["accessKeyId"]!!.jsonPrimitive.content)
        assertEquals("sk", ossfs["accessKeySecret"]!!.jsonPrimitive.content)
        assertEquals("2.0", ossfs["version"]!!.jsonPrimitive.content)
        assertEquals("prefix", serializedVolume["subPath"]!!.jsonPrimitive.content)
    }

    @Test
    fun `getSandboxInfo should parse response correctly`() {
        val sandboxId = "sandbox-id"
        val responseBody =
            """
            {
                "id": "$sandboxId",
                "status": {
                    "state": "Running",
                    "reason": null,
                    "message": null,
                    "lastTransitionAt": "2023-01-01T10:00:00Z"
                },
                "entrypoint": ["/bin/bash"],
                "expiresAt": "2023-01-01T11:00:00Z",
                "createdAt": "2023-01-01T10:00:00Z",
                "image": {
                    "uri": "ubuntu:latest"
                },
                "metadata": {}
            }
            """.trimIndent()

        mockWebServer.enqueue(MockResponse().setBody(responseBody))

        val result = sandboxesAdapter.getSandboxInfo(sandboxId)

        assertEquals(sandboxId, result.id)
        assertEquals(SandboxState.RUNNING, result.status.state)
        assertEquals("ubuntu:latest", result.image!!.image)

        val request = mockWebServer.takeRequest()
        assertEquals("/v1/sandboxes/$sandboxId", request.path)
    }

    @Test
    fun `getSandboxInfo should parse null expiresAt for manual cleanup`() {
        val sandboxId = "manual-sandbox"
        val responseBody =
            """
            {
                "id": "$sandboxId",
                "status": {
                    "state": "Running",
                    "reason": null,
                    "message": null,
                    "lastTransitionAt": "2023-01-01T10:00:00Z"
                },
                "entrypoint": ["/bin/bash"],
                "expiresAt": null,
                "createdAt": "2023-01-01T10:00:00Z",
                "image": {
                    "uri": "ubuntu:latest"
                },
                "metadata": {}
            }
            """.trimIndent()

        mockWebServer.enqueue(MockResponse().setBody(responseBody))

        val result = sandboxesAdapter.getSandboxInfo(sandboxId)

        assertEquals(sandboxId, result.id)
        assertEquals(null, result.expiresAt)
    }

    @Test
    fun `patchSandboxMetadata should preserve null values for delete semantics`() {
        val sandboxId = "sandbox-id"
        val responseBody =
            """
            {
                "id": "$sandboxId",
                "status": { "state": "Running" },
                "entrypoint": ["/bin/bash"],
                "expiresAt": null,
                "createdAt": "2023-01-01T10:00:00Z",
                "metadata": {
                    "env": "production"
                }
            }
            """.trimIndent()

        mockWebServer.enqueue(MockResponse().setBody(responseBody).setResponseCode(200))

        val result =
            sandboxesAdapter.patchSandboxMetadata(
                sandboxId,
                mapOf("team" to null, "env" to "production"),
            )

        assertEquals(sandboxId, result.id)
        assertEquals("production", result.metadata?.get("env"))

        val request = mockWebServer.takeRequest()
        assertEquals("PATCH", request.method)
        assertEquals("/v1/sandboxes/$sandboxId/metadata", request.path)
        val payload = Json.parseToJsonElement(request.body.readUtf8()).jsonObject
        assertEquals(JsonNull, payload["team"])
        assertEquals("production", payload["env"]!!.jsonPrimitive.content)
    }

    @Test
    fun `listSandboxes should construct query params correctly`() {
        val responseBody =
            """
            {
                "items": [],
                "pagination": {
                    "page": 0,
                    "pageSize": 10,
                    "totalItems": 0,
                    "totalPages": 0,
                    "hasNextPage": false
                }
            }
            """.trimIndent()

        mockWebServer.enqueue(MockResponse().setBody(responseBody))

        val filter =
            SandboxFilter.builder()
                .states("RUNNING", "PENDING")
                .metadata(mapOf("key" to "value"))
                .page(1)
                .pageSize(20)
                .build()

        sandboxesAdapter.listSandboxes(filter)

        val request = mockWebServer.takeRequest()
        val url = request.requestUrl
        assertNotNull(url)
        assertEquals("RUNNING", url!!.queryParameter("state"))
        assertEquals("PENDING", url.queryParameterValues("state")[1])
        assertEquals("key=value", url.queryParameter("metadata"))
        assertEquals("1", url.queryParameter("page"))
        assertEquals("20", url.queryParameter("pageSize"))
    }
}
