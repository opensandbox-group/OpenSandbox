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
import com.alibaba.opensandbox.sandbox.domain.models.execd.DEFAULT_EXECD_PORT
import org.slf4j.LoggerFactory

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

    companion object {
        private val logger = LoggerFactory.getLogger(CodeInterpreter::class.java)

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
         * This internal method handles the creation and initialization of CodeInterpreter
         * services, including the code execution service and language configuration.
         *
         * @param sandbox Existing sandbox instance to wrap with code execution capabilities
         * @return CodeInterpreter instance wrapping the sandbox
         * @throws SandboxException if creation fails
         * @throws SandboxInternalException if internal service initialization fails
         */
        internal fun create(sandbox: Sandbox): CodeInterpreter {
            logger.info("Creating code interpreter from existing sandbox: {}", sandbox.id)

            val factory = AdapterFactory(sandbox.httpClientProvider())

            try {
                // Connect to the execd daemon endpoint for code execution services
                val codeInterpreterEndpoint = sandbox.getEndpoint(DEFAULT_EXECD_PORT)
                val codeExecutionService = factory.createCodes(codeInterpreterEndpoint)

                logger.info("Code interpreter {} created from sandbox successfully", sandbox.id)

                return CodeInterpreter(sandbox, codeExecutionService)
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
         * Creates the CodeInterpreter instance from the configured sandbox.
         *
         * @return CodeInterpreter instance wrapping the specified sandbox
         * @throws InvalidArgumentException if no sandbox was specified via fromSandbox()
         */
        fun build(): CodeInterpreter {
            val sandboxInstance =
                sandbox ?: throw InvalidArgumentException(
                    "Sandbox instance must be specified via fromSandbox(). " +
                        "Create a Sandbox first, then wrap it with CodeInterpreter.",
                )
            return create(sandboxInstance)
        }
    }
}
