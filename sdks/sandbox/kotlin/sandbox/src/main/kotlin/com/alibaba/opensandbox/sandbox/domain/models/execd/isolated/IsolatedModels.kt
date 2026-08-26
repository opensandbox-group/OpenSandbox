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
    // Creation-parameter fields echoed by execd (may be absent on older builds).
    val profile: String? = null,
    val workspace: IsolatedWorkspaceSpec? = null,
    val extraWritable: List<String>? = null,
    val binds: List<BindMount>? = null,
    val shareNet: Boolean? = null,
    val envPassthrough: EnvPassthroughSpec? = null,
    val uid: Long? = null,
    val gid: Long? = null,
    val uidMode: String? = null,
    val idleTimeoutSeconds: Int? = null,
)

data class IsolatedSessionState(
    val status: String,
    val createdAt: OffsetDateTime? = null,
    val lastRunAt: OffsetDateTime? = null,
    val idleRemainingSeconds: Int? = null,
    // Creation-parameter fields echoed by execd (may be absent on older builds).
    val profile: String? = null,
    val workspace: IsolatedWorkspaceSpec? = null,
    val extraWritable: List<String>? = null,
    val binds: List<BindMount>? = null,
    val shareNet: Boolean? = null,
    val envPassthrough: EnvPassthroughSpec? = null,
    val uid: Long? = null,
    val gid: Long? = null,
    val uidMode: String? = null,
    val idleTimeoutSeconds: Int? = null,
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

/**
 * Options for running code in an isolated session.
 *
 * Background execution is only available through [IsolationSession.runBackground];
 * this opts type carries no background flag because [IsolationSession.run] is
 * foreground-only.
 */
data class IsolatedRunOpts(
    val envs: Map<String, String>? = null,
    val timeoutSeconds: Int? = null,
)

/**
 * Handle returned when a run is started with `background: true`.
 */
data class IsolatedBackgroundRun(
    val sessionId: String,
    val runId: String,
    val startedAt: OffsetDateTime? = null,
)

/**
 * Lifecycle state of an isolated background run.
 */
data class IsolatedRunStatus(
    val sessionId: String,
    val runId: String,
    val running: Boolean,
    val exitCode: Int? = null,
    val error: String? = null,
    val startedAt: OffsetDateTime? = null,
    val finishedAt: OffsetDateTime? = null,
)

/**
 * Incremental log read of an isolated background run.
 *
 * Each call returns at most 16 MiB; pass the returned [cursor] to the next
 * [IsolationSession.getRunLogs] call to fetch the remainder.
 */
data class IsolatedRunLogs(
    val text: String,
    val cursor: Long,
)

data class IsolatedCapabilities(
    val available: Boolean = false,
    val isolator: String? = null,
    val version: String? = null,
    val message: String? = null,
    val commitSupported: Boolean = false,
    val diffSupported: Boolean = false,
    val setprivAvailable: Boolean = false,
    val usernsAvailable: Boolean = false,
    val hardening: HardeningStatus? = null,
)

/** execd init-mode and workload-hardening state (OSEP-0018). */
data class HardeningStatus(
    // "pid1" | "subreaper" | "none"
    val initMode: String? = null,
    val signalShield: Boolean = false,
    val capDrop: HardeningLayerState? = null,
    val seccomp: HardeningLayerState? = null,
    val landlock: HardeningLayerState? = null,
    val ebpf: HardeningLayerState? = null,
)

/** Whether one hardening layer is actually enforced. */
data class HardeningLayerState(
    // active | disabled | degraded | unsupported
    val state: String? = null,
    val message: String? = null,
)
