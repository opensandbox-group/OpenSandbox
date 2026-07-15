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
Synchronous filesystem service adapter implementation.
"""

import json
import logging
import os
import time
from collections.abc import Iterator
from io import IOBase, TextIOBase
from typing import TypedDict

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


def _multipart_header_filename(filename: str) -> str:
    return (
        filename.replace("\\", "\\\\")
        .replace('"', r"\"")
        .replace("\r", "_")
        .replace("\n", "_")
    )


def _rewind_seekable_stream(stream: IOBase) -> None:
    if not stream.seekable():
        return
    stream.seek(0)


class _DownloadRequest(TypedDict):
    url: str
    params: dict[str, str]
    headers: dict[str, str]


class FilesystemAdapterSync(FilesystemSync):
    FILESYSTEM_UPLOAD_PATH = "/files/upload"
    FILESYSTEM_DOWNLOAD_PATH = "/files/download"

    def __init__(
        self, connection_config: ConnectionConfigSync, execd_endpoint: SandboxEndpoint
    ) -> None:
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

        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client = Client(base_url=base_url, timeout=timeout)
        self._client.set_httpx_client(self._httpx_client)

    def _get_execd_base_url(self) -> str:
        return f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}"

    def _get_execd_url(self, path: str) -> str:
        return (
            f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}{path}"
        )

    def _build_download_request(
        self,
        path: str,
        range_header: str | None = None,
        *,
        offset: int | None = None,
        limit: int | None = None,
    ) -> _DownloadRequest:
        url = self._get_execd_url(self.FILESYSTEM_DOWNLOAD_PATH)
        headers: dict[str, str] = {}
        params: dict[str, str] = {"path": path}
        if range_header:
            headers["Range"] = range_header
        if offset is not None:
            params["offset"] = str(offset)
        if limit is not None:
            params["limit"] = str(limit)
        return {"url": url, "params": params, "headers": headers}

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
        logger.debug(f"Reading file as bytes: {path}")
        try:
            request_data = self._build_download_request(
                path, range_header, offset=offset, limit=limit
            )
            response = self._httpx_client.get(
                request_data["url"],
                headers=request_data["headers"],
                params=request_data["params"],
            )
            response.raise_for_status()
            return response.content
        except Exception as e:
            logger.error(f"Failed to read file {path}", exc_info=e)
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
        logger.debug(f"Streaming file as bytes: {path} (chunk_size={chunk_size})")
        request_data = self._build_download_request(
            path, range_header, offset=offset, limit=limit
        )
        url = request_data["url"]
        params = request_data["params"]
        headers = request_data["headers"]

        request = self._httpx_client.build_request(
            "GET",
            url,
            headers=headers,
            params=params,
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
        """Write multiple files in a single operation using multipart upload.

        Uses chunked transfer encoding for direct execd access to bypass
        ingress proxy body size limits. Uses Content-Length when going
        through the server proxy, which does not support chunked multipart.
        """
        if not entries:
            return
        logger.debug(f"Writing {len(entries)} files")
        try:
            url = self._get_execd_url(self.FILESYSTEM_UPLOAD_PATH)

            if self.connection_config.use_server_proxy:
                response = self._write_files_with_content_length(url, entries)
            else:
                response = self._write_files_chunked(url, entries)

            response.raise_for_status()
        except Exception as e:
            logger.error(f"Failed to write {len(entries)} files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _write_files_with_content_length(
        self,
        url: str,
        entries: list[WriteEntry],
    ) -> httpx.Response:
        """Upload via httpx ``files=`` parameter with Content-Length.

        Used when going through the server proxy, which requires Content-Length
        for multipart requests.
        """
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

            content: bytes | str | IOBase
            content_type: str
            if isinstance(entry.data, bytes):
                content = entry.data
                content_type = "application/octet-stream"
            elif isinstance(entry.data, str):
                encoding = entry.encoding or "utf-8"
                content = entry.data.encode(encoding)
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

        return self._httpx_client.post(url, files=multipart_parts)

    def _write_files_chunked(
        self,
        url: str,
        entries: list[WriteEntry],
    ) -> httpx.Response:
        """Upload via chunked transfer encoding.

        Used for direct execd access to bypass ingress proxy body size limits.
        """
        boundary = f"opensandbox_{os.urandom(8).hex()}_{int(time.time())}"

        def _body() -> Iterator[bytes]:
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

                content: bytes | str | IOBase
                content_type: str
                if isinstance(entry.data, bytes):
                    content = entry.data
                    content_type = "application/octet-stream"
                elif isinstance(entry.data, str):
                    encoding = entry.encoding or "utf-8"
                    content = entry.data.encode(encoding)
                    content_type = f"text/plain; charset={encoding}"
                elif isinstance(entry.data, IOBase):
                    if isinstance(entry.data, TextIOBase):
                        raise InvalidArgumentException(
                            "File stream must be binary (opened with 'rb'). Text streams are not supported."
                        )
                    content = entry.data
                    _rewind_seekable_stream(content)
                    content_type = "application/octet-stream"
                else:
                    raise InvalidArgumentException(
                        f"Unsupported file data type: {type(entry.data)}"
                    )

                yield f"--{boundary}\r\n".encode()
                yield b'Content-Disposition: form-data; name="metadata"; filename="metadata"\r\n'
                yield b"Content-Type: application/json\r\n\r\n"
                yield metadata_json.encode()
                yield b"\r\n"

                filename = _multipart_header_filename(
                    os.path.basename(entry.path) or "file"
                )
                yield f"--{boundary}\r\n".encode()
                yield (
                    f'Content-Disposition: form-data; name="file"; '
                    f'filename="{filename}"\r\n'
                ).encode()
                yield f"Content-Type: {content_type}\r\n\r\n".encode()

                if isinstance(content, bytes):
                    yield content
                else:
                    while True:
                        chunk = content.read(64 * 1024)
                        if not chunk:
                            break
                        if isinstance(chunk, str):
                            yield chunk.encode()
                        else:
                            yield chunk
                yield b"\r\n"

            yield f"--{boundary}--\r\n".encode()

        return self._httpx_client.post(
            url,
            content=_body(),
            headers={"content-type": f"multipart/form-data; boundary={boundary}"},
        )

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
            from opensandbox.api.execd.api.filesystem import make_dirs

            response_obj = make_dirs.sync_detailed(
                client=self._client,
                body=FilesystemModelConverter.to_api_make_dirs_body(entries),
            )
            handle_api_error(response_obj, "Create directories")
        except Exception as e:
            logger.error("Failed to create directories", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_files(self, paths: list[str]) -> None:
        try:
            from opensandbox.api.execd.api.filesystem import remove_files

            response_obj = remove_files.sync_detailed(client=self._client, path=paths)
            handle_api_error(response_obj, "Delete files")
        except Exception as e:
            logger.error(f"Failed to delete {len(paths)} files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_directories(self, paths: list[str]) -> None:
        try:
            from opensandbox.api.execd.api.filesystem import remove_dirs

            response_obj = remove_dirs.sync_detailed(client=self._client, path=paths)
            handle_api_error(response_obj, "Delete directories")
        except Exception as e:
            logger.error(f"Failed to delete {len(paths)} directories", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def move_files(self, entries: list[MoveEntry]) -> None:
        try:
            from opensandbox.api.execd.api.filesystem import rename_files

            rename_items = FilesystemModelConverter.to_api_rename_file_items(entries)
            response_obj = rename_files.sync_detailed(
                client=self._client, body=rename_items
            )
            handle_api_error(response_obj, "Move files")
        except Exception as e:
            logger.error("Failed to move files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def set_permissions(self, entries: list[SetPermissionEntry]) -> None:
        try:
            from opensandbox.api.execd.api.filesystem import chmod_files

            response_obj = chmod_files.sync_detailed(
                client=self._client,
                body=FilesystemModelConverter.to_api_chmod_files_body(entries),
            )
            handle_api_error(response_obj, "Set permissions")
        except Exception as e:
            logger.error("Failed to set permissions", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def replace_contents(self, entries: list[ContentReplaceEntry]) -> None:
        try:
            from json import JSONDecodeError

            from opensandbox.api.execd.api.filesystem import replace_content

            try:
                response_obj = replace_content.sync_detailed(
                    client=self._client,
                    body=FilesystemModelConverter.to_api_replace_content_body(entries),
                )
                handle_api_error(response_obj, "Replace contents")
            except JSONDecodeError:
                pass
        except Exception as e:
            logger.error("Failed to replace contents", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def replace_contents_detailed(
        self, entries: list[ContentReplaceEntry]
    ) -> list[ContentReplaceResult]:
        try:
            from opensandbox.api.execd.api.filesystem import replace_content

            response_obj = replace_content.sync_detailed(
                client=self._client,
                body=FilesystemModelConverter.to_api_replace_content_body(entries),
                verbose=True,
            )

            handle_api_error(response_obj, "Replace contents")

            return FilesystemModelConverter.to_replace_results(response_obj.parsed)
        except Exception as e:
            logger.error("Failed to replace contents", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def search(self, entry: SearchEntry) -> list[EntryInfo]:
        try:
            from opensandbox.api.execd.api.filesystem import search_files
            from opensandbox.api.execd.models import FileInfo

            response_obj = search_files.sync_detailed(
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
            logger.error("Failed to search files", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_directory(self, entry: DirectoryListEntry) -> list[EntryInfo]:
        try:
            from opensandbox.api.execd.api.filesystem import list_directory
            from opensandbox.api.execd.models import FileInfo
            from opensandbox.api.execd.types import UNSET

            response_obj = list_directory.sync_detailed(
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
            logger.error("Failed to list directory", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_file_info(self, paths: list[str]) -> dict[str, EntryInfo]:
        try:
            from opensandbox.api.execd.api.filesystem import get_files_info

            response_obj = get_files_info.sync_detailed(client=self._client, path=paths)
            handle_api_error(response_obj, "Get file info")
            if not response_obj.parsed:
                return {}
            return FilesystemModelConverter.to_entry_info_map(response_obj.parsed)
        except Exception as e:
            logger.error(f"Failed to get file info for {len(paths)} paths", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
