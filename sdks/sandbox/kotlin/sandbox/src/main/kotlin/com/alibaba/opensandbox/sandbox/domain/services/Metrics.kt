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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxMetrics

/**
 * Metrics collection and monitoring operations for sandbox environments.
 */
interface Metrics {
    /**
     * Retrieves current resource utilization metrics for a sandbox.
     *
     * @param sandboxId Unique identifier of the target sandbox
     * @return Current resource utilization snapshot
     */
    fun getMetrics(sandboxId: String): SandboxMetrics
}
