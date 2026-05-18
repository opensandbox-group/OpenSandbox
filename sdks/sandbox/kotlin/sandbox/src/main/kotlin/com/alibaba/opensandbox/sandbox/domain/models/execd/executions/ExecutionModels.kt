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

package com.alibaba.opensandbox.sandbox.domain.models.execd.executions

/**
 * Represents a complete code execution session.
 *
 * This is the main model that tracks the entire lifecycle of code execution,
 * including results, errors, and output logs. It serves as the central container
 * for all execution-related data that is exposed to users.
 *
 * @property id Unique identifier for this execution session
 * @property executionCount Sequential execution counter for tracking execution order
 * @property result List of structured results produced by the code execution
 * @property error Error information if the execution failed
 * @property complete Completion metadata for the streamed execution
 * @property exitCode Command exit code when available; null for code execution or unfinished/background commands
 * @property logs Container for stdout and stderr output messages
 */
class Execution(
    var id: String? = null,
    var executionCount: Long? = null,
    val result: MutableList<ExecutionResult> = mutableListOf(),
    var error: ExecutionError? = null,
    var complete: ExecutionComplete? = null,
    var exitCode: Int? = null,
    val logs: ExecutionLogs = ExecutionLogs(),
    var lastEid: Long = 0L,
) {
    /**
     * Adds a new execution result to this execution.
     * @param result The execution result to add
     */
    fun addResult(result: ExecutionResult) {
        this.result.add(result)
    }
}

/**
 * Container for execution output logs.
 *
 * Separates standard output and error output streams for better organization
 * and allows users to process different types of output appropriately.
 *
 * @property stdout List of messages written to standard output
 * @property stderr List of messages written to standard error
 */
class ExecutionLogs(
    val stdout: MutableList<OutputMessage> = mutableListOf(),
    val stderr: MutableList<OutputMessage> = mutableListOf(),
) {
    /**
     * Adds a message to the standard output log.
     * @param outputMessage The output message to add to stdout
     */
    fun addStdout(outputMessage: OutputMessage) {
        this.stdout.add(outputMessage)
    }

    /**
     * Adds a message to the standard error log.
     * @param outputMessage The output message to add to stderr
     */
    fun addStderr(outputMessage: OutputMessage) {
        this.stderr.add(outputMessage)
    }
}

/**
 * Output message from code execution.
 *
 * Represents a single output message from either stdout or stderr streams
 * during code execution, including timing information.
 */
class OutputMessage(
    /**
     * The text content of the output message.
     * Contains the actual text that was written to the output stream.
     */
    val text: String,
    /**
     * Timestamp when this message was generated.
     * Unix timestamp in milliseconds indicating when the message was created.
     */
    val timestamp: Long,
    /**
     * Flag indicating if this is an error message.
     * True if the message came from stderr, false if from stdout.
     */
    val isError: Boolean = false,
)

/**
 * Result of code execution.
 *
 * Represents a single output result from code execution, which may include
 * text content, formatting information, and timing data.
 */
class ExecutionResult(
    /**
     * The UTF-8 encoded text content of the execution result.
     * Contains the actual output data from the executed code.
     */
    val text: String? = null,
    /**
     * Timestamp when this result was generated.
     * Unix timestamp in milliseconds indicating when the result was created.
     */
    var timestamp: Long,
    /**
     * Other result content in UTF-8 encoded format
     */
    val extraProperties: Map<String, String> = emptyMap(),
)

/**
 * Error information when code execution fails.
 *
 * Contains detailed error information following standard error reporting format,
 * including error type, message, timing, and stack trace for debugging purposes.
 *
 * @property name The error name/type (e.g., "SyntaxError", "RuntimeError", "TypeError")
 * @property value The error message or description explaining what went wrong
 * @property timestamp Unix timestamp in milliseconds when the error occurred
 * @property traceback List of traceback lines showing the complete error stack trace
 */
class ExecutionError(
    val name: String,
    val value: String,
    val timestamp: Long,
    val traceback: List<String> = emptyList(),
)

/**
 * Execution complete event.
 *
 * Represents the completion of a code execution,
 * including timing information about when the execution finished.
 */
class ExecutionComplete(
    /**
     * Timestamp when the execution completed.
     * Unix timestamp in milliseconds indicating when the execution finished.
     */
    val timestamp: Long,
    /**
     * Execution time in mills
     */
    val executionTimeInMillis: Long,
)

/**
 * Execution init event.
 *
 * Represents the initialization of a code execution.
 */
class ExecutionInit(
    /**
     * Execution id
     */
    var id: String,
    /**
     * Timestamp when the execution started.
     */
    var timestamp: Long,
)

fun interface OutputHandler<T> {
    fun handle(output: T)
}

/**
 * Handlers model for code execution output processing.
 */
class ExecutionHandlers private constructor(
    /**
     * Handler for standard output messages.
     * Called whenever text is written to stdout during execution.
     */
    val onStdout: OutputHandler<OutputMessage>? = null,
    /**
     * Handler for standard error messages.
     * Called whenever text is written to stderr during execution.
     */
    val onStderr: OutputHandler<OutputMessage>? = null,
    /**
     * Handler for execution results.
     * Called when structured results are generated from code execution.
     */
    val onResult: OutputHandler<ExecutionResult>? = null,
    /**
     * Handler for execution completion events.
     * Called when code execution finishes, regardless of success or failure.
     */
    val onExecutionComplete: OutputHandler<ExecutionComplete>? = null,
    /**
     * Handler for execution errors.
     * Called when an error occurs during code execution.
     */
    val onError: OutputHandler<ExecutionError>? = null,
    /**
     * Handler for execution initialization events.
     * Called when code execution starts.
     */
    val onInit: OutputHandler<ExecutionInit>? = null,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var onStdout: OutputHandler<OutputMessage>? = null
        private var onStderr: OutputHandler<OutputMessage>? = null
        private var onResult: OutputHandler<ExecutionResult>? = null
        private var onExecutionComplete: OutputHandler<ExecutionComplete>? = null
        private var onError: OutputHandler<ExecutionError>? = null
        private var onInit: OutputHandler<ExecutionInit>? = null

        fun onStdout(handler: OutputHandler<OutputMessage>): Builder {
            this.onStdout = handler
            return this
        }

        fun onStderr(handler: OutputHandler<OutputMessage>): Builder {
            this.onStderr = handler
            return this
        }

        fun onResult(handler: OutputHandler<ExecutionResult>): Builder {
            this.onResult = handler
            return this
        }

        fun onExecutionComplete(handler: OutputHandler<ExecutionComplete>): Builder {
            this.onExecutionComplete = handler
            return this
        }

        fun onError(handler: OutputHandler<ExecutionError>): Builder {
            this.onError = handler
            return this
        }

        fun onInit(handler: OutputHandler<ExecutionInit>): Builder {
            this.onInit = handler
            return this
        }

        fun build(): ExecutionHandlers {
            return ExecutionHandlers(
                onStdout = onStdout,
                onStderr = onStderr,
                onResult = onResult,
                onExecutionComplete = onExecutionComplete,
                onError = onError,
                onInit = onInit,
            )
        }
    }
}
