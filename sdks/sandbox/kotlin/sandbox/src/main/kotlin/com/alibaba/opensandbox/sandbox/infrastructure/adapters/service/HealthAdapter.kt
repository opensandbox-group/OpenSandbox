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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.execd.HealthApi
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Health
import com.alibaba.opensandbox.sandbox.transport.RequestDeadline
import org.slf4j.LoggerFactory

/**
 * Implementation of [Health] that adapts OpenAPI-generated [HealthApi].
 */
internal class HealthAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
) : Health {
    private val logger = LoggerFactory.getLogger(HealthAdapter::class.java)
    private val client =
        (
            if (httpClientProvider.config.singleAttemptHealthChecks) {
                httpClientProvider.singleAttemptClient
            } else {
                httpClientProvider.httpClient
            }
        ).newBuilder().addInterceptor { chain ->
            val builder = chain.request().newBuilder()
            execdEndpoint.headers.forEach { (key, value) -> builder.header(key, value) }
            chain.proceed(builder.build())
        }.build()

    override fun ping(sandboxId: String): Boolean {
        logger.debug("Checking health for sandbox: {}", sandboxId)

        return try {
            RequestDeadline.execute(client) { boundedClient ->
                HealthApi("${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}", boundedClient).ping()
            }
            logger.debug("Health check successful for sandbox {}", sandboxId)
            true
        } catch (e: InterruptedException) {
            Thread.currentThread().interrupt()
            throw e
        } catch (e: Exception) {
            logger.debug("Health check failed for sandbox: {}", sandboxId, e)
            false
        }
    }
}
