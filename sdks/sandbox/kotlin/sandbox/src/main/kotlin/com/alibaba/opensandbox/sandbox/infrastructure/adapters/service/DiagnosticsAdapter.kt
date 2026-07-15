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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.diagnostic.DiagnosticsApi
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent
import com.alibaba.opensandbox.sandbox.domain.services.Diagnostics
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.DiagnosticModelConverter.toDiagnosticContent
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import okhttp3.Interceptor
import org.slf4j.LoggerFactory

internal class DiagnosticsAdapter(
    private val provider: HttpClientProvider,
) : Diagnostics {
    private val logger = LoggerFactory.getLogger(DiagnosticsAdapter::class.java)
    private val diagnosticsClient =
        provider.authenticatedClient.newBuilder()
            .addInterceptor(
                Interceptor { chain ->
                    chain.proceed(
                        chain.request().newBuilder()
                            .header("Accept", "application/json")
                            .build(),
                    )
                },
            )
            .build()
    private val api = DiagnosticsApi(provider.config.getBaseUrl(), diagnosticsClient)

    override fun getLogs(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent {
        return try {
            api.sandboxesSandboxIdDiagnosticsLogsGet(sandboxId, scope).toDiagnosticContent()
        } catch (e: Exception) {
            logger.error("Failed to get diagnostic logs for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }

    override fun getEvents(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent {
        return try {
            api.sandboxesSandboxIdDiagnosticsEventsGet(sandboxId, scope).toDiagnosticContent()
        } catch (e: Exception) {
            logger.error("Failed to get diagnostic events for sandbox {}", sandboxId, e)
            throw e.toSandboxException()
        }
    }
}
