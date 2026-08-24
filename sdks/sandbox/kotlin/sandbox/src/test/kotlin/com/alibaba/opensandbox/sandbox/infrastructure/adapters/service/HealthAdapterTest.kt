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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test

class HealthAdapterTest {
    private lateinit var server: MockWebServer
    private lateinit var provider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        server = MockWebServer()
        server.start()
    }

    @AfterEach
    fun tearDown() {
        provider.close()
        server.shutdown()
    }

    @Test
    fun `staged warmup ping performs one HTTP attempt when sandbox is not ready`() {
        provider = provider(stagedWarmup = true)
        val adapter = adapter(provider)
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(MockResponse().setResponseCode(200))

        assertFalse(adapter.ping("sandbox-id"))
        assertEquals(1, server.requestCount)

        assertTrue(adapter.ping("sandbox-id"))
        assertEquals(2, server.requestCount)
    }

    @Test
    fun `regular ping retains configured retry behavior`() {
        provider = provider(stagedWarmup = false)
        val adapter = adapter(provider)
        server.enqueue(MockResponse().setResponseCode(503))
        server.enqueue(MockResponse().setResponseCode(200))

        assertTrue(adapter.ping("sandbox-id"))
        assertEquals(2, server.requestCount)
    }

    private fun provider(stagedWarmup: Boolean): HttpClientProvider {
        val config =
            ConnectionConfig.builder()
                .protocol("http")
                .build()
                .let { if (stagedWarmup) it.copyForStagedWarmup() else it }
        return HttpClientProvider(config)
    }

    private fun adapter(provider: HttpClientProvider): HealthAdapter =
        HealthAdapter(
            provider,
            SandboxEndpoint("${server.hostName}:${server.port}"),
        )
}
