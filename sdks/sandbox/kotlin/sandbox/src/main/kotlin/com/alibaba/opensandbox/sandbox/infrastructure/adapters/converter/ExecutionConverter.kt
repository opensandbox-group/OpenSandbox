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

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import java.time.Duration
import com.alibaba.opensandbox.sandbox.api.models.execd.CommandStatusResponse as ApiCommandStatusResponse
import com.alibaba.opensandbox.sandbox.api.models.execd.RunCommandRequest as ApiRunCommandRequest

object ExecutionConverter {
    fun RunCommandRequest.toApiRunCommandRequest(): ApiRunCommandRequest {
        return ApiRunCommandRequest(
            command = command,
            background = background,
            cwd = workingDirectory,
            timeout = timeout?.toCommandTimeoutMillis(),
            uid = uid,
            gid = gid,
            envs = envs,
        )
    }

    fun ApiCommandStatusResponse.toCommandStatus(): CommandStatus {
        return CommandStatus(
            id = id,
            content = content,
            running = running,
            exitCode = exitCode,
            error = error,
            startedAt = startedAt,
            finishedAt = finishedAt,
        )
    }
}

internal fun Duration.toCommandTimeoutMillis(): Long {
    require(!isNegative) { "Timeout must be non-negative, got: $this" }
    return try {
        toMillis()
    } catch (e: ArithmeticException) {
        throw IllegalArgumentException("Timeout is too large to represent in milliseconds: $this", e)
    }
}
