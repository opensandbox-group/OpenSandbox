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

package com.alibaba.opensandbox.sandbox.domain.models.sandboxes

import java.time.OffsetDateTime

/**
 * High-level lifecycle state of the sandbox.
 *
 * Common state values:
 * - Pending: Sandbox is being provisioned
 * - Running: Sandbox is running and ready to accept requests
 * - Pausing: Sandbox is in the process of pausing
 * - Paused: Sandbox has been paused while retaining its state
 * - Stopping: Sandbox is being terminated
 * - Terminated: Sandbox has been successfully terminated
 * - Failed: Sandbox encountered a critical error
 *
 * State transitions:
 * - Pending → Running (after creation completes)
 * - Running → Pausing (when pause is requested)
 * - Pausing → Paused (pause operation completes)
 * - Paused → Running (when resume is requested)
 * - Running/Paused → Stopping (when kill is requested or TTL expires)
 * - Stopping → Terminated (kill/timeout operation completes)
 * - Pending/Running/Paused → Failed (on error)
 *
 * Note: New state values may be added in future versions.
 * Clients should handle unknown state values gracefully.
 */
object SandboxState {
    const val PENDING = "Pending"
    const val RUNNING = "Running"
    const val PAUSING = "Pausing"
    const val PAUSED = "Paused"
    const val STOPPING = "Stopping"
    const val TERMINATED = "Terminated"
    const val FAILED = "Failed"
    const val UNKNOWN = "Unknown"
}

/**
 * Filter criteria for listing sandboxes.
 *
 * @property states Filter by sandbox states (e.g., RUNNING, PAUSED)
 * @property metadata Filter by metadata key-value pairs
 * @property pageSize Number of items per page
 * @property page Page number (0-indexed)
 */
class SandboxFilter private constructor(
    val states: List<String>?,
    val metadata: Map<String, String>?,
    val pageSize: Int?,
    val page: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var states: List<String>? = null
        private var metadata: Map<String, String>? = null
        private var pageSize: Int? = null
        private var page: Int? = null

        fun states(states: List<String>): Builder {
            this.states = states
            return this
        }

        fun states(vararg states: String): Builder {
            this.states = states.toList()
            return this
        }

        fun metadata(metadata: Map<String, String>): Builder {
            this.metadata = metadata
            return this
        }

        fun metadata(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = mutableMapOf<String, String>()
            map.configure()
            this.metadata = map
            return this
        }

        fun pageSize(pageSize: Int): Builder {
            require(pageSize > 0) { "Page size must be positive" }
            this.pageSize = pageSize
            return this
        }

        fun page(page: Int): Builder {
            require(page > 0) { "Page must be positive" }
            this.page = page
            return this
        }

        fun build(): SandboxFilter {
            return SandboxFilter(
                states = states,
                metadata = metadata,
                pageSize = pageSize,
                page = page,
            )
        }
    }
}

/**
 * Specification for a sandbox container image.
 *
 * @property image The image reference (e.g., "ubuntu:22.04", "python:3.11")
 * @property auth Authentication credentials for private registries
 */
class SandboxImageSpec private constructor(
    val image: String,
    val auth: SandboxImageAuth?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var image: String? = null
        private var auth: SandboxImageAuth? = null

        fun image(image: String): Builder {
            require(image.isNotBlank()) { "Image cannot be blank" }
            this.image = image
            return this
        }

        fun auth(auth: SandboxImageAuth): Builder {
            this.auth = auth
            return this
        }

        fun auth(
            username: String,
            password: String,
        ): Builder {
            this.auth =
                SandboxImageAuth.builder()
                    .username(username)
                    .password(password)
                    .build()
            return this
        }

        fun build(): SandboxImageSpec {
            val imageValue = image ?: throw IllegalArgumentException("Image must be specified")
            return SandboxImageSpec(
                image = imageValue,
                auth = auth,
            )
        }
    }
}

/**
 * Authentication credentials for container registries.
 *
 * @property username Registry username
 * @property password Registry password or access token
 */
class SandboxImageAuth private constructor(
    val username: String,
    val password: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var username: String? = null
        private var password: String? = null

        fun username(username: String): Builder {
            require(username.isNotBlank()) { "Username cannot be blank" }
            this.username = username
            return this
        }

        fun password(password: String): Builder {
            require(password.isNotBlank()) { "Password cannot be blank" }
            this.password = password
            return this
        }

        fun build(): SandboxImageAuth {
            val usernameValue = username ?: throw IllegalArgumentException("Username must be specified")
            val passwordValue = password ?: throw IllegalArgumentException("Password must be specified")
            return SandboxImageAuth(
                username = usernameValue,
                password = passwordValue,
            )
        }
    }
}

