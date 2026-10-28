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

@file:Suppress("TooManyFunctions")

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialSubstitution
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CustomHeaderEntry
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.InlineCredentialSource
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxApiException
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
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody

/** Shared Credential Vault JSON codec for the sidecar and lifecycle transports. */

internal fun CredentialVaultCreateRequest.toJsonObject(): JsonObject =
    buildJsonObject {
        put("credentials", credentials.toCredentialJsonArray())
        put("bindings", bindings.toBindingJsonArray())
    }

internal fun CredentialVaultPatchRequest.toJsonObject(): JsonObject =
    buildJsonObject {
        expectedRevision?.let { put("expectedRevision", JsonPrimitive(it)) }
        credentials?.let { put("credentials", it.toJsonObject()) }
        bindings?.let { put("bindings", it.toJsonObject()) }
    }

internal fun CredentialMutationSet.toJsonObject(): JsonObject =
    buildJsonObject {
        add?.let { put("add", it.toCredentialJsonArray()) }
        replace?.let { put("replace", it.toCredentialJsonArray()) }
        delete?.let { put("delete", it.toStringJsonArray()) }
    }

internal fun CredentialBindingMutationSet.toJsonObject(): JsonObject =
    buildJsonObject {
        add?.let { put("add", it.toBindingJsonArray()) }
        replace?.let { put("replace", it.toBindingJsonArray()) }
        delete?.let { put("delete", it.toStringJsonArray()) }
    }

internal fun List<Credential>.toCredentialJsonArray(): JsonArray = JsonArray(map { it.toJsonObject() })

internal fun Credential.toJsonObject(): JsonObject =
    buildJsonObject {
        put("name", JsonPrimitive(name))
        put("source", source.toJsonObject())
    }

internal fun InlineCredentialSource.toJsonObject(): JsonObject =
    buildJsonObject {
        put("type", JsonPrimitive(type))
        put("value", JsonPrimitive(value))
    }

internal fun List<CredentialBinding>.toBindingJsonArray(): JsonArray = JsonArray(map { it.toJsonObject() })

internal fun CredentialBinding.toJsonObject(): JsonObject =
    buildJsonObject {
        put("name", JsonPrimitive(name))
        put("match", match.toJsonObject())
        put("auth", auth.toJsonObject())
    }

internal fun CredentialMatch.toJsonObject(): JsonObject =
    buildJsonObject {
        schemes?.let { put("schemes", it.map { scheme -> scheme.wireName() }.toStringJsonArray()) }
        put("hosts", hosts.toStringJsonArray())
        methods?.let { put("methods", it.toStringJsonArray()) }
        paths?.let { put("paths", it.toStringJsonArray()) }
    }

internal fun CredentialAuth.toJsonObject(): JsonObject =
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
            CredentialAuth.Type.PASSTHROUGH -> Unit
        }
        substitutions.takeUnless { it.isNullOrEmpty() }?.let {
            put("substitutions", JsonArray(it.map { substitution -> substitution.toJsonObject() }))
        }
    }

internal fun CustomHeaderEntry.toJsonObject(): JsonObject =
    buildJsonObject {
        put("name", JsonPrimitive(name))
        put("credential", JsonPrimitive(credential))
    }

internal fun CredentialSubstitution.toJsonObject(): JsonObject =
    buildJsonObject {
        put("credential", JsonPrimitive(credential))
        put("placeholder", JsonPrimitive(placeholder))
        put("in", surfaces.map { it.wireName() }.toStringJsonArray())
    }

internal fun List<String>.toStringJsonArray(): JsonArray = JsonArray(map { JsonPrimitive(it) })

internal fun CredentialMatch.Scheme.wireName(): String =
    when (this) {
        CredentialMatch.Scheme.HTTPS -> "https"
        CredentialMatch.Scheme.HTTP -> "http"
    }

internal fun CredentialAuth.Type.wireName(): String =
    when (this) {
        CredentialAuth.Type.BEARER -> "bearer"
        CredentialAuth.Type.BASIC -> "basic"
        CredentialAuth.Type.API_KEY -> "apiKey"
        CredentialAuth.Type.CUSTOM_HEADERS -> "customHeaders"
        CredentialAuth.Type.PASSTHROUGH -> "passthrough"
    }

internal fun CredentialSubstitution.Surface.wireName(): String =
    when (this) {
        CredentialSubstitution.Surface.PATH -> "path"
        CredentialSubstitution.Surface.QUERY -> "query"
        CredentialSubstitution.Surface.HEADER -> "header"
        CredentialSubstitution.Surface.BODY -> "body"
    }

internal fun String.toCredentialVaultState(): CredentialVaultState {
    val root = jsonParser.parseToJsonElement(this).jsonObject
    return CredentialVaultState(
        revision = root.requiredInt("revision"),
        credentials = root.requiredArray("credentials").map { it.jsonObject.toCredentialMetadata() },
        bindings = root.requiredArray("bindings").map { it.jsonObject.toCredentialBindingMetadata() },
    )
}

internal fun String.toCredentialListResponse(): CredentialListResponse {
    val root = jsonParser.parseToJsonElement(this).jsonObject
    return CredentialListResponse(
        revision = root.requiredInt("revision"),
        credentials = root.requiredArray("credentials").map { it.jsonObject.toCredentialMetadata() },
    )
}

