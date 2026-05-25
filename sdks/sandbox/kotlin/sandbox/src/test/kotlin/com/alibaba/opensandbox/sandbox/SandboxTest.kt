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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxMetrics
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.Health
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import io.mockk.Runs
import io.mockk.every
import io.mockk.impl.annotations.MockK
import io.mockk.junit5.MockKExtension
import io.mockk.just
import io.mockk.mockk
import io.mockk.verify
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Assertions.assertNull
import org.junit.jupiter.api.Assertions.assertSame
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.BeforeEach
import org.junit.jupiter.api.Test
import org.junit.jupiter.api.extension.ExtendWith
import java.time.Duration

@ExtendWith(MockKExtension::class)
class SandboxTest {
    @MockK
    lateinit var sandboxService: Sandboxes

    @MockK
    lateinit var fileSystemService: Filesystem

    @MockK
    lateinit var commandService: Commands

    @MockK
    lateinit var healthService: Health

    @MockK
    lateinit var metricsService: Metrics

    @MockK
    lateinit var egressService: Egress

    @MockK
    lateinit var diagnosticsService: Diagnostics

    @MockK
    lateinit var httpClientProvider: HttpClientProvider

    private lateinit var sandbox: Sandbox
    private val sandboxId = "sandbox-id"

    @BeforeEach
    fun setUp() {
        every {
            httpClientProvider.config
        } returns
            ConnectionConfig.builder()
                .domain("localhost:8080")
                .useServerProxy(false)
                .build()

        sandbox =
            Sandbox(
                id = sandboxId,
                sandboxService = sandboxService,
                fileSystemService = fileSystemService,
                commandService = commandService,
                healthService = healthService,
                metricsService = metricsService,
                egressService = egressService,
                customHealthCheck = null,
                httpClientProvider = httpClientProvider,
                diagnosticsService = diagnosticsService,
            )
    }

    @Test
    fun `files should return filesystem service`() {
        assertSame(fileSystemService, sandbox.files())
    }

    @Test
    fun `commands should return command service`() {
        assertSame(commandService, sandbox.commands())
    }

    @Test
    fun `metrics should return metrics service`() {
        assertSame(metricsService, sandbox.metrics())
    }

    @Test
    fun `diagnostics should return diagnostics service`() {
        assertSame(diagnosticsService, sandbox.diagnostics())
    }

    @Test
    fun `httpClientProvider should return http client provider`() {
        assertSame(httpClientProvider, sandbox.httpClientProvider())
    }

    @Test
    fun `getInfo should delegate to sandboxService`() {
        val expectedInfo = mockk<SandboxInfo>()
        every { sandboxService.getSandboxInfo(sandboxId) } returns expectedInfo

        val result = sandbox.getInfo()

        assertSame(expectedInfo, result)
        verify { sandboxService.getSandboxInfo(sandboxId) }
    }

    @Test
    fun `getEndpoint should delegate to sandboxService`() {
        val port = 8080
        val expectedEndpoint = mockk<SandboxEndpoint>()
        val connectionConfig = ConnectionConfig.builder().build()
        every { httpClientProvider.config } returns connectionConfig
        every { sandboxService.getSandboxEndpoint(sandboxId, port, false) } returns expectedEndpoint

        val result = sandbox.getEndpoint(port)

        assertSame(expectedEndpoint, result)
        verify { sandboxService.getSandboxEndpoint(sandboxId, port, false) }
    }

    @Test
    fun `getMetrics should delegate to metricsService`() {
        val expectedMetrics = mockk<SandboxMetrics>()
        every { metricsService.getMetrics(sandboxId) } returns expectedMetrics

        val result = sandbox.getMetrics()

        assertSame(expectedMetrics, result)
        verify { metricsService.getMetrics(sandboxId) }
    }

    @Test
    fun `getDiagnosticLogs should delegate to diagnosticsService`() {
        val expected = mockk<DiagnosticContent>()
        every { diagnosticsService.getLogs(sandboxId, "container") } returns expected

        val result = sandbox.getDiagnosticLogs("container")

        assertSame(expected, result)
        verify { diagnosticsService.getLogs(sandboxId, "container") }
    }

    @Test
    fun `getDiagnosticEvents should delegate to diagnosticsService`() {
        val expected = mockk<DiagnosticContent>()
        every { diagnosticsService.getEvents(sandboxId, "runtime") } returns expected

        val result = sandbox.getDiagnosticEvents("runtime")

        assertSame(expected, result)
        verify { diagnosticsService.getEvents(sandboxId, "runtime") }
    }

