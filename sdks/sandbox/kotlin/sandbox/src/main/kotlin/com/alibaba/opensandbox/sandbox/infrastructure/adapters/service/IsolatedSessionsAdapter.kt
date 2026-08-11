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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.BindMount
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.CreateIsolatedSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.isolated.EnvPassthroughSpec
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
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.domain.services.IsolationService
import com.alibaba.opensandbox.sandbox.domain.services.IsolationSession
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionEventDispatcher
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxApiException
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import okhttp3.Headers.Companion.toHeaders
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.slf4j.LoggerFactory
import java.time.OffsetDateTime
import java.time.format.DateTimeFormatter

@Serializable
private data class IsolatedCreateBody(
    val workspace: IsolatedWorkspaceBody,
    val profile: String? = null,
    val extra_writable: List<String>? = null,
    val binds: List<BindMountBody>? = null,
    val share_net: Boolean? = null,
    val env_passthrough: EnvPassthroughBody? = null,
    val uid: Long? = null,
    val gid: Long? = null,
    val uid_mode: String? = null,
    val idle_timeout_seconds: Int? = null,
)

@Serializable
private data class IsolatedWorkspaceBody(val path: String, val mode: String? = null)

@Serializable
private data class BindMountBody(
    val source: String,
    val dest: String? = null,
    val readonly: Boolean? = null,
)

@Serializable
private data class EnvPassthroughBody(val mode: String? = null, val keys: List<String>? = null)

@Serializable
private data class IsolatedRunBody(
    val code: String,
    val envs: Map<String, String>? = null,
    val timeout_seconds: Int? = null,
    val background: Boolean? = null,
)

@Serializable
private data class IsolatedBackgroundRunResponse(
    val session_id: String,
    val run_id: String,
    val started_at: String? = null,
)

@Serializable
private data class IsolatedRunStatusResponse(
    val session_id: String,
    val run_id: String,
    val running: Boolean = false,
    val exit_code: Int? = null,
    val error: String? = null,
    val started_at: String? = null,
    val finished_at: String? = null,
)

@Serializable
private data class IsolatedSessionInfoResponse(
    val session_id: String,
    val created_at: String? = null,
)

@Serializable
private data class IsolatedSessionStateResponse(
    val status: String,
    val created_at: String? = null,
    val last_run_at: String? = null,
    val idle_remaining_seconds: Int? = null,
    // Creation-parameter echo fields. Older execd builds omit them.
    val profile: String? = null,
    val workspace: IsolatedWorkspaceBody? = null,
    val extra_writable: List<String>? = null,
    val binds: List<BindMountBody>? = null,
    val share_net: Boolean? = null,
    val env_passthrough: EnvPassthroughBody? = null,
    val uid: Long? = null,
    val gid: Long? = null,
    val uid_mode: String? = null,
    val idle_timeout_seconds: Int? = null,
)

@Serializable
private data class IsolatedSessionSummaryResponse(
    val session_id: String,
    val status: String,
    val created_at: String? = null,
    val last_run_at: String? = null,
    val idle_remaining_seconds: Int? = null,
)

@Serializable
private data class ListIsolatedSessionsResponse(
    val sessions: List<IsolatedSessionSummaryResponse> = emptyList(),
)

@Serializable
private data class IsolatedCapabilitiesResponse(
    val available: Boolean = false,
    val isolator: String? = null,
    val version: String? = null,
    val message: String? = null,
    val setpriv_available: Boolean = false,
    val userns_available: Boolean = false,
    val commit_supported: Boolean = false,
    val diff_supported: Boolean = false,
)

private val json = Json { ignoreUnknownKeys = true }

private const val TAIL_CURSOR_HEADER = "EXECD-ISOLATED-TAIL-CURSOR"

