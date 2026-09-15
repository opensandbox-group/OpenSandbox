/*
 * Copyright 2026 Alibaba Group Holding Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ByteRange
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ReadBytesResponse
import okhttp3.Response

private val contentRangeRegex = Regex("""^bytes\s+(\d+)-(\d+)/(\d+|\*)$""", RegexOption.IGNORE_CASE)

private fun parseContentRange(raw: String?): ByteRange? {
    if (raw.isNullOrEmpty()) return null

    val invalid = ByteRange(start = -1, end = -1, total = -1, raw = raw)
    val match = contentRangeRegex.matchEntire(raw) ?: return invalid
    val start = match.groupValues[1].toLongOrNull() ?: return invalid
    val end = match.groupValues[2].toLongOrNull() ?: return invalid
    val total =
        if (match.groupValues[3] == "*") {
            -1
        } else {
            match.groupValues[3].toLongOrNull() ?: return invalid
        }
    if (end < start || (total != -1L && total <= end)) return invalid
    return ByteRange(start = start, end = end, total = total, raw = raw)
}

internal fun <T> Response.toReadBytesResponse(body: T): ReadBytesResponse<T> {
    val isPartial = code == 206
    val contentLength = header("Content-Length")?.toLongOrNull()?.takeIf { it >= 0 } ?: -1
    val contentRange = if (isPartial) parseContentRange(header("Content-Range")) else null
    return ReadBytesResponse(
        body = body,
        statusCode = code,
        contentType = header("Content-Type"),
        contentDisposition = header("Content-Disposition"),
        contentLength = contentLength,
        totalSize = if (isPartial) contentRange?.total ?: -1 else contentLength,
        contentRange = contentRange,
    )
}
