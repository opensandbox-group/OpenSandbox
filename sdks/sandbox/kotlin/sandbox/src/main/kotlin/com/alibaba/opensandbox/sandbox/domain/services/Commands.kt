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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandLogs
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunInSessionRequest
import java.time.Duration
import kotlin.time.toJavaDuration

/**
 * Command execution operations for sandbox environments.
 *
 * This service provides secure command execution capabilities within sandbox
 * environments, with support for streaming output, timeout handling, and
 * session management.
 */
interface Commands {
    /**
     * Executes a shell command in the sandbox environment.
     *
     * The command can be executed in foreground (streaming) or background mode
     * based on the request configuration.
     *
     * @param request Configuration for the command execution including command text,
     *                working directory, and timeout settings
     * @return An [Execution] handle representing the running command instance
     */
    fun run(request: RunCommandRequest): Execution

    /**
     * Convenience overload for simple command execution.
     *
     * Equivalent to:
     * `run(RunCommandRequest.builder().command(command).build())`
     */
    fun run(command: String): Execution {
        return run(RunCommandRequest.builder().command(command).build())
    }

    /**
     * Interrupts and terminates a running command execution.
     *
     * This sends a termination signal (usually SIGTERM/SIGKILL) to the process
     * associated with the given execution ID.
     *
     * @param executionId Unique identifier of the execution to interrupt
     */
    fun interrupt(executionId: String)

    /**
     * Get the current running status for a command.
     *
     * @param executionId Unique identifier of the execution to query
     * @return Command status information
     */
    fun getCommandStatus(executionId: String): CommandStatus

    /**
     * Get background command logs (non-streamed).
     *
     * @param executionId Unique identifier of the execution to query
     * @param cursor Optional line cursor for incremental reads
     * @return Command logs content and tail cursor
     */
    fun getBackgroundCommandLogs(
        executionId: String,
        cursor: Long? = null,
    ): CommandLogs

    /**
     * Creates a new bash session with optional working directory.
     *
     * The session maintains shell state (e.g. working directory, environment) across multiple
     * [runInSession] calls. Use [deleteSession] when done to release resources.
     *
     * @param workingDirectory Optional working directory for the session
     * @return Session ID for use with [runInSession] and [deleteSession]
     */
    fun createSession(workingDirectory: String? = null): String

    /**
     * Runs a shell command in an existing bash session and streams output via SSE.
     *
     * @param sessionId Session ID from [createSession]
     * @param request Code to execute and optional workingDirectory/timeout/handlers
     * @return Execution result with stdout/stderr and completion status
     */
    fun runInSession(
        sessionId: String,
        request: RunInSessionRequest,
    ): Execution

    /**
     * Convenience overload for running a command in a session with minimal options.
     */
    fun runInSession(
        sessionId: String,
        command: String,
        workingDirectory: String? = null,
        timeout: Duration? = null,
    ): Execution {
        val builder =
            RunInSessionRequest.builder()
                .command(command)
                .workingDirectory(workingDirectory)
        if (timeout != null) {
            builder.timeout(timeout)
        }
        return runInSession(sessionId, builder.build())
    }

    @Deprecated(
        message = "Use java.time.Duration instead.",
        replaceWith =
            ReplaceWith(
                "runInSession(sessionId, command, workingDirectory, timeout.toJavaDuration())",
                "kotlin.time.toJavaDuration",
            ),
    )
    fun runInSession(
        sessionId: String,
        command: String,
        workingDirectory: String? = null,
        timeout: kotlin.time.Duration,
    ): Execution {
        return runInSession(sessionId, command, workingDirectory, timeout.toJavaDuration())
    }

    /**
     * Deletes a bash session and releases resources.
     *
     * @param sessionId Session ID to delete (from [createSession])
     */
    fun deleteSession(sessionId: String)
}