private class IsolationSessionHandle(
    override val info: IsolatedSessionInfo,
    private val adapter: IsolatedSessionsAdapter,
) : IsolationSession {
    override val sessionId: String get() = info.sessionId

    override val files: Filesystem by lazy {
        IsolatedFilesystemAdapter(adapter.httpClientProvider, adapter.execdEndpoint, info.sessionId)
    }

    override fun run(request: IsolatedRunRequest): Execution = adapter.runInternal(info.sessionId, request)

    override fun runBackground(
        code: String,
        opts: IsolatedRunOpts?,
    ): IsolatedBackgroundRun = adapter.runBackgroundInternal(info.sessionId, code, opts)

    override fun getRunStatus(runId: String): IsolatedRunStatus = adapter.getRunStatusInternal(info.sessionId, runId)

    override fun getRunLogs(
        runId: String,
        cursor: Long,
    ): IsolatedRunLogs = adapter.getRunLogsInternal(info.sessionId, runId, cursor)

    override fun get(): IsolatedSessionState = adapter.getInternal(info.sessionId)

    override fun delete() = adapter.deleteInternal(info.sessionId)
}

internal class IsolatedSessionsAdapter(
    internal val httpClientProvider: HttpClientProvider,
    internal val execdEndpoint: SandboxEndpoint,
) : IsolationService {
    private val logger = LoggerFactory.getLogger(IsolatedSessionsAdapter::class.java)
    private val execdBaseUrl =
        "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}"

    override fun create(request: CreateIsolatedSessionRequest): IsolationSession {
        try {
            val body =
                IsolatedCreateBody(
                    workspace =
                        IsolatedWorkspaceBody(request.workspace.path, request.workspace.mode),
                    profile = request.profile,
                    extra_writable = request.extraWritable,
                    binds =
                        request.binds?.map { BindMountBody(it.source, it.dest, it.readonly) },
                    share_net = request.shareNet,
                    env_passthrough =
                        request.envPassthrough?.let { EnvPassthroughBody(it.mode, it.keys) },
                    uid = request.uid,
                    gid = request.gid,
                    uid_mode = request.uidMode,
                    idle_timeout_seconds = request.idleTimeoutSeconds,
                )
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session")
                    .post(
                        json
                            .encodeToString(IsolatedCreateBody.serializer(), body)
                            .toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "create isolated session")
                val resp =
                    json.decodeFromString(
                        IsolatedSessionInfoResponse.serializer(),
                        response.body!!.string(),
                    )
                val info =
                    IsolatedSessionInfo(
                        sessionId = resp.session_id,
                        createdAt = resp.created_at?.let { parseDateTime(it) },
                    )
                return IsolationSessionHandle(info, this)
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun attach(sessionId: String): IsolationSession {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "attach isolated session")
                val resp =
                    json.decodeFromString(
                        IsolatedSessionStateResponse.serializer(),
                        response.body!!.string(),
                    )
                val info =
                    IsolatedSessionInfo(
                        sessionId = sessionId,
                        createdAt = resp.created_at?.let { parseDateTime(it) },
                        profile = resp.profile,
                        workspace =
                            resp.workspace?.let {
                                IsolatedWorkspaceSpec(path = it.path, mode = it.mode)
                            },
                        extraWritable = resp.extra_writable,
                        binds =
                            resp.binds?.map { BindMount(it.source, it.dest, it.readonly) },
                        shareNet = resp.share_net,
                        envPassthrough =
                            resp.env_passthrough?.let {
                                EnvPassthroughSpec(
                                    mode = it.mode ?: "deny",
                                    keys = it.keys ?: emptyList(),
                                )
                            },
                        uid = resp.uid,
                        gid = resp.gid,
                        uidMode = resp.uid_mode,
                        idleTimeoutSeconds = resp.idle_timeout_seconds,
                    )
                return IsolationSessionHandle(info, this)
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun getInternal(sessionId: String): IsolatedSessionState {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "get isolated session")
                val resp =
                    json.decodeFromString(
                        IsolatedSessionStateResponse.serializer(),
                        response.body!!.string(),
                    )
                return IsolatedSessionState(
                    status = resp.status,
                    createdAt = resp.created_at?.let { parseDateTime(it) },
                    lastRunAt = resp.last_run_at?.let { parseDateTime(it) },
                    idleRemainingSeconds = resp.idle_remaining_seconds,
                    profile = resp.profile,
                    workspace =
                        resp.workspace?.let {
                            IsolatedWorkspaceSpec(path = it.path, mode = it.mode)
                        },
                    extraWritable = resp.extra_writable,
                    binds =
                        resp.binds?.map { BindMount(it.source, it.dest, it.readonly) },
                    shareNet = resp.share_net,
                    envPassthrough =
                        resp.env_passthrough?.let {
                            EnvPassthroughSpec(
                                mode = it.mode ?: "deny",
                                keys = it.keys ?: emptyList(),
                            )
                        },
                    uid = resp.uid,
                    gid = resp.gid,
                    uidMode = resp.uid_mode,
                    idleTimeoutSeconds = resp.idle_timeout_seconds,
                )
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun runInternal(
        sessionId: String,
        request: IsolatedRunRequest,
    ): Execution {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        require(request.code.isNotBlank()) { "code cannot be empty" }
        try {
            val body =
                IsolatedRunBody(
                    code = request.code,
                    envs = request.envs,
                    timeout_seconds = request.timeoutSeconds,
                )
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId/run")
                    .post(
                        json
                            .encodeToString(IsolatedRunBody.serializer(), body)
                            .toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            val execution = Execution()

            httpClientProvider.sseClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "run in isolated session")
                response.body?.byteStream()?.bufferedReader(Charsets.UTF_8)?.use { reader ->
                    val dispatcher = ExecutionEventDispatcher(execution, null)
                    reader.lineSequence().forEach { line ->
                        decodeEventLine(line)?.let { eventNode ->
                            try {
                                dispatcher.dispatch(eventNode)
                            } catch (e: Exception) {
                                logger.error("Failed to dispatch SSE event: {}", eventNode, e)
                            }
                        }
                    }
                }
            }

            execution.exitCode = inferExitCode(execution)
            return execution
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun runBackgroundInternal(
        sessionId: String,
        code: String,
        opts: IsolatedRunOpts? = null,
    ): IsolatedBackgroundRun {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        require(code.isNotBlank()) { "code cannot be empty" }
        try {
            val body =
                IsolatedRunBody(
                    code = code,
                    envs = opts?.envs,
                    // timeout_seconds is foreground-only and deliberately not sent.
                    timeout_seconds = null,
                    background = true,
                )
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId/run")
                    .post(
                        json
                            .encodeToString(IsolatedRunBody.serializer(), body)
                            .toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                if (response.code != 202) {
                    throw response.toSandboxApiException { statusCode, body ->
                        "run background in isolated session failed. Status: $statusCode, Body: $body"
                    }
                }
                val resp =
                    json.decodeFromString(
                        IsolatedBackgroundRunResponse.serializer(),
                        response.body!!.string(),
                    )
                return IsolatedBackgroundRun(
                    sessionId = resp.session_id,
                    runId = resp.run_id,
                    startedAt = resp.started_at?.let { parseDateTime(it) },
                )
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun getRunStatusInternal(
        sessionId: String,
        runId: String,
    ): IsolatedRunStatus {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        require(runId.isNotBlank()) { "runId cannot be empty" }
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId/runs/$runId")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "get isolated run status")
                val resp =
                    json.decodeFromString(
                        IsolatedRunStatusResponse.serializer(),
                        response.body!!.string(),
                    )
                return IsolatedRunStatus(
                    sessionId = resp.session_id,
                    runId = resp.run_id,
                    running = resp.running,
                    exitCode = resp.exit_code,
                    error = resp.error,
                    startedAt = resp.started_at?.let { parseDateTime(it) },
                    finishedAt = resp.finished_at?.let { parseDateTime(it) },
                )
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun getRunLogsInternal(
        sessionId: String,
        runId: String,
        cursor: Long = 0,
    ): IsolatedRunLogs {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        require(runId.isNotBlank()) { "runId cannot be empty" }
        require(cursor >= 0) { "cursor cannot be negative" }
        try {
            val urlBuilder =
                "$execdBaseUrl/v1/isolated/session/$sessionId/runs/$runId/logs".toHttpUrl().newBuilder()
            if (cursor > 0) {
                urlBuilder.addQueryParameter("cursor", cursor.toString())
            }
            val httpRequest =
                Request.Builder()
                    .url(urlBuilder.build())
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "get isolated run logs")
                val bytes = response.body!!.bytes()
                val text = bytes.toString(Charsets.UTF_8)
                val nextCursor =
                    response.header(TAIL_CURSOR_HEADER)?.toLongOrNull()
                        ?: (cursor + bytes.size)
                return IsolatedRunLogs(text = text, cursor = nextCursor)
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    internal fun deleteInternal(sessionId: String) {
        require(sessionId.isNotBlank()) { "sessionId cannot be empty" }
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/session/$sessionId")
                    .delete()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "delete isolated session")
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun capabilities(): IsolatedCapabilities {
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/capabilities")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "get isolated capabilities")
                val resp =
                    json.decodeFromString(
                        IsolatedCapabilitiesResponse.serializer(),
                        response.body!!.string(),
                    )
                return IsolatedCapabilities(
                    available = resp.available,
                    isolator = resp.isolator,
                    version = resp.version,
                    message = resp.message,
                    setprivAvailable = resp.setpriv_available,
                    usernsAvailable = resp.userns_available,
                    commitSupported = resp.commit_supported,
                    diffSupported = resp.diff_supported,
                )
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    override fun list(): List<IsolatedSessionSummary> {
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl/v1/isolated/sessions")
                    .get()
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            httpClientProvider.httpClient.newCall(httpRequest).execute().use { response ->
                ensureSuccess(response, "list isolated sessions")
                val resp =
                    json.decodeFromString(
                        ListIsolatedSessionsResponse.serializer(),
                        response.body!!.string(),
                    )
                return resp.sessions.map { summary ->
                    IsolatedSessionSummary(
                        sessionId = summary.session_id,
                        status = summary.status,
                        createdAt = summary.created_at?.let { parseDateTime(it) },
                        lastRunAt = summary.last_run_at?.let { parseDateTime(it) },
                        idleRemainingSeconds = summary.idle_remaining_seconds,
                    )
                }
            }
        } catch (e: Exception) {
            throw e.toSandboxException()
        }
    }

    private fun ensureSuccess(
        response: Response,
        operation: String,
    ) {
        if (response.isSuccessful) return
        throw response.toSandboxApiException { statusCode, body ->
            "$operation failed. Status: $statusCode, Body: $body"
        }
    }

    private fun decodeEventLine(line: String): EventNode? {
        if (line.isBlank()) return null
        val payload =
            when {
                line.startsWith(":") -> return null
                line.startsWith("event:") -> return null
                line.startsWith("id:") -> return null
                line.startsWith("retry:") -> return null
                line.startsWith("data:") -> line.drop(5).trim()
                else -> line
            }
        if (payload.isEmpty()) return null
        return try {
            jsonParser.decodeFromString(EventNode.serializer(), payload)
        } catch (e: Exception) {
            logger.error("Failed to parse SSE line: {}", line, e)
            null
        }
    }

    private fun inferExitCode(execution: Execution): Int? {
        if (execution.error != null) {
            return execution.error?.value?.trim()?.toIntOrNull()
        }
        if (execution.complete != null) return 0
        return null
    }

    private fun parseDateTime(value: String): OffsetDateTime? {
        return try {
            OffsetDateTime.parse(value, DateTimeFormatter.ISO_OFFSET_DATE_TIME)
        } catch (_: Exception) {
            null
        }
    }
}
