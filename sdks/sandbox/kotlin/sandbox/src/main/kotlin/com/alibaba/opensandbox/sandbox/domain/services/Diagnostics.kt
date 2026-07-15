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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent

/**
 * Sandbox diagnostics service.
 */
interface Diagnostics {
    /**
     * Gets diagnostic log content for a sandbox by ID.
     *
     * @param sandboxId Unique identifier of the sandbox
     * @param scope Required diagnostic scope such as "container", "lifecycle", or "all"
     */
    fun getLogs(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent

    /**
     * Gets diagnostic event content for a sandbox by ID.
     *
     * @param sandboxId Unique identifier of the sandbox
     * @param scope Required diagnostic scope such as "runtime", "lifecycle", or "all"
     */
    fun getEvents(
        sandboxId: String,
        scope: String,
    ): DiagnosticContent
}
