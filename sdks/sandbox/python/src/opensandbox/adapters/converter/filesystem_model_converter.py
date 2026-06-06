#
# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
"""
Filesystem model converter utilities.

Provides conversion functions between API models and domain models for filesystem operations,
similar to SandboxModelConverter.

This converter is designed to work with openapi-python-client generated models.
"""

from typing import Any

from opensandbox.api.execd.models import FileInfo
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    ContentReplaceResult,
    EntryInfo,
    MoveEntry,
    SetPermissionEntry,
    WriteEntry,
)


class FilesystemModelConverter:
    """
    Filesystem model converter utilities.

    Provides static methods to convert between API models and domain models
    for filesystem operations, following the pattern from SandboxModelConverter.
    """

    @staticmethod
    def to_entry_info(api_file_info: FileInfo) -> EntryInfo:
        """Convert API FileInfo to domain EntryInfo."""
        return EntryInfo(
            path=api_file_info.path,
            mode=api_file_info.mode,
            owner=api_file_info.owner,
            group=api_file_info.group,
            size=api_file_info.size,
            modified_at=api_file_info.modified_at,
            created_at=api_file_info.created_at,
        )

    @staticmethod
    def to_entry_info_list(api_file_infos: list[FileInfo]) -> list[EntryInfo]:
        """Convert list of API FileInfo to list of domain EntryInfo."""
        if not api_file_infos:
            return []

        return [FilesystemModelConverter.to_entry_info(item) for item in api_file_infos]

    @staticmethod
    def to_entry_info_map(api_response: Any) -> dict[str, EntryInfo]:
        """Convert API response to a map of path to EntryInfo."""
        if not api_response:
            return {}

        result: dict[str, EntryInfo] = {}

        if hasattr(api_response, "additional_properties"):
            for path, info_data in api_response.additional_properties.items():
                if isinstance(info_data, FileInfo):
                    result[path] = FilesystemModelConverter.to_entry_info(info_data)
        elif isinstance(api_response, dict):
            for path, info_data in api_response.items():
                if isinstance(info_data, FileInfo):
                    result[path] = FilesystemModelConverter.to_entry_info(info_data)

        return result

    @staticmethod
    def to_api_make_dirs_body(entries: list[WriteEntry]):
        """Convert directory entries to MakeDirsBody."""
        from opensandbox.api.execd.models.make_dirs_body import MakeDirsBody

        dirs_data = {
            entry.path: {
                "mode": entry.mode,
                "owner": entry.owner,
                "group": entry.group,
            }
            for entry in entries
        }
        return MakeDirsBody.from_dict(dirs_data)

    @staticmethod
    def to_api_chmod_files_body(entries: list[SetPermissionEntry]):
        """Convert permission entries to ChmodFilesBody."""
        from opensandbox.api.execd.models.chmod_files_body import ChmodFilesBody

        permission_data = {
            entry.path: {
                "mode": entry.mode,
                "owner": entry.owner,
                "group": entry.group,
            }
            for entry in entries
        }
        return ChmodFilesBody.from_dict(permission_data)

    @staticmethod
    def to_api_replace_content_body(entries: list[ContentReplaceEntry]):
        """Convert content replacement entries to ReplaceContentBody."""
        from opensandbox.api.execd.models.replace_content_body import ReplaceContentBody

        replace_data = {
            entry.path: {
                # Execd API expects keys "old" and "new" (see execd-api.yaml ReplaceFileContentItem).
                "old": entry.old_content,
                "new": entry.new_content,
            }
            for entry in entries
        }
        return ReplaceContentBody.from_dict(replace_data)

    @staticmethod
    def to_replace_results(api_response: Any) -> list[ContentReplaceResult]:
        """Convert API replace response to list of ContentReplaceResult."""
        if not api_response:
            return []

        results: list[ContentReplaceResult] = []
        items: dict = {}
        if hasattr(api_response, "additional_properties"):
            items = api_response.additional_properties
        elif isinstance(api_response, dict):
            items = api_response

        for path, result_data in items.items():
            if hasattr(result_data, "replaced_count"):
                count = result_data.replaced_count
            elif isinstance(result_data, dict):
                count = result_data.get("replacedCount", 0)
            else:
                count = 0
            results.append(ContentReplaceResult(path=path, replaced_count=count))

        return results

    @staticmethod
    def to_api_rename_file_items(entries: list[MoveEntry]):
        """Convert move entries to list of RenameFileItem."""
        from opensandbox.api.execd.models.rename_file_item import RenameFileItem

        return [RenameFileItem(src=e.src, dest=e.dest) for e in entries]
