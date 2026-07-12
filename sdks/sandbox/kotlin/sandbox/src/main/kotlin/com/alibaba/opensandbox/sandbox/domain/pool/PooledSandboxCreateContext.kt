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

package com.alibaba.opensandbox.sandbox.domain.pool

import com.alibaba.opensandbox.sandbox.Sandbox
import com.alibaba.opensandbox.sandbox.config.ConnectionConfig
import java.time.Duration

/**
 * Context passed to [PooledSandboxCreator] when the pool needs a new sandbox.
 *
 * @property poolName User-defined pool namespace.
 * @property ownerId Current pool owner identity.
 * @property idleTimeout Pool idle timeout to apply to the newly created sandbox.
 * @property reason Why the pool is creating a sandbox.
 * @property readyTimeout Max time to wait for the created sandbox to become ready.
 * @property healthCheckPollingInterval Poll interval while waiting for readiness.
 * @property skipHealthCheck Whether readiness checks should be skipped for this creation.
 * @property healthCheck Optional custom health check for the created sandbox.
 * @property connectionConfig Connection config for lifecycle API calls.
 */
class PooledSandboxCreateContext internal constructor(
    val poolName: String,
    val ownerId: String,
    val idleTimeout: Duration,
    val reason: Reason,
    val readyTimeout: Duration,
    val healthCheckPollingInterval: Duration,
    val skipHealthCheck: Boolean,
    val healthCheck: ((Sandbox) -> Boolean)?,
    val connectionConfig: ConnectionConfig,
) {
    enum class Reason {
        WARMUP,
        DIRECT_CREATE,
    }
}
