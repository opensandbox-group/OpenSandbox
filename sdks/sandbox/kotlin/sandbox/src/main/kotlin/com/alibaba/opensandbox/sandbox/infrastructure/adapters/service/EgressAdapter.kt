/*
 * Copyright 2025 The OpenSandbox Authors
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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.CredentialVault
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiEgressNetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainEgressNetworkPolicy
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.JsonObject
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import org.slf4j.LoggerFactory

internal class EgressAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val egressEndpoint: SandboxEndpoint,
) : Egress, CredentialVault {
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
    ): String? =
        egressClient.executeVaultRequest(
            url = credentialVaultUrl(pathSegments),
            method = method,
            operation = operation,
            jsonBody = jsonBody,
        )

    private fun credentialVaultUrl(pathSegments: List<String>): HttpUrl {
        val builder = egressBaseUrl.toHttpUrl().newBuilder().addPathSegment("credential-vault")
        pathSegments.forEach { builder.addPathSegment(it) }
        return builder.build()
    }
}
