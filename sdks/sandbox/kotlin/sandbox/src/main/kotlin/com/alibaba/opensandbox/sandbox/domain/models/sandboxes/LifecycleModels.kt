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

package com.alibaba.opensandbox.sandbox.domain.models.sandboxes

private const val MAX_LIFECYCLE_HOOK_TIMEOUT_SECONDS = 300

/** Command executed by execd before the user entrypoint starts. */
class LifecycleHook private constructor(
    val command: List<String>,
    val timeoutSeconds: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var command: List<String>? = null
        private var timeoutSeconds: Int? = null

        fun command(command: List<String>): Builder {
            require(command.isNotEmpty() && command.first().isNotBlank()) {
                "Lifecycle hook command must not be empty"
            }
            this.command = command.toList()
            return this
        }

        fun command(vararg command: String): Builder = command(command.toList())

        fun timeoutSeconds(timeoutSeconds: Int): Builder {
            require(timeoutSeconds in 1..MAX_LIFECYCLE_HOOK_TIMEOUT_SECONDS) {
                "Lifecycle hook timeoutSeconds must be between 1 and $MAX_LIFECYCLE_HOOK_TIMEOUT_SECONDS"
            }
            this.timeoutSeconds = timeoutSeconds
            return this
        }

        fun build(): LifecycleHook {
            val commandValue = command ?: throw IllegalArgumentException("Lifecycle hook command must be specified")
            return LifecycleHook(command = commandValue, timeoutSeconds = timeoutSeconds)
        }
    }
}

/** Named command scheduled by execd while the sandbox is running. */
class PeriodicLifecycleHook private constructor(
    val name: String,
    val schedule: String,
    val command: List<String>,
    val timeoutSeconds: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var name: String? = null
        private var schedule: String? = null
        private var command: List<String>? = null
        private var timeoutSeconds: Int? = null

        fun name(name: String): Builder {
            require(name.isNotBlank()) { "Periodic lifecycle hook name must not be empty" }
            this.name = name.trim()
            return this
        }

        fun schedule(schedule: String): Builder {
            require(schedule.isNotBlank()) { "Periodic lifecycle hook schedule must not be empty" }
            this.schedule = schedule.trim()
            return this
        }

        fun command(command: List<String>): Builder {
            require(command.isNotEmpty() && command.first().isNotBlank()) {
                "Periodic lifecycle hook command must not be empty"
            }
            this.command = command.toList()
            return this
        }

        fun command(vararg command: String): Builder = command(command.toList())

        fun timeoutSeconds(timeoutSeconds: Int): Builder {
            require(timeoutSeconds in 1..MAX_LIFECYCLE_HOOK_TIMEOUT_SECONDS) {
                "Periodic lifecycle hook timeoutSeconds must be between 1 and $MAX_LIFECYCLE_HOOK_TIMEOUT_SECONDS"
            }
            this.timeoutSeconds = timeoutSeconds
            return this
        }

        fun build(): PeriodicLifecycleHook {
            val nameValue = name ?: throw IllegalArgumentException("Periodic lifecycle hook name must be specified")
            val scheduleValue =
                schedule ?: throw IllegalArgumentException("Periodic lifecycle hook schedule must be specified")
            val commandValue =
                command ?: throw IllegalArgumentException("Periodic lifecycle hook command must be specified")
            return PeriodicLifecycleHook(
                name = nameValue,
                schedule = scheduleValue,
                command = commandValue,
                timeoutSeconds = timeoutSeconds,
            )
        }
    }
}

/** Optional lifecycle hooks applied when a sandbox is created. */
class SandboxLifecycle private constructor(
    val preStart: LifecycleHook?,
    val periodic: List<PeriodicLifecycleHook>?,
) {
    internal val isEmpty: Boolean
        get() = preStart == null && periodic.isNullOrEmpty()

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var preStart: LifecycleHook? = null
        private var periodic: List<PeriodicLifecycleHook>? = null

        fun preStart(preStart: LifecycleHook): Builder {
            this.preStart = preStart
            return this
        }

        fun periodic(periodic: List<PeriodicLifecycleHook>): Builder {
            val periodicValue = periodic.toList()
            val names = periodicValue.map { it.name }
            require(names.size == names.toSet().size) {
                "Periodic lifecycle hook names must be unique"
            }
            this.periodic = periodicValue
            return this
        }

        fun periodic(vararg periodic: PeriodicLifecycleHook): Builder = periodic(periodic.toList())

        fun build(): SandboxLifecycle =
            SandboxLifecycle(
                preStart = preStart,
                periodic = periodic?.takeIf { it.isNotEmpty() },
            )
    }
}
