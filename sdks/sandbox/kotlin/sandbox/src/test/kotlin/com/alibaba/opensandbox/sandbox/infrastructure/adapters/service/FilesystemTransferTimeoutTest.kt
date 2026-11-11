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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import okhttp3.mockwebserver.SocketPolicy
import okio.Buffer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows
import java.io.InputStream
import java.time.Duration
import java.util.concurrent.TimeUnit

/**
 * File transfers are long-running by nature: a transfer that keeps making progress must be able to
 * outlive `requestTimeout`, while a transfer that stalls must still fail instead of hanging.
 */
class FilesystemTransferTimeoutTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var httpClientProvider: HttpClientProvider

    private val requestTimeout = Duration.ofSeconds(1)
    private val failureBound = Duration.ofSeconds(10)

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()
        httpClientProvider =
            HttpClientProvider(
                ConnectionConfig.builder()
                    .domain("${mockWebServer.hostName}:${mockWebServer.port}")
                    .protocol("http")
                    .requestTimeout(requestTimeout)
                    .build(),
            )
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    private fun filesystem(): FilesystemAdapter =
        FilesystemAdapter(
            httpClientProvider,
            SandboxEndpoint("${mockWebServer.hostName}:${mockWebServer.port}"),
        )

    @Test
    fun `upload body that outlives requestTimeout still completes`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("{}"))

        // ~2.4s of streaming with 300ms gaps: longer than requestTimeout (1s) as a whole, while
        // every individual write and read stays well under it.
        filesystem().writeFile(
            WriteEntry.builder()
                .path("/tmp/slow.bin")
                .data(chunkedStream(chunks = 8, chunkSize = 8 * 1024, delayMillis = 300))
                .build(),
        )

        assertEquals(1, mockWebServer.requestCount)
    }

    @Test
    fun `download body that outlives requestTimeout still completes`() {
        val body = ByteArray(256 * 1024) { 0x42 }
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(Buffer().write(body))
                // 8 chunks x 200ms = ~1.6s: longer than requestTimeout, no 200ms gap reaches it.
                .throttleBody(32L * 1024, 200L, TimeUnit.MILLISECONDS),
        )

        assertEquals(body.size, filesystem().readByteArray("/tmp/slow.bin", null, null, null).size)
    }

    @Test
    fun `download stream that outlives requestTimeout keeps yielding data`() {
        val body = ByteArray(256 * 1024) { 0x42 }
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(Buffer().write(body))
                .throttleBody(32L * 1024, 200L, TimeUnit.MILLISECONDS),
        )

        val received =
            filesystem().readStream("/tmp/slow.bin", null, null, null).use { it.readBytes() }

        assertEquals(body.size, received.size)
    }

    @Test
    fun `upload without response headers still fails at the request timeout`() {
        mockWebServer.enqueue(MockResponse().setSocketPolicy(SocketPolicy.NO_RESPONSE))
        val started = System.nanoTime()

        assertThrows<SandboxException> {
            filesystem().writeFile(
                WriteEntry.builder().path("/tmp/small.txt").data("hello").build(),
            )
        }

        assertTrue(elapsedSince(started) < failureBound, "upload hang was not bounded")
    }

    @Test
    fun `download with a stalled body still fails at the request timeout`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(Buffer().write(ByteArray(64 * 1024)))
                .setBodyDelay(3, TimeUnit.SECONDS),
        )
        val started = System.nanoTime()

        assertThrows<SandboxException> {
            filesystem().readByteArray("/tmp/stalled.bin", null, null, null)
        }

        assertTrue(elapsedSince(started) < failureBound, "download hang was not bounded")
    }

    @Test
    fun `download that keeps making progress still respects an explicit overallDeadline`() {
        val deadline = Duration.ofSeconds(1)
        val provider =
            HttpClientProvider(
                ConnectionConfig.builder()
                    .domain("${mockWebServer.hostName}:${mockWebServer.port}")
                    .protocol("http")
                    .requestTimeout(Duration.ofSeconds(30))
                    .retryPolicy(RetryPolicy(maxRetries = 0, overallDeadline = deadline))
                    .build(),
            )
        val body = ByteArray(256 * 1024) { 0x42 }
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody(Buffer().write(body))
                // 8 chunks x 200ms = ~1.6s: every read stays under the deadline, but the
                // whole transfer outlasts it.
                .throttleBody(32L * 1024, 200L, TimeUnit.MILLISECONDS),
        )
        val started = System.nanoTime()

        provider.use { clientProvider ->
            val filesystem =
                FilesystemAdapter(
                    clientProvider,
                    SandboxEndpoint("${mockWebServer.hostName}:${mockWebServer.port}"),
                )
            assertThrows<SandboxException> {
                filesystem.readByteArray("/tmp/slow.bin", null, null, null)
            }
        }

        assertEquals(1, mockWebServer.requestCount)
        assertTrue(elapsedSince(started) < failureBound, "overallDeadline was not enforced")
    }

    @Test
    fun `transfer with an overallDeadline beyond OkHttp's call timeout limit does not fail`() {
        val provider =
            HttpClientProvider(
                ConnectionConfig.builder()
                    .domain("${mockWebServer.hostName}:${mockWebServer.port}")
                    .protocol("http")
                    .requestTimeout(requestTimeout)
                    .retryPolicy(
                        RetryPolicy(maxRetries = 0, overallDeadline = Duration.ofDays(30)),
                    )
                    .build(),
            )
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("payload"))

        provider.use { clientProvider ->
            val filesystem =
                FilesystemAdapter(
                    clientProvider,
                    SandboxEndpoint("${mockWebServer.hostName}:${mockWebServer.port}"),
                )
            assertEquals("payload", filesystem.readFile("/tmp/small.txt", "UTF-8", null))
        }
    }

    @Test
    fun `isolated session upload body that outlives requestTimeout still completes`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("{}"))

        IsolatedFilesystemAdapter(
            httpClientProvider,
            SandboxEndpoint("${mockWebServer.hostName}:${mockWebServer.port}"),
            "0f7d1c1e-2a2c-4f0a-9b3e-3b6f3d8a1c2d",
        ).writeFile(
            WriteEntry.builder()
                .path("/tmp/slow.bin")
                .data(chunkedStream(chunks = 8, chunkSize = 8 * 1024, delayMillis = 300))
                .build(),
        )

        assertEquals(1, mockWebServer.requestCount)
    }

    private fun elapsedSince(startNanos: Long): Duration = Duration.ofNanos(System.nanoTime() - startNanos)

    private fun chunkedStream(
        chunks: Int,
        chunkSize: Int,
        delayMillis: Long,
    ): InputStream =
        object : InputStream() {
            private var remaining = chunks * chunkSize

            override fun read(): Int {
                val single = ByteArray(1)
                return if (read(single, 0, 1) == -1) -1 else single[0].toInt() and 0xff
            }

            override fun read(
                buffer: ByteArray,
                offset: Int,
                length: Int,
            ): Int {
                if (remaining <= 0) return -1
                Thread.sleep(delayMillis)
                val count = minOf(length, chunkSize, remaining)
                java.util.Arrays.fill(buffer, offset, offset + count, 0x41)
                remaining -= count
                return count
            }
        }
}
