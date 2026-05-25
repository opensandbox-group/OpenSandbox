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
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxInternalException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent
import com.alibaba.opensandbox.sandbox.domain.models.execd.DEFAULT_EGRESS_PORT
import com.alibaba.opensandbox.sandbox.domain.models.execd.DEFAULT_EXECD_PORT
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxMetrics
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.Health
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import com.alibaba.opensandbox.sandbox.infrastructure.factory.AdapterFactory
import org.slf4j.LoggerFactory
import java.time.Duration
import java.time.OffsetDateTime

/**
 * Main entrypoint for the Open Sandbox SDK providing secure, isolated execution environments.
 *
 * This class provides a comprehensive interface for interacting with containerized sandbox
 * environments, combining lifecycle management with high-level operations for file system
 * access, command execution, and real-time monitoring.
 *
 * ## Key Features
 *
 * - **Secure Isolation**: Complete Linux OS access in isolated containers
 * - **File System Operations**: Create, read, update, delete files and directories
 * - **Multi-language Execution**: Support for Python, Java, Bash, and other languages
 * - **Real-time Command Execution**: Streaming output with timeout handling
 * - **Resource Management**: CPU, memory, and storage constraints
 * - **Lifecycle Management**: Create, pause, resume, terminate operations
 * - **Health Monitoring**: Automatic readiness detection and status tracking
 *
 * ## Usage Example
 *
 * ```kotlin
 * // Create and configure a sandbox
 * val sandbox = Sandbox.builder()
 *     .image("python:3.11")
 *     .resource(mapOf("cpu" to "1", "memory" to "500Mi"))
 *     .timeout(Duration.ofMinutes(30))
 *     .build()
 *
 * // Use the sandbox
 * sandbox.writeFile("script.py", "print('Hello World')")
 * val result = sandbox.execute("python script.py")
 * println(result.stdout) // Output: Hello World
 *
 * // Always clean up resources
 * sandbox.terminate()
 * ```
 *
 */
