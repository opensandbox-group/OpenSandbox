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

package com.alibaba.opensandbox.sandbox.transport

import io.opentelemetry.context.Context
import okhttp3.Call
import okhttp3.EventListener
import okhttp3.OkHttpClient
import java.io.InterruptedIOException
import java.util.concurrent.ExecutionException
import java.util.concurrent.Executors
import java.util.concurrent.FutureTask
import java.util.concurrent.TimeUnit
import java.util.concurrent.TimeoutException
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicReference

internal object RequestDeadline {
    private val current = ThreadLocal<Long?>()
    private val requests =
        Executors.newCachedThreadPool { request ->
            Thread(request, "opensandbox-readiness-request").apply { isDaemon = true }
        }

    fun <T> within(
        deadline: Long,
        action: () -> T,
    ): T {
        val previous = current.get()
        current.set(if (previous == null) deadline else minOf(previous, deadline))
        try {
            return action()
        } finally {
            current.set(previous)
        }
    }

    fun <T> execute(
        client: OkHttpClient,
        action: (OkHttpClient) -> T,
    ): T {
        val deadline = current.get() ?: return action(client)
        val remaining = deadline - System.nanoTime()
        if (remaining <= 0) throw InterruptedIOException("Request deadline exceeded")
        val activeCall = AtomicReference<Call?>()
        val cancelled = AtomicBoolean(false)
        val boundedClient =
            client.newBuilder()
                .eventListener(
                    object : EventListener() {
                        override fun callStart(call: Call) {
                            activeCall.set(call)
                            if (cancelled.get()) call.cancel()
                        }
                    },
                ).build()
        val context = Context.current()
        val request = FutureTask<T> { context.makeCurrent().use { action(boundedClient) } }
        requests.execute(request)
        try {
            return request.get(maxOf(0, deadline - System.nanoTime()), TimeUnit.NANOSECONDS)
        } catch (error: ExecutionException) {
            throw error.cause ?: error
        } catch (error: InterruptedException) {
            Thread.currentThread().interrupt()
            throw error
        } catch (error: TimeoutException) {
            throw InterruptedIOException("Request deadline exceeded").apply { initCause(error) }
        } finally {
            if (!request.isDone) {
                cancelled.set(true)
                activeCall.get()?.cancel()
                request.cancel(true)
            }
        }
    }
}
