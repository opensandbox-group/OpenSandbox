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
Filesystem service adapter implementation.

Implementation of FilesystemService that adapts openapi-python-client generated FilesystemApi.
This adapter handles file operations within sandboxes using the auto-generated API client.
"""

import json
import logging
from collections.abc import AsyncIterator
from io import IOBase, TextIOBase
from typing import TypedDict
from urllib.parse import quote

import httpx

from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.filesystem_model_converter import (
    FilesystemModelConverter,
)
from opensandbox.adapters.converter.response_handler import (
    extract_request_id,
    handle_api_error,
)
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    EntryInfo,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.services.filesystem import Filesystem

logger = logging.getLogger(__name__)

class _DownloadRequest(TypedDict):
    url: str
    params: dict[str, str] | None
    headers: dict[str, str]


class FilesystemAdapter(Filesystem):
    """
    Implementation of FilesystemService that provides comprehensive file system operations.

    This adapter handles file operations within sandboxes using optimized approaches
    for different operation types - API calls for standard operations and direct HTTP
    for file upload/download operations requiring special handling.

    All HTTP clients created by this adapter share `ConnectionConfig.transport`.
    """

    FILESYSTEM_UPLOAD_PATH = "/files/upload"
    FILESYSTEM_DOWNLOAD_PATH = "/files/download"

    def __init__(
        self, connection_config: ConnectionConfig, execd_endpoint: SandboxEndpoint
    ) -> None:
        """
        Initialize the filesystem service adapter.

        Args:
            connection_config: Connection configuration (shared transport, headers, timeouts)
            execd_endpoint: Execd endpoint information for direct HTTP calls
        """
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint
        from opensandbox.api.execd import Client

        base_url = self._get_execd_base_url()
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.execd_endpoint.headers,
        }

        self._httpx_client = httpx.AsyncClient(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )

        # Execd API does not require authentication
        self._client = Client(
            base_url=base_url,
            timeout=timeout,
        )
        self._client.set_async_httpx_client(self._httpx_client)

    def _get_execd_base_url(self) -> str:
        protocol = self.connection_config.protocol
        return f"{protocol}://{self.execd_endpoint.endpoint}"

    async def _get_httpx_client(self) -> httpx.AsyncClient:
        """Return adapter-owned httpx client for execd (no auth required)."""
        return self._httpx_client

    async def _get_client(self):
        """Return the client for execd API (no auth required)."""
        return self._client

    def _get_execd_url(self, path: str) -> str:
        """Build URL for execd endpoint."""
        protocol = self.connection_config.protocol
        return f"{protocol}://{self.execd_endpoint.endpoint}{path}"

    async def read_file(
        self,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
    ) -> str:
        """Read file content as string via HTTP API."""
        content = await self.read_bytes(path, range_header=range_header)
        return content.decode(encoding)

    async def read_bytes(
        self,
        path: str,
        *,
        range_header: str | None = None,
    ) -> bytes:
        """Read file content as bytes with support for range requests.

        Args:
            path: Path to the file to read
            range_header: Optional range header for partial content requests

        Returns:
            File content as bytes

        Raises:
            SandboxApiException: If the read operation fails
        """
        logger.debug(f"Reading file as bytes: {path}")
        try:
            request_data = self._build_download_request(path, range_header)
            client = await self._get_httpx_client()

            if request_data["params"] is None:
                response = await client.get(
                    request_data["url"],
                    headers=request_data["headers"],
                )
            else:
                response = await client.get(
                    request_data["url"],
                    headers=request_data["headers"],
                    params=request_data["params"],
                )
            response.raise_for_status()
            return response.content
        except Exception as e:
            logger.error(f"Failed to read file {path}", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def read_bytes_stream(
            self,
            path: str,
            *,
            chunk_size: int = 64 * 1024,
            range_header: str | None = None,
    ) -> AsyncIterator[bytes]:
        """Stream file content as bytes chunks via HTTP (true streaming)."""
        logger.debug(f"Streaming file as bytes: {path} (chunk_size={chunk_size})")
        try:
            request_data = self._build_download_request(path, range_header)
            client = await self._get_httpx_client()

            url = request_data["url"]
            params = request_data["params"]
            headers = request_data["headers"]

            if params is None:
                request = client.build_request("GET", url, headers=headers)
            else:
                request = client.build_request(
                    "GET",
                    url,
                    headers=headers,
                    params=params,
                )

            response = await client.send(request, stream=True)

            if response.status_code >= 300:
                try:
                    await response.aread()
                finally:
                    await response.aclose()

                raise SandboxApiException(
                    f"Failed to stream file {path}: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
            return response.aiter_bytes(chunk_size=chunk_size)
        except Exception as e:
            logger.error(f"Failed to stream file {path}", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def write_files(self, entries: list[WriteEntry]) -> None:
        """Write multiple files in a single operation using multipart upload.

        Aligned with Kotlin SDK implementation.
        """
        if not entries:
            return

        logger.debug(f"Writing {len(entries)} files")

        try:
            client = await self._get_httpx_client()
            multipart_parts = []

            for entry in entries:
                if not entry.path:
                    raise InvalidArgumentException("File path cannot be null")
                if entry.data is None:
                    raise InvalidArgumentException("File data cannot be null")

                metadata = {
                    "path": entry.path,
                    "owner": entry.owner,
                    "group": entry.group,
                    "mode": entry.mode,
                }
                metadata_json = json.dumps(metadata)

                multipart_parts.append(
                    ("metadata", ("metadata", metadata_json, "application/json"))
                )

                content: bytes | str | IOBase
                content_type: str

                if isinstance(entry.data, bytes):
                    content = entry.data
                    content_type = "application/octet-stream"

                elif isinstance(entry.data, str):
                    encoding = entry.encoding or "utf-8"
                    content = entry.data
                    content_type = f"text/plain; charset={encoding}"

                elif isinstance(entry.data, IOBase):
                    if isinstance(entry.data, TextIOBase):
                        raise InvalidArgumentException(
                            "File stream must be binary (opened with 'rb'). Text streams are not supported."
                        )
                    else:
                        content = entry.data
                        content_type = "application/octet-stream"
                else:
                    raise InvalidArgumentException(
                        f"Unsupported file data type: {type(entry.data)}"
                    )
                multipart_parts.append(("file", (entry.path, content, content_type)))

            url = self._get_execd_url(self.FILESYSTEM_UPLOAD_PATH)
            response = await client.post(url, files=multipart_parts)
            response.raise_for_status()
        except Exception as e:
            logger.error(f"Failed to write {len(entries)} files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def write_file(
        self,
        path: str,
        data: str | bytes | IOBase,
        *,
        encoding: str = "utf-8",
        mode: int = 755,
        owner: str | None = None,
        group: str | None = None,
    ) -> None:
        """Write single file (convenience method)."""
        entry = WriteEntry(
            path=path,
            data=data,
            mode=mode,
            owner=owner,
            group=group,
            encoding=encoding,
        )
        await self.write_files([entry])

    async def create_directories(self, entries: list[WriteEntry]) -> None:
        """Create multiple directories with specified permissions.

        Args:
            entries: List of directory entries with paths and permissions

        Raises:
            SandboxException: If directory creation fails
        """
        try:
            from opensandbox.api.execd.api.filesystem import make_dirs

            client = await self._get_client()
            response_obj = await make_dirs.asyncio_detailed(
                client=client,
                body=FilesystemModelConverter.to_api_make_dirs_body(entries),
            )

            handle_api_error(response_obj, "Create directories")

        except Exception as e:
            logger.error("Failed to create directories", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_files(self, paths: list[str]) -> None:
        """Delete files using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import remove_files

            client = await self._get_client()
            response_obj = await remove_files.asyncio_detailed(
                client=client,
                path=paths,
            )

            handle_api_error(response_obj, "Delete files")

        except Exception as e:
            logger.error(f"Failed to delete {len(paths)} files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_directories(self, paths: list[str]) -> None:
        """Delete directories using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import remove_dirs

            client = await self._get_client()
            response_obj = await remove_dirs.asyncio_detailed(
                client=client,
                path=paths,
            )

            handle_api_error(response_obj, "Delete directories")

        except Exception as e:
            logger.error(f"Failed to delete {len(paths)} directories", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def move_files(self, entries: list[MoveEntry]) -> None:
        """Move or rename multiple files and directories.

        Args:
            entries: List of move operations with source and destination paths

        Raises:
            SandboxException: If move operations fail
        """
        try:
            from opensandbox.api.execd.api.filesystem import rename_files
            rename_items = FilesystemModelConverter.to_api_rename_file_items(entries)

            client = await self._get_client()
            response_obj = await rename_files.asyncio_detailed(
                client=client,
                body=rename_items,
            )

            handle_api_error(response_obj, "Move files")

        except Exception as e:
            logger.error("Failed to move files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def set_permissions(self, entries: list[SetPermissionEntry]) -> None:
        """Set file permissions using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import chmod_files

            client = await self._get_client()
            response_obj = await chmod_files.asyncio_detailed(
                client=client,
                body=FilesystemModelConverter.to_api_chmod_files_body(entries),
            )

            handle_api_error(response_obj, "Set permissions")

        except Exception as e:
            logger.error("Failed to set permissions", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def replace_contents(self, entries: list[ContentReplaceEntry]) -> None:
        """Replace file contents using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import replace_content

            client = await self._get_client()
            response_obj = await replace_content.asyncio_detailed(
                client=client,
                body=FilesystemModelConverter.to_api_replace_content_body(entries),
            )

            handle_api_error(response_obj, "Replace contents")

        except Exception as e:
            logger.error("Failed to replace contents", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def search(self, entry: SearchEntry) -> list[EntryInfo]:
        """Search files using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import search_files
            from opensandbox.api.execd.models import FileInfo

            client = await self._get_client()
            response_obj = await search_files.asyncio_detailed(
                client=client,
                path=entry.path,
                pattern=entry.pattern,
            )

            handle_api_error(response_obj, "Search files")

            parsed = response_obj.parsed
            if not parsed:
                return []

            if isinstance(parsed, list) and all(isinstance(x, FileInfo) for x in parsed):
                return FilesystemModelConverter.to_entry_info_list(parsed)
            raise SandboxApiException(
                message="Search files failed: unexpected response type",
                request_id=extract_request_id(getattr(response_obj, "headers", None)),
            )

        except Exception as e:
            logger.error("Failed to search files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]:
        """Get file information using auto-generated API."""
        try:
            from opensandbox.api.execd.api.filesystem import get_files_info

            client = await self._get_client()
            response_obj = await get_files_info.asyncio_detailed(
                client=client,
                path=paths,
            )

            handle_api_error(response_obj, "Get file info")

            if not response_obj.parsed:
                return {}

            return FilesystemModelConverter.to_entry_info_map(response_obj.parsed)

        except Exception as e:
            logger.error(f"Failed to get file info for {len(paths)} paths", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _build_download_request(
            self, path: str, range_header: str | None = None
    ) -> _DownloadRequest:
        """Build HTTP request for file download operations.

        Args:
            path: File path to download
            range_header: Optional range header for partial downloads

        Returns:
            Dictionary containing URL, parameters, and headers for the request
        """
        encoded_path = quote(path, safe="/")
        url = f"{self._get_execd_url(self.FILESYSTEM_DOWNLOAD_PATH)}?path={encoded_path}"
        headers: dict[str, str] = {}

        if range_header:
            headers["Range"] = range_header

        return {
            "url": url,
            "params": None,
            "headers": headers,
        }
