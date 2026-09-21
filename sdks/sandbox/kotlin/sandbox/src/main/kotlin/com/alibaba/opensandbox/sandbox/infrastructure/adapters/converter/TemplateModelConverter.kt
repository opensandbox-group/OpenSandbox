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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.models.CreateFsbTemplateRequest
import com.alibaba.opensandbox.sandbox.api.models.CreateSandboxRequest
import com.alibaba.opensandbox.sandbox.api.models.FsbTemplate
import com.alibaba.opensandbox.sandbox.api.models.FsbTemplateReadiness
import com.alibaba.opensandbox.sandbox.api.models.ListFsbTemplatesResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CreateTemplateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.PagedTemplateInfos
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateFormat
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateInfo
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateReadiness
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.TemplateStatus
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiNetworkPolicy
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toPaginationInfo
import java.time.Duration

/**
 * Converter between public template models and lifecycle API models.
 */
internal object TemplateModelConverter {
    /**
     * Converts Domain CreateTemplateRequest -> API CreateFsbTemplateRequest
     *
     * The generated serializer always encodes the format field, so an unset domain
     * format is pinned to the generated (spec) default instead of serializing an
     * explicit null the server would reject.
     */
    fun CreateTemplateRequest.toApiCreateTemplateRequest(): CreateFsbTemplateRequest {
        return CreateFsbTemplateRequest(
            image = this.image,
            publish = this.publish,
            resourceLimits = this.resourceLimits,
            entrypoint = this.entrypoint,
            metadata = this.metadata,
            readiness = this.readiness?.toApiFsbTemplateReadiness(),
            format =
                this.format?.let {
                    when (it) {
                        TemplateFormat.NATIVE -> CreateFsbTemplateRequest.Format.native
                        TemplateFormat.OVERLAYBD -> CreateFsbTemplateRequest.Format.overlaybd
                    }
                } ?: CreateFsbTemplateRequest.Format.overlaybd,
        )
    }

    /**
     * Converts API FsbTemplate -> Domain TemplateInfo
     */
    fun FsbTemplate.toTemplateInfo(): TemplateInfo {
        return TemplateInfo(
            templateId = this.templateId,
            image = this.image,
            publish = this.publish,
            format = toDomainTemplateFormat(this.format),
            status =
                TemplateStatus(
                    phase = this.status.phase.value,
                    manifestRef = this.status.manifestRef,
                    message = this.status.message,
                ),
            createdAt = this.createdAt,
            updatedAt = this.updatedAt,
            resourceLimits = this.resourceLimits,
            entrypoint = this.entrypoint,
            metadata = this.metadata,
            readiness = this.readiness?.toTemplateReadiness(),
        )
    }

    /**
     * Converts API List Response -> Domain Paged Template Infos
     */
    fun ListFsbTemplatesResponse.toPagedTemplateInfos(): PagedTemplateInfos {
        return PagedTemplateInfos(
            this.items.map { it.toTemplateInfo() },
            this.pagination.toPaginationInfo(),
        )
    }

    /**
     * Builds a template-mode create sandbox request.
     *
     * Template mode fixes the workload shape on the server: only the template id,
     * timeout, metadata, network policy and extensions may accompany the request.
     * Workload-shaping fields (image, snapshotId, entrypoint, env, resource limits,
     * volumes, platform, credential proxy, lifecycle) must stay unset because the
     * server rejects them in template mode.
     */
    fun toApiCreateTemplateSandboxRequest(
        templateId: String,
        timeout: Duration,
        metadata: Map<String, String>,
        networkPolicy: NetworkPolicy?,
        extensions: Map<String, String>,
    ): CreateSandboxRequest {
        return CreateSandboxRequest(
            templateId = templateId,
            timeout = timeout.seconds.toInt(),
            metadata = metadata,
            networkPolicy = networkPolicy?.toApiNetworkPolicy(),
            extensions = extensions,
        )
    }

    private fun TemplateReadiness.toApiFsbTemplateReadiness(): FsbTemplateReadiness {
        return FsbTemplateReadiness(
            probe = this.probe,
            warmupSeconds = this.warmupSeconds,
        )
    }

    private fun FsbTemplateReadiness.toTemplateReadiness(): TemplateReadiness {
        val builder = TemplateReadiness.builder()
        this.probe?.let { builder.probe(it) }
        this.warmupSeconds?.let { builder.warmupSeconds(it) }
        return builder.build()
    }

    private fun toDomainTemplateFormat(format: FsbTemplate.Format): TemplateFormat =
        when (format) {
            FsbTemplate.Format.native -> TemplateFormat.NATIVE
            FsbTemplate.Format.overlaybd -> TemplateFormat.OVERLAYBD
        }
}
