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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import okhttp3.Request
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import java.net.InetAddress

class ClientIpTest {
    private lateinit var server: MockWebServer

    @BeforeEach
    fun setUp() {
        server = MockWebServer()
        server.start()
        ClientIpDetector.resetForTest()
    }

    @AfterEach
    fun tearDown() {
        server.shutdown()
        ClientIpDetector.resetForTest()
    }

    private fun get(config: ConnectionConfig) {
        HttpClientProvider(config).use { provider ->
            val request = Request.Builder().url(server.url("/")).build()
            provider.httpClient.newCall(request).execute().use { it.body?.close() }
        }
    }

    @Test
    fun `detectOutboundIp returns a valid or empty address`() {
        val ip = ClientIpDetector.detectOutboundIp()
        if (ip.isEmpty()) return
        val addr = InetAddress.getByName(ip)
        assertTrue(!addr.isLoopbackAddress, "detected loopback IP: $ip")
        assertTrue(!addr.isAnyLocalAddress, "detected wildcard IP: $ip")
    }

    @Test
    fun `selectClientIp prefers named NIC and skips virtual`() {
        val nics =
            listOf(
                ClientIpDetector.NicAddrs("docker0", listOf("172.17.0.1")),
                ClientIpDetector.NicAddrs("utun3", listOf("10.8.0.3")),
                ClientIpDetector.NicAddrs("en0", listOf("10.1.1.1")),
                ClientIpDetector.NicAddrs("eth1", listOf("192.168.5.5")),
            )
        assertEquals("10.1.1.1", ClientIpDetector.selectClientIp(nics))
    }

    @Test
    fun `selectClientIp falls back to private when no named NIC`() {
        val nics =
            listOf(
                ClientIpDetector.NicAddrs("docker0", listOf("172.17.0.1")),
                ClientIpDetector.NicAddrs("custom9", listOf("8.8.4.4")),
                ClientIpDetector.NicAddrs("wlan9", listOf("10.0.0.50")),
            )
        assertEquals("10.0.0.50", ClientIpDetector.selectClientIp(nics))
    }

    @Test
    fun `selectClientIp skips loopback and link-local`() {
        val nics =
            listOf(
                ClientIpDetector.NicAddrs("eth0", listOf("127.0.0.1", "169.254.1.1", "10.1.2.3")),
            )
        assertEquals("10.1.2.3", ClientIpDetector.selectClientIp(nics))
    }

    @Test
    fun `selectClientIp returns empty when nothing usable`() {
        val nics =
            listOf(
                ClientIpDetector.NicAddrs("docker0", listOf("172.17.0.1")),
                ClientIpDetector.NicAddrs("eth0", listOf("169.254.1.1")),
            )
        assertEquals("", ClientIpDetector.selectClientIp(nics))
    }

    @Test
    fun `clientIp caches the detected value`() {
        ClientIpDetector.detector = { "10.1.2.3" }
        assertEquals("10.1.2.3", ClientIpDetector.clientIp())
        // Change the underlying detector; the cached value must be returned.
        ClientIpDetector.detector = { "10.9.9.9" }
        assertEquals("10.1.2.3", ClientIpDetector.clientIp())
    }

    @Test
    fun `request carries the client IP header`() {
        ClientIpDetector.detector = { "10.9.8.7" }
        server.enqueue(MockResponse().setResponseCode(200))

        get(ConnectionConfig.builder().build())

        val recorded = server.takeRequest()
        assertEquals("10.9.8.7", recorded.getHeader(ClientIpDetector.CLIENT_IP_HEADER))
    }

    @Test
    fun `user-provided header is not overwritten`() {
        ClientIpDetector.detector = { "10.9.8.7" }
        server.enqueue(MockResponse().setResponseCode(200))

        get(
            ConnectionConfig.builder()
                .addHeader(ClientIpDetector.CLIENT_IP_HEADER, "192.168.0.9")
                .build(),
        )

        val recorded = server.takeRequest()
        assertEquals("192.168.0.9", recorded.getHeader(ClientIpDetector.CLIENT_IP_HEADER))
    }

    @Test
    fun `header is omitted when the IP is undetectable`() {
        ClientIpDetector.detector = { "" }
        server.enqueue(MockResponse().setResponseCode(200))

        get(ConnectionConfig.builder().build())

        val recorded = server.takeRequest()
        assertNull(recorded.getHeader(ClientIpDetector.CLIENT_IP_HEADER))
    }
}
