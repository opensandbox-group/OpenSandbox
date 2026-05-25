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
import com.alibaba.opensandbox.sandbox.api.egress.PolicyApi
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkPolicy
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.NetworkRule
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Egress
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toApiEgressNetworkRule
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.SandboxModelConverter.toDomainEgressNetworkPolicy
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import org.slf4j.LoggerFactory

internal class EgressAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val egressEndpoint: SandboxEndpoint,
) : Egress {
    private val logger = LoggerFactory.getLogger(EgressAdapter::class.java)
    private val api =
        PolicyApi(
            "${httpClientProvider.config.protocol}://${egressEndpoint.endpoint}",
            httpClientProvider.httpClient.newBuilder()
                .addInterceptor { chain ->
                    val requestBuilder = chain.request().newBuilder()
                    egressEndpoint.headers.forEach { (key, value) ->
                        requestBuilder.header(key, value)
                    }
                    chain.proceed(requestBuilder.build())
                }
                .build(),
        )

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
}
