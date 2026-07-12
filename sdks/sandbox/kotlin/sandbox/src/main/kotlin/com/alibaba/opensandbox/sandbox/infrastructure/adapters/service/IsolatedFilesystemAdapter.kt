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

package com.alibaba.opensandbox.sandbox.infrastructure.adapters.service

import com.alibaba.opensandbox.sandbox.HttpClientProvider
import com.alibaba.opensandbox.sandbox.api.execd.IsolatedExecutionApi
import com.alibaba.opensandbox.sandbox.domain.exceptions.InvalidArgumentException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxApiException
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError
import com.alibaba.opensandbox.sandbox.domain.exceptions.SandboxError.Companion.UNEXPECTED_RESPONSE
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceResult
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.EntryInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.MoveEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SearchEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SetPermissionEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import com.alibaba.opensandbox.sandbox.domain.models.sandboxes.SandboxEndpoint
import com.alibaba.opensandbox.sandbox.domain.services.Filesystem
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.FilesystemConverter.toApiPermissionMap
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.FilesystemConverter.toApiRenameFileItems
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.FilesystemConverter.toApiReplaceFileContentMap
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.FilesystemConverter.toEntryInfo
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.FilesystemConverter.toEntryInfoMap
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.isFileNotFound
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.parseSandboxError
import com.alibaba.opensandbox.sandbox.infrastructure.adapters.converter.toSandboxException
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import okhttp3.Headers.Companion.toHeaders
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MediaType.Companion.toMediaTypeOrNull
import okhttp3.MultipartBody
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okio.BufferedSink
import okio.source
import org.slf4j.LoggerFactory
import java.io.InputStream
import java.nio.charset.Charset
import java.util.UUID

/**
 * Filesystem adapter for isolated sessions using generated [IsolatedExecutionApi].
 *
 * Uses auto-generated API methods with explicit sessionId parameter instead of
 * URL prefix composition.
 */
