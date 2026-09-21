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

package com.alibaba.opensandbox.sandbox.domain.models.sandboxes

import java.time.OffsetDateTime

/**
 * High-level build lifecycle phase of a fsb golden-image template.
 *
 * Common phase values:
 * - Pending: Template build request accepted, build has not started
 * - Building: Template build is in progress
 * - Succeeded: Template build finished; the template can create sandboxes
 * - Failed: Template build encountered a critical error
 *
 * Note: New phase values may be added in future versions.
 * Clients should handle unknown phase values gracefully.
 */
object TemplatePhase {
    const val PENDING = "Pending"
    const val BUILDING = "Building"
    const val SUCCEEDED = "Succeeded"
    const val FAILED = "Failed"
}

/**
 * Snapshot storage encoding used when publishing a template.
 */
enum class TemplateFormat {
    NATIVE,
    OVERLAYBD,
}

/**
 * Origin of a running sandbox.
 *
 * The protocol currently defines a single meaningful value: [TEMPLATE]. The server
 * reports it through the `OPEN-SANDBOX-ORIGIN` response header on endpoint lookups,
 * and the SDK sets it locally when a sandbox is created explicitly from a template.
 * Anything else means the sandbox is not template-backed.
 *
 * Note: The set of origin values may grow in future versions.
 * Clients should treat a missing or unknown origin as "not template-backed".
 */
object SandboxOrigin {
    /** Wire name of the server response header carrying the sandbox origin. */
    const val HEADER_NAME = "OPEN-SANDBOX-ORIGIN"

    const val TEMPLATE = "template"
    const val UNKNOWN = "unknown"
}

/**
 * Build-time readiness probe configuration for a template.
 *
 * @property probe Readiness probe expression, e.g. `tcp://127.0.0.1:44772` or `cmd://<command>`
 * @property warmupSeconds Fallback warmup window in seconds applied after the probe passes
 */
class TemplateReadiness private constructor(
    val probe: String?,
    val warmupSeconds: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var probe: String? = null
        private var warmupSeconds: Int? = null

        fun probe(probe: String): Builder {
            require(probe.isNotBlank()) { "Probe cannot be blank" }
            this.probe = probe
            return this
        }

        fun warmupSeconds(warmupSeconds: Int): Builder {
            require(warmupSeconds >= 0) { "Warmup seconds must not be negative" }
            this.warmupSeconds = warmupSeconds
            return this
        }

        fun build(): TemplateReadiness {
            return TemplateReadiness(
                probe = probe,
                warmupSeconds = warmupSeconds,
            )
        }
    }
}

/**
 * Build status of a fsb golden-image template.
 *
 * @property phase Current build phase (see [TemplatePhase])
 * @property manifestRef Published manifest reference; present once the build succeeds
 * @property message Human-readable failure reason; present when the build fails
 */
class TemplateStatus(
    val phase: String,
    val manifestRef: String? = null,
    val message: String? = null,
)

/**
 * Request to build a new fsb golden-image template from a source OCI image.
 *
 * The build is asynchronous: the create response starts at [TemplatePhase.PENDING];
 * poll [com.alibaba.opensandbox.sandbox.SandboxManager.getTemplate] until the status
 * reaches [TemplatePhase.SUCCEEDED] or [TemplatePhase.FAILED].
 *
 * @property image Source OCI image reference
 * @property publish S3-compatible publish target (e.g. `s3://bucket/publish`)
 * @property resourceLimits Runtime resource constraints (e.g. cpu/memory/disk)
 * @property entrypoint Guest business command (argv); defaults server-side when omitted
 * @property metadata User-defined metadata used for management and filtering
 * @property readiness Optional build readiness probe configuration
 * @property format Snapshot storage encoding; server defaults to overlaybd when omitted
 */
