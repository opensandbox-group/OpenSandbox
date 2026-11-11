/*
 * Copyright 2026 The OpenSandbox Authors
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

/**
 * Application-declared activity probe (OSEP-0024).
 *
 * Endpoint inside the sandbox that answers `{"active": bool, "message"?: string}`
 * with 200 while work is in progress. When the sandbox is otherwise idle, the
 * server issues `GET [path]` on `[port]`; a busy answer counts as activity and
 * resets the idle clock, while an explicit "not active", any failure, or a
 * timeout records nothing.
 */
class ActivityProbe private constructor(
    val port: Int,
    val path: String,
    val timeoutSeconds: Int?,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var port: Int? = null
        private var path: String? = null
        private var timeoutSeconds: Int? = null

        fun port(port: Int): Builder {
            require(port in 1..65535) { "Activity probe port must be between 1 and 65535, got $port" }
            this.port = port
            return this
        }

        fun path(path: String): Builder {
            require(path.isNotEmpty()) { "Activity probe path must not be empty" }
            require(path.length <= 512) { "Activity probe path must not exceed 512 characters" }
            require(path.startsWith("/")) { "Activity probe path must be an absolute HTTP path starting with '/'" }
            require(!path.any { it.isWhitespace() || it.code < 0x20 || it.code == 0x7f }) {
                "Activity probe path must not contain whitespace or control characters"
            }
            this.path = path
            return this
        }

        fun timeoutSeconds(timeoutSeconds: Int): Builder {
            require(timeoutSeconds in 1..10) {
                "Activity probe timeoutSeconds must be between 1 and 10, got $timeoutSeconds"
            }
            this.timeoutSeconds = timeoutSeconds
            return this
        }

        fun build(): ActivityProbe {
            val portValue = port ?: throw IllegalArgumentException("Activity probe port must be specified")
            val pathValue = path ?: throw IllegalArgumentException("Activity probe path must be specified")
            return ActivityProbe(port = portValue, path = pathValue, timeoutSeconds = timeoutSeconds)
        }
    }
}

/** Action taken at the idle threshold. Only [PAUSE] exists today; the enum keeps future actions additive. */
enum class OnIdleAction {
    PAUSE,
}

/**
 * Idle behavior declared by the sandbox (OSEP-0024, fast-sandbox only).
 *
 * With [idleTimeoutSeconds], the sandbox is auto-paused after that many seconds
 * without ingress-observed activity. Without it, the policy is wake-only: the
 * sandbox is never auto-paused, but a (manually) paused sandbox wakes on access.
 * Omitting the whole policy preserves non-automated behavior exactly.
 */
class IdlePolicy private constructor(
    val idleTimeoutSeconds: Int?,
    val onIdle: OnIdleAction?,
    val wakeOnAccess: Boolean?,
    val activeProbe: ActivityProbe?,
) {
    internal val isEmpty: Boolean
        get() = idleTimeoutSeconds == null && onIdle == null && wakeOnAccess == null && activeProbe == null

    companion object {
        const val MIN_IDLE_TIMEOUT_SECONDS = 30
        const val MAX_IDLE_TIMEOUT_SECONDS = 86400

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var idleTimeoutSeconds: Int? = null
        private var onIdle: OnIdleAction? = null
        private var wakeOnAccess: Boolean? = null
        private var activeProbe: ActivityProbe? = null

        fun idleTimeoutSeconds(idleTimeoutSeconds: Int): Builder {
            require(idleTimeoutSeconds in MIN_IDLE_TIMEOUT_SECONDS..MAX_IDLE_TIMEOUT_SECONDS) {
                "Idle timeout seconds must be between $MIN_IDLE_TIMEOUT_SECONDS and " +
                    "$MAX_IDLE_TIMEOUT_SECONDS, got $idleTimeoutSeconds"
            }
            this.idleTimeoutSeconds = idleTimeoutSeconds
            return this
        }

        fun onIdle(onIdle: OnIdleAction): Builder {
            this.onIdle = onIdle
            return this
        }

        fun wakeOnAccess(wakeOnAccess: Boolean): Builder {
            this.wakeOnAccess = wakeOnAccess
            return this
        }

        fun activeProbe(activeProbe: ActivityProbe): Builder {
            this.activeProbe = activeProbe
            return this
        }

        fun activeProbe(configure: ActivityProbe.Builder.() -> Unit): Builder {
            val builder = ActivityProbe.builder()
            builder.configure()
            this.activeProbe = builder.build()
            return this
        }

        fun build(): IdlePolicy {
            val timeoutValue = idleTimeoutSeconds
            val actionValue = onIdle
            require(actionValue == null || timeoutValue != null) {
                "IdlePolicy.onIdle must be omitted when idleTimeoutSeconds is omitted " +
                    "(a policy without a timeout is wake-only)"
            }
            require(timeoutValue == null || actionValue != null) {
                "IdlePolicy.onIdle is required when idleTimeoutSeconds is set"
            }
            val probeValue = activeProbe
            require(probeValue == null || timeoutValue != null) {
                "IdlePolicy.activeProbe requires idleTimeoutSeconds; the probe is relevant only with auto-pause"
            }
            val policy =
                IdlePolicy(
                    idleTimeoutSeconds = timeoutValue,
                    onIdle = actionValue,
                    wakeOnAccess = wakeOnAccess,
                    activeProbe = probeValue,
                )
            // Fail fast like the other builders: a policy with no fields at
            // all is always a caller mistake, never a valid opt-in.
            require(!policy.isEmpty) {
                "IdlePolicy must set at least one field"
            }
            return policy
        }
    }
}

