/*
 * Copyright 2025 Alibaba Group Holding Ltd.
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

package com.alibaba.opensandbox.sandbox.domain.services

import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceResult
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.EntryInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.MoveEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SearchEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SetPermissionEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.WriteEntry
import java.io.InputStream
import java.util.Collections

/**
 * Filesystem operations for sandbox environments.
 *
 * This service provides comprehensive file system management capabilities
 * within sandbox environments, including file operations, directory management,
 * and metadata handling with proper security controls.
 */
interface Filesystem {
    /**
     * Reads the content of a file as a string with specified encoding.
     *
     * @param path The absolute or relative path to the file to read
     * @param encoding Character encoding for the file content (default: UTF-8)
     * @param range HTTP byte range to read (e.g., "bytes=0-1023"). Mutually exclusive with offset/limit.
     * @param offset Starting line number (1-based) for line-based reading. Mutually exclusive with range.
     * @param limit Number of lines to return for line-based reading. Mutually exclusive with range.
     * @return The file content as a string
     * @throws SandboxException if the operation fails
     */
    fun readFile(
        path: String,
        encoding: String = "UTF-8",
        range: String? = null,
        offset: Int? = null,
        limit: Int? = null,
    ): String

    /**
     * Backward-compatible overload for Java callers using positional args.
     */
    fun readFile(
        path: String,
        encoding: String,
        range: String?,
    ): String {
        return readFile(path, encoding, range, null, null)
    }

    /**
     * Convenience overload for reading a file as a string using UTF-8.
     *
     * Equivalent to: `readFile(path, "UTF-8", null)`
     *
     * @param path The absolute or relative path to the file to read
     * @return The file content as a UTF-8 string
     */
    fun readFile(path: String): String {
        return readFile(path, "UTF-8", null)
    }

    /**
     * Reads the content of a file as a byte array.
     *
     * @param path The absolute or relative path to the file to read
     * @param range HTTP byte range to read (e.g., "bytes=0-1023"). Mutually exclusive with offset/limit.
     * @param offset Starting line number (1-based) for line-based reading. Mutually exclusive with range.
     * @param limit Number of lines to return for line-based reading. Mutually exclusive with range.
     * @return The file content as a byte array
     * @throws SandboxException if the operation fails
     */
    fun readByteArray(
        path: String,
        range: String? = null,
        offset: Int? = null,
        limit: Int? = null,
    ): ByteArray

    /**
     * Backward-compatible overload for Java callers using positional args.
     */
    fun readByteArray(
        path: String,
        range: String?,
    ): ByteArray {
        return readByteArray(path, range, null, null)
    }

    /**
     * Convenience overload for reading a file as a byte array.
     *
     * Equivalent to: `readByteArray(path, null)`
     *
     * @param path The absolute or relative path to the file to read
     * @return The full file content as a byte array
     */
    fun readByteArray(path: String): ByteArray {
        return readByteArray(path, null)
    }

    /**
     * Opens a file for reading as an InputStream.
     *
     * @param path The absolute or relative path to the file to read
     * @param range HTTP byte range to read (e.g., "bytes=0-1023"). Mutually exclusive with offset/limit.
     * @param offset Starting line number (1-based) for line-based reading. Mutually exclusive with range.
     * @param limit Number of lines to return for line-based reading. Mutually exclusive with range.
     * @return An InputStream for reading the file content
     * @throws SandboxException if the operation fails
     */
    fun readStream(
        path: String,
        range: String? = null,
        offset: Int? = null,
        limit: Int? = null,
    ): InputStream

    /**
     * Backward-compatible overload for Java callers using positional args.
     */
    fun readStream(
        path: String,
        range: String?,
    ): InputStream {
        return readStream(path, range, null, null)
    }

    /**
     * Convenience overload for opening a file stream.
     *
     * Equivalent to: `readStream(path, null)`
     *
     * @param path The absolute or relative path to the file to read
     * @return An InputStream for reading the file content
     */
    fun readStream(path: String): InputStream {
        return readStream(path, null)
    }

