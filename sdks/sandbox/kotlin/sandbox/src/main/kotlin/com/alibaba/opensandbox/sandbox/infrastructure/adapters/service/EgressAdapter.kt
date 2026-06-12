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
import com.alibaba.opensandbox.sandbox.api.egress.PolicyApi
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuth
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialAuthMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBinding
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingListResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialListResponse
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMatch
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CustomHeaderEntry
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.InlineCredentialSource
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.CredentialVault
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiEgressNetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainEgressNetworkPolicy
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.parseSandboxError
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.slf4j.LoggerFactory

internal class EgressAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val egressEndpoint: SandboxEndpoint,
) : Egress, CredentialVault {
    companion object {
        private val JSON_MEDIA_TYPE = "application/json".toMediaType()
    }

    private val logger = LoggerFactory.getLogger(EgressAdapter::class.java)
    private val egressBaseUrl = "${httpClientProvider.config.protocol}://${egressEndpoint.endpoint}"
    private val egressClient =
        httpClientProvider.httpClient.newBuilder()
            .addInterceptor { chain ->
                val requestBuilder = chain.request().newBuilder()
                egressEndpoint.headers.forEach { (key, value) ->
                    requestBuilder.header(key, value)
                }
                chain.proceed(requestBuilder.build())
            }
            .build()
    private val api =
        PolicyApi(
            egressBaseUrl,
            egressClient,
        )

    override fun create(request: CredentialVaultCreateRequest): CredentialVaultState {
        return try {
            val responseBody =
                requestJson(
                    method = "POST",
                    operation = "Create credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Credential Vault create response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to create credential vault via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun get(): CredentialVaultState {
        return try {
            val responseBody =
                requestJson(
                    method = "GET",
                    operation = "Get credential vault",
                ) ?: throw IllegalStateException("Credential Vault get response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to get credential vault via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun patch(request: CredentialVaultPatchRequest): CredentialVaultState {
        return try {
            val responseBody =
                requestJson(
                    method = "PATCH",
                    operation = "Patch credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Credential Vault patch response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to patch credential vault via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun delete() {
        try {
            requestJson(
                method = "DELETE",
                operation = "Delete credential vault",
            )
        } catch (e: Exception) {
            logger.error("Failed to delete credential vault via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun listCredentials(): List<CredentialMetadata> {
        return try {
            val responseBody =
                requestJson(
                    method = "GET",
                    operation = "List credential vault credentials",
                    pathSegments = listOf("credentials"),
                ) ?: throw IllegalStateException("Credential Vault credentials response did not contain body")
            responseBody.toCredentialListResponse().credentials
        } catch (e: Exception) {
            logger.error("Failed to list credential vault credentials via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun getCredential(name: String): CredentialMetadata {
        return try {
            val responseBody =
                requestJson(
                    method = "GET",
                    operation = "Get credential vault credential",
                    pathSegments = listOf("credentials", name),
                ) ?: throw IllegalStateException("Credential Vault credential response did not contain body")
            responseBody.toCredentialMetadata()
        } catch (e: Exception) {
            logger.error("Failed to get credential vault credential via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun listBindings(): List<CredentialBindingMetadata> {
        return try {
            val responseBody =
                requestJson(
                    method = "GET",
                    operation = "List credential vault bindings",
                    pathSegments = listOf("bindings"),
                ) ?: throw IllegalStateException("Credential Vault bindings response did not contain body")
            responseBody.toCredentialBindingListResponse().bindings
        } catch (e: Exception) {
            logger.error("Failed to list credential vault bindings via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun getBinding(name: String): CredentialBindingMetadata {
        return try {
            val responseBody =
                requestJson(
                    method = "GET",
                    operation = "Get credential vault binding",
                    pathSegments = listOf("bindings", name),
                ) ?: throw IllegalStateException("Credential Vault binding response did not contain body")
            responseBody.toCredentialBindingMetadata()
        } catch (e: Exception) {
            logger.error("Failed to get credential vault binding via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun getPolicy(): NetworkPolicy {
        return try {
            val policy =
                api.policyGet().policy
                    ?: throw IllegalStateException("Egress policy response did not contain policy")
            policy.toDomainEgressNetworkPolicy()
        } catch (e: Exception) {
            logger.error("Failed to fetch egress policy from endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun patchRules(rules: List<NetworkRule>) {
        try {
            api.policyPatch(rules.map { it.toApiEgressNetworkRule() })
        } catch (e: Exception) {
            logger.error("Failed to patch egress policy via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    override fun deleteRules(targets: List<String>) {
        try {
            api.policyDelete(targets)
        } catch (e: Exception) {
            logger.error("Failed to delete egress rules via endpoint {}", egressEndpoint.endpoint, e)
            throw e.toSandboxException()
        }
    }

    private fun requestJson(
        method: String,
        operation: String,
        pathSegments: List<String> = emptyList(),
        jsonBody: JsonObject? = null,
    ): String? {
        val requestBuilder =
            Request.Builder()
                .url(credentialVaultUrl(pathSegments))
                .header("Accept", "application/json")

        when (method) {
            "GET" -> requestBuilder.get()
            "DELETE" -> requestBuilder.delete()
            else ->
                requestBuilder.method(
                    method,
                    (jsonBody ?: buildJsonObject { }).toString().toRequestBody(JSON_MEDIA_TYPE),
                )
        }

        egressClient.newCall(requestBuilder.build()).execute().use { response ->
            val responseBody = response.body?.string().orEmpty()
            if (response.isSuccessful) {
                return if (response.code == 204 || responseBody.isBlank()) null else responseBody
            }
            throw SandboxApiException(
                message = "$operation failed. Status code: ${response.code}, Body: $responseBody",
                statusCode = response.code,
                error = parseSandboxError(responseBody) ?: SandboxError(UNEXPECTED_RESPONSE, responseBody.takeIf { it.isNotBlank() }),
                requestId = response.header("X-Request-ID"),
            )
        }
    }

    private fun credentialVaultUrl(pathSegments: List<String>): HttpUrl {
        val builder = egressBaseUrl.toHttpUrl().newBuilder().addPathSegment("credential-vault")
        pathSegments.forEach { builder.addPathSegment(it) }
        return builder.build()
    }

    private fun CredentialVaultCreateRequest.toJsonObject(): JsonObject =
        buildJsonObject {
            put("credentials", credentials.toCredentialJsonArray())
            put("bindings", bindings.toBindingJsonArray())
        }

    private fun CredentialVaultPatchRequest.toJsonObject(): JsonObject =
        buildJsonObject {
            expectedRevision?.let { put("expectedRevision", JsonPrimitive(it)) }
            credentials?.let { put("credentials", it.toJsonObject()) }
            bindings?.let { put("bindings", it.toJsonObject()) }
        }

    private fun CredentialMutationSet.toJsonObject(): JsonObject =
        buildJsonObject {
            add?.let { put("add", it.toCredentialJsonArray()) }
            replace?.let { put("replace", it.toCredentialJsonArray()) }
            delete?.let { put("delete", it.toStringJsonArray()) }
        }

    private fun CredentialBindingMutationSet.toJsonObject(): JsonObject =
        buildJsonObject {
            add?.let { put("add", it.toBindingJsonArray()) }
            replace?.let { put("replace", it.toBindingJsonArray()) }
            delete?.let { put("delete", it.toStringJsonArray()) }
        }

    private fun List<Credential>.toCredentialJsonArray(): JsonArray = JsonArray(map { it.toJsonObject() })

    private fun Credential.toJsonObject(): JsonObject =
        buildJsonObject {
            put("name", JsonPrimitive(name))
            put("source", source.toJsonObject())
        }

    private fun InlineCredentialSource.toJsonObject(): JsonObject =
        buildJsonObject {
            put("type", JsonPrimitive(type))
            put("value", JsonPrimitive(value))
        }

    private fun List<CredentialBinding>.toBindingJsonArray(): JsonArray = JsonArray(map { it.toJsonObject() })

    private fun CredentialBinding.toJsonObject(): JsonObject =
        buildJsonObject {
            put("name", JsonPrimitive(name))
            put("match", match.toJsonObject())
            put("auth", auth.toJsonObject())
        }

    private fun CredentialMatch.toJsonObject(): JsonObject =
        buildJsonObject {
            schemes?.let { put("schemes", it.map { scheme -> scheme.wireName() }.toStringJsonArray()) }
            ports?.let { put("ports", JsonArray(it.map { port -> JsonPrimitive(port) })) }
            put("hosts", hosts.toStringJsonArray())
            methods?.let { put("methods", it.toStringJsonArray()) }
            paths?.let { put("paths", it.toStringJsonArray()) }
        }

    private fun CredentialAuth.toJsonObject(): JsonObject =
        buildJsonObject {
            put("type", JsonPrimitive(type.wireName()))
            when (type) {
                CredentialAuth.Type.BEARER, CredentialAuth.Type.BASIC -> {
                    put("credential", JsonPrimitive(credential ?: throw IllegalStateException("Credential auth credential missing")))
                }
                CredentialAuth.Type.API_KEY -> {
                    put("name", JsonPrimitive(name ?: throw IllegalStateException("Credential auth name missing")))
                    put("credential", JsonPrimitive(credential ?: throw IllegalStateException("Credential auth credential missing")))
                }
                CredentialAuth.Type.CUSTOM_HEADERS -> {
                    put("headers", JsonArray(headers.orEmpty().map { it.toJsonObject() }))
                }
            }
        }

    private fun CustomHeaderEntry.toJsonObject(): JsonObject =
        buildJsonObject {
            put("name", JsonPrimitive(name))
            put("credential", JsonPrimitive(credential))
        }

    private fun List<String>.toStringJsonArray(): JsonArray = JsonArray(map { JsonPrimitive(it) })

    private fun CredentialMatch.Scheme.wireName(): String =
        when (this) {
            CredentialMatch.Scheme.HTTPS -> "https"
            CredentialMatch.Scheme.HTTP -> "http"
        }

    private fun CredentialAuth.Type.wireName(): String =
        when (this) {
            CredentialAuth.Type.BEARER -> "bearer"
            CredentialAuth.Type.BASIC -> "basic"
            CredentialAuth.Type.API_KEY -> "apiKey"
            CredentialAuth.Type.CUSTOM_HEADERS -> "customHeaders"
        }

    private fun String.toCredentialVaultState(): CredentialVaultState {
        val root = jsonParser.parseToJsonElement(this).jsonObject
        return CredentialVaultState(
            revision = root.requiredInt("revision"),
            credentials = root.requiredArray("credentials").map { it.jsonObject.toCredentialMetadata() },
            bindings = root.requiredArray("bindings").map { it.jsonObject.toCredentialBindingMetadata() },
        )
    }

    private fun String.toCredentialListResponse(): CredentialListResponse {
        val root = jsonParser.parseToJsonElement(this).jsonObject
        return CredentialListResponse(
            revision = root.requiredInt("revision"),
            credentials = root.requiredArray("credentials").map { it.jsonObject.toCredentialMetadata() },
        )
    }

    private fun String.toCredentialBindingListResponse(): CredentialBindingListResponse {
        val root = jsonParser.parseToJsonElement(this).jsonObject
        return CredentialBindingListResponse(
            revision = root.requiredInt("revision"),
            bindings = root.requiredArray("bindings").map { it.jsonObject.toCredentialBindingMetadata() },
        )
    }

    private fun String.toCredentialMetadata(): CredentialMetadata = jsonParser.parseToJsonElement(this).jsonObject.toCredentialMetadata()

    private fun String.toCredentialBindingMetadata(): CredentialBindingMetadata =
        jsonParser.parseToJsonElement(this).jsonObject.toCredentialBindingMetadata()

    private fun JsonObject.toCredentialMetadata(): CredentialMetadata =
        CredentialMetadata(
            name = requiredString("name"),
            sourceType = requiredString("sourceType"),
            revision = requiredInt("revision"),
        )

    private fun JsonObject.toCredentialBindingMetadata(): CredentialBindingMetadata =
        CredentialBindingMetadata(
            name = requiredString("name"),
            revision = requiredInt("revision"),
            match = optionalObject("match")?.toCredentialMatch(),
            auth = optionalObject("auth")?.toCredentialAuthMetadata(),
        )

    private fun JsonObject.toCredentialMatch(): CredentialMatch {
        val builder = CredentialMatch.builder().hosts(requiredStringArray("hosts"))
        optionalStringArray("schemes")?.let { schemes ->
            builder.schemes(schemes.map { it.toCredentialMatchScheme() })
        }
        optionalIntArray("ports")?.let { builder.ports(it) }
        optionalStringArray("methods")?.let { builder.methods(it) }
        optionalStringArray("paths")?.let { builder.paths(it) }
        return builder.build()
    }

    private fun JsonObject.toCredentialAuthMetadata(): CredentialAuthMetadata =
        CredentialAuthMetadata(
            type = requiredString("type"),
            name = optionalString("name"),
        )

    private fun String.toCredentialMatchScheme(): CredentialMatch.Scheme =
        when (lowercase()) {
            "https" -> CredentialMatch.Scheme.HTTPS
            "http" -> CredentialMatch.Scheme.HTTP
            else -> throw IllegalStateException("Unsupported Credential Match scheme: $this")
        }

    private fun JsonObject.requiredString(name: String): String =
        get(name)?.jsonPrimitive?.content
            ?: throw IllegalStateException("Credential Vault response missing required string field: $name")

    private fun JsonObject.optionalString(name: String): String? = get(name)?.takeUnless { it is JsonNull }?.jsonPrimitive?.contentOrNull

    private fun JsonObject.requiredInt(name: String): Int =
        get(name)?.jsonPrimitive?.int
            ?: throw IllegalStateException("Credential Vault response missing required integer field: $name")

    private fun JsonObject.requiredArray(name: String): JsonArray =
        get(name)?.jsonArray
            ?: throw IllegalStateException("Credential Vault response missing required array field: $name")

    private fun JsonObject.optionalObject(name: String): JsonObject? = get(name)?.takeUnless { it is JsonNull }?.jsonObject

    private fun JsonObject.optionalArray(name: String): JsonArray? = get(name)?.takeUnless { it is JsonNull }?.jsonArray

    private fun JsonObject.requiredStringArray(name: String): List<String> = requiredArray(name).map { it.requiredStringValue() }

    private fun JsonObject.optionalStringArray(name: String): List<String>? = optionalArray(name)?.map { it.requiredStringValue() }

    private fun JsonObject.optionalIntArray(name: String): List<Int>? = optionalArray(name)?.map { it.jsonPrimitive.int }

    private fun JsonElement.requiredStringValue(): String = jsonPrimitive.content
}
