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

package com.alibaba.opensandbox.sandbox

import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.internal.isCausedByInterruption
import com.alibaba.opensandbox.sandbox.transport.RequestDeadline
import java.time.Duration
import java.util.concurrent.TimeUnit

internal class ReadinessBudget(timeout: Duration, private val interval: Duration) {
    val timeout: Duration = timeout
    private var context: String? = null
    private var attempts = 0
    private val deadline = System.nanoTime() + timeout.toNanos()
    private var lastError: Throwable? = null

    fun remaining(): Long {
        if (Thread.currentThread().isInterrupted) throw InterruptedException("Sandbox connection interrupted")
        val remaining = deadline - System.nanoTime()
        if (remaining <= 0) {
            val detail = lastError?.let { "Last error: ${it.message}" } ?: "Check returned false continuously"
            throw SandboxReadyTimeoutException(
                if (context == null) {
                    "Sandbox readiness timed out. Last error: ${lastError?.message ?: "Endpoint not yet available."}"
                } else {
                    "Sandbox health check timed out after ${timeout.seconds}s ($attempts attempts). " +
                        "$detail Connection context: $context."
                },
                lastError,
            )
        }
        return remaining
    }

    private fun <T> run(action: () -> T): T {
        remaining()
        try {
            val result = RequestDeadline.within(deadline, action)
            remaining()
            return result
        } catch (error: Exception) {
            if (error.isCausedByInterruption()) {
                Thread.currentThread().interrupt()
                throw error
            }
            remaining()
            throw error
        }
    }

    private fun pause() = TimeUnit.NANOSECONDS.sleep(minOf(interval.toNanos(), remaining()))

    fun <T> endpoint(action: () -> T): T {
        while (true) {
            try {
                return run(action)
            } catch (error: SandboxApiException) {
                if (error.statusCode != 404 || error.error.code != "KUBERNETES::POD_IP_NOT_AVAILABLE") throw error
                lastError = error
            }
            pause()
        }
    }

    fun health(
        context: String,
        action: () -> Boolean,
    ) {
        this.context = context
        lastError = null
        while (true) {
            try {
                attempts++
                if (run(action)) return
                lastError = null
            } catch (error: InterruptedException) {
                throw error
            } catch (error: Exception) {
                if (error.isCausedByInterruption()) throw error
                remaining()
                lastError = error
            }
            pause()
        }
    }
}
