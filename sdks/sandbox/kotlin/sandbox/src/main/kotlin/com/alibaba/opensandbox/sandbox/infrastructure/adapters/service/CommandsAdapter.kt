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
import com.alibaba.opensandbox.sandbox.api.execd.CommandApi
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientError
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ClientException
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ResponseType
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.Serializer
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerError
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerException
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.Success
import com.alibaba.opensandbox.sandbox.api.models.execd.CreateCommandOperationRequest
import com.alibaba.opensandbox.sandbox.api.models.execd.CreatePTYOperationRequest
import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxException
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandLogs
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionInstance
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionOperation
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunInSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionConverter.toApiRunCommandRequest
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionConverter.toCommandStatus
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionEventDispatcher
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toCommandTimeoutMillis
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxApiException
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.Json
import okhttp3.Headers.Companion.toHeaders
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.slf4j.LoggerFactory
import java.util.concurrent.CompletableFuture
import java.util.concurrent.ExecutionException
import java.util.concurrent.TimeUnit
import com.alibaba.opensandbox.sandbox.api.models.execd.CreateSessionRequest as CreateSessionRequestApi
import com.alibaba.opensandbox.sandbox.api.models.execd.ExecutionOperation as ApiExecutionOperation
import com.alibaba.opensandbox.sandbox.api.models.execd.RunInSessionRequest as RunInSessionRequestApi

/**
 * Implementation of [Commands] that adapts OpenAPI-generated APIs and handles
 * streaming command execution for sandboxes.
 */
