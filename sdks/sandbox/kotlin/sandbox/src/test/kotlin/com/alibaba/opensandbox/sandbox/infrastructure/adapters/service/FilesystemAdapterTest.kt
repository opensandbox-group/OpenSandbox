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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.isFileNotFound
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.jupiter.api.AfterEach
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
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
