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
import com.alibaba.opensandbox.sandbox.api.execd.MetricApi
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxMetrics
import com.alibaba.opensandbox.sandbox.domain.services.Metrics
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toSandboxMetrics
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import org.slf4j.LoggerFactory

/**
 * Implementation of [Metrics] that adapts OpenAPI-generated [MetricApi].
 */
internal class MetricsAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
) : Metrics {
    private val logger = LoggerFactory.getLogger(MetricsAdapter::class.java)
    private val api =
        MetricApi(
            "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}",
            httpClientProvider.httpClient.newBuilder()
                .addInterceptor { chain ->
                    val requestBuilder = chain.request().newBuilder()
                    execdEndpoint.headers.forEach { (key, value) ->
                        requestBuilder.header(key, value)
                    }
                    chain.proceed(requestBuilder.build())
                }
                .build(),
        )

    override fun getMetrics(sandboxId: String): SandboxMetrics {
        logger.debug("Retrieving sandbox metrics for {}", sandboxId)
        return try {
            api.getMetrics().toSandboxMetrics()
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }
}
