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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ByteRange
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.isFileNotFound
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertArrayEquals
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows

class FilesystemAdapterTest {
    private lateinit var mockWebServer: MockWebServer
    private lateinit var filesystemAdapter: FilesystemAdapter
    private lateinit var httpClientProvider: HttpClientProvider

    @BeforeEach
    fun setUp() {
        mockWebServer = MockWebServer()
        mockWebServer.start()

        val host = mockWebServer.hostName
        val port = mockWebServer.port
        val endpoint = SandboxEndpoint("$host:$port")

        val config =
            ConnectionConfig.builder()
                .domain("$host:$port")
                .protocol("http")
                .build()

        httpClientProvider = HttpClientProvider(config)
        filesystemAdapter = FilesystemAdapter(httpClientProvider, endpoint)
    }

    @AfterEach
    fun tearDown() {
        mockWebServer.shutdown()
        httpClientProvider.close()
    }

    @Test
    fun `readFile surfaces FILE_NOT_FOUND error code on 404 so callers can distinguish it`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(404)
                .setBody(
                    """{"code":"FILE_NOT_FOUND","message":"file not found. open /tmp/missing.txt: no such file or directory"}""",
                ),
        )

        val exception =
            assertThrows<SandboxApiException> {
                filesystemAdapter.readFile("/tmp/missing.txt", "UTF-8", null)
            }

        assertEquals(404, exception.statusCode)
        assertEquals(SandboxError.FILE_NOT_FOUND, exception.error.code)
        // The exception itself is recognised as a "not found" condition, which is what the
        // adapter relies on to avoid emitting ERROR-level log noise for an expected outcome.
        assertTrue(exception.isFileNotFound())
    }

    @Test
    fun `readFile returns content on success`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("hello world"),
        )

        val content = filesystemAdapter.readFile("/tmp/hello.txt", "UTF-8", null)

        assertEquals("hello world", content)
    }

    @Test
    fun `readByteArrayDetailed exposes partial response metadata`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(206)
                .setHeader("Content-Type", "application/octet-stream")
                .setHeader("Content-Disposition", "attachment; filename=\"data.bin\"")
                .setHeader("Content-Range", "bytes 0-4/10")
                .setBody("hello"),
        )

        val response = filesystemAdapter.readByteArrayDetailed("/data.bin", "bytes=0-4")

        assertArrayEquals("hello".toByteArray(), response.body)
        assertEquals(206, response.statusCode)
        assertTrue(response.isPartial)
        assertEquals("application/octet-stream", response.contentType)
        assertEquals("attachment; filename=\"data.bin\"", response.contentDisposition)
        assertEquals(5, response.contentLength)
        assertEquals(10, response.totalSize)
        assertEquals(ByteRange(0, 4, 10, "bytes 0-4/10"), response.contentRange)
        assertEquals("bytes=0-4", mockWebServer.takeRequest().getHeader("Range"))
    }

    @Test
    fun `readByteArrayDetailed identifies an ignored Range request`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(200)
                .setBody("whole file"),
        )

        val response = filesystemAdapter.readByteArrayDetailed("/data.bin", "bytes=5-")

        assertFalse(response.isPartial)
        assertNull(response.contentRange)
        assertEquals(10, response.contentLength)
        assertEquals(10, response.totalSize)
    }

    @Test
    fun `readByteArrayDetailed identifies a partial response without Content-Range`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(206)
                .setBody("hello"),
        )

        val response = filesystemAdapter.readByteArrayDetailed("/data.bin")

        assertTrue(response.isPartial)
        assertNull(response.contentRange)
        assertEquals(-1, response.totalSize)
    }

    @Test
    fun `readByteArrayDetailed preserves invalid and unknown Content-Range values`() {
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(206)
                .setHeader("Content-Range", "garbage")
                .setBody("hello"),
        )
        mockWebServer.enqueue(
            MockResponse()
                .setResponseCode(206)
                .setHeader("Content-Range", "bytes 5-9/*")
                .setBody("hello"),
        )

        val invalid = filesystemAdapter.readByteArrayDetailed("/data.bin")
        val unknown = filesystemAdapter.readByteArrayDetailed("/data.bin")

        assertEquals(ByteRange(-1, -1, -1, "garbage"), invalid.contentRange)
        assertEquals(-1, invalid.totalSize)
        assertEquals(ByteRange(5, 9, -1, "bytes 5-9/*"), unknown.contentRange)
        assertEquals(-1, unknown.totalSize)
    }

    @Test
    fun `detailed and legacy reads return response bodies`() {
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("hello"))
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("hello"))
        mockWebServer.enqueue(MockResponse().setResponseCode(200).setBody("hello"))

        filesystemAdapter.readStreamDetailed("/data.bin").body.use { stream ->
            assertArrayEquals("hello".toByteArray(), stream.readBytes())
        }
        assertArrayEquals("hello".toByteArray(), filesystemAdapter.readByteArray("/data.bin"))
        filesystemAdapter.readStream("/data.bin").use { stream ->
            assertArrayEquals("hello".toByteArray(), stream.readBytes())
        }
    }

    @Test
    fun `isFileNotFound is true for FILE_NOT_FOUND error code`() {
        val exception =
            SandboxApiException(
                message = "Failed to read file. Status code: 404",
                statusCode = 404,
                error = SandboxError(SandboxError.FILE_NOT_FOUND),
            )

        assertTrue(exception.isFileNotFound())
    }

    @Test
    fun `isFileNotFound is false for other API errors`() {
        val exception =
            SandboxApiException(
                message = "Internal server error",
                statusCode = 500,
                error = SandboxError(SandboxError.UNEXPECTED_RESPONSE),
            )

        assertFalse(exception.isFileNotFound())
    }

    @Test
    fun `isFileNotFound is false for a 404 without an explicit FILE_NOT_FOUND code`() {
        // A 404 whose body could not be parsed is mapped to UNEXPECTED_RESPONSE. It may indicate a
        // real endpoint/routing regression, so it must NOT be downgraded to a not-found condition.
        val exception =
            SandboxApiException(
                message = "Failed to read file. Status code: 404",
                statusCode = 404,
                error = SandboxError(SandboxError.UNEXPECTED_RESPONSE),
            )

        assertFalse(exception.isFileNotFound())
    }

    @Test
    fun `isFileNotFound is false for non-sandbox exceptions`() {
        assertFalse(RuntimeException("boom").isFileNotFound())
    }
}