class Sandbox internal constructor(
    val id: String,
    private val sandboxService: Sandboxes,
    private val fileSystemService: Filesystem,
    private val commandService: Commands,
    private val healthService: Health,
    private val metricsService: Metrics,
    private val egressService: Egress,
    private val customHealthCheck: ((sandbox: Sandbox) -> Boolean)? = null,
    private val httpClientProvider: HttpClientProvider,
    private val diagnosticsService: Diagnostics,
) : AutoCloseable {
    private val logger = LoggerFactory.getLogger(Sandbox::class.java)

    /**
     * Provides access to file system operations within the sandbox.
     *
     * Allows writing, reading, listing, and deleting files and directories.
     *
     * @return Service for filesystem manipulation
     */
    fun files() = fileSystemService

    /**
     * Provides access to command execution operations.
     *
     * Allows running shell commands, capturing output, and managing processes.
     *
     * @return Service for command execution
     */
    fun commands() = commandService

    /**
     * Provides access to sandbox metrics and monitoring.
     *
     * Allows retrieving resource usage statistics (CPU, memory) and other performance metrics.
     *
     * @return Service for metrics retrieval
     */
    fun metrics() = metricsService

    /**
     * Provides access to sandbox diagnostic log and event descriptors.
     *
     * @return Service for sandbox diagnostics retrieval
     */
    fun diagnostics() = diagnosticsService

    /**
     * Provides access to shared httpclient provider
     *
     * Allows retrieving underlying http client resources initialized with connection config
     */
    fun httpClientProvider() = httpClientProvider

    companion object {
        private val logger = LoggerFactory.getLogger(Sandbox::class.java)

        /**
         * Creates a new [Builder] for fluent sandbox configuration.
         *
         * @return A new Builder instance
         */
        @JvmStatic
        fun builder(): Builder = Builder()

        /**
         * Creates a new [Connector] for fluent sandbox configuration.
         *
         * @return A new Connector instance
         */
        @JvmStatic
        fun connector(): Connector = Connector()

        @JvmStatic
        fun resumer(): Resumer = Resumer()

        /**
         * Initialization result indicating the type of sandbox being initialized.
         */
        private sealed class InitializationResult {
            abstract val id: String

            data class NewSandbox(override val id: String) : InitializationResult()

            data class ExistingSandbox(override val id: String) : InitializationResult()
        }

        /**
         * Common initialization logic for create, connect, and resume operations.
         *
         * @param operationName Operation name for logging
         * @param connectionConfig Connection configuration
         * @param healthCheck Custom health check function
         * @param timeout Timeout for readiness check
         * @param healthCheckPollingInterval Polling interval for health check
         * @param initAction Initialization action that returns the sandbox ID and type
         * @return Fully initialized Sandbox instance
         * @throws SandboxException if initialization fails
         */
        private fun initializeSandbox(
            operationName: String,
            connectionConfig: ConnectionConfig,
            healthCheck: ((Sandbox) -> Boolean)?,
            timeout: Duration,
            healthCheckPollingInterval: Duration,
            skipHealthCheck: Boolean,
            execdPort: Int = DEFAULT_EXECD_PORT,
            initAction: (Sandboxes) -> InitializationResult,
        ): Sandbox {
            logger.info("Starting {} operation", operationName)

            val httpClientProvider = HttpClientProvider(connectionConfig)
            val factory = AdapterFactory(httpClientProvider)
            var initResult: InitializationResult? = null
            var sandboxService: Sandboxes? = null

            try {
                sandboxService = factory.createSandboxes()
                initResult = initAction(sandboxService)

                val sandboxId = initResult.id

                val execdEndpoint =
                    sandboxService.getSandboxEndpoint(
                        sandboxId,
                        execdPort,
                        connectionConfig.useServerProxy,
                    )
                val fileSystemService = factory.createFilesystem(execdEndpoint)
                val commandService = factory.createCommands(execdEndpoint)
                val metricsService = factory.createMetrics(execdEndpoint)
                val healthService = factory.createHealth(execdEndpoint)
                val egressEndpoint =
                    sandboxService.getSandboxEndpoint(
                        sandboxId,
                        DEFAULT_EGRESS_PORT,
                        connectionConfig.useServerProxy,
                    )
                val egressService = factory.createEgress(egressEndpoint)
                val diagnosticsService = factory.createDiagnostics()

                val sandbox =
                    Sandbox(
                        id = sandboxId,
                        sandboxService = sandboxService,
                        fileSystemService = fileSystemService,
                        commandService = commandService,
                        metricsService = metricsService,
                        healthService = healthService,
                        egressService = egressService,
                        customHealthCheck = healthCheck,
                        httpClientProvider = httpClientProvider,
                        diagnosticsService = diagnosticsService,
                    )

                if (!skipHealthCheck) {
                    sandbox.checkReady(timeout, healthCheckPollingInterval)
                    logger.info("{} operation completed for sandbox {}", operationName, sandboxId)
                } else {
                    logger.info(
                        "{} operation completed for sandbox {} (skipHealthCheck=true, sandbox may not be ready yet)",
                        operationName,
                        sandboxId,
                    )
                }

                return sandbox
            } catch (e: Exception) {
                if (initResult is InitializationResult.NewSandbox && sandboxService != null) {
                    try {
                        logger.warn(
                            "Sandbox creation failed during initialization. Attempting to terminate zombie sandbox: {}",
                            initResult.id,
                        )
                        sandboxService.killSandbox(initResult.id)
                    } catch (cleanupEx: Exception) {
                        logger.error("Failed to clean up sandbox {} after creation failure", initResult.id, cleanupEx)
                        e.addSuppressed(cleanupEx)
                    }
                }

                httpClientProvider.close()
                when (e) {
                    is SandboxException -> throw e
                    else -> {
                        logger.error("Unexpected exception during {}", operationName, e)
                        throw SandboxInternalException(
                            message = "Failed to $operationName: ${e.message}",
                            cause = e,
                        )
                    }
                }
            }
        }

        /**
         * Creates a sandbox instance with the provided configuration.
         *
         * @param imageSpec Container image specification
         * @param entrypoint Sandbox entrypoint command
         * @param env Environment variables (optional)
         * @param metadata Metadata for the sandbox (optional)
         * @param timeout Sandbox timeout (automatic termination time)
         * @param readyTimeout Timeout for waiting for sandbox readiness
         * @param resource Resource limits (optional)
         * @param networkPolicy Optional outbound network policy (egress)
         * @param secureAccess Whether to enable secured access for sandbox endpoints
         * @param connectionConfig Connection configuration
         * @param healthCheck Custom health check function (optional)
         * @param healthCheckPollingInterval Polling interval for readiness/health check
         * @param extensions Optional extension parameters for server-side customized behaviors
         * @param volumes Optional list of volume mounts for persistent storage
         * @return Fully configured and ready Sandbox instance
         * @throws SandboxException if sandbox creation or initialization fails
         */
        private fun create(
            imageSpec: SandboxImageSpec?,
            entrypoint: List<String>?,
            snapshotId: String?,
            env: Map<String, String>,
            metadata: Map<String, String>,
            timeout: Duration?,
            readyTimeout: Duration,
            resource: Map<String, String>,
            platform: PlatformSpec?,
            networkPolicy: NetworkPolicy?,
            secureAccess: Boolean,
            connectionConfig: ConnectionConfig,
            healthCheck: ((Sandbox) -> Boolean)? = null,
            healthCheckPollingInterval: Duration,
            extensions: Map<String, String>,
            skipHealthCheck: Boolean,
            volumes: List<Volume>?,
        ): Sandbox {
            val timeoutLabel = if (timeout != null) "${timeout.seconds}s" else "manual-cleanup"
            return initializeSandbox(
                operationName = "create sandbox with startup source ${imageSpec?.image ?: snapshotId} (timeout: $timeoutLabel)",
                connectionConfig = connectionConfig,
                healthCheck = healthCheck,
                timeout = readyTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
            ) { sandboxService ->
                val response =
                    sandboxService.createSandbox(
                        imageSpec,
                        entrypoint,
                        env,
                        metadata,
                        timeout,
                        resource,
                        networkPolicy,
                        extensions,
                        volumes,
                        platform,
                        secureAccess,
                        snapshotId,
                    )
                InitializationResult.NewSandbox(response.id)
            }
        }

        /**
         * Connects to an existing sandbox instance by ID.
         *
         * This method allows you to reconnect to a previously created sandbox that
         * is still running, enabling you to resume work or share sandbox access.
         *
         * @param sandboxId Unique identifier of the existing sandbox
         * @return Connected Sandbox instance
         * @throws SandboxException if connection fails
         */
        private fun connect(
            sandboxId: String,
            connectionConfig: ConnectionConfig,
            healthCheck: ((Sandbox) -> Boolean)? = null,
            connectTimeout: Duration,
            healthCheckPollingInterval: Duration,
            skipHealthCheck: Boolean,
            execdPort: Int = DEFAULT_EXECD_PORT,
        ): Sandbox {
            return initializeSandbox(
                operationName = "connect to sandbox $sandboxId",
                connectionConfig = connectionConfig,
                healthCheck = healthCheck,
                timeout = connectTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
                execdPort = execdPort,
            ) { _ ->
                InitializationResult.ExistingSandbox(sandboxId)
            }
        }

        /**
         * Resumes a paused sandbox and waits until it becomes healthy.
         *
         * This method performs the following steps:
         * 1. Calls the server-side resume operation to transition the sandbox back to RUNNING.
         * 2. Re-resolves the execd endpoint (it may change across pause/resume on some backends).
         * 3. Rebuilds service adapters bound to the endpoint.
         * 4. Waits for readiness/health with polling until [resumeTimeout] elapses.
         *
         * @param sandboxId Sandbox ID to resume
         * @param connectionConfig Connection configuration
         * @param healthCheck Optional custom health check; falls back to [Sandbox.ping]
         * @param resumeTimeout Max time to wait for the sandbox to become ready after resuming
         * @param healthCheckPollingInterval Polling interval for readiness/health check
         * @return Resumed and ready Sandbox instance
         * @throws SandboxException if resume or readiness check fails
         */
        private fun resume(
            sandboxId: String,
            connectionConfig: ConnectionConfig,
            healthCheck: ((Sandbox) -> Boolean)? = null,
            resumeTimeout: Duration,
            healthCheckPollingInterval: Duration,
            skipHealthCheck: Boolean,
        ): Sandbox {
            return initializeSandbox(
                operationName = "resume sandbox $sandboxId",
                connectionConfig = connectionConfig,
                healthCheck = healthCheck,
                timeout = resumeTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
            ) { sandboxService ->
                sandboxService.resumeSandbox(sandboxId)
                InitializationResult.ExistingSandbox(sandboxId)
            }
        }
    }

    /**
     * Gets the current status of this sandbox.
     *
     * @return Current sandbox status including state and metadata
     * @throws SandboxException if status cannot be retrieved
     */
    fun getInfo(): SandboxInfo {
        return sandboxService.getSandboxInfo(id)
    }

    /**
     * Gets the current status of this sandbox.
     *
     * @return Current sandbox status including state and metadata
     * @throws SandboxException if status cannot be retrieved
     */
    fun getEndpoint(port: Int): SandboxEndpoint {
        return sandboxService.getSandboxEndpoint(id, port, httpClientProvider.config.useServerProxy)
    }

    /**
     * Gets a signed endpoint for a service port with an OSEP-0011 route token.
     *
     * @param port The port number to get the endpoint for
     * @param expires Unix epoch seconds for the signed route token expiry
     * @return Signed endpoint information
     */
    fun getSignedEndpoint(
        port: Int,
        expires: Long,
    ): SandboxEndpoint {
        return sandboxService.getSignedSandboxEndpoint(id, port, expires, httpClientProvider.config.useServerProxy)
    }

    /**
     * Gets the current status of this sandbox.
     *
     * @return Current sandbox status including state and metadata
     */
    fun getMetrics(): SandboxMetrics {
        return metricsService.getMetrics(id)
    }

    /**
     * Gets diagnostic log content for this sandbox.
     *
     * @param scope Required diagnostic scope such as "container", "lifecycle", or "all"
     * @return Diagnostic log content descriptor
     * @throws SandboxException if the operation fails
     */
    fun getDiagnosticLogs(scope: String): DiagnosticContent {
        return diagnosticsService.getLogs(id, scope)
    }

    /**
     * Gets diagnostic event content for this sandbox.
     *
     * @param scope Required diagnostic scope such as "runtime", "lifecycle", or "all"
     * @return Diagnostic event content descriptor
     * @throws SandboxException if the operation fails
     */
    fun getDiagnosticEvents(scope: String): DiagnosticContent {
        return diagnosticsService.getEvents(id, scope)
    }

    /**
     * Renew the sandbox expiration time to delay automatic termination.
     *
     * The new expiration time will be set to the current time plus the provided duration.
     *
     * @param timeout Duration to add to the current time to set the new expiration
     * @throws SandboxException if the operation fails
     */
    fun renew(timeout: Duration): SandboxRenewResponse {
        logger.info("Renew sandbox {} timeout, estimated expiration to {}", id, OffsetDateTime.now().plus(timeout))
        return sandboxService.renewSandboxExpiration(id, OffsetDateTime.now().plus(timeout))
    }

    /**
     * Patches sandbox metadata.
     *
     * Non-null values add or replace keys. Null values delete keys.
     */
    fun patchMetadata(patch: Map<String, String?>): SandboxInfo {
        return sandboxService.patchSandboxMetadata(id, patch)
    }

    fun createSnapshot(name: String? = null): SnapshotInfo = sandboxService.createSnapshot(id, name)

    /**
     * Gets current egress policy for this sandbox.
     *
     * @throws SandboxException if operation fails
     */
    fun getEgressPolicy(): NetworkPolicy {
        return egressService.getPolicy()
    }

    /**
     * Patches egress rules for this sandbox using sidecar merge semantics.
     *
     * Incoming rules take priority over existing rules with the same target.
     * Existing rules for other targets remain unchanged. Within one patch payload,
     * the first rule for a target wins. The current defaultAction is preserved.
     *
     * @throws SandboxException if operation fails
     */
    fun patchEgressRules(rules: List<NetworkRule>) {
        egressService.patchRules(rules)
    }

    /**
     * Pauses the sandbox while preserving its state.
     *
     * The sandbox will transition to PAUSED state and can be resumed later.
     * All running processes will be suspended.
     *
     * @throws SandboxException if pause operation fails
     */
    fun pause() {
        logger.info("Pausing sandbox: {}", id)
        sandboxService.pauseSandbox(id)
    }

    /**
     * This method sends a termination signal to the remote sandbox instance, causing it to stop immediately.
     * This is an irreversible operation.
     *
     * Note: This method does NOT close the local `Sandbox` object resources (like connection pools).
     * You should call `close()` or use a try-with-resources block to clean up local resources.
     *
     * @throws SandboxException if termination fails
     */
    fun kill() {
        sandboxService.killSandbox(id)
    }

    /**
     * Closes this resource, relinquishing any underlying resources.
     *
     * This method closes the local HTTP client resources associated with this sandbox instance.
     * It does **NOT** terminate the remote sandbox instance. If you wish to terminate the remote
     * sandbox, call [kill] before closing.
     *
     * If this sandbox was created with a user-managed (shared) connection pool, the pool will NOT be closed.
     * If it was created with a default (dedicated) pool, the pool will be evicted and destroyed.
     */
    override fun close() {
        try {
            httpClientProvider.close()
        } catch (e: Exception) {
            logger.warn("Error closing resources", e)
        }
    }

    /**
     * Waits for the sandbox to pass a custom health check with polling.
     *
     * @param timeout Maximum time to wait for health check to pass
     * @param pollingInterval Time between health check attempts
     * @throws SandboxReadyTimeoutException if health check doesn't pass within timeout
     * @throws SandboxException if health check fails
     */
    fun checkReady(
        timeout: Duration,
        pollingInterval: Duration,
    ) {
        logger.info("Waiting for sandbox {} to pass health check (timeout: {}s)", id, timeout.seconds)

        val deadline = System.currentTimeMillis() + timeout.toMillis()
        var attempt = 0
        var lastException: Throwable? = null

        while (System.currentTimeMillis() < deadline) {
            attempt++
            logger.debug("Health check attempt #{} for sandbox {}", attempt, id)

            val isHealthy =
                try {
                    isHealthy()
                } catch (e: Exception) {
                    lastException = e
                    logger.debug("Health check attempt #{} failed with exception: {}", attempt, e.message)
                    false
                }

            if (isHealthy) {
                logger.info("Sandbox {} passed health check after {} attempts", id, attempt)
                return
            }

            if (lastException == null) {
                logger.debug("Health check attempt #{} returned false", attempt)
            }

            Thread.sleep(pollingInterval.toMillis())
        }

        val errorDetail =
            if (lastException != null) {
                "Last error: ${lastException.message}"
            } else {
                "Check returned false continuously"
            }

        val context = "domain=${httpClientProvider.config.getDomain()}, useServerProxy=${httpClientProvider.config.useServerProxy}"
        var suggestion =
            "If this sandbox runs in Docker bridge or remote-network mode, consider enabling useServerProxy=true."
        if (!httpClientProvider.config.useServerProxy) {
            suggestion += " You can also configure server-side [docker].host_ip for direct endpoint access."
        }

        val finalMessage =
            "Sandbox health check timed out after ${timeout.seconds}s ($attempt attempts). $errorDetail " +
                "Connection context: $context. $suggestion"

        logger.error(finalMessage, lastException)

        throw SandboxReadyTimeoutException(
            message = finalMessage,
        )
    }

    /**
     * Checks if the sandbox is healthy and responsive.
     *
     * @return true if sandbox is healthy, false otherwise
     */
    fun isHealthy(): Boolean {
        return customHealthCheck?.invoke(this) ?: ping()
    }

    /**
     * Ping execd
     *
     * @return `true` if execd is reachable and healthy.
     */
    fun ping(): Boolean {
        return healthService.ping(id)
    }

    /**
     * Fluent connector for connecting to existing sandbox instances.
     *
     * This class provides a type-safe, fluent interface for configuring connection
     * parameters to connect to a running sandbox instance.
     *
     * ## Basic Usage
     *
     * ```kotlin
     * val sandbox = Sandbox.connector()
     *     .sandboxId("existing-sandbox-id")
     *     .build()
     * ```
     *
     * ## Advanced Configuration
     *
     * ```kotlin
     * val sandbox = Sandbox.connector()
     *     .sandboxId("existing-sandbox-id")
     *     .apiKey("your-api-key")
     *     .domain("api.custom-domain.com/v1")
     *     .requestTimeout(Duration.ofSeconds(60))
     *     .healthCheck { sandbox -> sandbox.isHealthy() }
     *     .build()
     * ```
     */
    class Connector internal constructor() {
        /**
         * Sandbox ID to connect to
         */
        private var sandboxId: String? = null

        /**
         * Connection config
         */
        private var connectionConfig: ConnectionConfig? = null

        /**
         * Health check logic
         */
        private var healthCheck: ((Sandbox) -> Boolean)? = null

        /**
         * Max time to wait for the sandbox to become ready after connecting
         */
        private var connectTimeout: Duration = Duration.ofSeconds(30)

        /**
         * Polling interval for readiness/health check while waiting for resume
         */
        private var healthCheckPollingInterval: Duration = Duration.ofMillis(200)

        /**
         * When true, do NOT wait for sandbox readiness/health during [connect].
         *
         * Default is false (wait until ready).
         */
        private var skipHealthCheck: Boolean = false

        /**
         * Custom execd port. Defaults to [DEFAULT_EXECD_PORT] (44772) when not set.
         */
        private var execdPort: Int = DEFAULT_EXECD_PORT

        /**
         * Sets the sandbox ID to connect to.
         *
         * @param sandboxId ID of the existing sandbox
         * @return This connector for method chaining
         * @throws InvalidArgumentException if sandboxId is blank
         */
        fun sandboxId(sandboxId: String): Connector {
            this.sandboxId = sandboxId
            return this
        }

        fun healthCheck(healthCheck: (Sandbox) -> Boolean): Connector {
            this.healthCheck = healthCheck
            return this
        }

        fun connectionConfig(connectionConfig: ConnectionConfig): Connector {
            this.connectionConfig = connectionConfig
            return this
        }

        /**
         * Sets the max time to wait for readiness after the connect operation.
         */
        fun connectTimeout(timeout: Duration): Connector {
            this.connectTimeout = timeout
            return this
        }

        /**
         * Sets the polling interval used while waiting for readiness after connecting.
         */
        fun healthCheckPollingInterval(pollingInterval: Duration): Connector {
            this.healthCheckPollingInterval = pollingInterval
            return this
        }

        /**
         * Skip readiness/health check during [connect]. The returned sandbox may not be ready yet.
         */
        fun skipHealthCheck(skip: Boolean = true): Connector {
            this.skipHealthCheck = skip
            return this
        }

        /**
         * Sets the execd port used to communicate with the sandbox.
         *
         * Defaults to [DEFAULT_EXECD_PORT] (44772) when not set.
         */
        fun execdPort(port: Int): Connector {
            this.execdPort = port
            return this
        }

        /**
         * Connects to the existing sandbox with the configured parameters.
         *
         * This method performs the following steps:
         * 1. Validates all required configuration
         * 2. Delegates to Sandbox.connect() to connect to the sandbox
         * 3. Returns a connected Sandbox instance
         *
         * @return Connected Sandbox instance
         * @throws InvalidArgumentException if required configuration is missing or invalid
         * @throws SandboxException if sandbox connection fails
         */
        fun connect(): Sandbox {
            // Validate required configuration
            val id =
                sandboxId ?: throw InvalidArgumentException(
                    message = "Sandbox ID must be specified",
                )
            return connect(
                sandboxId = id,
                connectionConfig = connectionConfig ?: ConnectionConfig.builder().build(),
                healthCheck = healthCheck,
                connectTimeout = connectTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
                execdPort = execdPort,
            )
        }
    }

    /**
     * Fluent builder for creating and configuring sandbox instances.
     *
     * This class provides a type-safe, fluent interface for configuring all aspects
     * of sandbox creation, from sandbox images and resource limits to environment
     * variables and lifecycle settings.
     *
     * ## Basic Usage
     *
     * ```kotlin
     * val sandbox = Sandbox.builder()
     *     .image("python:3.11")
     *     .build()
     * ```
     *
     * ## Advanced Configuration
     *
     * ```kotlin
     * val sandbox = Sandbox.builder()
     *     .image("myregistry.com/app:latest")
     *     .imageAuth("username", "password")
     *     .entrypoint("python", "-u", "app.py")
     *     .resource {
     *         put("cpu", "1000m")
     *         put("memory", "2Gi")
     *     }
     *     .env {
     *         put("LOG_LEVEL", "info")
     *     }
     *     .metadata {
     *         put("project", "my-project")
     *         put("team", "backend")
     *     }
     *     .timeout(Duration.ofMinutes(30))
     *     .readyTimeout(Duration.ofSeconds(120))
     *     .build()
     * ```
     */
    class Builder internal constructor() {
        /**
         * Image config
         */
        private var imageSpec: SandboxImageSpec? = null
        private var snapshotId: String? = null

        /**
         * Sandbox entrypoint
         */
        private var entrypoint: List<String> = listOf("tail", "-f", "/dev/null")

        /**
         * Resource limits config
         */
        private val resource = mutableMapOf("cpu" to "1", "memory" to "2Gi")

        /**
         * Env
         */
        private val env = mutableMapOf<String, String>()

        /**
         * Metadata
         */
        private val metadata = mutableMapOf<String, String>()

        /**
         * Optional extension parameters for server-side custom behaviors.
         *
         * This map is treated as opaque and is sent to the server as-is.
         * Prefer namespaced keys (e.g. `storage.id`) to avoid collisions.
         */
        private val extensions = mutableMapOf<String, String>()

        /**
         * Optional outbound network policy (egress).
         */
        private var networkPolicy: NetworkPolicy? = null

        /**
         * Enables secured access for sandbox endpoints.
         */
        private var secureAccess: Boolean = false

        /**
         * Optional runtime platform constraint used for sandbox provisioning.
         */
        private var platform: PlatformSpec? = null

        /**
         * Optional list of volume mounts for persistent storage.
         */
        private val volumes = mutableListOf<Volume>()

        /**
         * Lifecycle config
         */
        private var timeout: Duration? = Duration.ofSeconds(600)
        private var readyTimeout: Duration = Duration.ofSeconds(30)
        private var healthCheckPollingInterval: Duration = Duration.ofMillis(200)
        private var healthCheck: ((Sandbox) -> Boolean)? = null

        /**
         * When true, do NOT wait for sandbox readiness/health during [build].
         *
         * Default is false (wait until ready).
         */
        private var skipHealthCheck: Boolean = false

        /**
         * Connection config
         */
        private var connectionConfig: ConnectionConfig? = null

        /**
         * Sets the sandbox image for the sandbox.
         *
         * @param image Sandbox image reference (e.g., "ubuntu:22.04", "python:3.11")
         * @return This builder for method chaining
         * @throws InvalidArgumentException if image is blank
         */
        fun image(image: String): Builder {
            if (image.isBlank()) {
                throw InvalidArgumentException(
                    message = "Image cannot be blank",
                )
            }
            this.imageSpec =
                SandboxImageSpec.builder()
                    .image(image)
                    .build()
            this.snapshotId = null
            return this
        }

        /**
         * Sets the sandbox image specification.
         *
         * @param imageSpec Complete image specification including image and optional auth
         * @return This builder for method chaining
         */
        fun imageSpec(imageSpec: SandboxImageSpec): Builder {
            this.imageSpec = imageSpec
            this.snapshotId = null
            return this
        }

        fun snapshotId(snapshotId: String): Builder {
            if (snapshotId.isBlank()) {
                throw InvalidArgumentException(message = "Snapshot ID cannot be blank")
            }
            this.snapshotId = snapshotId
            this.imageSpec = null
            return this
        }

        /**
         * Sets the entrypoint command for the sandbox.
         *
         * @param entrypoint List of command and arguments to use as entrypoint
         * @return This builder for method chaining
         */
        fun entrypoint(entrypoint: List<String>): Builder {
            this.entrypoint = entrypoint
            return this
        }

        /**
         * Sets the entrypoint command for the sandbox.
         *
         * @param entrypoint Vararg command and arguments to use as entrypoint
         * @return This builder for method chaining
         */
        fun entrypoint(vararg entrypoint: String): Builder {
            this.entrypoint = entrypoint.toList()
            return this
        }

        /**
         * Sets resource limits for the sandbox using a fluent configuration block.
         *
         * @param configure Configuration block for resource limits
         * @return This builder for method chaining
         */
        fun resource(configure: MutableMap<String, String>.() -> Unit): Builder {
            resource.configure()
            return this
        }

        /**
         * Sets resource limits for the sandbox.
         *
         * @param resource Resource limits map
         * @return This builder for method chaining
         */
        fun resource(resource: Map<String, String>): Builder {
            this.resource.clear()
            this.resource.putAll(resource)
            return this
        }

        /**
         * Adds a single environment variable.
         *
         * @param key Environment variable name
         * @param value Environment variable value
         * @return This builder for method chaining
         */
        fun env(
            key: String,
            value: String,
        ): Builder {
            if (key.isBlank()) {
                throw InvalidArgumentException(
                    message = "Environment variable key cannot be blank",
                )
            }
            env[key] = value
            return this
        }

        /**
         * Adds multiple environment variables.
         *
         * @param env Map of environment variables to add
         * @return This builder for method chaining
         */
        fun env(env: Map<String, String>): Builder {
            this.env.putAll(env)
            return this
        }

        /**
         * Configures environment variables using a fluent configuration block.
         *
         * @param configure Configuration block that receives a mutable map
         * @return This builder for method chaining
         */
        fun env(configure: MutableMap<String, String>.() -> Unit): Builder {
            env.configure()
            return this
        }

        /**
         * Adds a single metadata entry.
         *
         * @param key Metadata key
         * @param value Metadata value
         * @return This builder for method chaining
         */
        fun metadata(
            key: String,
            value: String,
        ): Builder {
            if (key.isBlank()) {
                throw InvalidArgumentException(
                    message = "Metadata key cannot be blank",
                )
            }
            metadata[key] = value
            return this
        }

        /**
         * Adds multiple metadata entries.
         *
         * @param metadata Map of metadata to add
         * @return This builder for method chaining
         */
        fun metadata(metadata: Map<String, String>): Builder {
            this.metadata.putAll(metadata)
            return this
        }

        /**
         * Configures metadata using a fluent configuration block.
         *
         * @param configure Configuration block that receives a mutable map
         * @return This builder for method chaining
         */
        fun metadata(configure: MutableMap<String, String>.() -> Unit): Builder {
            metadata.configure()
            return this
        }

        /**
         * Sets a sandbox outbound network policy (egress).
         */
        fun networkPolicy(networkPolicy: NetworkPolicy): Builder {
            this.networkPolicy = networkPolicy
            return this
        }

        /**
         * Configures a sandbox outbound network policy (egress).
         */
        fun networkPolicy(configure: NetworkPolicy.Builder.() -> Unit): Builder {
            val builder = NetworkPolicy.builder()
            builder.configure()
            this.networkPolicy = builder.build()
            return this
        }

        /**
         * Enables or disables secured access for sandbox endpoints.
         *
         * Default is false for backward compatibility. When true, the server may
         * return required endpoint headers that SDK calls must include.
         */
        @JvmOverloads
        fun secureAccess(enabled: Boolean = true): Builder {
            this.secureAccess = enabled
            return this
        }

        /**
         * Sets an explicit runtime platform constraint.
         */
        fun platform(platform: PlatformSpec): Builder {
            this.platform = platform
            return this
        }

        /**
         * Configures runtime platform constraint for sandbox provisioning.
         */
        fun platform(configure: PlatformSpec.Builder.() -> Unit): Builder {
            val builder = PlatformSpec.builder()
            builder.configure()
            this.platform = builder.build()
            return this
        }

        /**
         * Adds a single volume mount.
         *
         * @param volume Volume configuration
         * @return This builder for method chaining
         */
        fun volume(volume: Volume): Builder {
            this.volumes.add(volume)
            return this
        }

        /**
         * Adds multiple volume mounts.
         *
         * @param volumes List of volume configurations to add
         * @return This builder for method chaining
         */
        fun volumes(volumes: List<Volume>): Builder {
            this.volumes.addAll(volumes)
            return this
        }

        /**
         * Configures a volume mount using a fluent configuration block.
         *
         * @param configure Configuration block for Volume.Builder
         * @return This builder for method chaining
         */
        fun volume(configure: Volume.Builder.() -> Unit): Builder {
            val builder = Volume.builder()
            builder.configure()
            this.volumes.add(builder.build())
            return this
        }

        /**
         * Adds a single extension parameter.
         *
         * Extensions are opaque client-side and are passed through to the server.
         * Prefer stable, namespaced keys (e.g. `storage.id`).
         *
         * @throws InvalidArgumentException if [key] is blank
         */
        fun extension(
            key: String,
            value: String,
        ): Builder {
            if (key.isBlank()) {
                throw InvalidArgumentException(
                    message = "Extension key cannot be blank",
                )
            }
            extensions[key] = value
            return this
        }

        /**
         * Adds multiple extension parameters.
         *
         * Extensions are opaque client-side and are passed through to the server.
         */
        fun extensions(extensions: Map<String, String>): Builder {
            this.extensions.putAll(extensions)
            return this
        }

        /**
         * Configures extension parameters using a fluent configuration block.
         *
         * Extensions are opaque client-side and are passed through to the server.
         */
        fun extensions(configure: MutableMap<String, String>.() -> Unit): Builder {
            extensions.configure()
            return this
        }

        /**
         * Sets the sandbox timeout (automatic termination time).
         *
         * @param timeout Maximum sandbox lifetime. Pass null to require explicit cleanup.
         * @return This builder for method chaining
         * @throws InvalidArgumentException if timeout is negative or zero
         */
        fun timeout(timeout: Duration?): Builder {
            if (timeout != null && (timeout.isNegative || timeout.isZero)) {
                throw InvalidArgumentException(
                    message = "Timeout must be positive, got: $timeout",
                )
            }
            this.timeout = timeout
            return this
        }

        /**
         * Disables automatic expiration and requires explicit cleanup.
         *
         * This provides a stable Java interop entrypoint for non-expiring sandboxes.
         */
        fun manualCleanup(): Builder {
            this.timeout = null
            return this
        }

        /**
         * Sets the timeout for waiting for sandbox readiness.
         *
         * @param readyTimeout Maximum time to wait for sandbox to become ready
         * @return This builder for method chaining
         * @throws InvalidArgumentException if timeout is negative or zero
         */
        fun readyTimeout(readyTimeout: Duration): Builder {
            if (readyTimeout.isNegative || readyTimeout.isZero) {
                throw InvalidArgumentException(
                    message = "Ready timeout must be positive, got: $readyTimeout",
                )
            }
            this.readyTimeout = readyTimeout
            return this
        }

        /**
         * Sets the interval between readiness polling attempts.
         *
         * @param pollingInterval Time between readiness checks
         * @return This builder for method chaining
         * @throws InvalidArgumentException if interval is negative or zero
         */
        fun healthCheckPollingInterval(pollingInterval: Duration): Builder {
            if (pollingInterval.isNegative || pollingInterval.isZero) {
                throw InvalidArgumentException(
                    message = "Ready polling interval must be positive, got: $pollingInterval",
                )
            }
            this.healthCheckPollingInterval = pollingInterval
            return this
        }

        fun healthCheck(healthCheck: (Sandbox) -> Boolean): Builder {
            this.healthCheck = healthCheck
            return this
        }

        /**
         * Skip readiness/health check during [build]. The returned sandbox may not be ready yet.
         */
        fun skipHealthCheck(skip: Boolean = true): Builder {
            this.skipHealthCheck = skip
            return this
        }

        fun connectionConfig(connectionConfig: ConnectionConfig): Builder {
            this.connectionConfig = connectionConfig
            return this
        }

        /**
         * Creates and starts the sandbox with the configured parameters.
         *
         * This method performs the following steps:
         * 1. Validates all required configuration
         * 2. Delegates to Sandbox.create() to create the sandbox
         * 3. Returns a fully initialized Sandbox instance
         *
         * @return Fully configured and ready Sandbox instance
         * @throws InvalidArgumentException if required configuration is missing or invalid
         * @throws SandboxException if sandbox creation or initialization fails
         */
        fun build(): Sandbox {
            // Validate required configuration
            val spec =
                imageSpec
            if ((spec == null) == (snapshotId == null)) {
                throw InvalidArgumentException(
                    message = "Exactly one of sandbox image or snapshotId must be specified",
                )
            }
            if (spec != null && spec.image.isBlank()) {
                throw InvalidArgumentException("Sandbox image cannot be blank")
            }

            return create(
                imageSpec = spec,
                snapshotId = snapshotId,
                entrypoint = entrypoint,
                env = env,
                metadata = metadata,
                timeout = timeout,
                readyTimeout = readyTimeout,
                resource = resource,
                platform = platform,
                networkPolicy = networkPolicy,
                secureAccess = secureAccess,
                extensions = extensions,
                connectionConfig = connectionConfig ?: ConnectionConfig.builder().build(),
                healthCheckPollingInterval = healthCheckPollingInterval,
                healthCheck = healthCheck,
                skipHealthCheck = skipHealthCheck,
                volumes = if (volumes.isEmpty()) null else volumes.toList(),
            )
        }
    }

    /**
     * Fluent resumer for resuming paused sandbox instances.
     *
     * This class provides a type-safe, fluent interface for configuring connection parameters
     * and readiness behavior when resuming an existing sandbox.
     *
     * ## Basic Usage
     *
     * ```kotlin
     * val sandbox = Sandbox.resumer()
     *     .sandboxId(existingSandboxId)
     *     .resume()
     * ```
     *
     * ## Advanced Configuration
     *
     * ```kotlin
     * val sandbox = Sandbox.resumer()
     *     .sandboxId(existingSandboxId)
     *     .connectionConfig(ConnectionConfig.builder().apiKey("...").build())
     *     .resumeTimeout(Duration.ofSeconds(60))
     *     .healthCheckPollingInterval(Duration.ofMillis(200))
     *     .healthCheck { it.isHealthy() }
     *     .resume()
     * ```
     */
    class Resumer internal constructor() {
        /**
         * Sandbox ID to resume
         */
        private var sandboxId: String? = null

        /**
         * Connection config
         */
        private var connectionConfig: ConnectionConfig? = null

        /**
         * Health check logic
         */
        private var healthCheck: ((Sandbox) -> Boolean)? = null

        /**
         * Max time to wait for the sandbox to become ready after resuming
         */
        private var resumeTimeout: Duration = Duration.ofSeconds(30)

        /**
         * Polling interval for readiness/health check while waiting for resume
         */
        private var healthCheckPollingInterval: Duration = Duration.ofMillis(200)

        /**
         * When true, do NOT wait for sandbox readiness/health during [resume].
         *
         * Default is false (wait until ready).
         */
        private var skipHealthCheck: Boolean = false

        /**
         * Sets the sandbox ID to resume.
         *
         * @param sandboxId ID of the paused sandbox
         * @return This resumer for method chaining
         */
        fun sandboxId(sandboxId: String): Resumer {
            this.sandboxId = sandboxId
            return this
        }

        /**
         * Sets a custom health check used by [Sandbox.checkReady] after resuming.
         *
         * If not set, [Sandbox.ping] will be used.
         */
        fun healthCheck(healthCheck: (Sandbox) -> Boolean): Resumer {
            this.healthCheck = healthCheck
            return this
        }

        /**
         * Sets the connection configuration used to talk to the Open Sandbox API.
         */
        fun connectionConfig(connectionConfig: ConnectionConfig): Resumer {
            this.connectionConfig = connectionConfig
            return this
        }

        /**
         * Sets the max time to wait for readiness after the resume operation.
         */
        fun resumeTimeout(timeout: Duration): Resumer {
            this.resumeTimeout = timeout
            return this
        }

        /**
         * Sets the polling interval used while waiting for readiness after resuming.
         */
        fun healthCheckPollingInterval(pollingInterval: Duration): Resumer {
            this.healthCheckPollingInterval = pollingInterval
            return this
        }

        /**
         * Skip readiness/health check during [resume]. The returned sandbox may not be ready yet.
         */
        fun skipHealthCheck(skip: Boolean = true): Resumer {
            this.skipHealthCheck = skip
            return this
        }

        /**
         * Resumes the sandbox with the configured parameters.
         *
         * This method validates required configuration, performs the server-side resume,
         * rebuilds service adapters, and waits for readiness.
         *
         * @return Resumed and ready Sandbox instance
         * @throws InvalidArgumentException if sandboxId is missing
         * @throws SandboxException if resume or readiness check fails
         */
        fun resume(): Sandbox {
            val id =
                sandboxId ?: throw InvalidArgumentException(
                    message = "Sandbox ID must be specified",
                )

            return resume(
                sandboxId = id,
                connectionConfig = connectionConfig ?: ConnectionConfig.builder().build(),
                healthCheck = healthCheck,
                resumeTimeout = resumeTimeout,
                healthCheckPollingInterval = healthCheckPollingInterval,
                skipHealthCheck = skipHealthCheck,
            )
        }
    }
}
