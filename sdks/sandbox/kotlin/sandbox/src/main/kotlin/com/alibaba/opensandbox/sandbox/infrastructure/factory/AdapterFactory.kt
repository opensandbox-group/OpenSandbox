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

package com.alibaba.opensandbox.sandbox.infrastructure.factory

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.Health
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import com.alibaba.opensandbox.sandbox.domain.services.Sandboxes
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.CommandsAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.DiagnosticsAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.EgressAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.FilesystemAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.HealthAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.MetricsAdapter
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.service.SandboxesAdapter

/**
 * Factory responsible for creating adapter instances.
 *
 * This factory encapsulates the instantiation logic of specific adapters,
 * decoupling the Sandbox domain object from infrastructure implementation details.
 */
internal class AdapterFactory(
    private val httpClientProvider: HttpClientProvider,
) {
    fun createSandboxes(): Sandboxes {
        return SandboxesAdapter(httpClientProvider)
    }

    fun createDiagnostics(): Diagnostics {
        return DiagnosticsAdapter(httpClientProvider)
    }

    fun createFilesystem(endpoint: SandboxEndpoint): Filesystem {
        return FilesystemAdapter(httpClientProvider, endpoint)
    }

    fun createCommands(endpoint: SandboxEndpoint): Commands {
        return CommandsAdapter(httpClientProvider, endpoint)
    }

    fun createEgress(endpoint: SandboxEndpoint): Egress {
        return EgressAdapter(httpClientProvider, endpoint)
    }

    fun createMetrics(endpoint: SandboxEndpoint): Metrics {
        return MetricsAdapter(httpClientProvider, endpoint)
    }

    fun createHealth(endpoint: SandboxEndpoint): Health {
        return HealthAdapter(httpClientProvider, endpoint)
    }
}
