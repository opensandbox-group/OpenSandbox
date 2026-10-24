/*
 * Copyright 2025 The OpenSandbox Authors
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

package com.alibaba.opensandbox.sandbox.api.models.execd

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

@Serializable
data class EventNode @JvmOverloads constructor(
    val type: String,
    val timestamp: Long,
    val text: String? = null,
    val results: ResultData? = null,
    @SerialName("execution_time")
    val executionTimeInMillis: Long? = null,
    @SerialName("execution_count")
    val executionCount: Long? = null,
    val error: ErrorData? = null,
    // Successful foreground POST /command exit status (0). Present only on
    // execution_complete after the process has exited; omitted for background
    // startup, code execution, and older servers. Non-zero exits use error.exit_code.
    @SerialName("exit_code")
    val exitCode: Long? = null,
)

@Serializable
@JvmInline
value class ResultData(val raw: JsonObject) {
    fun getText(): String? {
        return raw["text"]?.jsonPrimitive?.contentOrNull
    }

    fun getStringResult(key: String): String? = raw[key]?.jsonPrimitive?.contentOrNull
}

@Serializable
data class ErrorData @JvmOverloads constructor(
    @SerialName("ename")
    val name: String? = null,
    @SerialName("evalue")
    val value: String? = null,
    @SerialName("traceback")
    val traceback: List<String> = emptyList(),
    // Exit status for CommandExecError when the command ran and finished
    // non-zero (-1 when terminated by a signal). Null when the process failed
    // to start, for non-command errors, and on servers that predate error.exit_code.
    @SerialName("exit_code")
    val exitCode: Long? = null,
)