internal fun String.toCredentialBindingListResponse(): CredentialBindingListResponse {
    val root = jsonParser.parseToJsonElement(this).jsonObject
    return CredentialBindingListResponse(
        revision = root.requiredInt("revision"),
        bindings = root.requiredArray("bindings").map { it.jsonObject.toCredentialBindingMetadata() },
    )
}

internal fun String.toCredentialMetadata(): CredentialMetadata = jsonParser.parseToJsonElement(this).jsonObject.toCredentialMetadata()

internal fun String.toCredentialBindingMetadata(): CredentialBindingMetadata =
    jsonParser.parseToJsonElement(this).jsonObject.toCredentialBindingMetadata()

internal fun JsonObject.toCredentialMetadata(): CredentialMetadata =
    CredentialMetadata(
        name = requiredString("name"),
        sourceType = requiredString("sourceType"),
        revision = requiredInt("revision"),
    )

internal fun JsonObject.toCredentialBindingMetadata(): CredentialBindingMetadata =
    CredentialBindingMetadata(
        name = requiredString("name"),
        revision = requiredInt("revision"),
        match = optionalObject("match")?.toCredentialMatch(),
        auth = optionalObject("auth")?.toCredentialAuthMetadata(),
    )

internal fun JsonObject.toCredentialMatch(): CredentialMatch {
    val builder = CredentialMatch.builder().hosts(requiredStringArray("hosts"))
    optionalStringArray("schemes")?.let { schemes ->
        builder.schemes(schemes.map { it.toCredentialMatchScheme() })
    }
    optionalStringArray("methods")?.let { builder.methods(it) }
    optionalStringArray("paths")?.let { builder.paths(it) }
    return builder.build()
}

internal fun JsonObject.toCredentialAuthMetadata(): CredentialAuthMetadata =
    CredentialAuthMetadata(
        type = requiredString("type"),
        name = optionalString("name"),
    )

internal fun String.toCredentialMatchScheme(): CredentialMatch.Scheme =
    when (lowercase()) {
        "https" -> CredentialMatch.Scheme.HTTPS
        "http" -> CredentialMatch.Scheme.HTTP
        else -> throw IllegalStateException("Unsupported Credential Match scheme: $this")
    }

internal fun JsonObject.requiredString(name: String): String =
    get(name)?.takeUnless { it is JsonNull }?.jsonPrimitive?.content
        ?: throw IllegalStateException("Credential Vault response missing required string field: $name")

internal fun JsonObject.optionalString(name: String): String? = get(name)?.takeUnless { it is JsonNull }?.jsonPrimitive?.contentOrNull

internal fun JsonObject.requiredInt(name: String): Int =
    get(name)?.takeUnless { it is JsonNull }?.jsonPrimitive?.int
        ?: throw IllegalStateException("Credential Vault response missing required integer field: $name")

internal fun JsonObject.requiredArray(name: String): JsonArray =
    get(name)?.takeUnless { it is JsonNull }?.jsonArray
        ?: throw IllegalStateException("Credential Vault response missing required array field: $name")

internal fun JsonObject.optionalObject(name: String): JsonObject? = get(name)?.takeUnless { it is JsonNull }?.jsonObject

internal fun JsonObject.optionalArray(name: String): JsonArray? = get(name)?.takeUnless { it is JsonNull }?.jsonArray

internal fun JsonObject.requiredStringArray(name: String): List<String> = requiredArray(name).map { it.requiredStringValue() }

internal fun JsonObject.optionalStringArray(name: String): List<String>? = optionalArray(name)?.map { it.requiredStringValue() }

internal fun JsonElement.requiredStringValue(): String =
    takeUnless { it is JsonNull }?.jsonPrimitive?.content
        ?: throw IllegalStateException("Credential Vault response contains a null string value")

/** JSON media type shared by both vault transports. */
internal val VAULT_JSON_MEDIA_TYPE = "application/json".toMediaType()

/**
 * Shared raw transport for the vault surfaces: executes one request and
 * returns the decoded body, or null for a 204/empty response. Error messages
 * carry the operation and status only — a backend error body may echo
 * submitted vault material, so it stays on the exception's responseBody for
 * programmatic use and never lands in messages or logs.
 */
internal fun OkHttpClient.executeVaultRequest(
    url: HttpUrl,
    method: String,
    operation: String,
    jsonBody: JsonObject? = null,
): String? {
    val requestBuilder =
        Request.Builder()
            .url(url)
            .header("Accept", "application/json")

    when (method) {
        "GET" -> requestBuilder.get()
        "DELETE" -> requestBuilder.delete()
        else ->
            requestBuilder.method(
                method,
                (jsonBody ?: buildJsonObject { }).toString().toRequestBody(VAULT_JSON_MEDIA_TYPE),
            )
    }

    newCall(requestBuilder.build()).execute().use { response ->
        val responseBody = response.body?.string().orEmpty()
        if (response.isSuccessful) {
            return if (response.code == 204 || responseBody.isBlank()) null else responseBody
        }
        throw response.toSandboxApiException(responseBody) { statusCode, _ ->
            "$operation failed. Status code: $statusCode"
        }
    }
}
