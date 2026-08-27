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

package com.alibaba.opensandbox.sandbox.pool

import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxConnectionException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxRateLimitException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxReadyTimeoutException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxTimeoutException

/** Stable, internal vocabulary shared by pool warmup tracing and structured logs. */
internal enum class WarmupStage(
    val value: String,
) {
    ADMISSION("admission"),
    CREATE("create"),
    READINESS("readiness"),
    PREPARE("prepare"),
    POST_PREPARE_READINESS("post_prepare_readiness"),
    RENEW("renew"),
    COMMIT("commit"),
}

internal enum class WarmupResult(
    val value: String,
) {
    SUCCESS("success"),
    FAILURE("failure"),
    DROPPED("dropped"),
    CANCELLED("cancelled"),
}

internal enum class WarmupReason(
    val value: String,
) {
    CREATE_EXECUTOR_REJECTED("create_executor_rejected"),
    CREATE_FAILED("create_failed"),
    READINESS_TIMEOUT("readiness_timeout"),
    PREPARE_FAILED("prepare_failed"),
    POST_PREPARE_READINESS_TIMEOUT("post_prepare_readiness_timeout"),
    RENEW_FAILED("renew_failed"),
    COMMIT_FAILED("commit_failed"),
    PRIMARY_LOCK_LOST("primary_lock_lost"),
    STALE_RUN("stale_run"),
    RUN_RETIRED("run_retired"),
    POOL_STOPPED("pool_stopped"),
    INTERRUPTED("interrupted"),
    UNEXPECTED_FAILURE("unexpected_failure"),
}

internal enum class WarmupErrorCategory(
    val value: String,
) {
    RATE_LIMIT("rate_limit"),
    HTTP_4XX("http_4xx"),
    HTTP_5XX("http_5xx"),
    HTTP_OTHER("http_other"),
    TIMEOUT("timeout"),
    CONNECTION("connection"),
    CALLBACK("callback"),
    STATE_STORE("state_store"),
    UNCLASSIFIED("unclassified"),
}

internal data class WarmupTerminalOutcome(
    val stage: WarmupStage,
    val result: WarmupResult,
    val reason: WarmupReason? = null,
    val error: Throwable? = null,
    val sandboxId: String? = null,
    val errorCategory: WarmupErrorCategory? = classifyWarmupError(stage, error),
)

internal fun classifyWarmupError(
    stage: WarmupStage,
    error: Throwable?,
): WarmupErrorCategory? =
    when (error) {
        null -> null
        is SandboxRateLimitException -> WarmupErrorCategory.RATE_LIMIT
        is SandboxTimeoutException, is SandboxReadyTimeoutException -> WarmupErrorCategory.TIMEOUT
        is SandboxConnectionException -> WarmupErrorCategory.CONNECTION
        is SandboxApiException ->
            when (error.statusCode) {
                in 400..499 -> WarmupErrorCategory.HTTP_4XX
                in 500..599 -> WarmupErrorCategory.HTTP_5XX
                else -> WarmupErrorCategory.HTTP_OTHER
            }
        else ->
            when (stage) {
                WarmupStage.READINESS,
                WarmupStage.PREPARE,
                WarmupStage.POST_PREPARE_READINESS,
                -> WarmupErrorCategory.CALLBACK
                WarmupStage.COMMIT -> WarmupErrorCategory.STATE_STORE
                else -> WarmupErrorCategory.UNCLASSIFIED
            }
    }
