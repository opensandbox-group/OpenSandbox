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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSandboxInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedSnapshotInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PlatformSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxCreateResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxImageSpec
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxRenewResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotFilter
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SnapshotInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Volume
import java.time.Duration
import java.time.OffsetDateTime

/**
 * Core sandbox lifecycle management service.
 *
 * This service provides a clean abstraction over sandbox creation, management,
 * and termination operations, completely isolating business logic from API implementation details.
 */
interface Sandboxes {
    /**
     * Creates a new sandbox with the specified configuration.
     *
     * @param spec Container image specification for provisioning the sandbox
     * @param entrypoint The command to run as the sandbox's main process (e.g. `["python", "/app/main.py"]`)
     * @param env Environment variables injected into the sandbox runtime
     * @param metadata User-defined metadata used for management and filtering
     * @param timeout Sandbox lifetime. Pass null to require explicit cleanup.
     * @param resource Runtime resource limits (e.g. cpu/memory). Exact semantics are server-defined
     * @param platform Optional runtime platform constraint used for provisioning
     * @param networkPolicy Optional outbound network policy (egress)
     * @param secureAccess Whether to enable secured access for sandbox endpoints
     * @param extensions Opaque extension parameters passed through to the server as-is. Prefer namespaced keys
     * @param volumes Optional list of volume mounts for persistent storage
     * @param snapshotId Optional snapshot identifier used to restore a sandbox instead of booting from an image
     * @return Sandbox creation response containing the sandbox id
     */
    fun createSandbox(
        spec: SandboxImageSpec?,
        entrypoint: List<String>?,
        env: Map<String, String>,
        metadata: Map<String, String>,
        timeout: Duration?,
        resource: Map<String, String>,
        networkPolicy: NetworkPolicy?,
        extensions: Map<String, String>,
        volumes: List<Volume>?,
        platform: PlatformSpec? = null,
        secureAccess: Boolean = false,
        snapshotId: String? = null,
    ): SandboxCreateResponse

    /**
     * Retrieves information about an existing sandbox.
     *
     * @param sandboxId Unique identifier of the sandbox
     * @return Current sandbox information
     */
    fun getSandboxInfo(sandboxId: String): SandboxInfo

    /**
     * Lists sandboxes with optional filtering.
     *
     * @param filter Optional filter criteria
     * @return List of sandbox information matching the filter
     */
    fun listSandboxes(filter: SandboxFilter): PagedSandboxInfos

    /**
     * Patches sandbox metadata.
     *
     * @param sandboxId Unique identifier of the sandbox
     * @param patch Metadata merge patch. Non-null values add or replace keys; null values delete keys
     * @return Current sandbox information after applying the patch
     */
    fun patchSandboxMetadata(
        sandboxId: String,
        patch: Map<String, String?>,
    ): SandboxInfo

    fun createSnapshot(
        sandboxId: String,
        name: String? = null,
    ): SnapshotInfo

    fun getSnapshot(snapshotId: String): SnapshotInfo

    fun listSnapshots(filter: SnapshotFilter): PagedSnapshotInfos

    fun deleteSnapshot(snapshotId: String)

    /**
     * Get sandbox endpoint
     *
     * @param sandboxId sandbox id
     * @param port endpoint port number
     * @return Target sandbox endpoint
     */
    fun getSandboxEndpoint(
        sandboxId: String,
        port: Int,
    ): SandboxEndpoint

    /**
     * Get sandbox endpoint
     *
     * @param sandboxId sandbox id
     * @param port endpoint port number
     * @param useServerProxy whether to use server proxy for endpoint (default false)
     * @return Target sandbox endpoint
     */
    fun getSandboxEndpoint(
        sandboxId: String,
        port: Int,
        useServerProxy: Boolean,
    ): SandboxEndpoint

    /**
     * Get signed sandbox endpoint with an OSEP-0011 route token.
     *
     * @param sandboxId sandbox id
     * @param port endpoint port number
     * @param expires Unix epoch seconds for the signed route token expiry
     * @param useServerProxy whether to use server proxy for endpoint (default false)
     * @return Target sandbox endpoint
     */
    fun getSignedSandboxEndpoint(
        sandboxId: String,
        port: Int,
        expires: Long,
        useServerProxy: Boolean = false,
    ): SandboxEndpoint

    /**
     * Pauses a running sandbox, preserving its state.
     *
     * @param sandboxId Unique identifier of the sandbox
     */
    fun pauseSandbox(sandboxId: String)

    /**
     * Resumes a paused sandbox.
     *
     * @param sandboxId Unique identifier of the sandbox
     */
    fun resumeSandbox(sandboxId: String)

    /**
     * Renew the expiration time of a sandbox.
     *
     * @param sandboxId Unique identifier of the sandbox
     * @param newExpirationTime New expiration timestamp
     *
     * @return Sandbox renew response with new expire info
     */
    fun renewSandboxExpiration(
        sandboxId: String,
        newExpirationTime: OffsetDateTime,
    ): SandboxRenewResponse

    /**
     * Terminates a sandbox and releases all associated resources.
     *
     * @param sandboxId Unique identifier of the sandbox
     */
    fun killSandbox(sandboxId: String)
}