/**
 * Runtime platform constraint for sandbox provisioning.
 *
 * @property os Target operating system (linux or windows)
 * @property arch Target CPU architecture (amd64 or arm64)
 */
class PlatformSpec private constructor(
    val os: String,
    val arch: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var os: String? = null
        private var arch: String? = null

        fun os(os: String): Builder {
            require(os == "linux" || os == "windows") { "Platform os must be one of: linux, windows" }
            this.os = os
            return this
        }

        fun arch(arch: String): Builder {
            require(arch == "amd64" || arch == "arm64") {
                "Platform arch must be one of: amd64, arm64"
            }
            this.arch = arch
            return this
        }

        fun build(): PlatformSpec {
            val osValue = os ?: throw IllegalArgumentException("Platform os must be specified")
            val archValue = arch ?: throw IllegalArgumentException("Platform arch must be specified")
            return PlatformSpec(os = osValue, arch = archValue)
        }
    }
}

/**
 * Egress rule for matching network targets.
 *
 * @property action Whether to allow or deny matching targets.
 * @property target FQDN or wildcard domain (e.g., "example.com", "*.example.com")
 */
class NetworkRule private constructor(
    val action: Action,
    val target: String,
) {
    enum class Action {
        ALLOW,
        DENY,
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var action: Action? = null
        private var target: String? = null

        fun action(action: Action): Builder {
            this.action = action
            return this
        }

        fun target(target: String): Builder {
            require(target.isNotBlank()) { "Target cannot be blank" }
            this.target = target
            return this
        }

        fun build(): NetworkRule {
            val actionValue = action ?: throw IllegalArgumentException("Action must be specified")
            val targetValue = target ?: throw IllegalArgumentException("Target must be specified")
            return NetworkRule(
                action = actionValue,
                target = targetValue,
            )
        }
    }
}

/**
 * Egress network policy matching the sidecar `/policy` request body.
 *
 * @property defaultAction Default action when no egress rule matches. Defaults to "deny".
 * @property egress Egress rules evaluated in order
 */
class NetworkPolicy private constructor(
    val defaultAction: DefaultAction?,
    val egress: List<NetworkRule>?,
) {
    enum class DefaultAction {
        ALLOW,
        DENY,
    }

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var defaultAction: DefaultAction = DefaultAction.DENY
        private val egress = mutableListOf<NetworkRule>()

        fun defaultAction(action: DefaultAction): Builder {
            this.defaultAction = action
            return this
        }

        fun addEgress(rule: NetworkRule): Builder {
            egress.add(rule)
            return this
        }

        fun egress(rules: List<NetworkRule>): Builder {
            egress.clear()
            egress.addAll(rules)
            return this
        }

        fun build(): NetworkPolicy {
            return NetworkPolicy(
                defaultAction = defaultAction,
                egress = if (egress.isEmpty()) null else egress.toList(),
            )
        }
    }
}

// ============================================================================
// Volume Models
// ============================================================================

/**
 * Host path bind mount backend.
 *
 * Maps a directory on the host filesystem into the container.
 * Only available when the runtime supports host mounts.
 *
 * @property path Absolute path on the host filesystem to mount
 */
class Host private constructor(
    val path: String,
) {
    companion object {
        private val HOST_PATH_PATTERN = Regex("""^(/|[A-Za-z]:[\\/])""")

        @JvmStatic
        fun builder(): Builder = Builder()

        @JvmStatic
        fun of(path: String): Host = builder().path(path).build()
    }

    class Builder {
        private var path: String? = null

        fun path(path: String): Builder {
            require(HOST_PATH_PATTERN.containsMatchIn(path)) {
                "Host path must be an absolute path starting with '/' or a Windows drive letter (e.g. 'C:\\' or 'D:/')"
            }
            this.path = path
            return this
        }

        fun build(): Host {
            val pathValue = path ?: throw IllegalArgumentException("Path must be specified")
            return Host(path = pathValue)
        }
    }
}

