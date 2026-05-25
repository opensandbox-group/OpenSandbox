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

package com.alibaba.opensandbox.codeinterpreter.infrastructure.adapters.service

import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.CodeContext
import com.alibaba.opensandbox.codeinterpreter.domain.models.execd.executions.RunCodeRequest
import com.alibaba.opensandbox.codeinterpreter.domain.services.Codes
import com.alibaba.opensandbox.codeinterpreter.infrastructure.adapters.converter.CodeExecutionConverter.toApiRunCodeRequest
import com.alibaba.opensandbox.codeinterpreter.infrastructure.adapters.converter.CodeExecutionConverter.toCodeContext
import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.execd.CodeInterpretingApi
import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.ExecutionEventDispatcher
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.parseSandboxError
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import okhttp3.Headers.Companion.toHeaders
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import org.slf4j.LoggerFactory
import com.alibaba.opensandbox.sandbox.api.models.execd.CodeContextRequest as ApiCodeContextRequest

class CodesAdapter(
    private val execdEndpoint: SandboxEndpoint,
    private val httpClientProvider: HttpClientProvider,
) : Codes {
    companion object {
        private const val RUN_CODE_PATH = "/code"
    }

    private val logger = LoggerFactory.getLogger(CodesAdapter::class.java)
    private val baseUrl = "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}"
    private val apiClient =
        httpClientProvider.httpClient.newBuilder()
            .addInterceptor { chain ->
                val requestBuilder = chain.request().newBuilder()
                execdEndpoint.headers.forEach { (key, value) ->
                    requestBuilder.header(key, value)
                }
                chain.proceed(requestBuilder.build())
            }
            .build()
    private val api =
        CodeInterpretingApi(baseUrl, apiClient)

    override fun getContext(id: String): CodeContext {
        try {
            val result = api.getContext(id)
            return result.toCodeContext()
        } catch (e: Exception) {
            logger.error("Failed to get context", e)
            throw e.toSandboxException()
        }
    }

    override fun listContexts(language: String): List<CodeContext> {
        try {
            val list = api.listContexts(language)
            return list.map { it.toCodeContext() }
        } catch (e: Exception) {
            logger.error("Failed to list contexts", e)
            throw e.toSandboxException()
        }
    }

    override fun createContext(language: String): CodeContext {
        try {
            val request = ApiCodeContextRequest(language = language)
            val result = api.createCodeContext(request)
            return result.toCodeContext()
        } catch (e: Exception) {
            logger.error("Failed to create context", e)
            throw e.toSandboxException()
        }
    }

    override fun deleteContext(id: String) {
        try {
            api.deleteContext(id)
        } catch (e: Exception) {
            logger.error("Failed to delete context", e)
            throw e.toSandboxException()
        }
    }

    override fun deleteContexts(language: String) {
        try {
            api.deleteContextsByLanguage(language)
        } catch (e: Exception) {
            logger.error("Failed to delete contexts", e)
            throw e.toSandboxException()
        }
    }

    override fun run(request: RunCodeRequest): Execution {
        if (request.code.isEmpty()) {
            throw InvalidArgumentException("Code cannot be empty")
        }
        try {
            val apiRequest = request.toApiRunCodeRequest()
            val httpRequest =
                Request.Builder()
                    .url("$baseUrl$RUN_CODE_PATH")
                    .post(
                        jsonParser.encodeToString(apiRequest).toRequestBody("application/json".toMediaType()),
                    )
                    .headers(execdEndpoint.headers.toHeaders())
                    .build()

            val execution = Execution()

            httpClientProvider.sseClient.newCall(httpRequest).execute().use { response ->
                if (!response.isSuccessful) {
                    val errorBodyString = response.body?.string()
                    val sandboxError = parseSandboxError(errorBodyString)
                    val message = "Failed to run code. Status code: ${response.code}, Body: $errorBodyString"
                    throw SandboxApiException(
                        message = message,
                        statusCode = response.code,
                        error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
                        requestId = response.header("X-Request-ID"),
                    )
                }

                response.body?.byteStream()?.bufferedReader(Charsets.UTF_8)?.use { reader ->
                    val dispatcher = ExecutionEventDispatcher(execution, request.handlers)
                    reader.lineSequence()
                        .filter(String::isNotBlank)
                        .forEach { line ->
                            try {
                                val eventNode = jsonParser.decodeFromString<EventNode>(line)
                                dispatcher.dispatch(eventNode)
                            } catch (e: Exception) {
                                logger.error("Failed to parse SSE line: {}", line, e)
                            }
                        }
                }
            }

            return execution
        } catch (e: Exception) {
            logger.error("Failed to run code (length: {})", request.code.length, e)
            throw e.toSandboxException()
        }
    }

    override fun interrupt(executionId: String) {
        try {
            api.interruptCode(executionId)
        } catch (e: Exception) {
            logger.error("Failed to interrupt code execution", e)
            throw e.toSandboxException()
        }
    }
}