    @Test
    fun `renew should delegate to sandboxService`() {
        val timeout = Duration.ofMinutes(10)
        val expectedRenew = mockk<SandboxRenewResponse>()
        every { sandboxService.renewSandboxExpiration(sandboxId, any()) } returns expectedRenew

        val actualRenew = sandbox.renew(timeout)

        assertSame(expectedRenew, actualRenew)
    }

    @Test
    fun `getEgressPolicy should delegate to egressService`() {
        val expectedPolicy = mockk<NetworkPolicy>()
        every { egressService.getPolicy() } returns expectedPolicy

        val result = sandbox.getEgressPolicy()

        assertSame(expectedPolicy, result)
        verify { egressService.getPolicy() }
    }

    @Test
    fun `patchEgressRules should delegate to egressService`() {
        val rules = listOf(mockk<NetworkRule>())
        every { egressService.patchRules(rules) } just Runs

        sandbox.patchEgressRules(rules)

        verify { egressService.patchRules(rules) }
    }

    @Test
    fun `builder manualCleanup should clear timeout`() {
        val builder =
            Sandbox.builder()
                .image("python:3.12")
                .timeout(Duration.ofMinutes(5))
                .manualCleanup()

        val timeoutField = builder.javaClass.getDeclaredField("timeout")
        timeoutField.isAccessible = true

        assertNull(timeoutField.get(builder))
    }

    @Test
    fun `pause should delegate to sandboxService`() {
        every { sandboxService.pauseSandbox(sandboxId) } just Runs

        sandbox.pause()

        verify { sandboxService.pauseSandbox(sandboxId) }
    }

    @Test
    fun `kill should delegate to sandboxService`() {
        every { sandboxService.killSandbox(sandboxId) } just Runs

        sandbox.kill()

        verify { sandboxService.killSandbox(sandboxId) }
    }

    @Test
    fun `close should close httpClientProvider`() {
        every { httpClientProvider.close() } just Runs

        sandbox.close()

        verify { httpClientProvider.close() }
    }

    @Test
    fun `isHealthy should return true when healthService returns true`() {
        every { healthService.ping(sandboxId) } returns true

        assertTrue(sandbox.isHealthy())
        verify { healthService.ping(sandboxId) }
    }

    @Test
    fun `isHealthy should return false when healthService returns false`() {
        every { healthService.ping(sandboxId) } returns false

        assertFalse(sandbox.isHealthy())
        verify { healthService.ping(sandboxId) }
    }

    @Test
    fun `checkReady should return when healthy`() {
        every { healthService.ping(sandboxId) } returns true

        sandbox.checkReady(Duration.ofSeconds(1), Duration.ofMillis(10))

        verify { healthService.ping(sandboxId) }
    }

    @Test
    fun `checkReady should throw exception when timeout`() {
        every { healthService.ping(sandboxId) } returns false

        assertThrows(SandboxReadyTimeoutException::class.java) {
            sandbox.checkReady(Duration.ofMillis(100), Duration.ofMillis(10))
        }
    }

    @Test
    fun `checkReady timeout should include connection context and bridge hint`() {
        every { healthService.ping(sandboxId) } throws RuntimeException("connect ECONNREFUSED")

        val ex =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                sandbox.checkReady(Duration.ofMillis(100), Duration.ofMillis(10))
            }

        assertTrue(ex.message!!.contains("Connection context: domain=localhost:8080, useServerProxy=false"))
        assertTrue(ex.message!!.contains("useServerProxy=true"))
        assertTrue(ex.message!!.contains("[docker].host_ip"))
        assertTrue(ex.message!!.contains("Last error: connect ECONNREFUSED"))
    }

    @Test
    fun `checkReady timeout should omit host_ip hint when server proxy is enabled`() {
        val proxyEnabledConfig =
            ConnectionConfig.builder()
                .domain("localhost:8080")
                .useServerProxy(true)
                .build()
        every { httpClientProvider.config } returns proxyEnabledConfig
        every { healthService.ping(sandboxId) } returns false

        val ex =
            assertThrows(SandboxReadyTimeoutException::class.java) {
                sandbox.checkReady(Duration.ofMillis(100), Duration.ofMillis(10))
            }

        assertTrue(ex.message!!.contains("useServerProxy=true"))
        assertFalse(ex.message!!.contains("[docker].host_ip"))
    }
}