/**
 * Platform-managed named volume backend.
 *
 * Runtime-neutral abstraction for referencing a pre-existing named volume:
 * - Kubernetes: maps to a PersistentVolumeClaim in the same namespace.
 * - Docker: maps to a Docker named volume.
 *
 * @property claimName Name of the platform volume. In Kubernetes this is the PVC name;
 * in Docker this is the named volume name.
 * @property createIfNotExists When true (default), auto-create volume if absent.
 * @property deleteOnSandboxTermination When true, delete auto-created Docker volume on sandbox deletion.
 * @property storageClass Kubernetes StorageClass for auto-created PVCs. Null means default class.
 * @property storage PVC storage request for auto-created PVCs (e.g. "1Gi").
 * @property accessModes Access modes for auto-created PVCs (e.g. ["ReadWriteOnce"]).
 */
class PVC private constructor(
    val claimName: String,
    val createIfNotExists: Boolean,
    val deleteOnSandboxTermination: Boolean,
    val storageClass: String?,
    val storage: String?,
    val accessModes: List<String>?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()

        @JvmStatic
        fun of(claimName: String): PVC = builder().claimName(claimName).build()
    }

    class Builder {
        private var claimName: String? = null
        private var createIfNotExists: Boolean = true
        private var deleteOnSandboxTermination: Boolean = false
        private var storageClass: String? = null
        private var storage: String? = null
        private var accessModes: List<String>? = null

        fun claimName(claimName: String): Builder {
            require(claimName.isNotBlank()) { "Claim name cannot be blank" }
            this.claimName = claimName
            return this
        }

        fun createIfNotExists(createIfNotExists: Boolean): Builder {
            this.createIfNotExists = createIfNotExists
            return this
        }

        fun deleteOnSandboxTermination(deleteOnSandboxTermination: Boolean): Builder {
            this.deleteOnSandboxTermination = deleteOnSandboxTermination
            return this
        }

        fun storageClass(storageClass: String?): Builder {
            this.storageClass = storageClass
            return this
        }

        fun storage(storage: String?): Builder {
            this.storage = storage
            return this
        }

        fun accessModes(accessModes: List<String>?): Builder {
            this.accessModes = accessModes
            return this
        }

        fun accessModes(vararg accessModes: String): Builder {
            this.accessModes = accessModes.toList()
            return this
        }

        fun build(): PVC {
            val claimNameValue = claimName ?: throw IllegalArgumentException("Claim name must be specified")
            return PVC(
                claimName = claimNameValue,
                createIfNotExists = createIfNotExists,
                deleteOnSandboxTermination = deleteOnSandboxTermination,
                storageClass = storageClass,
                storage = storage,
                accessModes = accessModes,
            )
        }
    }
}

/**
 * Alibaba Cloud OSS mount backend via ossfs.
 *
 * @property bucket OSS bucket name
 * @property endpoint OSS endpoint (for example, `oss-cn-hangzhou.aliyuncs.com`)
 * @property accessKeyId OSS access key ID for inline credentials mode
 * @property accessKeySecret OSS access key secret for inline credentials mode
 * @property version ossfs major version used by runtime mount integration
 * @property options Additional ossfs mount options
 */
class OSSFS private constructor(
    val bucket: String,
    val endpoint: String,
    val accessKeyId: String,
    val accessKeySecret: String,
    val version: String,
    val options: List<String>?,
) {
    companion object {
        const val VERSION_1_0 = "1.0"
        const val VERSION_2_0 = "2.0"

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var bucket: String? = null
        private var endpoint: String? = null
        private var accessKeyId: String? = null
        private var accessKeySecret: String? = null
        private var version: String = VERSION_2_0
        private var options: List<String>? = null

        fun bucket(bucket: String): Builder {
            require(bucket.isNotBlank()) { "Bucket cannot be blank" }
            this.bucket = bucket
            return this
        }

        fun endpoint(endpoint: String): Builder {
            require(endpoint.isNotBlank()) { "Endpoint cannot be blank" }
            this.endpoint = endpoint
            return this
        }

        fun accessKeyId(accessKeyId: String): Builder {
            require(accessKeyId.isNotBlank()) { "Access key ID cannot be blank" }
            this.accessKeyId = accessKeyId
            return this
        }

        fun accessKeySecret(accessKeySecret: String): Builder {
            require(accessKeySecret.isNotBlank()) { "Access key secret cannot be blank" }
            this.accessKeySecret = accessKeySecret
            return this
        }

        fun version(version: String): Builder {
            require(version == VERSION_1_0 || version == VERSION_2_0) {
                "OSSFS version must be one of: 1.0, 2.0"
            }
            this.version = version
            return this
        }

        fun options(options: List<String>?): Builder {
            this.options = options
            return this
        }

        fun options(vararg options: String): Builder {
            this.options = options.toList()
            return this
        }

        fun build(): OSSFS {
            val bucketValue = bucket ?: throw IllegalArgumentException("Bucket must be specified")
            val endpointValue = endpoint ?: throw IllegalArgumentException("Endpoint must be specified")
            val accessKeyIdValue = accessKeyId ?: throw IllegalArgumentException("Access key ID must be specified")
            val accessKeySecretValue =
                accessKeySecret ?: throw IllegalArgumentException("Access key secret must be specified")
            return OSSFS(
                bucket = bucketValue,
                endpoint = endpointValue,
                accessKeyId = accessKeyIdValue,
                accessKeySecret = accessKeySecretValue,
                version = version,
                options = options,
            )
        }
    }
}

