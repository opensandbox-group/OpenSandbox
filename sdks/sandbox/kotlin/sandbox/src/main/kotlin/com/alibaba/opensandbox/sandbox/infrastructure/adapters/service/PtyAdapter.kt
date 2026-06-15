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
import com.alibaba.opensandbox.sandbox.api.execd.PTYApi
import com.alibaba.opensandbox.sandbox.api.models.execd.CreatePtySessionRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySession
import com.alibaba.opensandbox.sandbox.domain.models.execd.pty.PtySessionStatus
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Pty
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import org.slf4j.LoggerFactory

/**
 * Implementation of [Pty] that adapts the OpenAPI-generated [PTYApi] for the execd PTY session
 * lifecycle. Mirrors the wiring of the other execd adapters: the generated client is bound to the
 * resolved sandbox endpoint and carries its routing/auth headers, and errors are mapped through
 * [toSandboxException].
 */
internal class PtyAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
) : Pty {
    private val logger = LoggerFactory.getLogger(PtyAdapter::class.java)
    private val api =
        PTYApi(
            "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}",
            httpClientProvider.httpClient.newBuilder()
                .addInterceptor { chain ->
                    val requestBuilder = chain.request().newBuilder()
                    execdEndpoint.headers.forEach { (key, value) ->
                        requestBuilder.header(key, value)
                    }
                    chain.proceed(requestBuilder.build())
                }
                .build(),
        )

    override fun createSession(
        cwd: String?,
        command: String?,
    ): PtySession {
        return try {
            val response = api.createPtySession(CreatePtySessionRequest(cwd = cwd, command = command))
            PtySession(response.sessionId)
        } catch (e: Exception) {
            logger.error("Failed to create PTY session", e)
            throw e.toSandboxException()
        }
    }

    override fun getSession(sessionId: String): PtySessionStatus {
        return try {
            val response = api.getPtySession(sessionId)
            PtySessionStatus(response.sessionId, response.running, response.outputOffset)
        } catch (e: Exception) {
            logger.error("Failed to get PTY session {}", sessionId, e)
            throw e.toSandboxException()
        }
    }

    override fun deleteSession(sessionId: String) {
        try {
            api.deletePtySession(sessionId)
        } catch (e: Exception) {
            logger.error("Failed to delete PTY session {}", sessionId, e)
            throw e.toSandboxException()
        }
    }
}
