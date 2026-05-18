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
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerError
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.ServerException
import com.alibaba.opensandbox.sandbox.api.execd.infrastructure.Success
import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandLogs
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunInSessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Commands
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionConverter.toApiRunCommandRequest
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionConverter.toCommandStatus
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionEventDispatcher
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.parseSandboxError
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import okhttp3.Headers.Companion.toHeaders
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import org.slf4j.LoggerFactory
import java.io.IOException
import com.alibaba.opensandbox.sandbox.api.models.execd.CreateSessionRequest as CreateSessionRequestApi
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
        private const val RESUME_PATH = "/command/%s/resume"
        private const val MAX_RESUME_RETRIES = 3
    }

    private val logger = LoggerFactory.getLogger(CommandsAdapter::class.java)
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

    override fun run(request: RunCommandRequest): Execution {
        if (request.command.isEmpty()) {
            throw InvalidArgumentException("Command cannot be empty")
        }

        val execution = Execution()
        var commandID: String? = null

        for (attempt in 0..MAX_RESUME_RETRIES) {
            try {
                val httpRequest = if (attempt == 0) {
                    Request.Builder()
                        .url("$execdBaseUrl$RUN_COMMAND_PATH")
                        .post(
                            jsonParser.encodeToString(request.toApiRunCommandRequest())
                                .toRequestBody("application/json".toMediaType()),
                        )
                        .headers(execdEndpoint.headers.toHeaders())
                        .build()
                } else {
                    val resumeUrl = RESUME_PATH.format(commandID!!)
                    Request.Builder()
                        .url("$execdBaseUrl$resumeUrl?after_eid=${execution.lastEid}")
                        .get()
                        .headers(execdEndpoint.headers.toHeaders())
                        .build()
                }

                streamEvents(
                    httpRequest = httpRequest,
                    execution = execution,
                    handlers = request.handlers,
                    failureMessage = { statusCode, errorBody ->
                        "Failed to run commands. Status code: $statusCode, Body: $errorBody"
                    },
                )

                if (!request.background) {
                    execution.exitCode = inferForegroundExitCode(execution)
                }
                return execution
            } catch (e: Exception) {
                if (execution.id != null) {
                    commandID = execution.id
                }

                if (e is SandboxApiException) {
                    if (e.statusCode == 409 && attempt < MAX_RESUME_RETRIES) {
                        retrySleep(1000L)
                        continue
                    }
                    if (e.statusCode == 404 && attempt > 0) {
                        return execution
                    }
                    logger.error("Failed to run command (length: {})", request.command.length, e)
                    throw e
                }

                if (commandID == null || attempt >= MAX_RESUME_RETRIES || !isNetworkError(e)) {
                    logger.error("Failed to run command (length: {})", request.command.length, e)
                    throw e.toSandboxException()
                }
            }
        }

        return execution
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
                            "Client error : ${localVarError.statusCode} ${localVarError.message.orEmpty()}",
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
                    timeout = request.timeout?.inWholeMilliseconds,
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

            val execution = Execution()
            streamEvents(
                httpRequest = httpRequest,
                execution = execution,
                handlers = request.handlers,
                failureMessage = { statusCode, errorBody ->
                    "run_in_session failed. Status: $statusCode, Body: $errorBody"
                },
            )
            execution.exitCode = inferForegroundExitCode(execution)
            return execution
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

    private fun streamEvents(
        httpRequest: Request,
        execution: Execution,
        handlers: ExecutionHandlers?,
        failureMessage: (Int, String?) -> String,
    ) {
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
    }

    private fun ensureSuccessfulStreamingResponse(
        response: Response,
        failureMessage: (Int, String?) -> String,
    ) {
        if (response.isSuccessful) {
            return
        }

        val errorBodyString = response.body?.string()
        val sandboxError = parseSandboxError(errorBodyString)
        throw SandboxApiException(
            message = failureMessage(response.code, errorBodyString),
            statusCode = response.code,
            error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
            requestId = response.header("X-Request-ID"),
        )
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

    private fun isNetworkError(e: Exception): Boolean {
        if (e is IOException) return true
        var cause: Throwable? = e.cause
        while (cause != null) {
            if (cause is IOException) return true
            cause = cause.cause
        }
        return false
    }

    private fun retrySleep(millis: Long) {
        try {
            Thread.sleep(millis)
        } catch (e: InterruptedException) {
            Thread.currentThread().interrupt()
            throw SandboxApiException(
                message = "Interrupted during SSE resume retry sleep",
                statusCode = 0,
                error = SandboxError(UNEXPECTED_RESPONSE),
            )
        }
    }
}
