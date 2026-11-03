# Copyright 2026 The OpenSandbox Authors
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

"""Type-check the public download cleanup examples without a live sandbox."""

from collections.abc import AsyncGenerator, Iterator
from contextlib import aclosing
from io import IOBase
from typing import Protocol

from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
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
from opensandbox.services import DownloadFilesystem, Filesystem
from opensandbox.sync.services import DownloadFilesystemSync, FilesystemSync


async def close_download(
    files: Filesystem | FilesystemAdapter | IsolatedFilesystemAdapter,
) -> None:
    chunks = await files.read_bytes_stream("/tmp/large.bin")
    await chunks.aclose()

    async with aclosing(await files.read_bytes_stream("/tmp/large.bin")) as chunks:
        async for _chunk in chunks:
            break


async def close_detailed_download(files: Filesystem) -> None:
    if isinstance(files, DownloadFilesystem):
        download = await files.read_bytes_stream_detailed("/tmp/large.bin")
        async with download.body:
            async for _chunk in download.body:
                break


def close_detailed_download_sync(files: FilesystemSync) -> None:
    if isinstance(files, DownloadFilesystemSync):
        with files.read_bytes_stream_detailed("/tmp/large.bin").body as chunks:
            for _chunk in chunks:
                break


# Preserve the pre-metadata structural contracts to catch required-method regressions.
class _LegacyAsyncFilesystem(Protocol):
    async def read_file(
        self,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> str: ...

    async def read_bytes(
        self,
        path: str,
        *,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> bytes: ...

    async def read_bytes_stream(
        self,
        path: str,
        *,
        chunk_size: int = 64 * 1024,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> AsyncGenerator[bytes, None]: ...

    async def write_files(self, entries: list[WriteEntry]) -> None: ...

    async def write_file(
        self,
        path: str,
        data: str | bytes | IOBase,
        *,
        encoding: str = "utf-8",
        mode: int = 755,
        owner: str | None = None,
        group: str | None = None,
    ) -> None: ...

    async def create_directories(self, entries: list[WriteEntry]) -> None: ...

    async def delete_files(self, paths: list[str]) -> None: ...

    async def delete_directories(self, paths: list[str]) -> None: ...

    async def move_files(self, entries: list[MoveEntry]) -> None: ...

    async def set_permissions(self, entries: list[SetPermissionEntry]) -> None: ...

    async def replace_contents(self, entries: list[ContentReplaceEntry]) -> None: ...

    async def replace_contents_detailed(
        self, entries: list[ContentReplaceEntry]
    ) -> list[ContentReplaceResult]: ...

    async def search(self, entry: SearchEntry) -> list[EntryInfo]: ...

    async def list_directory(self, entry: DirectoryListEntry) -> list[EntryInfo]: ...

    async def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]: ...


class _LegacySyncFilesystem(Protocol):
    def read_file(
        self,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> str: ...

    def read_bytes(
        self,
        path: str,
        *,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> bytes: ...

    def read_bytes_stream(
        self,
        path: str,
        *,
        chunk_size: int = 64 * 1024,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> Iterator[bytes]: ...

    def write_files(self, entries: list[WriteEntry]) -> None: ...

    def write_file(
        self,
        path: str,
        data: str | bytes | IOBase,
        *,
        encoding: str = "utf-8",
        mode: int = 755,
        owner: str | None = None,
        group: str | None = None,
    ) -> None: ...

    def create_directories(self, entries: list[WriteEntry]) -> None: ...

    def delete_files(self, paths: list[str]) -> None: ...

    def delete_directories(self, paths: list[str]) -> None: ...

    def move_files(self, entries: list[MoveEntry]) -> None: ...

    def set_permissions(self, entries: list[SetPermissionEntry]) -> None: ...

    def replace_contents(self, entries: list[ContentReplaceEntry]) -> None: ...

    def replace_contents_detailed(
        self, entries: list[ContentReplaceEntry]
    ) -> list[ContentReplaceResult]: ...

    def search(self, entry: SearchEntry) -> list[EntryInfo]: ...

    def list_directory(self, entry: DirectoryListEntry) -> list[EntryInfo]: ...

    def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]: ...


def accept_legacy_adapters(
    async_files: _LegacyAsyncFilesystem,
    sync_files: _LegacySyncFilesystem,
) -> tuple[Filesystem, FilesystemSync]:
    return async_files, sync_files
