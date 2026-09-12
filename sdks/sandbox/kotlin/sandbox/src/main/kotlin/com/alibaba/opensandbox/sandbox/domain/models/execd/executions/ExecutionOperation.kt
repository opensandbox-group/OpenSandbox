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

package com.alibaba.opensandbox.sandbox.domain.models.execd.executions

import java.time.OffsetDateTime
import java.util.UUID

/** Recovery scope for one execd lifetime. */
data class ExecutionInstance(
    val instanceId: String,
    val issuedAt: Long,
    val retentionSeconds: Long,
    val capacity: Int,
) {
    /** Generate once, persist before creation, and never refresh during recovery. */
    fun newOperationId(): String = "$instanceId.$issuedAt.${UUID.randomUUID().toString().replace("-", "")}"
}

/** Creation only: created does not mean script completion or business success. */
data class ExecutionOperation(
    val id: String,
    val kind: String,
    val state: String,
    val expiresAt: OffsetDateTime,
)
