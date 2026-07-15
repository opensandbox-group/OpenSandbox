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

import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.ContentReplaceEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.EntryInfo
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.MoveEntry
import com.alibaba.opensandbox.sandbox.domain.models.execd.filesystem.SetPermissionEntry
import com.alibaba.opensandbox.sandbox.api.models.execd.FileInfo as ApiFileInfo
import com.alibaba.opensandbox.sandbox.api.models.execd.Permission as ApiPermission
import com.alibaba.opensandbox.sandbox.api.models.execd.RenameFileItem as ApiRenameFileItem
import com.alibaba.opensandbox.sandbox.api.models.execd.ReplaceFileContentItem as ApiReplaceFileContentItem

/**
 * Converter between domain models and API models for filesystem operations.
 *
 * @author ninan
 * @since 2025/12/2
 */
object FilesystemConverter {
    /**
     * Converts API FileInfo to domain EntryInfo.
     */
    fun ApiFileInfo.toEntryInfo(): EntryInfo {
        return EntryInfo(
            path = this.path,
            mode = this.mode,
            owner = this.owner,
            group = this.group,
            createdAt = this.createdAt,
            modifiedAt = this.modifiedAt,
            size = this.propertySize,
            type = this.type?.value,
        )
    }

    /**
     * Converts domain SetPermissionEntry to API Permission.
     */
    fun SetPermissionEntry.toApiPermission(): ApiPermission {
        return ApiPermission(
            owner = this.owner,
            group = this.group,
            mode = this.mode,
        )
    }

    /**
     * Converts domain MoveEntry to API RenameFileItem.
     */
    fun MoveEntry.toApiRenameFileItem(): ApiRenameFileItem {
        return ApiRenameFileItem(
            src = this.src,
            dest = this.dest,
        )
    }

    /**
     * Converts domain ContentReplaceEntry to API ReplaceFileContentItem.
     */
    fun ContentReplaceEntry.toApiReplaceFileContentItem(): ApiReplaceFileContentItem {
        return ApiReplaceFileContentItem(
            old = this.oldContent,
            new = this.newContent,
        )
    }

    /**
     * Converts list of domain MoveEntry to list of API RenameFileItem.
     */
    fun List<MoveEntry>.toApiRenameFileItems(): List<ApiRenameFileItem> {
        return this.map { it.toApiRenameFileItem() }
    }

    /**
     * Converts list of domain SetPermissionEntry to map of path to API Permission.
     */
    fun List<SetPermissionEntry>.toApiPermissionMap(): Map<String, ApiPermission> {
        return this.associate { entry ->
            entry.path to entry.toApiPermission()
        }
    }

    /**
     * Converts list of domain ContentReplaceEntry to map of path to API ReplaceFileContentItem.
     */
    fun List<ContentReplaceEntry>.toApiReplaceFileContentMap(): Map<String, ApiReplaceFileContentItem> {
        return this.associate { entry ->
            entry.path to entry.toApiReplaceFileContentItem()
        }
    }

    /**
     * Converts map of path to API FileInfo to map of path to domain EntryInfo.
     */
    fun Map<String, ApiFileInfo>.toEntryInfoMap(): Map<String, EntryInfo> {
        return this.mapValues { (_, apiFileInfo) ->
            apiFileInfo.toEntryInfo()
        }
    }
}