/**
 * Storage mount definition for a sandbox.
 *
 * Each volume entry contains:
 * - A unique name identifier
 * - Exactly one backend (host, pvc, ossfs) with backend-specific fields
 * - Common mount settings (mountPath, readOnly, subPath)
 *
 * Example usage:
 * ```kotlin
 * // Host path mount (read-write by default)
 * val volume = Volume.builder()
 *     .name("workdir")
 *     .host(Host.of("/data/opensandbox"))
 *     .mountPath("/mnt/work")
 *     .build()
 *
 * // PVC mount (read-only)
 * val volume = Volume.builder()
 *     .name("models")
 *     .pvc(PVC.of("shared-models-pvc"))
 *     .mountPath("/mnt/models")
 *     .readOnly(true)
 *     .build()
 * ```
 *
 * @property name Unique identifier for the volume within the sandbox
 * @property host Host path bind mount backend (mutually exclusive with pvc/ossfs)
 * @property pvc Kubernetes PVC mount backend (mutually exclusive with host/ossfs)
 * @property ossfs OSSFS mount backend (mutually exclusive with host/pvc)
 * @property mountPath Absolute path inside the container where the volume is mounted
 * @property readOnly If true, the volume is mounted as read-only. Defaults to false (read-write).
 * @property subPath Optional subdirectory under the backend path to mount
 */
class Volume private constructor(
    val name: String,
    val host: Host?,
    val pvc: PVC?,
    val ossfs: OSSFS?,
    val mountPath: String,
    val readOnly: Boolean,
    val subPath: String?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var name: String? = null
        private var host: Host? = null
        private var pvc: PVC? = null
        private var ossfs: OSSFS? = null
        private var mountPath: String? = null
        private var readOnly: Boolean = false
        private var subPath: String? = null

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Volume name cannot be blank" }
            this.name = name
            return this
        }

        fun host(host: Host): Builder {
            this.host = host
            return this
        }

        fun pvc(pvc: PVC): Builder {
            this.pvc = pvc
            return this
        }

        fun ossfs(ossfs: OSSFS): Builder {
            this.ossfs = ossfs
            return this
        }

        fun mountPath(mountPath: String): Builder {
            require(mountPath.startsWith("/")) { "Mount path must be an absolute path starting with '/'" }
            this.mountPath = mountPath
            return this
        }

        fun readOnly(readOnly: Boolean): Builder {
            this.readOnly = readOnly
            return this
        }

        fun subPath(subPath: String): Builder {
            this.subPath = subPath
            return this
        }

        fun build(): Volume {
            val nameValue = name ?: throw IllegalArgumentException("Name must be specified")
            val mountPathValue = mountPath ?: throw IllegalArgumentException("Mount path must be specified")

            // Validate exactly one backend is specified
            val backendsSpecified = listOfNotNull(host, pvc, ossfs).size
            if (backendsSpecified == 0) {
                throw IllegalArgumentException(
                    "Exactly one backend (host, pvc, ossfs) must be specified, but none was provided",
                )
            }
            if (backendsSpecified > 1) {
                throw IllegalArgumentException(
                    "Exactly one backend (host, pvc, ossfs) must be specified, but multiple were provided",
                )
            }

            return Volume(
                name = nameValue,
                host = host,
                pvc = pvc,
                ossfs = ossfs,
                mountPath = mountPathValue,
                readOnly = readOnly,
                subPath = subPath,
            )
        }
    }
}

/**
 * Detailed information about a sandbox instance.
 *
 * @property id Unique identifier of the sandbox
 * @property status Current status of the sandbox
 * @property entrypoint Command line arguments used to start the sandbox
 * @property expiresAt Timestamp when the sandbox is scheduled for automatic termination. Null means manual cleanup mode.
 * @property createdAt Timestamp when the sandbox was created
 * @property image Image specification used to create this sandbox
 * @property platform Effective platform used for sandbox provisioning
 * @property metadata Custom metadata attached to the sandbox
 */
