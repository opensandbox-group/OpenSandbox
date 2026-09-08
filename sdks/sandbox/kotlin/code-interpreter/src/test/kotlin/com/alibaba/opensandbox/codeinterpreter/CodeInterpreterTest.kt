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
import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.models.execd.DEFAULT_EXECD_PORT
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionError
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
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
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.extension.ExtendWith
import java.time.Duration

@ExtendWith(MockKExtension::class)
class CodeInterpreterTest {
    @MockK
    lateinit var sandbox: Sandbox

    @MockK
    lateinit var codeService: Codes

    @MockK
    lateinit var commands: Commands

    private lateinit var codeInterpreter: CodeInterpreter
    private val sandboxId = "sandbox-id"

    @BeforeEach
    fun setUp() {
        every { sandbox.id } returns sandboxId
        every { sandbox.commands() } returns commands
        every { commands.run(any<String>()) } returns Execution()
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

    @Test
    fun `ping should delegate to code service`() {
        every { codeService.ping() } returns true

        assertTrue(codeInterpreter.ping())
        verify(exactly = 1) { codeService.ping() }
    }

    @Test
    fun `isHealthy should treat ping exceptions as unhealthy`() {
        every { codeService.ping() } throws IllegalStateException("connection refused")

        assertEquals(false, codeInterpreter.isHealthy())
    }

    @Test
    fun `checkReady should return once code service answers ping`() {
        var attempts = 0
        every { codeService.ping() } answers {
            attempts++
            attempts >= 3
        }

        codeInterpreter.checkReady(Duration.ofSeconds(5), Duration.ofMillis(10))

        assertEquals(3, attempts)
        // The runtime leg is short-circuited while ping fails; it runs only on the
        // successful attempt (attempt 3).
        verify(exactly = 1) { commands.run(CodeInterpreter.RUNTIME_CHECK_COMMAND) }
    }

    @Test
    fun `checkReady should throw when code service never answers`() {
        every { codeService.ping() } returns false

        assertThrows(SandboxReadyTimeoutException::class.java) {
            codeInterpreter.checkReady(Duration.ofMillis(100), Duration.ofMillis(10))
        }
        // The runtime-process leg must not run when the daemon ping fails.
        verify(exactly = 0) { commands.run(any<String>()) }
    }

    @Test
    fun `checkReady should throw when runtime never serves`() {
        every { codeService.ping() } returns true
        every { commands.run(any<String>()) } returns
            Execution(
                error = ExecutionError(name = "CommandExecError", value = "1", timestamp = 0),
            )

        val exception =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                codeInterpreter.checkReady(Duration.ofMillis(100), Duration.ofMillis(10))
            }

        assertTrue(exception.message!!.contains("Jupyter"))
    }

    @Test
    fun `checkReady should reject non-positive polling interval`() {
        assertThrows(InvalidArgumentException::class.java) {
            codeInterpreter.checkReady(Duration.ofSeconds(1), Duration.ZERO)
        }
        assertThrows(InvalidArgumentException::class.java) {
            codeInterpreter.checkReady(Duration.ofSeconds(1), Duration.ofMillis(-1))
        }
    }

    @Test
    fun `builder should run strict health check on build by default`() {
        every { sandbox.httpClientProvider() } returns mockk<HttpClientProvider>(relaxed = true)
        every { sandbox.getEndpoint(DEFAULT_EXECD_PORT) } returns
            SandboxEndpoint(endpoint = "127.0.0.1:1")

        val exception =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                CodeInterpreter.builder()
                    .fromSandbox(sandbox)
                    .readyTimeout(Duration.ofMillis(100))
                    .healthCheckPollingInterval(Duration.ofMillis(10))
                    .build()
            }

        assertTrue(exception.message!!.contains("code execution service"))
    }

    @Test
    fun `builder should skip strict health check when requested`() {
        every { sandbox.httpClientProvider() } returns mockk<HttpClientProvider>(relaxed = true)
        every { sandbox.getEndpoint(DEFAULT_EXECD_PORT) } returns
            SandboxEndpoint(endpoint = "127.0.0.1:1")

        val interpreter =
            CodeInterpreter.builder()
                .fromSandbox(sandbox)
                .skipHealthCheck(true)
                .build()

        assertSame(sandbox, interpreter.sandbox())
        assertEquals(sandboxId, interpreter.id)
    }
}
