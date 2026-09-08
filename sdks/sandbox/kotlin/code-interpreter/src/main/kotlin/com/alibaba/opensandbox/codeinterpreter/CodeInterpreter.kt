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
import com.alibaba.opensandbox.codeinterpreter.infrastructure.factory.AdapterFactory
import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxInternalException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.models.execd.DEFAULT_EXECD_PORT
import org.slf4j.LoggerFactory
import java.time.Duration
import java.util.concurrent.TimeUnit

/**
 * Code Interpreter SDK providing secure, isolated code execution capabilities.
 *
 * This class extends the basic Sandbox functionality with specialized code execution features,
 * including multi-language support, session management, and variable persistence.
 *
 * ## Key Features
 *
 * - **Multi-language Code Execution**: Support for Python, JavaScript, Bash, Java, Kotlin
 * - **Session Management**: Persistent execution contexts with variable state
 * - **Sandbox Integration**: Full access to underlying sandbox file system and command execution
 * - **Streaming Execution**: Real-time code execution with output streaming
 * - **Variable Inspection**: Access to execution variables and state
 *
 * ## Usage Example
 *
 * ```kotlin
 * // First create a sandbox instance
 * val sandbox = Sandbox.builder()
 *     .image("python:3.11")
 *     .resource { put("memory", "2Gi") }
 *     .build()
 *
 * // Then wrap it with code interpreter capabilities
 * val interpreter = CodeInterpreter.builder()
 *     .fromSandbox(sandbox)
 *     .build()
 *
 * // Execute code with context
 * val context = interpreter.codes().createContext(SupportedLanguage.PYTHON)
 * val result = interpreter.codes().run(
 *     RunCodeRequest.builder()
 *         .code("print('Hello World')")
 *         .context(context)
 *         .build()
 * )
 * println(result.stdout) // Output: Hello World
 *
 * // Access underlying sandbox for file operations
 * interpreter.sandbox().files().writeFile("data.txt", "Hello")
 * val fileResult = interpreter.codes().run(
 *     RunCodeRequest.builder()
 *         .code("with open('data.txt') as f: print(f.read())")
 *         .context(context)
 *         .build()
 * )
 *
 * // Always clean up resources
 * interpreter.kill()
 * interpreter.sandbox().close()
 * ```
 */
