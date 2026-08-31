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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxCapacityExceededException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxNotFoundException
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Protocol
import okhttp3.Request
import okhttp3.Response
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertInstanceOf
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class SandboxErrorClassificationTest {
    private fun errorResponse(
        code: Int,
        body: String?,
    ): Response =
        Response.Builder()
            .request(Request.Builder().url("https://sandbox.local/v1/sandboxes/sb-1").build())
            .protocol(Protocol.HTTP_1_1)
            .code(code)
            .message("status")
            .apply {
                if (body != null) {
                    body(body.toResponseBody("application/json".toMediaType()))
                }
            }
            .build()

    @Test
    fun `507 response should surface a capacity exceeded exception`() {
        val ex = errorResponse(507, null).toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxCapacityExceededException::class.java, ex)
        assertEquals(SandboxError.STORAGE_CAPACITY_EXCEEDED, ex.error.code)
        assertEquals(507, ex.statusCode)
        assertEquals("status=507 body=null", ex.error.message)
    }

    @Test
    fun `507 response with an unparseable body should keep the raw payload as the error message`() {
        val ex =
            errorResponse(507, "not a structured error body")
                .toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxCapacityExceededException::class.java, ex)
        assertEquals(SandboxError.STORAGE_CAPACITY_EXCEEDED, ex.error.code)
        assertEquals("not a structured error body", ex.error.message)
    }

    @Test
    fun `507 response should preserve a structured server code when present`() {
        val ex =
            errorResponse(507, """{"code":"QUOTA::WORKSPACE_FULL","message":"workspace is full"}""")
                .toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxCapacityExceededException::class.java, ex)
        assertEquals("QUOTA::WORKSPACE_FULL", ex.error.code)
    }

    @Test
    fun `sandbox not found code should surface a sandbox not found exception`() {
        val ex =
            errorResponse(404, """{"code":"SANDBOX_NOT_FOUND","message":"no such sandbox"}""")
                .toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxNotFoundException::class.java, ex)
        assertEquals(SandboxError.SANDBOX_NOT_FOUND, ex.error.code)
        assertTrue(ex.isSandboxNotFound())
    }

    @Test
    fun `backend-prefixed sandbox not found codes should surface a sandbox not found exception`() {
        for (code in listOf("DOCKER::SANDBOX_NOT_FOUND", "KUBERNETES::SANDBOX_NOT_FOUND")) {
            val ex =
                errorResponse(404, """{"code":"$code","message":"no such sandbox"}""")
                    .toSandboxApiException { status, body -> "status=$status body=$body" }

            assertInstanceOf(SandboxNotFoundException::class.java, ex)
            assertEquals(code, ex.error.code)
            assertTrue(ex.isSandboxNotFound())
        }
    }

    @Test
    fun `bare 404 without a sandbox-not-found code must not be classified as sandbox not found`() {
        val ex =
            errorResponse(404, "not a structured error body")
                .toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxApiException::class.java, ex)
        assertEquals(SandboxError.UNEXPECTED_RESPONSE, ex.error.code)
        assertFalse(ex.isSandboxNotFound())
    }

    @Test
    fun `isSandboxNotFound should only match explicit sandbox-not-found codes`() {
        assertTrue(
            SandboxApiException(
                message = "missing",
                statusCode = 404,
                error = SandboxError(SandboxError.SANDBOX_NOT_FOUND),
            ).isSandboxNotFound(),
        )
        for (code in listOf("FILE_NOT_FOUND", SandboxError.UNEXPECTED_RESPONSE, "RATE_LIMIT")) {
            assertFalse(
                SandboxApiException(
                    message = "other",
                    statusCode = 404,
                    error = SandboxError(code),
                ).isSandboxNotFound(),
            )
        }
        assertFalse(IllegalStateException("boom").isSandboxNotFound())
    }

    @Test
    fun `new exceptions should remain catchable as the existing api exception types`() {
        val notFound = SandboxNotFoundException("gone")
        val capacity = SandboxCapacityExceededException("full")

        assertInstanceOf(SandboxApiException::class.java, notFound)
        assertInstanceOf(SandboxApiException::class.java, capacity)
        assertEquals(SandboxError.SANDBOX_NOT_FOUND, notFound.error.code)
        assertEquals(SandboxError.STORAGE_CAPACITY_EXCEEDED, capacity.error.code)
        assertEquals(404, notFound.statusCode)
        assertEquals(507, capacity.statusCode)
    }

    @Test
    fun `execd generated-client errors should be classified through the same choke point`() {
        val notFound =
            ClientError<Unit>(
                message = "execd error",
                body = """{"code":"DOCKER::SANDBOX_NOT_FOUND","message":"no such sandbox"}""",
                statusCode = 404,
                headers = mapOf("X-Request-ID" to listOf("req-1")),
            ).toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxNotFoundException::class.java, notFound)
        assertEquals("DOCKER::SANDBOX_NOT_FOUND", notFound.error.code)
        assertEquals(404, notFound.statusCode)
        assertEquals("req-1", notFound.requestId)
        assertTrue(notFound.isSandboxNotFound())

        val capacity =
            ClientError<Unit>(
                message = "execd error",
                body = """{"code":"QUOTA::WORKSPACE_FULL","message":"workspace is full"}""",
                statusCode = 507,
            ).toSandboxApiException { status, body -> "status=$status body=$body" }

        assertInstanceOf(SandboxCapacityExceededException::class.java, capacity)
        assertEquals("QUOTA::WORKSPACE_FULL", capacity.error.code)
        assertEquals(507, capacity.statusCode)
    }
}
