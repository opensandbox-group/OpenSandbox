// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.models.CreateSandboxResponse
import com.alibaba.opensandbox.sandbox.api.models.Sandbox
import com.alibaba.opensandbox.sandbox.api.models.SandboxStatus
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxCreateResponse
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxInfo
import org.junit.jupiter.api.Assertions.assertEquals
import org.junit.jupiter.api.Test
import java.time.OffsetDateTime

class ResolvedImageDigestTest {
    @Test
    fun `converters preserve optional runtime digest`() {
        val createdAt = OffsetDateTime.parse("2026-10-09T00:00:00Z")
        for (digest in listOf(null, "sha256:" + "a".repeat(64))) {
            val info =
                Sandbox(
                    id = "sandbox-1",
                    status = SandboxStatus(state = "Running"),
                    entrypoint = listOf("python"),
                    createdAt = createdAt,
                    resolvedImageDigest = digest,
                ).toSandboxInfo()
            val created =
                CreateSandboxResponse(
                    id = "sandbox-1",
                    status = SandboxStatus(state = "Running"),
                    entrypoint = listOf("python"),
                    createdAt = createdAt,
                    resolvedImageDigest = digest,
                ).toSandboxCreateResponse()
            assertEquals(digest, info.resolvedImageDigest)
            assertEquals(digest, created.resolvedImageDigest)
        }
    }
}
