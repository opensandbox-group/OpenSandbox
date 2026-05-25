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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter

import com.alibaba.opensandbox.sandbox.api.models.diagnostic.DiagnosticContentResponse
import com.alibaba.opensandbox.sandbox.domain.models.diagnostics.DiagnosticContent

internal object DiagnosticModelConverter {
    fun DiagnosticContentResponse.toDiagnosticContent(): DiagnosticContent {
        return DiagnosticContent(
            sandboxId = sandboxId,
            kind = kind.value,
            scope = scope,
            delivery = delivery.value,
            contentType = contentType,
            truncated = truncated,
            content = content,
            contentUrl = contentUrl,
            contentLength = contentLength,
            expiresAt = expiresAt,
            warnings = warnings,
        )
    }
}