class CodeInterpreter internal constructor(
    private val sandbox: Sandbox,
    private val codeService: Codes,
) {
    private val logger = LoggerFactory.getLogger(CodeInterpreter::class.java)

    /**
     * Provides access to the underlying sandbox instance.
     */
    fun sandbox(): Sandbox = sandbox

    /**
     * Gets the unique identifier of this code interpreter (same as underlying sandbox ID).
     */
    val id: String get() = sandbox.id

    /**
     * Provides access to file system operations within the sandbox.
     *
     * Allows writing, reading, listing, and deleting files and directories.
     *
     * @return Service for filesystem manipulation
     */
    fun files() = sandbox.files()

    /**
     * Provides access to command execution operations.
     *
     * Allows running shell commands, capturing output, and managing processes.
     *
     * @return Service for command execution
     */
    fun commands() = sandbox.commands()

    /**
     * Provides access to sandbox metrics and monitoring.
     *
     * Allows retrieving resource usage statistics (CPU, memory) and other performance metrics.
     *
     * @return Service for metrics retrieval
     */
    fun metrics() = sandbox.metrics()

    /**
     * Provides access to code execution operations.
     *
     * This service enables:
     * - Multi-language code execution (Python, JavaScript, Bash, etc.)
     * - Execution context management with persistent variables
     * - Real-time output streaming and interruption capabilities
     *
     * @return Service for advanced code execution with session support
     */
    fun codes() = codeService

    /**
     * Checks whether the code execution service (execd) is responsive.
     *
     * @return `true` if the code execution service is responsive
     */
    fun ping(): Boolean = codeService.ping()

    /**
     * Checks if the code interpreter is healthy (strict check).
     *
     * Healthy means both:
     * - the code execution service (execd) answers `GET /ping`; and
     * - the code interpreter runtime (Jupyter kernel gateway) is serving inside
     *   the sandbox, verified by probing its listen port through the execd
     *   command API.
     *
     * Exceptions raised by either leg are treated as unhealthy.
     *
     * @return `true` if the code interpreter is healthy
     */
    fun isHealthy(): Boolean =
        try {
            ping() && isRuntimeServing()
        } catch (e: Exception) {
            logger.debug("Health check failed for code interpreter {}: {}", id, e.message)
            false
        }

    private fun isRuntimeServing(): Boolean =
        try {
            sandbox.commands().run(RUNTIME_CHECK_COMMAND).error == null
        } catch (e: Exception) {
            logger.debug("Runtime process check failed for code interpreter {}: {}", id, e.message)
            false
        }

    /**
     * Waits for the code interpreter to pass the strict health check with polling.
     *
     * @param timeout Maximum time to wait for the health check to pass
     * @param pollingInterval Time between health check attempts
     * @throws InvalidArgumentException if pollingInterval is negative or zero
     * @throws SandboxReadyTimeoutException if the health check doesn't pass within timeout
     */
    fun checkReady(
        timeout: Duration,
        pollingInterval: Duration,
    ) {
        if (pollingInterval.isNegative || pollingInterval.isZero) {
            throw InvalidArgumentException(
                message = "Ready polling interval must be positive, got: $pollingInterval",
            )
        }
        logger.info("Waiting for code interpreter {} to pass health check (timeout: {}s)", id, timeout.seconds)

        val deadline = System.nanoTime() + timeout.toNanos()
        var attempt = 0

        while (System.nanoTime() < deadline) {
            attempt++
            if (isHealthy()) {
                logger.info("Code interpreter {} passed health check after {} attempts", id, attempt)
                return
            }

            // Clamp the sleep to the remaining budget so the final failed check
            // does not overshoot the timeout by a full polling interval.
            val remainingNanos = deadline - System.nanoTime()
            if (remainingNanos <= 0) break
            try {
                TimeUnit.NANOSECONDS.sleep(minOf(pollingInterval.toNanos(), remainingNanos))
            } catch (e: InterruptedException) {
                Thread.currentThread().interrupt()
                throw SandboxReadyTimeoutException(
                    message = "Interrupted while waiting for code interpreter $id to become ready",
                    cause = e,
                )
            }
        }

        val finalMessage =
            "Code interpreter $id health check timed out after ${timeout.seconds}s ($attempt attempts). " +
                "The code execution service (execd) or the interpreter runtime (Jupyter) did not become ready. " +
                "Pass skipHealthCheck(true) to skip this check."

        logger.error(finalMessage)

        throw SandboxReadyTimeoutException(
            message = finalMessage,
        )
    }

    companion object {
        private val logger = LoggerFactory.getLogger(CodeInterpreter::class.java)

        /** Default timeout for the strict code-executor readiness check. */
        internal val DEFAULT_READY_TIMEOUT: Duration = Duration.ofSeconds(30)

        /** Default polling interval for the strict code-executor readiness check. */
        internal val DEFAULT_HEALTH_CHECK_POLLING_INTERVAL: Duration = Duration.ofMillis(200)

        /**
         * Strict health check script: verifies the code interpreter runtime (Jupyter
         * kernel gateway) is actually serving inside the sandbox. execd starts serving
         * `/ping` before the entrypoint launches Jupyter, and the setup stage may run
         * short-lived "jupyter kernelspec" helpers, so a daemon ping or a process-name
         * grep cannot prove the runtime is ready. Probing the Jupyter listen port
         * (127.0.0.1:${'$'}{JUPYTER_PORT:-44771}, same default as the entrypoint) only
         * passes once the server accepts connections.
         */
        internal const val RUNTIME_CHECK_COMMAND =
            "bash -c 'exec 3<>/dev/tcp/127.0.0.1/${'$'}{JUPYTER_PORT:-44771}' && exit 0 || exit 1"

        /**
         * Creates a new [Builder] for creating CodeInterpreter instances.
         *
         * CodeInterpreter instances must be created from existing Sandbox instances
         * using the fromSandbox() method on the builder.
         *
         * @return A new Builder instance
         */
        @JvmStatic
        fun builder(): Builder = Builder()

        /**
         * Creates a CodeInterpreter from an existing Sandbox instance.
         *
         * By default a strict health check runs before the interpreter is returned:
         * the code execution service (execd) must answer `GET /ping` AND the code
         * interpreter runtime (Jupyter kernel gateway) must be serving, both within
         * [Builder.readyTimeout]. execd starts serving before the runtime launches,
         * so the daemon ping alone is not enough.
         * Opt out via [Builder.skipHealthCheck].
         *
         * This internal method handles the creation and initialization of CodeInterpreter
         * services, including the code execution service and language configuration.
         *
         * @param sandbox Existing sandbox instance to wrap with code execution capabilities
         * @param skipHealthCheck Whether to skip the strict readiness check
         * @param readyTimeout Max time to wait for the readiness check
         * @param healthCheckPollingInterval Polling interval for the readiness check
         * @return CodeInterpreter instance wrapping the sandbox
         * @throws SandboxException if creation fails
         * @throws SandboxInternalException if internal service initialization fails
         */
        internal fun create(
            sandbox: Sandbox,
            skipHealthCheck: Boolean = false,
            readyTimeout: Duration = DEFAULT_READY_TIMEOUT,
            healthCheckPollingInterval: Duration = DEFAULT_HEALTH_CHECK_POLLING_INTERVAL,
        ): CodeInterpreter {
            logger.info("Creating code interpreter from existing sandbox: {}", sandbox.id)

            val factory = AdapterFactory(sandbox.httpClientProvider())

            try {
                // Connect to the execd daemon endpoint for code execution services
                val codeInterpreterEndpoint = sandbox.getEndpoint(DEFAULT_EXECD_PORT)
                val codeExecutionService = factory.createCodes(codeInterpreterEndpoint)

                val interpreter = CodeInterpreter(sandbox, codeExecutionService)

                if (!skipHealthCheck) {
                    interpreter.checkReady(readyTimeout, healthCheckPollingInterval)
                }

                logger.info("Code interpreter {} created from sandbox successfully", sandbox.id)

                return interpreter
            } catch (e: Exception) {
                throw when (e) {
                    is SandboxException -> e
                    else -> SandboxInternalException("Failed to create code interpreter from sandbox: ${e.message}", e)
                }
            }
        }
    }

    /**
     * Builder for creating CodeInterpreter instances from existing Sandbox instances.
     *
     * CodeInterpreter must be created by wrapping an existing Sandbox instance with
     * code execution capabilities. This design ensures clear separation of concerns:
     * - Sandbox handles infrastructure (containers, resources, networking)
     * - CodeInterpreter adds code execution capabilities on top
     *
     * ## Usage Example
     *
     * ```kotlin
     * // First create a sandbox with desired configuration
     * val sandbox = Sandbox.builder()
     *     .image("python:3.11")
     *     .resource { put("memory", "4Gi") }
     *     .env { put("PYTHONPATH", "/custom/path") }
     *     .build()
     *
     * // Then wrap it with code interpreter capabilities
     * val interpreter = CodeInterpreter.builder()
     *     .fromSandbox(sandbox)
     *     .connectionConfig(customConfig)  // Optional
     *     .build()
     *
     * // Use the interpreter
     * val result = interpreter.codes().run(RunCodeRequest.builder().code("print('Hello World!')").build())
     * ```
     */

    class Builder internal constructor() {
        private var sandbox: Sandbox? = null
        private var skipHealthCheck: Boolean = false
        private var readyTimeout: Duration = DEFAULT_READY_TIMEOUT
        private var healthCheckPollingInterval: Duration = DEFAULT_HEALTH_CHECK_POLLING_INTERVAL

        /**
         * Specifies the Sandbox instance to wrap with code interpreter capabilities.
         *
         * This is the only way to create a CodeInterpreter - by extending an existing
         * Sandbox instance with code execution functionality.
         *
         * @param sandbox Existing sandbox instance to wrap
         * @return This builder for method chaining
         * @throws InvalidArgumentException if sandbox is null
         */
        fun fromSandbox(sandbox: Sandbox): Builder {
            this.sandbox = sandbox
            return this
        }

        /**
         * Skips the strict code-executor readiness check performed by [build].
         *
         * The returned interpreter may fail on first use if the execd daemon
         * is not serving yet.
         *
         * @param skipHealthCheck Whether to skip the readiness check
         * @return This builder for method chaining
         */
        fun skipHealthCheck(skipHealthCheck: Boolean): Builder {
            this.skipHealthCheck = skipHealthCheck
            return this
        }

        /**
         * Sets the max time to wait for the code execution service (execd)
         * health check performed by [build].
         *
         * @param readyTimeout Max time to wait for the health check
         * @return This builder for method chaining
         */
        fun readyTimeout(readyTimeout: Duration): Builder {
            this.readyTimeout = readyTimeout
            return this
        }

        /**
         * Sets the polling interval for the code execution service (execd)
         * health check performed by [build].
         *
         * @param healthCheckPollingInterval Time between health check attempts
         * @return This builder for method chaining
         */
        fun healthCheckPollingInterval(healthCheckPollingInterval: Duration): Builder {
            this.healthCheckPollingInterval = healthCheckPollingInterval
            return this
        }

        /**
         * Creates the CodeInterpreter instance from the configured sandbox.
         *
         * By default a strict health check runs before the interpreter is
         * returned: the code execution service (execd) must answer
         * `GET /ping` within the configured [readyTimeout]. Opt out via
         * [skipHealthCheck].
         *
         * @return CodeInterpreter instance wrapping the specified sandbox
         * @throws InvalidArgumentException if no sandbox was specified via fromSandbox()
         * @throws com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
         * if the code execution service health check times out
         */
        fun build(): CodeInterpreter {
            val sandboxInstance =
                sandbox ?: throw InvalidArgumentException(
                    "Sandbox instance must be specified via fromSandbox(). " +
                        "Create a Sandbox first, then wrap it with CodeInterpreter.",
                )
            return create(sandboxInstance, skipHealthCheck, readyTimeout, healthCheckPollingInterval)
        }
    }
}
