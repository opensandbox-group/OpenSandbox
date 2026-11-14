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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxInternalException
import kotlinx.serialization.KSerializer
import kotlinx.serialization.Serializable
import kotlinx.serialization.builtins.serializer
import kotlinx.serialization.descriptors.PrimitiveKind
import kotlinx.serialization.descriptors.PrimitiveSerialDescriptor
import kotlinx.serialization.descriptors.SerialDescriptor
import kotlinx.serialization.encoding.Decoder
import kotlinx.serialization.encoding.Encoder
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.assertThrows

class ResponseBodyDecodingTest {
    @Serializable
    private data class Sample(val id: String)

    private class FailingSerializer(
        private val error: Throwable,
    ) : KSerializer<Sample> {
        override val descriptor: SerialDescriptor = PrimitiveSerialDescriptor("Sample", PrimitiveKind.STRING)

        override fun deserialize(decoder: Decoder): Sample = throw error

        override fun serialize(
            encoder: Encoder,
            value: Sample,
        ) = throw error
    }

    private val payload = """{"error":{"code":"INTERNAL","message":"execd exploded"}}"""

    @Test
    fun `decodes well-formed bodies unchanged`() {
        val decoded = jsonParser.decodeResponseBody(String.serializer(), "\"ok\"", "op")
        assertEquals("ok", decoded)
    }

    @Test
    fun `wraps serialization failures with the raw body`() {
        val boom = IllegalStateException("Unexpected JSON token")
        val thrown =
            assertThrows<SandboxInternalException> {
                jsonParser.decodeResponseBody(FailingSerializer(boom), payload, "create isolated session")
            }

        assertTrue(thrown.message!!.contains("create isolated session"))
        assertTrue(thrown.message!!.contains(payload))
        assertSame(boom, thrown.cause)
    }

    @Test
    fun `wraps linkage errors such as AbstractMethodError with the raw body`() {
        val boom = AbstractMethodError("missing typeParametersSerializers()")
        val thrown =
            assertThrows<SandboxInternalException> {
                jsonParser.decodeResponseBody(FailingSerializer(boom), payload, "get isolated session")
            }

        assertTrue(thrown.message!!.contains("get isolated session"))
        assertTrue(thrown.message!!.contains(payload))
        assertSame(boom, thrown.cause)
    }

    @Test
    fun `truncates oversized payloads to the configured limit`() {
        val oversized = "x".repeat(DECODE_FAILURE_PAYLOAD_LIMIT + 1000) + "TAIL-MARKER"
        val thrown =
            assertThrows<SandboxInternalException> {
                jsonParser.decodeResponseBody(FailingSerializer(IllegalStateException("no")), oversized, "op")
            }

        assertTrue(thrown.message!!.contains("x".repeat(DECODE_FAILURE_PAYLOAD_LIMIT)))
        assertTrue(!thrown.message!!.contains("TAIL-MARKER"))
    }
}