internal class CommandsAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
) : Commands {
    companion object {
        private const val RUN_COMMAND_PATH = "/command"
        private const val SESSION_PATH_SEGMENT = "session"
    }

    private val commandJson = Json(jsonParser) { explicitNulls = false }
    private val logger = LoggerFactory.getLogger(CommandsAdapter::class.java)
    private val instanceLock = Any()
    private var cachedInstance: ExecutionInstance? = null
    private var instanceFetchedAt = 0L
    private var instanceGeneration = 0L
    private var instancePending: CompletableFuture<ExecutionInstance>? = null
    private val execdBaseUrl = "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}"
    private val execdApiClient =
        httpClientProvider.httpClient.newBuilder()
            .addInterceptor { chain ->
                val requestBuilder = chain.request().newBuilder()
                execdEndpoint.headers.forEach { (key, value) ->
                    requestBuilder.header(key, value)
                }
                chain.proceed(requestBuilder.build())
            }
            .build()
    private val commandApi =
        CommandApi(
            execdBaseUrl,
            execdApiClient,
        )

    private fun ApiExecutionOperation.toOperation(): ExecutionOperation = ExecutionOperation(id, kind.value, state.value, expiresAt)

    override fun getExecutionInstance(): ExecutionInstance {
        var owner = false
        val pending: CompletableFuture<ExecutionInstance>
        val generation: Long
        val started: Long
        synchronized(instanceLock) {
            cachedInstance?.let {
                if (System.nanoTime() - instanceFetchedAt < TimeUnit.MINUTES.toNanos(1)) return it.copy()
            }
            pending = instancePending ?: CompletableFuture<ExecutionInstance>().also {
                instancePending = it
                owner = true
            }
            generation = instanceGeneration
            started = System.nanoTime()
        }
        if (owner) {
            try {
                val instance = commandApi.getExecutionInstance()
                val result = ExecutionInstance(instance.instanceId, instance.issuedAt, instance.retentionSeconds, instance.capacity)
                synchronized(instanceLock) {
                    if (generation == instanceGeneration) {
                        cachedInstance = result
                        instanceFetchedAt = started
                    }
                }
                pending.complete(result)
            } catch (e: Exception) {
                pending.completeExceptionally(e.toSandboxException())
            } finally {
                synchronized(instanceLock) {
                    if (instancePending === pending) instancePending = null
                }
            }
        }
        try {
            return pending.get().copy()
        } catch (e: ExecutionException) {
            throw (e.cause as? RuntimeException ?: e.toSandboxException())
        } catch (e: InterruptedException) {
            Thread.currentThread().interrupt()
            throw e.toSandboxException()
        }
    }

    private fun operationException(error: Exception): SandboxException {
        val converted = error.toSandboxException()
        if (converted.error.code in setOf("operation_instance_mismatch", "operation_expired")) {
            synchronized(instanceLock) {
                cachedInstance = null
                instancePending = null
                instanceGeneration++
            }
        }
        return converted
    }

    override fun getExecutionOperation(
        kind: String,
        operationId: String,
    ): ExecutionOperation {
        try {
            return commandApi.getExecutionOperation(CommandApi.KindGetExecutionOperation.valueOf(kind), operationId).toOperation()
        } catch (e: Exception) {
            throw operationException(e)
        }
    }

    override fun createCommandOperation(
        operationId: String,
        request: RunCommandRequest,
    ): ExecutionOperation {
        if (operationId.isBlank()) throw InvalidArgumentException("operationId is required")
        try {
            val original = request.toApiRunCommandRequest()
            val body =
                CreateCommandOperationRequest(
                    operationId = operationId,
                    command = original.command,
                    argv = original.argv,
                    cwd = original.cwd,
                    background = original.background,
                    timeout = original.timeout,
                    uid = original.uid,
                    gid = original.gid,
                    envs = original.envs,
                )
            // As with /command, omit the unused command/argv alternative.
            // The generated client's default serializer emits explicit nulls.
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl$RUN_COMMAND_PATH/operations")
                    .post(commandJson.encodeToString(body).toRequestBody("application/json".toMediaType()))
                    .build()
            return execdApiClient.newCall(httpRequest).execute().use { response ->
                if (!response.isSuccessful) {
                    throw response.toSandboxApiException { status, _ -> "Failed to create command operation. Status code: $status" }
                }
                Serializer.kotlinxSerializationJson.decodeFromString<ApiExecutionOperation>(
                    response.body?.string() ?: throw IllegalStateException("Missing execution operation"),
                ).toOperation()
            }
        } catch (e: Exception) {
            throw operationException(e)
        }
    }

    override fun createPTYOperation(
        operationId: String,
        cwd: String,
        command: String,
    ): ExecutionOperation {
        if (operationId.isBlank()) throw InvalidArgumentException("operationId is required")
        try {
            return commandApi.createPTYOperation(
                CreatePTYOperationRequest(operationId = operationId, cwd = cwd, command = command),
            ).toOperation()
        } catch (e: Exception) {
            throw operationException(e)
        }
    }

    override fun run(request: RunCommandRequest): Execution {
        if (request.argv == null && request.command.isEmpty()) {
            throw InvalidArgumentException("Command cannot be empty")
        }
        try {
            val httpRequest =
                Request.Builder()
                    .url("$execdBaseUrl$RUN_COMMAND_PATH")
                    .post(
                        commandJson.encodeToString(request.toApiRunCommandRequest()).toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            return executeStreamingRequest(
                httpRequest = httpRequest,
                handlers = request.handlers,
                inferExitCode = !request.background,
                failureMessage = { statusCode, errorBody ->
                    "Failed to run commands. Status code: $statusCode, Body: $errorBody"
                },
            )
        } catch (e: Exception) {
            logger.error("Failed to run command (length: {})", request.command.length, e)
            throw e.toSandboxException()
        }
    }

    override fun interrupt(executionId: String) {
        try {
            commandApi.interruptCommand(executionId)
        } catch (e: Exception) {
            logger.error("Failed to interrupt command", e)
            throw e.toSandboxException()
        }
    }

    override fun getCommandStatus(executionId: String): CommandStatus {
        return try {
            val status = commandApi.getCommandStatus(executionId)
            status.toCommandStatus()
        } catch (e: Exception) {
            logger.error("Failed to get command status", e)
            throw e.toSandboxException()
        }
    }

    override fun getBackgroundCommandLogs(
        executionId: String,
        cursor: Long?,
    ): CommandLogs {
        return try {
            val localVarResponse = commandApi.getBackgroundCommandLogsWithHttpInfo(executionId, cursor)
            val content =
                when (localVarResponse.responseType) {
                    ResponseType.Success -> (localVarResponse as Success<*>).data as String
                    ResponseType.Informational ->
                        throw UnsupportedOperationException("Client does not support Informational responses.")
                    ResponseType.Redirection ->
                        throw UnsupportedOperationException("Client does not support Redirection responses.")
                    ResponseType.ClientError -> {
                        val localVarError = localVarResponse as ClientError<*>
                        throw ClientException(
                            "Client error : ${localVarError.statusCode} ${localVarError.message.orEmpty()} ${localVarError.body}",
                            localVarError.statusCode,
                            localVarResponse,
                        )
                    }
                    ResponseType.ServerError -> {
                        val localVarError = localVarResponse as ServerError<*>
                        throw ServerException(
                            "Server error : ${localVarError.statusCode} ${localVarError.message.orEmpty()} ${localVarError.body}",
                            localVarError.statusCode,
                            localVarResponse,
                        )
                    }
                }
            val cursorHeader =
                localVarResponse.headers["EXECD-COMMANDS-TAIL-CURSOR"]?.firstOrNull()
            val nextCursor = cursorHeader?.toLongOrNull()
            CommandLogs(content = content, cursor = nextCursor)
        } catch (e: Exception) {
            logger.error("Failed to get command logs", e)
            throw e.toSandboxException()
        }
    }

    override fun createSession(workingDirectory: String?): String {
        if (workingDirectory != null && workingDirectory.isBlank()) {
            throw InvalidArgumentException("workingDirectory cannot be blank when provided")
        }
        return try {
            val apiRequest = workingDirectory?.let { CreateSessionRequestApi(cwd = it) }
            commandApi.createSession(apiRequest).sessionId
        } catch (e: Exception) {
            logger.error("Failed to create session", e)
            throw e.toSandboxException()
        }
    }

    override fun runInSession(
        sessionId: String,
        request: RunInSessionRequest,
    ): Execution {
        if (sessionId.isBlank()) {
            throw InvalidArgumentException("session_id cannot be empty")
        }
        try {
            val apiRequest =
                RunInSessionRequestApi(
                    command = request.command,
                    cwd = request.workingDirectory,
                    timeout = request.timeout?.toCommandTimeoutMillis(),
                )
            val runUrl =
                execdBaseUrl
                    .toHttpUrlOrNull()!!
                    .newBuilder()
                    .addPathSegment(SESSION_PATH_SEGMENT)
                    .addPathSegment(sessionId)
                    .addPathSegment("run")
                    .build()
                    .toString()
            val httpRequest =
                Request.Builder()
                    .url(runUrl)
                    .post(
                        jsonParser.encodeToString(apiRequest).toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            return executeStreamingRequest(
                httpRequest = httpRequest,
                handlers = request.handlers,
                inferExitCode = true,
                failureMessage = { statusCode, errorBody ->
                    "run_in_session failed. Status: $statusCode, Body: $errorBody"
                },
            )
        } catch (e: Exception) {
            logger.error("Failed to run in session", e)
            throw e.toSandboxException()
        }
    }

    override fun deleteSession(sessionId: String) {
        if (sessionId.isBlank()) {
            throw InvalidArgumentException("session_id cannot be empty")
        }
        try {
            commandApi.deleteSession(sessionId)
        } catch (e: Exception) {
            logger.error("Failed to delete session", e)
            throw e.toSandboxException()
        }
    }

    private fun executeStreamingRequest(
        httpRequest: Request,
        handlers: ExecutionHandlers?,
        inferExitCode: Boolean,
        failureMessage: (Int, String?) -> String,
    ): Execution {
        val execution = Execution()

        httpClientProvider.sseClient.newCall(httpRequest).execute().use { response ->
            ensureSuccessfulStreamingResponse(response, failureMessage)

            response.body?.byteStream()?.bufferedReader(Charsets.UTF_8)?.use { reader ->
                val dispatcher = ExecutionEventDispatcher(execution, handlers)
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

        if (inferExitCode) {
            execution.exitCode = inferForegroundExitCode(execution)
        }
        return execution
    }

    private fun ensureSuccessfulStreamingResponse(
        response: Response,
        failureMessage: (Int, String?) -> String,
    ) {
        if (response.isSuccessful) {
            return
        }

        throw response.toSandboxApiException(message = failureMessage)
    }

    private fun decodeEventLine(line: String): EventNode? {
        if (line.isBlank()) {
            return null
        }

        val payload =
            when {
                line.startsWith(":") -> return null
                line.startsWith("event:") -> return null
                line.startsWith("id:") -> return null
                line.startsWith("retry:") -> return null
                line.startsWith("data:") -> line.drop(5).trim()
                else -> line
            }

        if (payload.isBlank()) {
            return null
        }

        return try {
            jsonParser.decodeFromString<EventNode>(payload)
        } catch (e: Exception) {
            logger.error("Failed to parse SSE line: {}", line, e)
            null
        }
    }

    private fun inferForegroundExitCode(execution: Execution): Int? {
        return if (execution.error != null) {
            execution.error?.value?.toIntOrNull()
        } else {
            if (execution.complete != null) 0 else null
        }
    }
}
