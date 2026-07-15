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

package com.alibaba.opensandbox.codeinterpreter

import com.alibaba.opensandbox.codeinterpreter.domain.services.Codes
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import io.mockk.every
import io.mockk.impl.annotations.MockK
import io.mockk.junit5.MockKExtension
import io.mockk.mockk
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.extension.ExtendWith

@ExtendWith(MockKExtension::class)
class CodeInterpreterTest {
    @MockK
    lateinit var sandbox: Sandbox

    @MockK
    lateinit var codeService: Codes

    private lateinit var codeInterpreter: CodeInterpreter
    private val sandboxId = "sandbox-id"

    @BeforeEach
    fun setUp() {
        every { sandbox.id } returns sandboxId
        codeInterpreter = CodeInterpreter(sandbox, codeService)
    }

    @Test
    fun `id should return sandbox id`() {
        assertEquals(sandboxId, codeInterpreter.id)
    }

    @Test
    fun `sandbox should return underlying sandbox`() {
        assertSame(sandbox, codeInterpreter.sandbox())
    }

    @Test
    fun `files should delegate to sandbox files`() {
        val filesService = mockk<Filesystem>()
        every { sandbox.files() } returns filesService

        assertSame(filesService, codeInterpreter.files())
        verify { sandbox.files() }
    }

    @Test
    fun `commands should delegate to sandbox commands`() {
        val commandService = mockk<Commands>()
        every { sandbox.commands() } returns commandService

        assertSame(commandService, codeInterpreter.commands())
        verify { sandbox.commands() }
    }

    @Test
    fun `metrics should delegate to sandbox metrics`() {
        val metricsService = mockk<Metrics>()
        every { sandbox.metrics() } returns metricsService

        assertSame(metricsService, codeInterpreter.metrics())
        verify { sandbox.metrics() }
    }

    @Test
    fun `codes should return code service`() {
        assertSame(codeService, codeInterpreter.codes())
    }
}
