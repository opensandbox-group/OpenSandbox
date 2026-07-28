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

import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandPagination
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandStatus
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.CommandSummary
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.ListCommandsPage
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunCommandRequest
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.RunningCommandSummary
import com.alibaba.opensandbox.sandbox.domain.models.execd.executions.TerminalCommandSummary
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import java.time.Duration
import java.time.OffsetDateTime
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

    /** Converts a raw command summary while enforcing the inventory branch contract. */
    fun JsonObject.toCommandSummary(): CommandSummary {
        val running = requiredBoolean("running")
        if (running) {
            requireExactKeys(RUNNING_COMMAND_KEYS, "running command")
            val session = requiredString("session")
            val background = requiredBoolean("background")
            val startedAt = requiredRfc3339("started_at")
            return RunningCommandSummary(session, background, startedAt)
        }
        requireTerminalKeys()
        val session = requiredString("session")
        val background = requiredBoolean("background")
        val startedAt = requiredRfc3339("started_at")
        val finishedAt = requiredRfc3339("finished_at")
        val exitCode = requiredNullableInt("exit_code")
        return TerminalCommandSummary(
            session,
            background,
            startedAt,
            finishedAt,
            exitCode,
            optionalString("error"),
        )
    }

    private fun JsonObject.requiredString(name: String): String =
        get(name).requirePrimitive(name).let { primitive ->
            require(primitive.isString) { "Command field $name must be a string" }
            primitive.content
        }

    private fun JsonObject.requiredBoolean(name: String): Boolean =
        get(name).requirePrimitive(name).let { primitive ->
            require(!primitive.isString && (primitive.content == "true" || primitive.content == "false")) {
                "Command field $name must be a boolean"
            }
            primitive.content.toBoolean()
        }

    private fun JsonObject.optionalString(name: String): String? {
        if (name !in this) return null
        return requiredString(name)
    }

    private fun JsonObject.requiredNullableInt(name: String): Int? {
        val element = get(name) ?: throw IllegalArgumentException("Missing required command field: $name")
        if (element is JsonNull) return null
        val primitive = element.requirePrimitive(name)
        require(!primitive.isString && INTEGER_LITERAL_PATTERN.matches(primitive.content)) {
            "Command field $name must be an int32"
        }
        return primitive.content.toLongOrNull()
            ?.takeIf { it in Int.MIN_VALUE.toLong()..Int.MAX_VALUE.toLong() }
            ?.toInt()
            ?: throw IllegalArgumentException("Command field $name must be an int32")
    }

    private fun JsonObject.requiredRfc3339(name: String): OffsetDateTime {
        val value = requiredString(name)
        require(RFC3339_PATTERN.matches(value)) { "Command field $name must be an RFC3339 timestamp" }
        return try {
            OffsetDateTime.parse(value)
        } catch (e: Exception) {
            throw IllegalArgumentException("Command field $name must be an RFC3339 timestamp", e)
        }
    }

    private fun JsonObject.requireExactKeys(
        allowedKeys: Set<String>,
        branch: String,
    ) {
        require(keys == allowedKeys) { "$branch has invalid fields" }
    }

    private fun JsonObject.requireTerminalKeys() {
        require(TERMINAL_REQUIRED_COMMAND_KEYS.all { it in this } && keys.all { it in TERMINAL_ALLOWED_COMMAND_KEYS }) {
            "terminal command has invalid fields"
        }
    }

    private fun kotlinx.serialization.json.JsonElement?.requirePrimitive(name: String): JsonPrimitive {
        if (this == null) throw IllegalArgumentException("Missing required command field: $name")
        if (this is JsonNull || this !is JsonPrimitive) {
            throw IllegalArgumentException("Command field $name must be a primitive")
        }
        return this
    }

    private val RUNNING_COMMAND_KEYS = setOf("session", "running", "background", "started_at")
    private val TERMINAL_REQUIRED_COMMAND_KEYS =
        setOf("session", "running", "background", "started_at", "finished_at", "exit_code")
    private val TERMINAL_ALLOWED_COMMAND_KEYS = TERMINAL_REQUIRED_COMMAND_KEYS + "error"
    private val RFC3339_PATTERN =
        Regex("[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\\.[0-9]+)?(?:[Zz]|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])")
}

internal fun parseListCommandsPage(payload: String): ListCommandsPage {
    val page = parseCommandInventoryObject(commandInventoryJson.parseToJsonElement(payload), "response")
    val commands =
        page["commands"] as? JsonArray
            ?: throw IllegalArgumentException("response.commands must be an array")
    val pagination = parseCommandInventoryObject(page["pagination"], "pagination")
    val limit = pagination["limit"].requireInventoryInt("pagination.limit")
    require(limit in 1..100) { "pagination.limit must be between 1 and 100" }
    val nextCursor =
        if ("nextCursor" in pagination) {
            pagination["nextCursor"].requireInventoryString("pagination.nextCursor")
        } else {
            null
        }
    require(nextCursor == null || nextCursor.isNotBlank()) {
        "pagination.nextCursor must not be blank"
    }
    return ListCommandsPage(
        commands = commands.map { ExecutionConverter.run { parseCommandInventoryObject(it, "command").toCommandSummary() } },
        pagination = CommandPagination(limit, nextCursor),
    )
}

private fun parseCommandInventoryObject(
    element: kotlinx.serialization.json.JsonElement?,
    name: String,
): JsonObject = element as? JsonObject ?: throw IllegalArgumentException("$name must be an object")

private fun kotlinx.serialization.json.JsonElement?.requireInventoryString(name: String): String {
    val primitive =
        this as? JsonPrimitive
            ?: throw IllegalArgumentException("$name must be a string")
    require(primitive.isString) { "$name must be a string" }
    return primitive.content
}

private fun kotlinx.serialization.json.JsonElement?.requireInventoryInt(name: String): Int {
    val primitive =
        this as? JsonPrimitive
            ?: throw IllegalArgumentException("$name must be an integer")
    require(!primitive.isString && INTEGER_LITERAL_PATTERN.matches(primitive.content)) {
        "$name must be an integer"
    }
    return primitive.content.toIntOrNull() ?: throw IllegalArgumentException("$name must be an integer")
}

private val commandInventoryJson =
    Json {
        isLenient = false
    }

private val INTEGER_LITERAL_PATTERN = Regex("-?(0|[1-9][0-9]*)")

internal fun Duration.toCommandTimeoutMillis(): Long {
    require(!isNegative) { "Timeout must be non-negative, got: $this" }
    return try {
        toMillis()
    } catch (e: ArithmeticException) {
        throw IllegalArgumentException("Timeout is too large to represent in milliseconds: $this", e)
    }
}
