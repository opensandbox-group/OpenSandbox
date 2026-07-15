/*
 * Copyright 2026 Alibaba Group Holding Ltd.
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

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.CredentialProxyConfig
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
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
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Assertions.assertThrows
import org.junit.jupiter.api.Assertions.assertTrue
import org.junit.jupiter.api.Test
import java.time.Duration
import java.time.OffsetDateTime

class SandboxesCompatibilityTest {
    @Test
    fun `custom implementation can implement previous createSandbox signature`() {
        val sandboxes = LegacySandboxes()

        val response =
            sandboxes.createSandbox(
                spec = null,
                entrypoint = null,
                env = emptyMap(),
                metadata = emptyMap(),
                timeout = null,
                resource = emptyMap(),
                networkPolicy = null,
                extensions = emptyMap(),
                volumes = null,
                credentialProxy = null,
            )

        assertEquals("legacy-sandbox", response.id)
    }

    @Test
    fun `default credential proxy overload rejects unsupported non-null config`() {
        val sandboxes = LegacySandboxes()

        val error =
            assertThrows(UnsupportedOperationException::class.java) {
                sandboxes.createSandbox(
                    spec = null,
                    entrypoint = null,
                    env = emptyMap(),
                    metadata = emptyMap(),
                    timeout = null,
                    resource = emptyMap(),
                    networkPolicy = null,
                    extensions = emptyMap(),
                    volumes = null,
                    credentialProxy = CredentialProxyConfig.enabled(),
                )
            }

        assertTrue(error.message!!.contains("Credential Vault proxy is not supported"))
    }

    @Test
    fun `custom egress implementation can stay policy only`() {
        val egress: Egress = PolicyOnlyEgress()

        assertEquals(NetworkPolicy.DefaultAction.DENY, egress.getPolicy().defaultAction)
    }

    private class LegacySandboxes : Sandboxes {
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
            resourceRequests: Map<String, String>?,
        ): SandboxCreateResponse = SandboxCreateResponse("legacy-sandbox")

        override fun getSandboxInfo(sandboxId: String): SandboxInfo = unsupported()

        override fun listSandboxes(filter: SandboxFilter): PagedSandboxInfos = unsupported()

        override fun patchSandboxMetadata(
            sandboxId: String,
            patch: Map<String, String?>,
        ): SandboxInfo = unsupported()

        override fun renewSandboxExpiration(
            sandboxId: String,
            newExpirationTime: OffsetDateTime,
        ): SandboxRenewResponse = unsupported()

        override fun createSnapshot(
            sandboxId: String,
            name: String?,
        ): SnapshotInfo = unsupported()

        override fun getSnapshot(snapshotId: String): SnapshotInfo = unsupported()

        override fun listSnapshots(filter: SnapshotFilter): PagedSnapshotInfos = unsupported()

        override fun deleteSnapshot(snapshotId: String) = unsupported()

        override fun getSandboxEndpoint(
            sandboxId: String,
            port: Int,
        ): SandboxEndpoint = unsupported()

        override fun getSandboxEndpoint(
            sandboxId: String,
            port: Int,
            useServerProxy: Boolean,
        ): SandboxEndpoint = unsupported()

        override fun getSignedSandboxEndpoint(
            sandboxId: String,
            port: Int,
            expires: Long,
            useServerProxy: Boolean,
        ): SandboxEndpoint = unsupported()

        override fun pauseSandbox(sandboxId: String) = unsupported()

        override fun resumeSandbox(sandboxId: String) = unsupported()

        override fun killSandbox(sandboxId: String) = unsupported()

        private fun unsupported(): Nothing = throw UnsupportedOperationException("not used")
    }

    private class PolicyOnlyEgress : Egress {
        override fun getPolicy(): NetworkPolicy = NetworkPolicy.builder().build()

        override fun patchRules(rules: List<NetworkRule>) {
        }

        override fun deleteRules(targets: List<String>) {
        }
    }
}
