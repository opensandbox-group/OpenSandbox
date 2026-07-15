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

package com.alibaba.opensandbox.sandbox.domain.models.diagnostics

import java.net.URI
import java.time.OffsetDateTime

/**
 * Descriptor for best-effort plain-text diagnostic content.
 *
 * @property sandboxId Unique identifier of the sandbox
 * @property kind Diagnostic payload kind, such as "logs" or "events"
 * @property scope Diagnostic scope used for this response
 * @property delivery How the diagnostic text payload is delivered, such as "inline" or "url"
 * @property contentType Media type of the diagnostic payload
 * @property truncated Whether the diagnostic payload was intentionally truncated
 * @property content Inline diagnostic text payload when delivery is "inline"
 * @property contentUrl URL where the diagnostic text payload can be downloaded when delivery is "url"
 * @property contentLength Payload size in bytes when known
 * @property expiresAt Expiration time for the download URL when delivery is "url"
 * @property warnings Non-fatal warnings about payload completeness or availability
 */
class DiagnosticContent(
    val sandboxId: String,
    val kind: String,
    val scope: String,
    val delivery: String,
    val contentType: String,
    val truncated: Boolean,
    val content: String? = null,
    val contentUrl: URI? = null,
    val contentLength: Int? = null,
    val expiresAt: OffsetDateTime? = null,
    val warnings: List<String>? = null,
)
