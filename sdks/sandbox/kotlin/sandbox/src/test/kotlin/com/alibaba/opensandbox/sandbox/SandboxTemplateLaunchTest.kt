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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxOrigin
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration

class SandboxTemplateLaunchTest {
    @Test
    fun `create from template routes egress through the lifecycle control plane`() {
        MockWebServer().use { server ->
            server.start()
            server.enqueue(
                MockResponse()
                    .setBody("""{"id":"fsb-001","status":{"state":"Pending"},"createdAt":"2026-09-17T10:00:00Z","entrypoint":[]}""")
                    .setResponseCode(201),
            )
            server.enqueue(
                MockResponse()
                    .setHeader(SandboxOrigin.HEADER_NAME, SandboxOrigin.TEMPLATE)
                    .setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )
            server.enqueue(
                MockResponse().setBody("""{"status":"ok","mode":"deny_all","policy":{"defaultAction":"deny","egress":[]}}"""),
            )

            val config =
                ConnectionConfig
                    .builder()
                    .domain(server.url("/").toString())
                    .disableMetrics()
                    .build()
            val sandbox =
                Sandbox
                    .fromTemplate()
                    .templateId("tpl_1")
                    .timeout(Duration.ofMinutes(5))
                    .connectionConfig(config)
                    .skipHealthCheck()
                    .create()

            assertEquals(SandboxOrigin.TEMPLATE, sandbox.origin)

            val createRequest = server.takeRequest()
            assertEquals("POST", createRequest.method)
            assertEquals("/v1/sandboxes", createRequest.path)
            val payload = Json.parseToJsonElement(createRequest.body.readUtf8()).jsonObject
            assertEquals("tpl_1", payload["templateId"]!!.jsonPrimitive.content)

            val endpointRequest = server.takeRequest()
            assertTrue(endpointRequest.path!!.contains("/endpoints/44772"))

            val policy = sandbox.getEgressPolicy()
            assertEquals(NetworkPolicy.DefaultAction.DENY, policy.defaultAction)
            val policyRequest = server.takeRequest()
            assertEquals("/v1/sandboxes/fsb-001/networkpolicy", policyRequest.path)

            // Template-backed sandboxes never resolve the egress sidecar endpoint.
            assertEquals(3, server.requestCount)

            sandbox.close()
        }
    }

    @Test
    fun `connect auto-detects template origin and skips the egress sidecar endpoint`() {
        MockWebServer().use { server ->
            server.start()
            server.enqueue(
                MockResponse()
                    .setHeader(SandboxOrigin.HEADER_NAME, SandboxOrigin.TEMPLATE)
                    .setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )
            server.enqueue(
                MockResponse().setBody("""{"status":"ok","mode":"deny_all","policy":{"defaultAction":"deny","egress":[]}}"""),
            )

            val config =
                ConnectionConfig
                    .builder()
                    .domain(server.url("/").toString())
                    .disableMetrics()
                    .build()
            val sandbox =
                Sandbox
                    .connector()
                    .sandboxId("fsb-001")
                    .connectionConfig(config)
                    .skipHealthCheck()
                    .connect()

            assertEquals(SandboxOrigin.TEMPLATE, sandbox.origin)
            assertTrue(server.takeRequest().path!!.contains("/endpoints/44772"))
            assertEquals(1, server.requestCount)

            sandbox.patchEgressRules(
                listOf(NetworkRule.builder().action(NetworkRule.Action.ALLOW).target("pypi.org").build()),
            )
            val patchRequest = server.takeRequest()
            assertEquals("PATCH", patchRequest.method)
            assertEquals("/v1/sandboxes/fsb-001/networkpolicy", patchRequest.path)

            sandbox.close()
        }
    }

    @Test
    fun `endpoint origin survives endpoint cache fetch and hit`() {
        MockWebServer().use { server ->
            server.start()
            server.enqueue(
                MockResponse()
                    .setHeader(SandboxOrigin.HEADER_NAME, SandboxOrigin.TEMPLATE)
                    .setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )
            server.enqueue(
                MockResponse()
                    .setHeader(SandboxOrigin.HEADER_NAME, SandboxOrigin.TEMPLATE)
                    .setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )

            val config =
                ConnectionConfig
                    .builder()
                    .domain(server.url("/").toString())
                    .disableMetrics()
                    .build()
            val sandbox =
                Sandbox
                    .connector()
                    .sandboxId("fsb-001")
                    .connectionConfig(config)
                    .skipHealthCheck()
                    .connect()

            // Fresh fetch of a not-yet-cached port carries the server-reported origin.
            assertEquals(SandboxOrigin.TEMPLATE, sandbox.getEndpoint(8080).origin)
            // Cache hits keep it: the execd endpoint was resolved (and cached) during
            // connect, and the second lookup reuses the 8080 entry.
            assertEquals(SandboxOrigin.TEMPLATE, sandbox.getEndpoint(44772).origin)
            assertEquals(SandboxOrigin.TEMPLATE, sandbox.getEndpoint(8080).origin)
            assertEquals(2, server.requestCount)

            sandbox.close()
        }
    }

    @Test
    fun `image-based create keeps resolving the egress sidecar endpoint`() {
        MockWebServer().use { server ->
            server.start()
            server.enqueue(
                MockResponse()
                    .setBody("""{"id":"sb-001","status":{"state":"Pending"},"createdAt":"2026-09-17T10:00:00Z","entrypoint":[]}""")
                    .setResponseCode(201),
            )
            server.enqueue(
                MockResponse().setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )
            server.enqueue(
                MockResponse().setBody("""{"endpoint":"localhost:${server.port}","headers":{}}"""),
            )

            val config =
                ConnectionConfig
                    .builder()
                    .domain(server.url("/").toString())
                    .disableMetrics()
                    .build()
            val sandbox =
                Sandbox
                    .builder()
                    .image("ubuntu:22.04")
                    .timeout(Duration.ofMinutes(5))
                    .connectionConfig(config)
                    .skipHealthCheck()
                    .build()

            assertEquals(SandboxOrigin.UNKNOWN, sandbox.origin)
            assertTrue(server.takeRequest().path!!.contains("/v1/sandboxes"))
            assertTrue(server.takeRequest().path!!.contains("/endpoints/44772"))
            assertTrue(server.takeRequest().path!!.contains("/endpoints/18080"))
            assertEquals(3, server.requestCount)

            sandbox.close()
        }
    }
}
