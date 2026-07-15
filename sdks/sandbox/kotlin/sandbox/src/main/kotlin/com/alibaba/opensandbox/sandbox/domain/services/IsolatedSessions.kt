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
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedCapabilities
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.IsolatedRunRequest
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

    fun get(): IsolatedSessionState

    fun delete()
}

interface IsolationService {
    fun create(request: CreateIsolatedSessionRequest): IsolationSession

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
