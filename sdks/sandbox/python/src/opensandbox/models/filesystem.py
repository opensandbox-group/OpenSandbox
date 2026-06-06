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
Filesystem-related data models.

Models for file operations, directory listings, and filesystem metadata.
"""

from datetime import datetime
from io import IOBase

from pydantic import BaseModel, ConfigDict, Field, field_validator


class EntryInfo(BaseModel):
    """
    Metadata information for a file or directory entry.

    Contains complete filesystem metadata including path, permissions, ownership,
    size, and timestamp information for files and directories in the sandbox.
    """

    path: str = Field(description="Absolute path of the file or directory")
    mode: int = Field(description="Unix file mode/permissions as integer (e.g., 644)")
    owner: str = Field(description="Owner username of the file or directory")
    group: str = Field(description="Group name of the file or directory")
    size: int = Field(description="Size of the file in bytes (0 for directories)")
    modified_at: datetime = Field(
        description="Timestamp when entry was last modified", alias="modified_at"
    )
    created_at: datetime = Field(
        description="Timestamp when entry was created", alias="created_at"
    )

    model_config = ConfigDict(populate_by_name=True)


class WriteEntry(BaseModel):
    """
    Request to write content to a file.

    Creates or overwrites a file with the specified content, permissions, and ownership.
    Supports both text and binary data through flexible data parameter.
    """

    path: str = Field(description="Destination file path where content will be written")
    data: str | bytes | IOBase | None = Field(
        default=None, description="Content to write - can be str or bytes"
    )
    mode: int = Field(default=755, description="Unix file permissions as integer")
    owner: str | None = Field(default=None, description="Owner username to set")
    group: str | None = Field(default=None, description="Group name to set")
    encoding: str = Field(
        default="utf-8", description="Character encoding for string data"
    )
    model_config = ConfigDict(arbitrary_types_allowed=True)

    @field_validator("path")
    @classmethod
    def path_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Path cannot be blank")
        return v

    @field_validator("mode")
    @classmethod
    def mode_must_be_non_negative(cls, v: int) -> int:
        if v < 0:
            raise ValueError("Mode must be non-negative")
        return v

    @field_validator("encoding")
    @classmethod
    def encoding_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Encoding cannot be blank")
        return v


class MoveEntry(BaseModel):
    """
    Request to move/rename a file or directory.

    Moves a file or directory from one location to another within the sandbox filesystem.
    Can be used for both renaming (same directory) and moving (different directory).
    """

    src: str = Field(
        description="Source path of the file or directory to move", alias="source"
    )
    dest: str = Field(
        description="Destination path where the file or directory should be moved",
        alias="destination",
    )

    @field_validator("src")
    @classmethod
    def src_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Source path cannot be blank")
        return v

    @field_validator("dest")
    @classmethod
    def dest_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Destination path cannot be blank")
        return v

    model_config = ConfigDict(populate_by_name=True)


class SetPermissionEntry(BaseModel):
    """
    Request to set permissions/ownership of a file or directory.

    Updates the permissions and/or ownership of an existing file or directory
    without modifying its content. Only specified properties will be changed.
    """

    path: str = Field(description="Target path of the file or directory to modify")
    owner: str | None = Field(default=None, description="New owner username")
    group: str | None = Field(default=None, description="New group name")
    mode: int = Field(default=755, description="New Unix file permissions as integer")

    @field_validator("path")
    @classmethod
    def path_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Path cannot be blank")
        return v

    @field_validator("mode")
    @classmethod
    def mode_must_be_non_negative(cls, v: int) -> int:
        if v < 0:
            raise ValueError("Mode must be non-negative")
        return v


class ContentReplaceEntry(BaseModel):
    """
    Request to replace content within a file.

    Performs string replacement within a file by finding exact matches of the old content
    and replacing them with new content. Only affects string matches, preserving the rest.
    """

    path: str = Field(description="Target file path containing content to replace")
    old_content: str = Field(
        description="Exact string content to find and replace", alias="old_content"
    )
    new_content: str = Field(
        description="Replacement string content to substitute", alias="new_content"
    )

    @field_validator("path")
    @classmethod
    def path_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Path cannot be blank")
        return v

    model_config = ConfigDict(populate_by_name=True)


class ContentReplaceResult(BaseModel):
    """
    Result of a content replacement operation on a single file.
    """

    path: str = Field(description="File path where replacement was performed")
    replaced_count: int = Field(description="Number of occurrences replaced. 0 means old content was not found.")


class SearchEntry(BaseModel):
    """
    Request to search for files matching a pattern.

    Searches the filesystem starting from the specified path to find files
    that match the given pattern. Used for file discovery and filtering.
    """

    path: str = Field(description="Starting directory path for the search")
    pattern: str = Field(
        description="Search pattern (supports glob patterns like *.py, *.txt)"
    )

    @field_validator("path")
    @classmethod
    def path_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Path cannot be blank")
        return v

    @field_validator("pattern")
    @classmethod
    def pattern_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Pattern cannot be blank")
        return v
