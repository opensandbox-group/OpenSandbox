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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionComplete
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionError
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionHandlers
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionInit
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ExecutionResult
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.OutputMessage

class ExecutionEventDispatcher(
    private val execution: Execution,
    private val handlers: ExecutionHandlers? = null,
) {
    fun dispatch(eventNode: EventNode) {
        val type = eventNode.type
        val timestamp = eventNode.timestamp
        when (type) {
            "stdout" -> handleStdout(eventNode, timestamp)
            "stderr" -> handleStderr(eventNode, timestamp)
            "result" -> handleResult(eventNode, timestamp)
            "error" -> handleError(eventNode, timestamp)
            "execution_complete" -> handleExecutionComplete(eventNode, timestamp)
            "init" -> handleInit(eventNode, timestamp)
            "execution_count" -> execution.executionCount = eventNode.executionCount
        }
    }

    private fun handleInit(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val init =
            ExecutionInit(
                id = eventNode.text ?: "",
                timestamp = timestamp,
            )
        execution.id = init.id
        handlers?.onInit?.handle(init)
    }

    private fun handleStdout(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val stdoutText = eventNode.text ?: ""
        val stdoutMessage = OutputMessage(stdoutText, timestamp, false)
        execution.logs.addStdout(stdoutMessage)
        handlers?.onStdout?.handle(stdoutMessage)
    }

    private fun handleStderr(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val stderrText = eventNode.text ?: ""
        val stderrMessage = OutputMessage(stderrText, timestamp, true)
        execution.logs.addStderr(stderrMessage)
        handlers?.onStderr?.handle(stderrMessage)
    }

    private fun handleResult(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val resultText = eventNode.results?.getText() ?: ""
        val result =
            ExecutionResult(resultText, timestamp).apply {
                this.timestamp = timestamp
            }
        execution.addResult(result)
        handlers?.onResult?.handle(result)
    }

    private fun handleError(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val errorData = eventNode.error!!
        val error =
            ExecutionError(
                name = errorData.name ?: "",
                value = errorData.value ?: "",
                traceback = errorData.traceback,
                timestamp = timestamp,
            )
        execution.error = error
        handlers?.onError?.handle(error)
    }

    private fun handleExecutionComplete(
        eventNode: EventNode,
        timestamp: Long,
    ) {
        val complete =
            ExecutionComplete(
                executionTimeInMillis = eventNode.executionTimeInMillis ?: 0L,
                timestamp = timestamp,
            )
        execution.complete = complete
        handlers?.onExecutionComplete?.handle(complete)
    }
}
