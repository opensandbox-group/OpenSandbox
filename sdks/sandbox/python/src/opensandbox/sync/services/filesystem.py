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
Synchronous filesystem service interface.

Defines the contract for **blocking** filesystem operations inside a sandbox.
This is the sync counterpart of :mod:`opensandbox.services.filesystem`.
"""

from collections.abc import Iterator
from io import IOBase
from typing import Protocol

from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    ContentReplaceResult,
    DirectoryListEntry,
    EntryInfo,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)


class FilesystemSync(Protocol):
    """
    Filesystem operations service for sandbox environments (sync).

    This service provides comprehensive file system management capabilities within sandbox
    environments, including file operations, directory management, and metadata handling.

    Notes:
        - All methods are **blocking**.
        - Paths may be absolute or relative to the sandbox working directory (server-defined).
    """

    def read_file(
        self,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> str:
        """
        Read the content of a file as a string with specified encoding.

        Args:
            path: The absolute or relative path to the file to read.
            encoding: Character encoding for the file content (default: UTF-8).
            range_header: HTTP byte range to read (e.g., "bytes=0-1023").
                Mutually exclusive with offset/limit.
            offset: Starting line number (1-based) for line-based reading.
                Mutually exclusive with range_header.
            limit: Number of lines to return for line-based reading.
                Mutually exclusive with range_header.

        Returns:
            The file content as a string.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def read_bytes(
        self,
        path: str,
        *,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> bytes:
        """
        Read the content of a file as bytes.

        Args:
            path: The absolute or relative path to the file to read.
            range_header: HTTP byte range to read (e.g., "bytes=0-1023").
                Mutually exclusive with offset/limit.
            offset: Starting line number (1-based) for line-based reading.
                Mutually exclusive with range_header.
            limit: Number of lines to return for line-based reading.
                Mutually exclusive with range_header.

        Returns:
            The file content as bytes.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def read_bytes_stream(
        self,
        path: str,
        *,
        chunk_size: int = 64 * 1024,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> Iterator[bytes]:
        """
        Stream file content as bytes chunks (blocking iterator).

        Args:
            path: File path to read.
            chunk_size: Chunk size in bytes (default: 64KiB).
            range_header: Optional HTTP range header.

        Yields:
            Byte chunks from the file.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def write_files(self, entries: list[WriteEntry]) -> None:
        """
        Write content to files based on the provided write entries.

        Args:
            entries: List of WriteEntry objects specifying files to write and their content.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def write_file(
        self,
        path: str,
        data: str | bytes | IOBase,
        *,
        encoding: str = "utf-8",
        mode: int = 755,
        owner: str | None = None,
        group: str | None = None,
    ) -> None:
        """
        Write content to a single file (convenience method).

        Args:
            path: Destination file path.
            data: Content to write (str/bytes/file-like).
            encoding: Character encoding (when data is str).
            mode: Unix file permissions (implementation-defined).
            owner: Owner username.
            group: Group name.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def create_directories(self, entries: list[WriteEntry]) -> None:
        """
        Create directories based on the provided entries.

        Args:
            entries: List of WriteEntry objects specifying directories to create.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def delete_files(self, paths: list[str]) -> None:
        """
        Delete the specified files.

        Args:
            paths: List of file paths to delete.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def delete_directories(self, paths: list[str]) -> None:
        """
        Delete the specified directories.

        Args:
            paths: List of directory paths to delete.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def move_files(self, entries: list[MoveEntry]) -> None:
        """
        Move files from source to destination paths.

        Args:
            entries: List of MoveEntry objects specifying source and destination paths.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def set_permissions(self, entries: list[SetPermissionEntry]) -> None:
        """
        Set file system permissions for the specified entries.

        Args:
            entries: List of SetPermissionEntry objects specifying files and their new permissions.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def replace_contents(self, entries: list[ContentReplaceEntry]) -> None:
        """
        Replace content in files based on search and replace patterns.

        Args:
            entries: List of ContentReplaceEntry objects specifying replacement operations.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def replace_contents_detailed(self, entries: list[ContentReplaceEntry]) -> list[ContentReplaceResult]:
        """
        Replace content in files and return per-file replacement counts.

        Args:
            entries: List of ContentReplaceEntry objects specifying replacement operations.

        Returns:
            List of ContentReplaceResult with replacement counts per file.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def search(self, entry: SearchEntry) -> list[EntryInfo]:
        """
        Search for files and directories based on the specified criteria.

        Args:
            entry: SearchEntry object containing search parameters and criteria.

        Returns:
            List of EntryInfo objects containing metadata for matching files/directories.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def list_directory(self, entry: DirectoryListEntry) -> list[EntryInfo]:
        """
        List directory contents with optional depth control.

        Args:
            entry: DirectoryListEntry object containing path and optional depth.

        Returns:
            List of EntryInfo objects containing metadata for directory entries.

        Raises:
            SandboxException: If the operation fails.
        """
        ...

    def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]:
        """
        Retrieve file information for the specified paths.

        Args:
            paths: List of file/directory paths to get information for.

        Returns:
            Mapping where keys are paths and values are EntryInfo objects containing metadata.

        Raises:
            SandboxException: If the operation fails.
        """
        ...
