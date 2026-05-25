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
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSnapshotInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import com.alibaba.opensandbox.sandbox.infrastructure.factory.AdapterFactory
import org.slf4j.LoggerFactory
import java.time.Duration
import java.time.OffsetDateTime

/**
 * Sandbox management interface for administrative operations and monitoring sandbox instances.
 *
 * This class provides a centralized interface for managing sandbox instances,
 * enabling administrative operations and sandbox discovery.
 * It focuses on high-level management operations rather than individual sandbox interactions.
 *
 * ## Key Features
 *
 * - **Sandbox Discovery**: List and filter sandbox instances by various criteria
 * - **Administrative Operations**: Individual sandbox management operations
 * - **Connection Pool Management**: Efficient HTTP client reuse for multiple operations
 *
 * ## Usage Example
 *
 * ```kotlin
 * val manager = SandboxManager.builder()
 *     .connectionConfig(connectionConfig)
 *     .build()
 *
 * // List all running sandboxes
 * val runningSandboxes = manager.listSandboxInfos(
 *     SandboxFilter.builder().state("RUNNING").build()
 * )
 *
 * // Individual operations
 * val sandboxId = "sandbox-id"
 * manager.getSandboxInfo(sandboxId)
 * manager.pauseSandbox(sandboxId)
 * manager.resumeSandbox(sandboxId)
 * manager.killSandbox(sandboxId)
 *
 * // Cleanup
 * manager.close()
 * ```
 *
 * **Note**: This class is designed for administrative operations.
 * For individual sandbox interactions, use the [Sandbox] class directly.
 */
class SandboxManager internal constructor(
    private val sandboxService: Sandboxes,
    private val httpClientProvider: HttpClientProvider,
    private val diagnosticsService: Diagnostics,
) : AutoCloseable {
    private val logger = LoggerFactory.getLogger(SandboxManager::class.java)

    /**
     * Provides access to shared httpclient provider
     *
     * Allows retrieving underlying http client resources initialized with connection config
     */
    fun httpClientProvider() = httpClientProvider

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()

        internal fun create(connectionConfig: ConnectionConfig): SandboxManager {
            val httpClientProvider = HttpClientProvider(connectionConfig)
            val factory = AdapterFactory(httpClientProvider)
            val sandboxService = factory.createSandboxes()
            val diagnosticsService = factory.createDiagnostics()
            return SandboxManager(sandboxService, httpClientProvider, diagnosticsService)
        }
    }

    fun listSandboxInfos(filter: SandboxFilter): PagedSandboxInfos {
        return sandboxService.listSandboxes(filter)
    }

    /**
     * Gets information for a single sandbox by its ID.
     *
     * @param sandboxId Sandbox ID to retrieve information for
     * @return SandboxInfo for the specified sandbox
     * @throws SandboxException if the operation fails
     */
    fun getSandboxInfo(sandboxId: String): SandboxInfo {
        logger.debug("Getting info for sandbox: {}", sandboxId)
        return sandboxService.getSandboxInfo(sandboxId)
    }

    /**
     * Gets diagnostic log content for a sandbox by ID.
     *
     * @param sandboxId Sandbox ID to retrieve diagnostics for
     * @param scope Required diagnostic scope such as "container", "lifecycle", or "all"
     * @return Diagnostic log content descriptor
     * @throws SandboxException if the operation fails
     */
    fun getDiagnosticLogs(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent {
        return diagnosticsService.getLogs(sandboxId, scope)
    }

    /**
     * Gets diagnostic event content for a sandbox by ID.
     *
     * @param sandboxId Sandbox ID to retrieve diagnostics for
     * @param scope Required diagnostic scope such as "runtime", "lifecycle", or "all"
     * @return Diagnostic event content descriptor
     * @throws SandboxException if the operation fails
     */
    fun getDiagnosticEvents(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent {
        return diagnosticsService.getEvents(sandboxId, scope)
    }

    /**
     * Patches metadata for a single sandbox.
     *
     * @param sandboxId Sandbox ID to patch
     * @param patch Metadata merge patch. Non-null values add or replace keys; null values delete keys
     * @return SandboxInfo for the patched sandbox
     * @throws SandboxException if the operation fails
     */
    fun patchSandboxMetadata(
        sandboxId: String,
        patch: Map<String, String?>,
    ): SandboxInfo {
        logger.info("Patching metadata for sandbox: {}", sandboxId)
        return sandboxService.patchSandboxMetadata(sandboxId, patch)
    }

    /**
     * Terminates a single sandbox.
     *
     * @param sandboxId Sandbox ID to terminate
     * @throws SandboxException if the operation fails
     */
    fun killSandbox(sandboxId: String) {
        logger.info("Terminating sandbox: {}", sandboxId)
        sandboxService.killSandbox(sandboxId)
        logger.info("Successfully terminated sandbox: {}", sandboxId)
    }

    /**
     * Renew expiration time for a single sandbox.
     *
     * The new expiration time will be set to the current time plus the provided duration.
     *
     * @param sandboxId Sandbox ID to renew
     * @param timeout Duration to add to the current time to set the new expiration
     * @throws SandboxException if the operation fails
     */
    fun renewSandbox(
        sandboxId: String,
        timeout: Duration,
    ): SandboxRenewResponse {
        logger.info("Renew expiration for sandbox {} to {}", sandboxId, OffsetDateTime.now().plus(timeout))
        return sandboxService.renewSandboxExpiration(sandboxId, OffsetDateTime.now().plus(timeout))
    }

    /**
     * Pauses a single sandbox while preserving its state.
     *
     * @param sandboxId Sandbox ID to pause
     * @throws SandboxException if the operation fails
     */
    fun pauseSandbox(sandboxId: String) {
        logger.info("Pausing sandbox: {}", sandboxId)
        sandboxService.pauseSandbox(sandboxId)
    }

    /**
     * Resumes a previously paused sandbox.
     *
     * @param sandboxId Sandbox ID to resume
     * @throws SandboxException if the operation fails
     */
    fun resumeSandbox(sandboxId: String) {
        logger.info("Resuming sandbox: {}", sandboxId)
        sandboxService.resumeSandbox(sandboxId)
    }

    fun createSnapshot(
        sandboxId: String,
        name: String? = null,
    ): SnapshotInfo = sandboxService.createSnapshot(sandboxId, name)

    fun getSnapshot(snapshotId: String): SnapshotInfo = sandboxService.getSnapshot(snapshotId)

    fun listSnapshots(filter: SnapshotFilter): PagedSnapshotInfos = sandboxService.listSnapshots(filter)

    fun deleteSnapshot(snapshotId: String) = sandboxService.deleteSnapshot(snapshotId)

    /**
     * Closes this resource, relinquishing any underlying resources.
     *
     * This method closes the local HTTP client resources associated with this sandbox manager instance.
     */
    override fun close() {
        try {
            httpClientProvider.close()
        } catch (e: Exception) {
            logger.warn("Error closing resources", e)
        }
    }

    class Builder internal constructor() {
        /**
         * Connection config
         */
        private var connectionConfig: ConnectionConfig? = null

        fun connectionConfig(connectionConfig: ConnectionConfig): Builder {
            this.connectionConfig = connectionConfig
            return this
        }

        /**
         * Creates the sandbox manager with the configured parameters.
         *
         * @return Fully configured SandboxManager instance
         * @throws InvalidArgumentException if required configuration is missing or invalid
         * @throws SandboxException if manager creation fails
         */
        fun build(): SandboxManager {
            return SandboxManager.create(
                connectionConfig = connectionConfig ?: ConnectionConfig.builder().build(),
            )
        }
    }
}
