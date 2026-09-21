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
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import org.slf4j.LoggerFactory

/**
 * Egress implementation backed by the lifecycle control plane
 * (`/sandboxes/{sandboxId}/networkpolicy`).
 *
 * Template-backed sandboxes run without a sandbox-side egress sidecar, so their
 * egress policy is managed through the lifecycle control plane instead. Credential
 * Vault is rejected because it requires the sandbox-side egress sidecar.
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

    override fun create(request: CredentialVaultCreateRequest): CredentialVaultState = unsupported()

    override fun get(): CredentialVaultState = unsupported()

    override fun patch(request: CredentialVaultPatchRequest): CredentialVaultState = unsupported()

    override fun delete() = unsupported()

    override fun listCredentials(): List<CredentialMetadata> = unsupported()

    override fun getCredential(name: String): CredentialMetadata = unsupported()

    override fun listBindings(): List<CredentialBindingMetadata> = unsupported()

    override fun getBinding(name: String): CredentialBindingMetadata = unsupported()

    private fun unsupported(): Nothing = throw UnsupportedOperationException(CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE)

    companion object {
        const val CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE =
            "Credential Vault requires the sandbox-side egress sidecar and is not available " +
                "for sandboxes created from fsb templates."
    }
}
