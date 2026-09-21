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

package com.alibaba.opensandbox.sandbox.domain.models.execd.executions

import java.time.OffsetDateTime

/**
 * Command execution status (foreground or background).
 *
 * @property id Command ID returned by run command
 * @property content Original command content
 * @property running Whether the command is still running
 * @property exitCode Exit code if the command has finished
 * @property error Error message if the command failed
 * @property startedAt Start time in RFC3339 format
 * @property finishedAt Finish time in RFC3339 format (null if still running)
 */
class CommandStatus(
    val id: String?,
    val content: String?,
    val running: Boolean?,
    val exitCode: Int?,
    val error: String?,
    val startedAt: OffsetDateTime?,
    val finishedAt: OffsetDateTime?,
)

/**
 * Background command logs with tail cursor.
 *
 * @property content Raw stdout/stderr content
 * @property cursor Latest cursor for incremental reads
 */
class CommandLogs(
    val content: String,
    val cursor: Long?,
)