    /**
     * Writes content to files based on the provided write entries.
     *
     * @param entries List of WriteEntry objects specifying files to write and their content
     * @throws SandboxException if the operation fails
     */
    fun write(entries: List<WriteEntry>)

    /**
     * Writes a single file based on the provided [WriteEntry].
     */
    fun writeFile(entry: WriteEntry) {
        write(Collections.singletonList(entry))
    }

    /**
     * Convenience overload for writing a single text file with custom options.
     */
    fun writeFile(
        path: String,
        data: Any,
    ) {
        writeFile(
            WriteEntry
                .builder()
                .path(path)
                .data(data)
                .build(),
        )
    }

    /**
     * Creates directories based on the provided entries.
     *
     * @param entries List of WriteEntry objects specifying directories to create
     * @throws SandboxException if the operation fails
     */
    fun createDirectories(entries: List<WriteEntry>)

    /**
     * Deletes the specified files.
     *
     * @param paths List of file paths to delete
     * @throws SandboxException if the operation fails
     */
    fun deleteFiles(paths: List<String>)

    /**
     * Deletes the specified directories.
     *
     * @param paths List of directory paths to delete
     * @throws SandboxException if the operation fails
     */
    fun deleteDirectories(paths: List<String>)

    /**
     * Lists directory contents with optional depth control.
     *
     * @param path Directory path to list
     * @param depth Optional maximum child depth to include. `null` lets the
     *   server apply its default (currently 1 — immediate children only).
     *   `0` returns an empty list. Larger values include descendants up to
     *   that many levels below `path`.
     * @return List of EntryInfo objects containing metadata for directory entries
     * @throws SandboxException if the operation fails
     */
    fun listDirectory(
        path: String,
        depth: Int?,
    ): List<EntryInfo>

    /**
     * Java-friendly overload of [listDirectory] that uses the server-side
     * default depth (currently 1). Equivalent to `listDirectory(path, null)`.
     */
    fun listDirectory(path: String): List<EntryInfo> = listDirectory(path, null)

    /**
     * Moves files from source to destination paths.
     *
     * @param entries List of MoveEntry objects specifying source and destination paths
     * @throws SandboxException if the operation fails
     */
    fun moveFiles(entries: List<MoveEntry>)

    /**
     * Sets file system permissions for the specified entries.
     *
     * @param entries List of SetPermissionEntry objects specifying files and their new permissions
     * @throws SandboxException if the operation fails
     */
    fun setPermissions(entries: List<SetPermissionEntry>)

    /**
     * Replaces content in files based on search and replace patterns.
     *
     * @param entries List of ContentReplaceEntry objects specifying replacement operations
     * @throws SandboxException if the operation fails
     */
    fun replaceContents(entries: List<ContentReplaceEntry>)

    /**
     * Replaces content in files and returns per-file replacement counts.
     *
     * @param entries List of ContentReplaceEntry objects specifying replacement operations
     * @return List of ContentReplaceResult with replacement counts per file
     * @throws SandboxException if the operation fails
     */
    fun replaceContentsDetailed(entries: List<ContentReplaceEntry>): List<ContentReplaceResult>

    /**
     * Searches for files and directories based on the specified criteria.
     *
     * @param entry SearchEntry object containing search parameters and criteria
     * @return List of EntryInfo objects containing metadata for matching files/directories
     * @throws SandboxException if the operation fails
     */
    fun search(entry: SearchEntry): List<EntryInfo>

    /**
     * Retrieves file information for the specified paths.
     *
     * @param paths List of file/directory paths to get information for
     * @return Map where keys are file paths and values are EntryInfo objects containing file metadata
     * @throws SandboxException if the operation fails
     */
    fun readFileInfo(paths: List<String>): Map<String, EntryInfo>
}