class CreateTemplateRequest private constructor(
    val image: String,
    val publish: String,
    val resourceLimits: Map<String, String>?,
    val entrypoint: List<String>?,
    val metadata: Map<String, String>?,
    val readiness: TemplateReadiness?,
    val format: TemplateFormat?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var image: String? = null
        private var publish: String? = null
        private var resourceLimits: Map<String, String>? = null
        private var entrypoint: List<String>? = null
        private var metadata: Map<String, String>? = null
        private var readiness: TemplateReadiness? = null
        private var format: TemplateFormat? = null

        fun image(image: String): Builder {
            require(image.isNotBlank()) { "Template image cannot be blank" }
            this.image = image
            return this
        }

        fun publish(publish: String): Builder {
            require(publish.isNotBlank()) { "Template publish target cannot be blank" }
            this.publish = publish
            return this
        }

        fun resourceLimits(resourceLimits: Map<String, String>): Builder {
            this.resourceLimits = resourceLimits
            return this
        }

        fun resourceLimits(configure: MutableMap<String, String>.() -> Unit): Builder {
            val map = mutableMapOf<String, String>()
            map.configure()
            this.resourceLimits = map
            return this
        }

        fun entrypoint(entrypoint: List<String>): Builder {
            this.entrypoint = entrypoint
            return this
        }

        fun entrypoint(vararg entrypoint: String): Builder {
            this.entrypoint = entrypoint.toList()
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

        fun readiness(readiness: TemplateReadiness): Builder {
            this.readiness = readiness
            return this
        }

        fun readiness(configure: TemplateReadiness.Builder.() -> Unit): Builder {
            val builder = TemplateReadiness.builder()
            builder.configure()
            this.readiness = builder.build()
            return this
        }

        fun format(format: TemplateFormat): Builder {
            this.format = format
            return this
        }

        fun build(): CreateTemplateRequest {
            val imageValue = image ?: throw IllegalArgumentException("Template image must be specified")
            val publishValue = publish ?: throw IllegalArgumentException("Template publish target must be specified")
            return CreateTemplateRequest(
                image = imageValue,
                publish = publishValue,
                resourceLimits = resourceLimits,
                entrypoint = entrypoint,
                metadata = metadata,
                readiness = readiness,
                format = format,
            )
        }
    }
}

/**
 * Detailed information about a fsb golden-image template.
 *
 * @property templateId Server-generated template ID (`tpl_<uuid>`)
 * @property image Source OCI image reference
 * @property publish S3-compatible publish target
 * @property format Snapshot storage encoding
 * @property status Current build status
 * @property createdAt Timestamp when the template was created
 * @property updatedAt Timestamp when the template was last updated
 * @property resourceLimits Runtime resource constraints
 * @property entrypoint Guest business command (argv)
 * @property metadata Custom metadata attached to the template
 * @property readiness Build readiness probe configuration
 */
class TemplateInfo(
    val templateId: String,
    val image: String,
    val publish: String,
    val format: TemplateFormat,
    val status: TemplateStatus,
    val createdAt: OffsetDateTime,
    val updatedAt: OffsetDateTime,
    val resourceLimits: Map<String, String>? = null,
    val entrypoint: List<String>? = null,
    val metadata: Map<String, String>? = null,
    val readiness: TemplateReadiness? = null,
)

/**
 * Filter criteria for listing templates.
 *
 * @property metadata Filter by metadata key-value pairs (combined with AND logic)
 * @property pageSize Number of items per page
 * @property page Page number (1-indexed)
 */
class TemplateFilter private constructor(
    val metadata: Map<String, String>?,
    val pageSize: Int?,
    val page: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var metadata: Map<String, String>? = null
        private var pageSize: Int? = null
        private var page: Int? = null

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
            require(page >= 1) { "Page must be at least 1 (1-indexed)" }
            this.page = page
            return this
        }

        fun build(): TemplateFilter {
            return TemplateFilter(
                metadata = metadata,
                pageSize = pageSize,
                page = page,
            )
        }
    }
}

/**
 * A paginated list of template information.
 *
 * @property templateInfos List of template details for the current page
 * @property pagination Pagination metadata
 */
class PagedTemplateInfos(
    val templateInfos: List<TemplateInfo>,
    val pagination: PaginationInfo,
)