class SandboxInfo(
    val id: String,
    val status: SandboxStatus,
    val entrypoint: List<String>,
    val expiresAt: OffsetDateTime?,
    val createdAt: OffsetDateTime,
    val image: SandboxImageSpec? = null,
    val snapshotId: String? = null,
    val platform: PlatformSpec? = null,
    val metadata: Map<String, String>? = null,
)

/**
 * Status information for a sandbox.
 *
 * @property state Current state (e.g., RUNNING, PENDING, PAUSED, TERMINATED)
 * @property reason Short reason code for the current state
 * @property message Human-readable message explaining the status
 * @property lastTransitionAt Timestamp of the last state transition
 */
class SandboxStatus(
    val state: String,
    val reason: String?,
    val message: String?,
    val lastTransitionAt: java.time.OffsetDateTime?,
)

/**
 * Response returned when a sandbox is created.
 *
 * @property id Unique identifier of the newly created sandbox
 * @property platform Effective platform used for sandbox provisioning
 */
class SandboxCreateResponse(
    val id: String,
    val platform: PlatformSpec? = null,
)

class SnapshotStatus(
    val state: String,
    val reason: String?,
    val message: String?,
    val lastTransitionAt: OffsetDateTime?,
)

class SnapshotInfo(
    val id: String,
    val sandboxId: String,
    val name: String? = null,
    val status: SnapshotStatus,
    val createdAt: OffsetDateTime,
)

class SnapshotFilter private constructor(
    val sandboxId: String?,
    val states: List<String>?,
    val pageSize: Int?,
    val page: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var sandboxId: String? = null
        private var states: List<String>? = null
        private var pageSize: Int? = null
        private var page: Int? = null

        fun sandboxId(sandboxId: String): Builder {
            this.sandboxId = sandboxId
            return this
        }

        fun states(states: List<String>): Builder {
            this.states = states
            return this
        }

        fun states(vararg states: String): Builder {
            this.states = states.toList()
            return this
        }

        fun pageSize(pageSize: Int): Builder {
            require(pageSize > 0) { "Page size must be positive" }
            this.pageSize = pageSize
            return this
        }

        fun page(page: Int): Builder {
            require(page > 0) { "Page must be positive" }
            this.page = page
            return this
        }

        fun build(): SnapshotFilter = SnapshotFilter(sandboxId, states, pageSize, page)
    }
}

class PagedSnapshotInfos(
    val snapshotInfos: List<SnapshotInfo>,
    val pagination: PaginationInfo,
)

/**
 * Response returned when a sandbox is renewed
 *
 * @property expiresAt new expire time after renewal
 */
class SandboxRenewResponse(
    val expiresAt: java.time.OffsetDateTime,
)

/**
 * Connection endpoint information for a sandbox.
 *
 * @property endpoint Sandbox endpoint
 * @property headers Headers that must be included on every request targeting this endpoint (e.g. when the server requires them for routing or auth). Empty if not required.
 */
class SandboxEndpoint(
    val endpoint: String,
    val headers: Map<String, String> = emptyMap(),
)

/**
 * A paginated list of sandbox information.
 *
 * @property sandboxInfos List of sandbox details for the current page
 * @property pagination Pagination metadata
 */
class PagedSandboxInfos(
    val sandboxInfos: List<SandboxInfo>,
    val pagination: PaginationInfo,
)

/**
 * Pagination metadata.
 *
 * @property page Current page number (0-indexed)
 * @property pageSize Number of items per page
 * @property totalItems Total number of items across all pages
 * @property totalPages Total number of pages
 * @property hasNextPage True if there is a next page available
 */
class PaginationInfo(
    val page: Int,
    val pageSize: Int,
    val totalItems: Int,
    val totalPages: Int,
    val hasNextPage: Boolean,
)

/**
 * Real-time resource usage metrics for a sandbox.
 *
 * @property cpuCount Number of CPU cores available/allocated
 * @property cpuUsedPercentage Current CPU usage as a percentage (0.0 - 100.0)
 * @property memoryTotalInMiB Total memory available in Mebibytes
 * @property memoryUsedInMiB Memory currently used in Mebibytes
 * @property timestamp Timestamp of the metric collection (Unix epoch milliseconds)
 */
class SandboxMetrics(
    val cpuCount: Float,
    val cpuUsedPercentage: Float,
    val memoryTotalInMiB: Float,
    val memoryUsedInMiB: Float,
    val timestamp: Long,
)