internal class IsolatedFilesystemAdapter(
    private val httpClientProvider: HttpClientProvider,
    private val execdEndpoint: SandboxEndpoint,
    private val sessionId: String,
) : Filesystem {
    companion object {
        private const val UPLOAD_PATH = "/v1/isolated/session/{sessionId}/files/upload"
        private const val DOWNLOAD_PATH = "/v1/isolated/session/{sessionId}/files/download"
    }

    private val logger = LoggerFactory.getLogger(IsolatedFilesystemAdapter::class.java)
    private val sessionUuid: UUID = UUID.fromString(sessionId)
    private val execdBaseUrl =
        "${httpClientProvider.config.protocol}://${execdEndpoint.endpoint}"
    private val api =
        IsolatedExecutionApi(
            execdBaseUrl,
            httpClientProvider.httpClient.newBuilder()
                .addInterceptor { chain ->
                    val requestBuilder = chain.request().newBuilder()
                    execdEndpoint.headers.forEach { (key, value) ->
                        requestBuilder.header(key, value)
                    }
                    chain.proceed(requestBuilder.build())
                }
                .build(),
        )

    private fun resolvedPath(template: String): String = template.replace("{sessionId}", sessionId)

    override fun readFile(
        path: String,
        encoding: String,
        range: String?,
        offset: Int?,
        limit: Int?,
    ): String {
        try {
            val request = buildDownloadRequest(path, range, offset, limit)
            httpClientProvider.httpClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    val errorBodyString = response.body?.string()
                    val sandboxError = parseSandboxError(errorBodyString)
                    throw SandboxApiException(
                        message = "Failed to read file. Status code: ${response.code}, Body: $errorBodyString",
                        statusCode = response.code,
                        error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
                        requestId = response.header("X-Request-ID"),
                    )
                }
                val charset = getCharsetFromEncoding(encoding)
                return response.body?.source()?.readString(charset) ?: ""
            }
        } catch (e: Exception) {
            logReadFailure("Failed to read file with encoding $encoding: $path", e)
            throw e.toSandboxException()
        }
    }

    override fun readByteArray(
        path: String,
        range: String?,
        offset: Int?,
        limit: Int?,
    ): ByteArray {
        try {
            val request = buildDownloadRequest(path, range, offset, limit)
            httpClientProvider.httpClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    val errorBodyString = response.body?.string()
                    val sandboxError = parseSandboxError(errorBodyString)
                    throw SandboxApiException(
                        message = "Failed to read file. Status code: ${response.code}, Body: $errorBodyString",
                        statusCode = response.code,
                        error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
                        requestId = response.header("X-Request-ID"),
                    )
                }
                return response.body?.bytes() ?: ByteArray(0)
            }
        } catch (e: Exception) {
            logReadFailure("Failed to read file as byte array: $path", e)
            throw e.toSandboxException()
        }
    }

    override fun readStream(
        path: String,
        range: String?,
        offset: Int?,
        limit: Int?,
    ): InputStream {
        try {
            val request = buildDownloadRequest(path, range, offset, limit)
            val response = httpClientProvider.httpClient.newCall(request).execute()
            if (!response.isSuccessful) {
                try {
                    val errorBodyString = response.body?.string()
                    val sandboxError = parseSandboxError(errorBodyString)
                    throw SandboxApiException(
                        message = "Failed to read file. Status code: ${response.code}, Body: $errorBodyString",
                        statusCode = response.code,
                        error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
                        requestId = response.header("X-Request-ID"),
                    )
                } catch (e: Exception) {
                    response.close()
                    throw e
                }
            }
            return response.body?.byteStream()
                ?: throw IllegalStateException("Response body is null")
        } catch (e: Exception) {
            logReadFailure("Failed to read file as stream: $path", e)
            throw e.toSandboxException()
        }
    }

    override fun write(entries: List<WriteEntry>) {
        if (entries.isEmpty()) return
        try {
            val builder = MultipartBody.Builder().setType(MultipartBody.FORM)
            entries.forEach { entry ->
                val path = entry.path
                val data = entry.data
                requireNotNull(path) { "File path cannot be null" }
                requireNotNull(data) { "File data cannot be null" }
                val metadataJson =
                    buildJsonObject {
                        put("path", path)
                        put("owner", entry.owner)
                        put("group", entry.group)
                        put("mode", entry.mode)
                    }.toString()

                builder.addFormDataPart(
                    "metadata",
                    "metadata",
                    metadataJson.toRequestBody("application/json".toMediaType()),
                )

                val fileBody =
                    when (data) {
                        is ByteArray -> data.toRequestBody("application/octet-stream".toMediaType())
                        is String -> {
                            val charset = getCharsetFromEncoding(entry.encoding)
                            data.toRequestBody("text/plain; charset=${charset.name()}".toMediaType())
                        }
                        is InputStream ->
                            object : RequestBody() {
                                override fun contentType() = "application/octet-stream".toMediaTypeOrNull()

                                override fun contentLength() = -1L

                                override fun writeTo(sink: BufferedSink) {
                                    data.source().use { source -> sink.writeAll(source) }
                                }
                            }
                        else -> throw IllegalArgumentException("Unsupported file data type: ${data::class.java}")
                    }
                builder.addFormDataPart("file", path, fileBody)
            }

            val url = "$execdBaseUrl${resolvedPath(UPLOAD_PATH)}"
            val request =
                Request.Builder()
                    .url(url)
                    .headers(execdEndpoint.headers.toHeaders())
                    .post(builder.build())
                    .build()

            httpClientProvider.httpClient.newCall(request).execute().use { response ->
                if (!response.isSuccessful) {
                    val errorBodyString = response.body?.string()
                    val sandboxError = parseSandboxError(errorBodyString)
                    throw SandboxApiException(
                        message = "Failed to write files. Status code: ${response.code}, Body: $errorBodyString",
                        statusCode = response.code,
                        error = sandboxError ?: SandboxError(UNEXPECTED_RESPONSE),
                        requestId = response.header("X-Request-ID"),
                    )
                }
            }
        } catch (e: Exception) {
            logger.error("Failed to write {} files", entries.size, e)
            throw e.toSandboxException()
        }
    }

    override fun createDirectories(entries: List<WriteEntry>) {
        try {
            val permissionMap =
                entries.associate { entry ->
                    entry.path to
                        com.alibaba.opensandbox.sandbox.api.models.execd.Permission(
                            mode = entry.mode,
                            group = entry.group,
                            owner = entry.owner,
                        )
                }
            api.isolatedMakeDirs(sessionUuid, permissionMap)
        } catch (e: Exception) {
            logger.error("Failed to create directories", e)
            throw e.toSandboxException()
        }
    }

    override fun deleteFiles(paths: List<String>) {
        try {
            api.isolatedRemoveFiles(sessionUuid, paths)
        } catch (e: Exception) {
            logger.error("Failed to delete {} files", paths.size, e)
            throw e.toSandboxException()
        }
    }

    override fun deleteDirectories(paths: List<String>) {
        try {
            api.isolatedRemoveDirs(sessionUuid, paths)
        } catch (e: Exception) {
            logger.error("Failed to delete {} directories", paths.size, e)
            throw e.toSandboxException()
        }
    }

    override fun listDirectory(
        path: String,
        depth: Int?,
    ): List<EntryInfo> {
        return try {
            api.isolatedListDirectory(sessionUuid, path, depth).map { it.toEntryInfo() }
        } catch (e: Exception) {
            logger.error("Failed to list directory {}", path, e)
            throw e.toSandboxException()
        }
    }

    override fun moveFiles(entries: List<MoveEntry>) {
        try {
            val renameItems = entries.toApiRenameFileItems()
            api.isolatedRenameFiles(sessionUuid, renameItems)
        } catch (e: Exception) {
            logger.error("Failed to move files", e)
            throw e.toSandboxException()
        }
    }

    override fun setPermissions(entries: List<SetPermissionEntry>) {
        try {
            val permissionMap = entries.toApiPermissionMap()
            api.isolatedChmodFiles(sessionUuid, permissionMap)
        } catch (e: Exception) {
            logger.error("Failed to set permissions", e)
            throw e.toSandboxException()
        }
    }

    override fun replaceContents(entries: List<ContentReplaceEntry>) {
        try {
            val replaceMap = entries.toApiReplaceFileContentMap()
            val response = api.isolatedReplaceContentWithHttpInfo(sessionUuid, replaceMap, verbose = false)
            if (response.statusCode !in 200..299) {
                throw SandboxApiException(
                    message = "Replace contents failed. Status: ${response.statusCode}",
                    statusCode = response.statusCode,
                    error = SandboxError(UNEXPECTED_RESPONSE),
                )
            }
        } catch (e: Exception) {
            logger.error("Failed to replace contents", e)
            throw e.toSandboxException()
        }
    }

    override fun replaceContentsDetailed(entries: List<ContentReplaceEntry>): List<ContentReplaceResult> {
        return try {
            val replaceMap = entries.toApiReplaceFileContentMap()
            val response = api.isolatedReplaceContentWithHttpInfo(sessionUuid, replaceMap, verbose = true)
            if (response is com.alibaba.opensandbox.sandbox.api.execd.infrastructure.Success) {
                response.data?.map { (path, item) ->
                    ContentReplaceResult(
                        path = path,
                        replacedCount = item.replacedCount,
                    )
                } ?: emptyList()
            } else {
                emptyList()
            }
        } catch (e: Exception) {
            logger.error("Failed to replace contents", e)
            throw e.toSandboxException()
        }
    }

    override fun search(entry: SearchEntry): List<EntryInfo> {
        return try {
            api.isolatedSearchFiles(sessionUuid, entry.path, entry.pattern).map { it.toEntryInfo() }
        } catch (e: Exception) {
            logger.error("Failed to search files", e)
            throw e.toSandboxException()
        }
    }

    override fun readFileInfo(paths: List<String>): Map<String, EntryInfo> {
        return try {
            api.isolatedGetFilesInfo(sessionUuid, paths).toEntryInfoMap()
        } catch (e: Exception) {
            logger.error("Failed to get file info for {} paths", paths.size, e)
            throw e.toSandboxException()
        }
    }

    private fun logReadFailure(
        message: String,
        e: Exception,
    ) {
        if (e.isFileNotFound()) {
            logger.debug(message, e)
        } else {
            logger.error(message, e)
        }
    }

    private fun getCharsetFromEncoding(encoding: String): Charset {
        try {
            return charset(encoding)
        } catch (e: IllegalArgumentException) {
            throw InvalidArgumentException("Invalid encoding $encoding", e)
        }
    }

    private fun buildDownloadRequest(
        path: String,
        range: String?,
        offset: Int? = null,
        limit: Int? = null,
    ): Request {
        val url = "$execdBaseUrl${resolvedPath(DOWNLOAD_PATH)}"
        val urlBuilder =
            url.toHttpUrl().newBuilder()
                .addQueryParameter("path", path)
        if (offset != null) urlBuilder.addQueryParameter("offset", offset.toString())
        if (limit != null) urlBuilder.addQueryParameter("limit", limit.toString())

        val requestBuilder =
            Request.Builder()
                .url(urlBuilder.build())
                .headers(execdEndpoint.headers.toHeaders())
                .get()
        if (range != null) requestBuilder.header("Range", range)
        return requestBuilder.build()
    }
}
