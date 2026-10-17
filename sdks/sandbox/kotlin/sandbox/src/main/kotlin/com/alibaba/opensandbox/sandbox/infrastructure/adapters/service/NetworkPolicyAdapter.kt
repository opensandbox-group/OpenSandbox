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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.SandboxesApi
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.services.CredentialVault
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiNetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainNetworkPolicy
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxApiException
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.slf4j.LoggerFactory

/** JSON media type shared by the raw vault transport. */
private val JSON_MEDIA_TYPE = "application/json; charset=utf-8".toMediaType()

/**
 * Egress and Credential Vault implementation backed by the lifecycle control
 * plane (`/sandboxes/{sandboxId}/networkpolicy` and
 * `/sandboxes/{sandboxId}/credential-vault`).
 *
 * Template-backed sandboxes run without a sandbox-side egress sidecar, so their
 * egress policy is managed through the lifecycle control plane, and the
 * credential vault is egress-held behind the server (OSEP-0022): the server
 * relays vault revisions over the FastPath handler relay and the relay
 * response is the synchronous ack. A 412 patch failure means another writer
 * won the revision — re-read and retry with the fresh `expectedRevision`.
 */
internal class NetworkPolicyAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val sandboxId: String,
) : Egress, CredentialVault {
    private val logger = LoggerFactory.getLogger(NetworkPolicyAdapter::class.java)

    private val api = SandboxesApi(httpClientProvider.config.getBaseUrl(), httpClientProvider.authenticatedClient)

    override fun getPolicy(): NetworkPolicy {
        return try {
            val policy =
                api.getSandboxNetworkPolicy(sandboxId).policy
                    ?: throw IllegalStateException("Sandbox network policy response did not contain policy payload")
            policy.toDomainNetworkPolicy()
        } catch (e: Exception) {
            logger.error("Failed to fetch network policy for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun patchRules(rules: List<NetworkRule>) {
        try {
            api.patchSandboxNetworkPolicy(sandboxId, rules.map { it.toApiNetworkRule() })
        } catch (e: Exception) {
            logger.error("Failed to patch network policy for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun deleteRules(targets: List<String>) {
        try {
            api.deleteSandboxNetworkPolicyRules(sandboxId, targets)
        } catch (e: Exception) {
            logger.error("Failed to delete network policy rules for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun create(request: CredentialVaultCreateRequest): CredentialVaultState {
        return try {
            val responseBody =
                vaultRequestJson(
                    method = "POST",
                    pathSuffix = "",
                    operation = "Create credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Credential Vault create response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to create credential vault for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun get(): CredentialVaultState {
        return try {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "",
                    operation = "Get credential vault",
                ) ?: throw IllegalStateException("Credential Vault get response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to get credential vault for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun patch(request: CredentialVaultPatchRequest): CredentialVaultState {
        return try {
            val responseBody =
                vaultRequestJson(
                    method = "PATCH",
                    pathSuffix = "",
                    operation = "Patch credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Credential Vault patch response did not contain body")
            responseBody.toCredentialVaultState()
        } catch (e: Exception) {
            logger.error("Failed to patch credential vault for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun delete() {
        try {
            vaultRequestJson(
                method = "DELETE",
                pathSuffix = "",
                operation = "Delete credential vault",
            )
        } catch (e: Exception) {
            logger.error("Failed to delete credential vault for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun listCredentials(): List<CredentialMetadata> {
        return try {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "/credentials",
                    operation = "List credential vault credentials",
                ) ?: throw IllegalStateException("Credential Vault credential list response did not contain body")
            responseBody.toCredentialListResponse().credentials
        } catch (e: Exception) {
            logger.error("Failed to list credential vault credentials for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun getCredential(name: String): CredentialMetadata {
        return try {
            vaultRequestJson(
                method = "GET",
                pathSuffix = "/credentials",
                pathSegment = name,
                operation = "Get credential vault credential",
            )?.toCredentialMetadata()
                ?: throw IllegalStateException("Credential Vault credential response did not contain body")
        } catch (e: Exception) {
            logger.error("Failed to get credential vault credential for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun listBindings(): List<CredentialBindingMetadata> {
        return try {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "/bindings",
                    operation = "List credential vault bindings",
                ) ?: throw IllegalStateException("Credential Vault binding list response did not contain body")
            responseBody.toCredentialBindingListResponse().bindings
        } catch (e: Exception) {
            logger.error("Failed to list credential vault bindings for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun getBinding(name: String): CredentialBindingMetadata {
        return try {
            vaultRequestJson(
                method = "GET",
                pathSuffix = "/bindings",
                pathSegment = name,
                operation = "Get credential vault binding",
            )?.toCredentialBindingMetadata()
                ?: throw IllegalStateException("Credential Vault binding response did not contain body")
        } catch (e: Exception) {
            logger.error("Failed to get credential vault binding for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    /**
     * Raw vault request against the lifecycle control plane
     * (`/sandboxes/{sandboxId}/credential-vault[...]'). Returns the decoded
     * response body, or null for a 204/empty body.
     */
    private fun vaultRequestJson(
        method: String,
        pathSuffix: String,
        operation: String,
        pathSegment: String? = null,
        jsonBody: JsonObject? = null,
    ): String? {
        val url =
            httpClientProvider.config
                .getBaseUrl()
                .toHttpUrl()
                .newBuilder()
                .addPathSegments("sandboxes")
                .addPathSegment(sandboxId)
                .addPathSegment("credential-vault")
                .apply {
                    if (pathSuffix.isNotEmpty()) addPathSegment(pathSuffix.removePrefix("/"))
                    pathSegment?.let { addPathSegment(it) }
                }
                .build()
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
                    (jsonBody ?: buildJsonObject { }).toString().toRequestBody(JSON_MEDIA_TYPE),
                )
        }

        httpClientProvider.authenticatedClient.newCall(requestBuilder.build()).execute().use { response ->
            val responseBody = response.body?.string().orEmpty()
            if (response.isSuccessful) {
                return if (response.code == 204 || responseBody.isBlank()) null else responseBody
            }
            throw response.toSandboxApiException(responseBody) { statusCode, body ->
                "$operation failed. Status code: $statusCode, Body: $body"
            }
        }
    }
}
