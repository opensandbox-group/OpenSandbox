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

package com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem

import java.time.OffsetDateTime

/**
 * Metadata information for a file or directory entry.
 *
 * Contains complete filesystem metadata including path, permissions, ownership,
 * size, and timestamp information for files and directories in the sandbox.
 *
 * @property path Absolute path of the file or directory
 * @property mode Unix file mode/permissions as integer (e.g., 644 for rw-r--r--)
 * @property owner Owner username of the file or directory
 * @property group Group name of the file or directory
 * @property size Size of the file in bytes (0 for directories)
 * @property modifiedAt Timestamp when the entry was last modified
 * @property createdAt Timestamp when the entry was created
 */
class EntryInfo(
    val path: String,
    val mode: Int,
    val owner: String,
    val group: String,
    val size: Long,
    val modifiedAt: OffsetDateTime,
    val createdAt: OffsetDateTime,
)

/**
 * Request to write content to a file.
 *
 * Creates or overwrites a file with the specified content, permissions, and ownership.
 * Supports both text and binary data through flexible data parameter.
 *
 * @property path Destination file path where content will be written
 * @property data Content to write - can be String or ByteArray
 * @property mode Unix file permissions as integer (default: 755)
 * @property owner Owner username to set (null to use default in sandbox)
 * @property group Group name to set (null to use default in sandbox)
 * @property encoding Character encoding for String data (default: UTF-8)
 */
class WriteEntry private constructor(
    val path: String,
    val data: Any?,
    val mode: Int,
    val owner: String?,
    val group: String?,
    val encoding: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var path: String? = null
        private var data: Any? = null
        private var mode: Int = 755
        private var owner: String? = null
        private var group: String? = null
        private var encoding: String = "UTF-8"

        fun path(path: String): Builder {
            require(path.isNotBlank()) { "Path cannot be blank" }
            this.path = path
            return this
        }

        fun data(data: Any): Builder {
            this.data = data
            return this
        }

        fun mode(mode: Int): Builder {
            require(mode >= 0) { "Mode must be non-negative" }
            this.mode = mode
            return this
        }

        fun owner(owner: String?): Builder {
            this.owner = owner
            return this
        }

        fun group(group: String?): Builder {
            this.group = group
            return this
        }

        fun encoding(encoding: String): Builder {
            require(encoding.isNotBlank()) { "Encoding cannot be blank" }
            this.encoding = encoding
            return this
        }

        fun build(): WriteEntry {
            return WriteEntry(
                path = path ?: throw IllegalArgumentException("Path must be specified"),
                data = data,
                mode = mode,
                owner = owner,
                group = group,
                encoding = encoding,
            )
        }
    }
}

/**
 * Request to move/rename a file or directory.
 *
 * Moves a file or directory from one location to another within the sandbox filesystem.
 * Can be used for both renaming (same directory) and moving (different directory).
 *
 * @property src Source path of the file or directory to move
 * @property dest Destination path where the file or directory should be moved
 */
class MoveEntry private constructor(
    val src: String,
    val dest: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var src: String? = null
        private var dest: String? = null

        fun src(src: String): Builder {
            require(src.isNotBlank()) { "Source path cannot be blank" }
            this.src = src
            return this
        }

        fun dest(dest: String): Builder {
            require(dest.isNotBlank()) { "Destination path cannot be blank" }
            this.dest = dest
            return this
        }

        fun build(): MoveEntry {
            val srcValue = src ?: throw IllegalArgumentException("Source path must be specified")
            val destValue = dest ?: throw IllegalArgumentException("Destination path must be specified")
            return MoveEntry(
                src = srcValue,
                dest = destValue,
            )
        }
    }
}

/**
 * Request to set permissions/ownership of a file or directory.
 *
 * Updates the permissions and/or ownership of an existing file or directory
 * without modifying its content. Only specified properties will be changed.
 *
 * @property path Target path of the file or directory to modify
 * @property owner New owner username (null to keep current owner)
 * @property group New group name (null to keep current group)
 * @property mode New Unix file permissions as integer (default: 755)
 */
class SetPermissionEntry private constructor(
    val path: String,
    val owner: String?,
    val group: String?,
    val mode: Int,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var path: String? = null
        private var owner: String? = null
        private var group: String? = null
        private var mode: Int = 755

        fun path(path: String): Builder {
            require(path.isNotBlank()) { "Path cannot be blank" }
            this.path = path
            return this
        }

        fun owner(owner: String?): Builder {
            this.owner = owner
            return this
        }

        fun group(group: String?): Builder {
            this.group = group
            return this
        }

        fun mode(mode: Int): Builder {
            require(mode >= 0) { "Mode must be non-negative" }
            this.mode = mode
            return this
        }

        fun build(): SetPermissionEntry {
            val pathValue = path ?: throw IllegalArgumentException("Path must be specified")
            return SetPermissionEntry(
                path = pathValue,
                owner = owner,
                group = group,
                mode = mode,
            )
        }
    }
}

/**
 * Request to replace content within a file.
 *
 * Performs string replacement within a file by finding exact matches of the old content
 * and replacing them with new content. Only affects string matches, preserving the rest.
 *
 * @property path Target file path containing content to replace
 * @property oldContent Exact string content to find and replace
 * @property newContent Replacement string content to substitute
 */
class ContentReplaceEntry private constructor(
    val path: String,
    val oldContent: String,
    val newContent: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var path: String? = null
        private var oldContent: String? = null
        private var newContent: String? = null

        fun path(path: String): Builder {
            require(path.isNotBlank()) { "Path cannot be blank" }
            this.path = path
            return this
        }

        fun oldContent(oldContent: String): Builder {
            this.oldContent = oldContent
            return this
        }

        fun newContent(newContent: String): Builder {
            this.newContent = newContent
            return this
        }

        fun build(): ContentReplaceEntry {
            val pathValue = path ?: throw IllegalArgumentException("Path must be specified")
            val oldContentValue = oldContent ?: throw IllegalArgumentException("Old content must be specified")
            val newContentValue = newContent ?: throw IllegalArgumentException("New content must be specified")
            return ContentReplaceEntry(
                path = pathValue,
                oldContent = oldContentValue,
                newContent = newContentValue,
            )
        }
    }
}

/**
 * Result of a content replacement operation on a single file.
 *
 * @property path File path where replacement was performed
 * @property replacedCount Number of occurrences replaced. 0 means old content was not found.
 */
data class ContentReplaceResult(
    val path: String,
    val replacedCount: Int,
)

/**
 * Request to search for files matching a pattern.
 *
 * Searches the filesystem starting from the specified path to find files
 * that match the given pattern. Used for file discovery and filtering.
 *
 * @property path Starting directory path for the search
 * @property pattern Search pattern (supports glob patterns like *.kt, *.txt)
 */
class SearchEntry private constructor(
    val path: String,
    val pattern: String,
) {
    companion object {
        @JvmStatic
        fun builder(): Builder = Builder()
    }

    class Builder {
        private var path: String? = null
        private var pattern: String? = null

        fun path(path: String): Builder {
            require(path.isNotBlank()) { "Path cannot be blank" }
            this.path = path
            return this
        }

        fun pattern(pattern: String): Builder {
            require(pattern.isNotBlank()) { "Pattern cannot be blank" }
            this.pattern = pattern
            return this
        }

        fun build(): SearchEntry {
            val pathValue = path ?: throw IllegalArgumentException("Path must be specified")
            val patternValue = pattern ?: throw IllegalArgumentException("Pattern must be specified")
            return SearchEntry(
                path = pathValue,
                pattern = patternValue,
            )
        }
    }
}
