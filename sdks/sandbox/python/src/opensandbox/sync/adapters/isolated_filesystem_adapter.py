#
# Copyright 2026 Alibaba Group Holding Ltd.
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
Isolated filesystem adapter implementation (sync).

Uses auto-generated isolated_execution API modules with explicit session_id.
"""

import json
import logging
from collections.abc import Iterator
from io import IOBase, TextIOBase
from uuid import UUID

import httpx

from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.filesystem_model_converter import (
    FilesystemModelConverter,
)
from opensandbox.adapters.converter.response_handler import (
    extract_request_id,
    handle_api_error,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
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
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.services.filesystem import FilesystemSync

logger = logging.getLogger(__name__)


class IsolatedFilesystemAdapterSync(FilesystemSync):
    """
    Sync filesystem adapter for isolated sessions using auto-generated API client.

    Uses generated isolated_execution modules with explicit session_id parameter
    instead of URL prefix composition.
    """

    UPLOAD_PATH = "/v1/isolated/session/{session_id}/files/upload"
    DOWNLOAD_PATH = "/v1/isolated/session/{session_id}/files/download"

    def __init__(
        self,
        connection_config: ConnectionConfigSync,
        execd_endpoint: SandboxEndpoint,
        session_id: str,
    ) -> None:
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint
        self._session_id = session_id
        self._session_uuid = UUID(session_id)

        from opensandbox.api.execd import Client

        protocol = self.connection_config.protocol
        base_url = f"{protocol}://{self.execd_endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.execd_endpoint.headers,
        }

        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )

        self._client = Client(base_url=base_url, timeout=timeout)
        self._client.set_httpx_client(self._httpx_client)

    def _get_url(self, path_template: str) -> str:
        protocol = self.connection_config.protocol
        path = path_template.format(session_id=self._session_id)
        return f"{protocol}://{self.execd_endpoint.endpoint}{path}"

    def read_file(
        self,
        path: str,
        *,
        encoding: str = "utf-8",
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> str:
        content = self.read_bytes(
            path, range_header=range_header, offset=offset, limit=limit
        )
        return content.decode(encoding)

    def read_bytes(
        self,
        path: str,
        *,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> bytes:
        try:
            url = self._get_url(self.DOWNLOAD_PATH)
            params: dict[str, str] = {"path": path}
            headers: dict[str, str] = {}
            if range_header:
                headers["Range"] = range_header
            if offset is not None:
                params["offset"] = str(offset)
            if limit is not None:
                params["limit"] = str(limit)

            response = self._httpx_client.get(url, headers=headers, params=params)
            response.raise_for_status()
            return response.content
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def read_bytes_stream(
        self,
        path: str,
        *,
        chunk_size: int = 64 * 1024,
        range_header: str | None = None,
        offset: int | None = None,
        limit: int | None = None,
    ) -> Iterator[bytes]:
        url = self._get_url(self.DOWNLOAD_PATH)
        params: dict[str, str] = {"path": path}
        headers: dict[str, str] = {}
        if range_header:
            headers["Range"] = range_header
        if offset is not None:
            params["offset"] = str(offset)
        if limit is not None:
            params["limit"] = str(limit)

        request = self._httpx_client.build_request(
            "GET", url, headers=headers, params=params
        )
        response = self._httpx_client.send(request, stream=True)

        if response.status_code >= 300:
            try:
                response.read()
            finally:
                response.close()
            raise SandboxApiException(
                f"Failed to stream file {path}: {response.status_code}",
                status_code=response.status_code,
                request_id=extract_request_id(response.headers),
            )

        def _iter() -> Iterator[bytes]:
            try:
                yield from response.iter_bytes(chunk_size=chunk_size)
            finally:
                response.close()

        return _iter()

    def write_files(self, entries: list[WriteEntry]) -> None:
        if not entries:
            return
        try:
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
                multipart_parts.append(
                    ("metadata", ("metadata", json.dumps(metadata), "application/json"))
                )

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
                    content = entry.data
                    content_type = "application/octet-stream"
                else:
                    raise InvalidArgumentException(
                        f"Unsupported file data type: {type(entry.data)}"
                    )

                multipart_parts.append(("file", (entry.path, content, content_type)))

            url = self._get_url(self.UPLOAD_PATH)
            response = self._httpx_client.post(url, files=multipart_parts)
            response.raise_for_status()
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

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
        entry = WriteEntry(
            path=path, data=data, mode=mode, owner=owner, group=group, encoding=encoding
        )
        self.write_files([entry])

    def create_directories(self, entries: list[WriteEntry]) -> None:
        try:
            from opensandbox.api.execd.api.isolated_execution import isolated_make_dirs

            response_obj = isolated_make_dirs.sync_detailed(
                self._session_uuid,
                client=self._client,
                body=FilesystemModelConverter.to_api_isolated_make_dirs_body(entries),
            )
            handle_api_error(response_obj, "Create directories")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_files(self, paths: list[str]) -> None:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_remove_files,
            )

            response_obj = isolated_remove_files.sync_detailed(
                self._session_uuid,
                client=self._client,
                path=paths,
            )
            handle_api_error(response_obj, "Delete files")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_directories(self, paths: list[str]) -> None:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_remove_dirs,
            )

            response_obj = isolated_remove_dirs.sync_detailed(
                self._session_uuid,
                client=self._client,
                path=paths,
            )
            handle_api_error(response_obj, "Delete directories")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def move_files(self, entries: list[MoveEntry]) -> None:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_rename_files,
            )

            rename_items = FilesystemModelConverter.to_api_rename_file_items(entries)
            response_obj = isolated_rename_files.sync_detailed(
                self._session_uuid,
                client=self._client,
                body=rename_items,
            )
            handle_api_error(response_obj, "Move files")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def set_permissions(self, entries: list[SetPermissionEntry]) -> None:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_chmod_files,
            )

            response_obj = isolated_chmod_files.sync_detailed(
                self._session_uuid,
                client=self._client,
                body=FilesystemModelConverter.to_api_isolated_chmod_files_body(entries),
            )
            handle_api_error(response_obj, "Set permissions")
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def replace_contents(self, entries: list[ContentReplaceEntry]) -> None:
        try:
            from json import JSONDecodeError

            from opensandbox.api.execd.api.isolated_execution import (
                isolated_replace_content,
            )

            try:
                response_obj = isolated_replace_content.sync_detailed(
                    self._session_uuid,
                    client=self._client,
                    body=FilesystemModelConverter.to_api_isolated_replace_content_body(
                        entries
                    ),
                )
                handle_api_error(response_obj, "Replace contents")
            except JSONDecodeError:
                pass
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def replace_contents_detailed(
        self, entries: list[ContentReplaceEntry]
    ) -> list[ContentReplaceResult]:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_replace_content,
            )

            response_obj = isolated_replace_content.sync_detailed(
                self._session_uuid,
                client=self._client,
                body=FilesystemModelConverter.to_api_isolated_replace_content_body(
                    entries
                ),
            )
            handle_api_error(response_obj, "Replace contents")
            return FilesystemModelConverter.to_replace_results(response_obj.parsed)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def search(self, entry: SearchEntry) -> list[EntryInfo]:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_search_files,
            )
            from opensandbox.api.execd.models import FileInfo

            response_obj = isolated_search_files.sync_detailed(
                self._session_uuid,
                client=self._client,
                path=entry.path,
                pattern=entry.pattern,
            )
            handle_api_error(response_obj, "Search files")

            parsed = response_obj.parsed
            if not parsed:
                return []
            if isinstance(parsed, list) and all(
                isinstance(x, FileInfo) for x in parsed
            ):
                return FilesystemModelConverter.to_entry_info_list(parsed)
            raise SandboxApiException(
                message="Search files failed: unexpected response type",
                request_id=extract_request_id(getattr(response_obj, "headers", None)),
            )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_directory(self, entry: DirectoryListEntry) -> list[EntryInfo]:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_list_directory,
            )
            from opensandbox.api.execd.models import FileInfo
            from opensandbox.api.execd.types import UNSET

            response_obj = isolated_list_directory.sync_detailed(
                self._session_uuid,
                client=self._client,
                path=entry.path,
                depth=entry.depth if entry.depth is not None else UNSET,
            )
            handle_api_error(response_obj, "List directory")

            parsed = response_obj.parsed
            if not parsed:
                return []
            if isinstance(parsed, list) and all(
                isinstance(x, FileInfo) for x in parsed
            ):
                return FilesystemModelConverter.to_entry_info_list(parsed)
            raise SandboxApiException(
                message="List directory failed: unexpected response type",
                request_id=extract_request_id(getattr(response_obj, "headers", None)),
            )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]:
        try:
            from opensandbox.api.execd.api.isolated_execution import (
                isolated_get_files_info,
            )

            response_obj = isolated_get_files_info.sync_detailed(
                self._session_uuid,
                client=self._client,
                path=paths,
            )
            handle_api_error(response_obj, "Get file info")

            if not response_obj.parsed:
                return {}
            return FilesystemModelConverter.to_entry_info_map(response_obj.parsed)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e
