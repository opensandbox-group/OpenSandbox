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

import java.time.Duration
import kotlin.time.toJavaDuration

/**
 * Request to run a command in an existing bash session.
 *
 * @property command Shell command to execute
 * @property workingDirectory Optional working directory override for this run
 * @property timeout Optional max execution time
 * @property handlers Optional execution handlers for streaming events
 */
class RunInSessionRequest private constructor(
    val command: String,
    val workingDirectory: String?,
    val timeout: Duration?,
    val handlers: ExecutionHandlers?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var command: String? = null
        private var workingDirectory: String? = null
        private var timeout: Duration? = null
        private var handlers: ExecutionHandlers? = null

        fun command(command: String): Builder {
            require(command.isNotBlank()) { "Command cannot be blank" }
            this.command = command
            return this
        }

        fun workingDirectory(workingDirectory: String?): Builder {
            this.workingDirectory = workingDirectory
            return this
        }

        fun timeout(timeout: Duration): Builder {
            this.timeout = timeout
            return this
        }

        @Deprecated(
            message = "Use java.time.Duration instead.",
            replaceWith = ReplaceWith("timeout(timeout.toJavaDuration())", "kotlin.time.toJavaDuration"),
        )
        fun timeout(timeout: kotlin.time.Duration): Builder {
            return timeout(timeout.toJavaDuration())
        }

        fun handlers(handlers: ExecutionHandlers?): Builder {
            this.handlers = handlers
            return this
        }

        fun build(): RunInSessionRequest {
            val commandValue = command ?: throw IllegalArgumentException("Command must be specified")
            return RunInSessionRequest(
                command = commandValue,
                workingDirectory = workingDirectory,
                timeout = timeout,
                handlers = handlers,
            )
        }
    }
}
