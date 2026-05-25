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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.SandboxesApi
import com.alibaba.opensandbox.sandbox.api.SnapshotsApi
import com.alibaba.opensandbox.sandbox.api.infrastructure.Serializer
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
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
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiRenewRequest
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toPagedSandboxInfos
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toPagedSnapshotInfos
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxCreateResponse
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxInfo
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxRenewResponse
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSnapshotInfo
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.parseSandboxError
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.slf4j.LoggerFactory
import java.time.Duration
import java.time.OffsetDateTime
import com.alibaba.opensandbox.sandbox.api.models.Sandbox as ApiSandbox

/**
 * Implementation of [Sandboxes] that adapts OpenAPI-generated [SandboxesApi].
 *
 * This adapter provides a clean abstraction layer between business logic and
 * the auto-generated API client, handling all model conversions and error mapping.
 */
internal class SandboxesAdapter(
    private val provider: HttpClientProvider,
) : Sandboxes {
    private val logger = LoggerFactory.getLogger(SandboxesAdapter::class.java)

    private val api = SandboxesApi(provider.config.getBaseUrl(), provider.authenticatedClient)
    private val snapshotApi = SnapshotsApi(provider.config.getBaseUrl(), provider.authenticatedClient)

    override fun createSandbox(
        spec: SandboxImageSpec?,
        entrypoint: List<String>?,
        env: Map<String, String>,
        metadata: Map<String, String>,
        timeout: Duration?,
        resource: Map<String, String>,
        networkPolicy: NetworkPolicy?,
        extensions: Map<String, String>,
        volumes: List<Volume>?,
        platform: PlatformSpec?,
        secureAccess: Boolean,
        snapshotId: String?,
    ): SandboxCreateResponse {
        logger.info("Creating sandbox with startup source: {}", spec?.image ?: snapshotId)

        return try {
            val createRequest =
                SandboxModelConverter.toApiCreateSandboxRequest(
                    spec = spec,
                    entrypoint = entrypoint,
                    env = env,
                    metadata = metadata,
                    timeout = timeout,
                    resource = resource,
                    platform = platform,
                    networkPolicy = networkPolicy,
                    secureAccess = secureAccess,
                    extensions = extensions,
                    volumes = volumes,
                    snapshotId = snapshotId,
                )
            val apiResponse = api.sandboxesPost(createRequest)
            val response = apiResponse.toSandboxCreateResponse()

            logger.info("Successfully created sandbox: {}", response.id)

            response
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun getSandboxInfo(sandboxId: String): SandboxInfo {
        logger.debug("Retrieving sandbox information: {}", sandboxId)

        return try {
            api.sandboxesSandboxIdGet(sandboxId).toSandboxInfo()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun listSandboxes(filter: SandboxFilter): PagedSandboxInfos {
        logger.debug("Listing sandboxes with filter: {}", filter)
        val metadataQuery: String? =
            filter.metadata?.entries?.joinToString("&") { "${it.key}=${it.value}" }
        return try {
            api.sandboxesGet(filter.states, metadataQuery, filter.page, filter.pageSize).toPagedSandboxInfos()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun patchSandboxMetadata(
        sandboxId: String,
        patch: Map<String, String?>,
    ): SandboxInfo {
        return try {
            patchSandboxMetadataRaw(sandboxId, patch).toSandboxInfo()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    private fun patchSandboxMetadataRaw(
        sandboxId: String,
        patch: Map<String, String?>,
    ): ApiSandbox {
        // The generated Kotlin client currently maps the merge-patch body to
        // Map<String, String>, which drops the null value used to delete keys.
        val url =
            provider.config.getBaseUrl().toHttpUrl().newBuilder()
                .addPathSegment("sandboxes")
                .addPathSegment(sandboxId)
                .addPathSegment("metadata")
                .build()
        val body =
            Serializer.kotlinxSerializationJson
                .encodeToString<Map<String, String?>>(patch)
                .toRequestBody("application/json".toMediaType())
        val request =
            Request.Builder()
                .url(url)
                .patch(body)
                .header("Accept", "application/json")
                .build()

        provider.authenticatedClient.newCall(request).execute().use { response ->
            val responseBody = response.body?.string().orEmpty()
            if (response.isSuccessful) {
                return Serializer.kotlinxSerializationJson.decodeFromString<ApiSandbox>(responseBody)
            }
            throw SandboxApiException(
                message = "Failed to patch sandbox metadata. Status code: ${response.code}, Body: $responseBody",
                statusCode = response.code,
                error = parseSandboxError(responseBody) ?: SandboxError(UNEXPECTED_RESPONSE),
                requestId = response.header("X-Request-ID"),
            )
        }
    }

    override fun createSnapshot(
        sandboxId: String,
        name: String?,
    ): SnapshotInfo {
        return try {
            snapshotApi.sandboxesSandboxIdSnapshotsPost(
                sandboxId,
                name?.let { com.alibaba.opensandbox.sandbox.api.models.CreateSnapshotRequest(name = it) },
            )
                .toSnapshotInfo()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun getSnapshot(snapshotId: String): SnapshotInfo {
        return try {
            snapshotApi.snapshotsSnapshotIdGet(snapshotId).toSnapshotInfo()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun listSnapshots(filter: SnapshotFilter): PagedSnapshotInfos {
        return try {
            snapshotApi.snapshotsGet(filter.sandboxId, filter.states, filter.page, filter.pageSize).toPagedSnapshotInfos()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun deleteSnapshot(snapshotId: String) {
        try {
            snapshotApi.snapshotsSnapshotIdDelete(snapshotId)
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun getSandboxEndpoint(
        sandboxId: String,
        port: Int,
    ): SandboxEndpoint {
        return getSandboxEndpoint(sandboxId, port, false)
    }

    override fun getSandboxEndpoint(
        sandboxId: String,
        port: Int,
        useServerProxy: Boolean,
    ): SandboxEndpoint {
        logger.debug("Retrieving sandbox endpoint: {}, port {}", sandboxId, port)
        return try {
            api.sandboxesSandboxIdEndpointsPortGet(sandboxId, port, useServerProxy).toSandboxEndpoint()
        } catch (e: Exception) {
            logger.error("Failed to retrieve sandbox endpoint for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun getSignedSandboxEndpoint(
        sandboxId: String,
        port: Int,
        expires: Long,
        useServerProxy: Boolean,
    ): SandboxEndpoint {
        logger.debug("Retrieving signed sandbox endpoint: {}, port {}", sandboxId, port)
        return try {
            api.sandboxesSandboxIdEndpointsPortGet(sandboxId, port, useServerProxy, expires.toString()).toSandboxEndpoint()
        } catch (e: Exception) {
            logger.error("Failed to retrieve signed sandbox endpoint for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun pauseSandbox(sandboxId: String) {
        logger.info("Pausing sandbox: {}", sandboxId)

        try {
            api.sandboxesSandboxIdPausePost(sandboxId)
            logger.info("Initiated pause for sandbox: {}", sandboxId)
        } catch (e: Exception) {
            logger.error("Failed to initiate pause sandbox: {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun resumeSandbox(sandboxId: String) {
        logger.info("Resuming sandbox: {}", sandboxId)

        try {
            api.sandboxesSandboxIdResumePost(sandboxId)
            logger.info("Initiated resume for sandbox: {}", sandboxId)
        } catch (e: Exception) {
            logger.error("Failed initiate resume sandbox: {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun renewSandboxExpiration(
        sandboxId: String,
        newExpirationTime: OffsetDateTime,
    ): SandboxRenewResponse {
        logger.info("Renew sandbox {} expiration to {}", sandboxId, newExpirationTime)

        return try {
            val response =
                api.sandboxesSandboxIdRenewExpirationPost(
                    sandboxId,
                    newExpirationTime.toApiRenewRequest(),
                ).toSandboxRenewResponse()

            logger.info("Successfully renewed sandbox {} expiration", sandboxId)

            response
        } catch (e: Exception) {
            logger.error("Failed to renew sandbox {} expiration", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun killSandbox(sandboxId: String) {
        logger.info("Terminating sandbox: {}", sandboxId)

        return try {
            api.sandboxesSandboxIdDelete(sandboxId)
            logger.info("Successfully terminated sandbox: {}", sandboxId)
        } catch (e: Exception) {
            logger.error("Failed to terminate sandbox: {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }
}
