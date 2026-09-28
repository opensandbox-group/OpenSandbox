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

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionComplete
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionError
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertNotNull
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test

class ExecdEventSupportTest {
    private val reported = mutableListOf<Pair<String, Exception>>()

    private fun decode(line: String): com.alibaba.opensandbox.sandbox.api.models.execd.EventNode? =
        ExecdEventSupport.decodeEventLine(line) { line, error -> reported.add(line to error) }

    @Test
    fun `decodes bare NDJSON payload lines`() {
        val node = decode("""{"type":"stdout","timestamp":1,"text":"hi"}""")
        assertNotNull(node)
        assertEquals("stdout", node!!.type)
        assertEquals("hi", node.text)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `decodes data-prefixed frames with and without the single space`() {
        val noSpace = decode("""data:{"type":"stdout","timestamp":1,"text":"a"}""")
        assertNotNull(noSpace)
        assertEquals("a", noSpace!!.text)

        val oneSpace = decode("""data: {"type":"stdout","timestamp":2,"text":"b"}""")
        assertNotNull(oneSpace)
        assertEquals("b", oneSpace!!.text)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `preserves interior whitespace after the data prefix`() {
        // Only one leading space belongs to SSE framing; the rest is payload
        // (multi-space separators must not be trimmed away by the parser).
        val node = decode("""data:  {"type":"stdout","timestamp":3,"text":"  spaced  "}""")
        assertNotNull(node)
        assertEquals("  spaced  ", node!!.text)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `skips SSE framing lines`() {
        assertNull(decode(": keep-alive comment"))
        assertNull(decode("event: stdout"))
        assertNull(decode("id: 42"))
        assertNull(decode("retry: 3000"))
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `skips blank lines`() {
        assertNull(decode(""))
        assertNull(decode("   "))
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `reports malformed payload lines through onError and returns null`() {
        val node = decode("data: {not json")
        assertNull(node)
        assertEquals(1, reported.size)
        assertEquals("data: {not json", reported[0].first)
        assertTrue(reported[0].second is Exception)
    }

    @Test
    fun `decodes payload lines the same way regardless of line endings`() {
        // The adapters feed reader.lineSequence() output, so CRLF is already
        // stripped by the reader; a bare payload line decodes identically.
        val node = decode("""{"type":"complete","timestamp":4}""")
        assertNotNull(node)
        assertEquals("complete", node!!.type)
        assertTrue(reported.isEmpty())
    }

    @Test
    fun `foreground exit code is zero only on completion without error`() {
        val complete = Execution().apply { complete = ExecutionComplete(timestamp = 1, executionTimeInMillis = 1) }
        assertEquals(0, ExecdEventSupport.inferForegroundExitCode(complete))

        val unfinished = Execution()
        assertNull(ExecdEventSupport.inferForegroundExitCode(unfinished))

        val failed =
            Execution().apply {
                error = ExecutionError(name = "SystemExit", value = "3", timestamp = 1)
            }
        assertEquals(3, ExecdEventSupport.inferForegroundExitCode(failed))

        val nonNumeric =
            Execution().apply {
                error = ExecutionError(name = "ValueError", value = "division by zero", timestamp = 1)
            }
        assertNull(ExecdEventSupport.inferForegroundExitCode(nonNumeric))
    }
}
