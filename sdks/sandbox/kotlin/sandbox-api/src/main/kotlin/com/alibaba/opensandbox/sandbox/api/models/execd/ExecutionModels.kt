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

package com.alibaba.opensandbox.sandbox.api.models.execd

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonPrimitive

@Serializable
data class EventNode(
    val type: String,
    val timestamp: Long,
    val text: String? = null,
    val results: ResultData? = null,
    @SerialName("execution_time")
    val executionTimeInMillis: Long? = null,
    @SerialName("execution_count")
    val executionCount: Long? = null,
    val error: ErrorData? = null,
    @SerialName("eid")
    val eid: Long? = null,
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
data class ErrorData(
    @SerialName("ename")
    val name: String? = null,
    @SerialName("evalue")
    val value: String? = null,
    @SerialName("traceback")
    val traceback: List<String> = emptyList(),
)