/**
 * Renew-on-access behavior (OSEP-0009): observed access moves the expiration
 * to `max(now + extendSeconds, current)`. Control-plane policy; supersedes
 * the deprecated `extensions["access.renew.extend.seconds"]` key.
 */
class RenewalPolicy private constructor(
    val extendSeconds: Int,
) {
    companion object {
        const val MIN_EXTEND_SECONDS = 300
        const val MAX_EXTEND_SECONDS = 86400

        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var extendSeconds: Int? = null

        fun extendSeconds(extendSeconds: Int): Builder {
            require(extendSeconds in MIN_EXTEND_SECONDS..MAX_EXTEND_SECONDS) {
                "RenewalPolicy.extendSeconds must be between $MIN_EXTEND_SECONDS and " +
                    "$MAX_EXTEND_SECONDS, got $extendSeconds"
            }
            this.extendSeconds = extendSeconds
            return this
        }

        fun build(): RenewalPolicy {
            val seconds = extendSeconds
            requireNotNull(seconds) {
                "RenewalPolicy must set extendSeconds"
            }
            return RenewalPolicy(extendSeconds = seconds)
        }
    }
}

/** Optional declarative sandbox lifecycle configuration. */
class SandboxLifecycle private constructor(
    val preStart: LifecycleHook?,
    val periodic: List<PeriodicLifecycleHook>?,
    val idlePolicy: IdlePolicy?,
    val renewalPolicy: RenewalPolicy?,
) {
    internal val isEmpty: Boolean
        get() = preStart == null && periodic.isNullOrEmpty() && idlePolicy == null && renewalPolicy == null

    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var preStart: LifecycleHook? = null
        private var periodic: List<PeriodicLifecycleHook>? = null
        private var idlePolicy: IdlePolicy? = null
        private var renewalPolicy: RenewalPolicy? = null

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

        fun idlePolicy(idlePolicy: IdlePolicy): Builder {
            this.idlePolicy = idlePolicy
            return this
        }

        fun idlePolicy(configure: IdlePolicy.Builder.() -> Unit): Builder {
            val builder = IdlePolicy.builder()
            builder.configure()
            this.idlePolicy = builder.build()
            return this
        }

        fun renewalPolicy(renewalPolicy: RenewalPolicy): Builder {
            this.renewalPolicy = renewalPolicy
            return this
        }

        fun renewalPolicy(configure: RenewalPolicy.Builder.() -> Unit): Builder {
            val builder = RenewalPolicy.builder()
            builder.configure()
            this.renewalPolicy = builder.build()
            return this
        }

        fun build(): SandboxLifecycle =
            SandboxLifecycle(
                preStart = preStart,
                periodic = periodic?.takeIf { it.isNotEmpty() },
                idlePolicy = idlePolicy?.takeUnless { it.isEmpty },
                renewalPolicy = renewalPolicy,
            )
    }
}
