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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.Credential
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBinding
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialBindingMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMetadata
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialMutationSet
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultCreateRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultPatchRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialVaultState
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule

interface CredentialVault {
    fun create(request: CredentialVaultCreateRequest): CredentialVaultState

    fun create(
        credentials: List<Credential>,
        bindings: List<CredentialBinding>,
    ): CredentialVaultState =
        create(
            CredentialVaultCreateRequest.builder()
                .credentials(credentials)
                .bindings(bindings)
                .build(),
        )

    fun get(): CredentialVaultState

    fun patch(request: CredentialVaultPatchRequest): CredentialVaultState

    fun patch(
        expectedRevision: Int? = null,
        credentials: CredentialMutationSet? = null,
        bindings: CredentialBindingMutationSet? = null,
    ): CredentialVaultState =
        patch(
            CredentialVaultPatchRequest.builder()
                .expectedRevision(expectedRevision)
                .credentials(credentials)
                .bindings(bindings)
                .build(),
        )

    fun delete()

    fun listCredentials(): List<CredentialMetadata>

    fun getCredential(name: String): CredentialMetadata

    fun listBindings(): List<CredentialBindingMetadata>

    fun getBinding(name: String): CredentialBindingMetadata
}

interface Egress {
    fun getPolicy(): NetworkPolicy

    fun patchRules(rules: List<NetworkRule>)

    fun deleteRules(targets: List<String>)
}
