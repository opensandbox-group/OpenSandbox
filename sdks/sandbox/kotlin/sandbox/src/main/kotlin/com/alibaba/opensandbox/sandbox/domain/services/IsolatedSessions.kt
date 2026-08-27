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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.BindMount
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.CreateIsolatedSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedBackgroundRun
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedCapabilities
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunLogs
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunOpts
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedSessionInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedSessionState
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedSessionSummary
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedWorkspaceSpec
import org.slf4j.LoggerFactory

private val isolationServiceLogger = LoggerFactory.getLogger(IsolationService::class.java)

interface IsolationSession {
    val sessionId: String
    val info: IsolatedSessionInfo
    val files: Filesystem

    fun run(request: IsolatedRunRequest): Execution

    fun run(code: String): Execution = run(IsolatedRunRequest(code = code))

    /**
     * Start [code] detached inside the session and return a run handle.
     *
     * The run's combined output and exit code are captured by execd; poll
     * them with [getRunStatus] and [getRunLogs]. The run is not time-limited
     * and idle GC is suspended while it is active. Background runs require a
     * writable log location, so sessions with a read-only (`ro`) workspace
     * reject them.
     */
    fun runBackground(
        code: String,
        opts: IsolatedRunOpts? = null,
    ): IsolatedBackgroundRun

    /**
     * Return the lifecycle state of a background run started with [runBackground].
     */
    fun getRunStatus(runId: String): IsolatedRunStatus

    /**
     * Return the background run's log from [cursor] plus the next cursor.
     *
     * Each call returns at most 16 MiB; pass the returned cursor to fetch the
     * remainder. Per-run log retention is capped at 16 MiB (output beyond it is
     * discarded when the run finishes), so drain incrementally while the run is
     * active if more than one page is needed.
     */
    fun getRunLogs(
        runId: String,
        cursor: Long = 0,
    ): IsolatedRunLogs

    fun get(): IsolatedSessionState

    fun delete()
}

interface IsolationService {
    fun create(request: CreateIsolatedSessionRequest): IsolationSession

    /**
     * Rebuild an [IsolationSession] handle from an existing execd session id, without recreating
     * the session. Intended for stateless callers (e.g. serverless workers restarted mid-flight)
     * that only have the session id.
     *
     * The returned handle exposes `run`, `get`, `delete`, and `files` keyed by [sessionId]. The
     * handle's [IsolationSession.info] is populated from what execd echoes back on the GET
     * response; older execd builds may omit creation parameters, in which case those fields on
     * [IsolatedSessionInfo] will be null / empty and only [IsolatedSessionInfo.sessionId] and
     * [IsolatedSessionInfo.createdAt] are guaranteed. `run`, `get`, and `delete` on the returned
     * handle only need the session id and are unaffected by missing echo fields.
     *
     * @throws com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException with
     *   `statusCode = 404` if the session does not exist on execd.
     */
    fun attach(sessionId: String): IsolationSession

    fun capabilities(): IsolatedCapabilities

    fun list(): List<IsolatedSessionSummary>

    fun runOnce(
        code: String,
        workspace: String,
        workspaceMode: String? = null,
        envs: Map<String, String>? = null,
        timeoutSeconds: Int? = null,
        profile: String? = null,
        shareNet: Boolean? = null,
        binds: List<BindMount>? = null,
    ): Execution {
        val session =
            create(
                CreateIsolatedSessionRequest(
                    workspace = IsolatedWorkspaceSpec(path = workspace, mode = workspaceMode),
                    profile = profile,
                    shareNet = shareNet,
                    binds = binds,
                ),
            )
        try {
            return session.run(
                IsolatedRunRequest(code = code, envs = envs, timeoutSeconds = timeoutSeconds),
            )
        } finally {
            try {
                session.delete()
            } catch (e: Exception) {
                isolationServiceLogger.warn("failed to delete isolated session {}", session.sessionId, e)
            }
        }
    }

    fun <T> withSession(
        request: CreateIsolatedSessionRequest,
        block: (IsolationSession) -> T,
    ): T {
        val session = create(request)
        try {
            return block(session)
        } finally {
            try {
                session.delete()
            } catch (e: Exception) {
                isolationServiceLogger.warn("failed to delete isolated session {}", session.sessionId, e)
            }
        }
    }
}
