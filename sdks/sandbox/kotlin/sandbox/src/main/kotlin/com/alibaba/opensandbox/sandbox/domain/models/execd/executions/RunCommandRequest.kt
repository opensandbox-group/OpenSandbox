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
 * Parameters for command execution.
 *
 * @property command The command content to execute
 * @property background Whether to run in background (detached)
 * @property workingDirectory Directory to execute command in
 * @property timeout Maximum execution time; server will terminate when reached.  Null means the server will not enforce any timeout.
 * @property uid Unix user ID used to run the command process
 * @property gid Unix group ID used to run the command process. Requires uid.
 * @property envs Environment variables injected into the command process
 * @property handlers Optional execution handlers
 */
class RunCommandRequest private constructor(
    val command: String,
    val background: Boolean,
    val workingDirectory: String?,
    val timeout: Duration?,
    val uid: Int?,
    val gid: Int?,
    val envs: Map<String, String>,
    val handlers: ExecutionHandlers?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var command: String? = null
        private var background: Boolean = false
        private var workingDirectory: String? = null
        private var timeout: Duration? = null
        private var uid: Int? = null
        private var gid: Int? = null
        private val envs: MutableMap<String, String> = mutableMapOf()
        private var handlers: ExecutionHandlers? = null

        fun command(command: String): Builder {
            require(command.isNotBlank()) { "Command cannot be blank" }
            this.command = command
            return this
        }

        fun background(background: Boolean): Builder {
            this.background = background
            return this
        }

        fun workingDirectory(workingDirectory: String?): Builder {
            this.workingDirectory = workingDirectory
            return this
        }

        /**
         * Maximum execution time; server will terminate the command when reached.
         * If omitted, the server will not enforce any timeout.
         */
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

        fun uid(uid: Int?): Builder {
            require(uid == null || uid >= 0) { "Uid must be >= 0" }
            this.uid = uid
            return this
        }

        fun gid(gid: Int?): Builder {
            require(gid == null || gid >= 0) { "Gid must be >= 0" }
            this.gid = gid
            return this
        }

        fun env(
            key: String,
            value: String,
        ): Builder {
            require(key.isNotBlank()) { "Environment variable key cannot be blank" }
            this.envs[key] = value
            return this
        }

        fun envs(envs: Map<String, String>): Builder {
            envs.keys.forEach { key ->
                require(key.isNotBlank()) { "Environment variable key cannot be blank" }
            }
            this.envs.putAll(envs)
            return this
        }

        fun handlers(handlers: ExecutionHandlers?): Builder {
            this.handlers = handlers
            return this
        }

        fun build(): RunCommandRequest {
            val commandValue = command ?: throw IllegalArgumentException("Command must be specified")
            require(gid == null || uid != null) { "Uid is required when gid is provided" }
            return RunCommandRequest(
                command = commandValue,
                background = background,
                workingDirectory = workingDirectory,
                timeout = timeout,
                uid = uid,
                gid = gid,
                envs = envs.toMap(),
                handlers = handlers,
            )
        }
    }
}
