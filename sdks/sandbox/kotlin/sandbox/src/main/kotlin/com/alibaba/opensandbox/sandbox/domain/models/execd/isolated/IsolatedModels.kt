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

package com.alibaba.opensandbox.sandbox.domain.models.execd.isolated

import java.time.OffsetDateTime

data class IsolatedWorkspaceSpec(
    val path: String,
    val mode: String? = null,
)

data class EnvPassthroughSpec(
    val mode: String = "deny",
    val keys: List<String> = emptyList(),
)

data class BindMount(
    val source: String,
    val dest: String? = null,
    val readonly: Boolean? = null,
)

data class CreateIsolatedSessionRequest(
    val workspace: IsolatedWorkspaceSpec,
    val profile: String? = null,
    val extraWritable: List<String>? = null,
    val binds: List<BindMount>? = null,
    val shareNet: Boolean? = null,
    val envPassthrough: EnvPassthroughSpec? = null,
    val uid: Long? = null,
    val gid: Long? = null,
    val uidMode: String? = null,
    val idleTimeoutSeconds: Int? = null,
)

data class IsolatedSessionInfo(
    val sessionId: String,
    val createdAt: OffsetDateTime?,
)

data class IsolatedSessionState(
    val status: String,
    val createdAt: OffsetDateTime? = null,
    val lastRunAt: OffsetDateTime? = null,
    val idleRemainingSeconds: Int? = null,
)

data class IsolatedSessionSummary(
    val sessionId: String,
    val status: String,
    val createdAt: OffsetDateTime? = null,
    val lastRunAt: OffsetDateTime? = null,
    val idleRemainingSeconds: Int? = null,
)

data class IsolatedRunRequest(
    val code: String,
    val envs: Map<String, String>? = null,
    val timeoutSeconds: Int? = null,
)

data class IsolatedCapabilities(
    val available: Boolean = false,
    val isolator: String? = null,
    val version: String? = null,
    val message: String? = null,
    val commitSupported: Boolean = false,
    val diffSupported: Boolean = false,
)
