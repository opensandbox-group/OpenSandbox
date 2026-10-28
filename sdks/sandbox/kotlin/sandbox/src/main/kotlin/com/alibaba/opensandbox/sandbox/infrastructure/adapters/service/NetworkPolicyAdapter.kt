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
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
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
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.JsonObject
import okhttp3.HttpUrl.Companion.toHttpUrl
import org.slf4j.LoggerFactory

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
        return vaultCall("Create credential vault") {
            val responseBody =
                vaultRequestJson(
                    method = "POST",
                    pathSuffix = "",
                    operation = "Create credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Create credential vault response did not contain body")
            responseBody.toCredentialVaultState()
        }
    }

    override fun get(): CredentialVaultState {
        return vaultCall("Get credential vault") {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "",
                    operation = "Get credential vault",
                ) ?: throw IllegalStateException("Get credential vault response did not contain body")
            responseBody.toCredentialVaultState()
        }
    }

    override fun patch(request: CredentialVaultPatchRequest): CredentialVaultState {
        return vaultCall("Patch credential vault") {
            val responseBody =
                vaultRequestJson(
                    method = "PATCH",
                    pathSuffix = "",
                    operation = "Patch credential vault",
                    jsonBody = request.toJsonObject(),
                ) ?: throw IllegalStateException("Patch credential vault response did not contain body")
            responseBody.toCredentialVaultState()
        }
    }

    override fun delete(): Unit =
        vaultCall("Delete credential vault") {
            vaultRequestJson(
                method = "DELETE",
                pathSuffix = "",
                operation = "Delete credential vault",
            )
        }

    override fun listCredentials(): List<CredentialMetadata> {
        return vaultCall("List credential vault credentials") {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "/credentials",
                    operation = "List credential vault credentials",
                )
                    ?: throw IllegalStateException(
                        "List credential vault credentials response did not contain body",
                    )
            responseBody.toCredentialListResponse().credentials
        }
    }

    override fun getCredential(name: String): CredentialMetadata {
        return vaultCall("Get credential vault credential") {
            vaultRequestJson(
                method = "GET",
                pathSuffix = "/credentials",
                pathSegment = name,
                operation = "Get credential vault credential",
            )?.toCredentialMetadata()
                ?: throw IllegalStateException("Get credential vault credential response did not contain body")
        }
    }

    override fun listBindings(): List<CredentialBindingMetadata> {
        return vaultCall("List credential vault bindings") {
            val responseBody =
                vaultRequestJson(
                    method = "GET",
                    pathSuffix = "/bindings",
                    operation = "List credential vault bindings",
                )
                    ?: throw IllegalStateException(
                        "List credential vault bindings response did not contain body",
                    )
            responseBody.toCredentialBindingListResponse().bindings
        }
    }

    override fun getBinding(name: String): CredentialBindingMetadata {
        return vaultCall("Get credential vault binding") {
            vaultRequestJson(
                method = "GET",
                pathSuffix = "/bindings",
                pathSegment = name,
                operation = "Get credential vault binding",
            )?.toCredentialBindingMetadata()
                ?: throw IllegalStateException("Get credential vault binding response did not contain body")
        }
    }

    /**
     * Uniform error handling for the vault operations: API failures are
     * logged as operation + HTTP status only — the exception message and
     * responseBody can carry a backend error body that echoes submitted
     * vault material, so the exception object itself is never logged.
     * Transport-level failures (no vault material involved) keep the full
     * stack trace.
     */
    private fun <T> vaultCall(
        operation: String,
        block: () -> T,
    ): T =
        try {
            block()
        } catch (e: SandboxApiException) {
            logger.error(
                "Credential Vault {} failed for sandbox {} with HTTP status {}",
                operation,
                sandboxId,
                e.statusCode,
            )
            throw e
        } catch (e: Exception) {
            logger.error("Credential Vault {} failed for sandbox {}", operation, sandboxId, e)
            throw e.toSandboxException()
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
        return httpClientProvider.authenticatedClient.executeVaultRequest(
            url = url,
            method = method,
            operation = operation,
            jsonBody = jsonBody,
        )
    }
}
