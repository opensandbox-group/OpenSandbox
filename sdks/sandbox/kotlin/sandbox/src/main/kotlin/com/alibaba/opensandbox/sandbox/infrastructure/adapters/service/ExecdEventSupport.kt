// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.api.models.execd.EventNode
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.Execution
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.jsonParser

/**
 * Shared execd SSE-line decoding and foreground exit-code inference for every
 * execd streaming consumer. Not part of the stable public API.
 */
public object ExecdEventSupport {
    /**
     * Decode one SSE/NDJSON line into an [EventNode], skipping framing lines
     * and unwrapping the `data:` prefix. Returns null for non-payload lines.
     */
    public fun decodeEventLine(
        line: String,
        onError: (String, Exception) -> Unit = { _, _ -> },
    ): EventNode? {
        if (line.isBlank()) {
            return null
        }

        val payload =
            when {
                line.startsWith(":") -> return null
                line.startsWith("event:") -> return null
                line.startsWith("id:") -> return null
                line.startsWith("retry:") -> return null
                line.startsWith("data:") -> line.drop(5).removePrefix(" ")
                else -> line
            }

        if (payload.isBlank()) {
            return null
        }

        return try {
            jsonParser.decodeFromString(EventNode.serializer(), payload)
        } catch (e: Exception) {
            onError(line, e)
            null
        }
    }

    /** Error payload carries the code on failure; completion implies success. */
    public fun inferForegroundExitCode(execution: Execution): Int? {
        return if (execution.error != null) {
            execution.error?.value?.toIntOrNull()
        } else {
            if (execution.complete != null) 0 else null
        }
    }
}
