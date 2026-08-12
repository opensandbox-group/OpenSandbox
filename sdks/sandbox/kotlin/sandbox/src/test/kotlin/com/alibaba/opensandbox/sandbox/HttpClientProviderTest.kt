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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import com.alibaba.opensandbox.sandbox.transport.RetryInterceptor
import com.alibaba.opensandbox.sandbox.transport.RetryPolicy
import org.junit.jupiter.api.Assertions.assertFalse
import org.junit.jupiter.api.Test
import java.time.Duration

class HttpClientProviderTest {
    @Test
    fun `SSE client disables all automatic retries for every retry policy`() {
        val policies =
            listOf(
                RetryPolicy(),
                RetryPolicy.disabled(),
                RetryPolicy(maxRetries = 0, overallDeadline = Duration.ofSeconds(5)),
            )

        policies.forEach { policy ->
            HttpClientProvider(
                ConnectionConfig.builder()
                    .retryPolicy(policy)
                    .build(),
            ).use { provider ->
                assertFalse(provider.sseClient.retryOnConnectionFailure)
                assertFalse(provider.sseClient.interceptors.any { it is RetryInterceptor })
            }
        }
    }
}
