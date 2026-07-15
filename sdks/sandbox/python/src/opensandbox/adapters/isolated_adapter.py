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
Isolated session adapter implementation (async).

Adapter for /v1/isolated/* endpoints using direct httpx + SSE streaming.
"""

import json
import logging

import httpx

from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.execution_event_dispatcher import (
    ExecutionEventDispatcher,
)
from opensandbox.adapters.converter.response_handler import extract_request_id
from opensandbox.adapters.isolated_filesystem_adapter import IsolatedFilesystemAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import InvalidArgumentException, SandboxApiException
from opensandbox.models.execd import Execution, ExecutionHandlers
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest,
    IsolatedCapabilities,
    IsolatedRunOpts,
    IsolatedSessionInfo,
    IsolatedSessionState,
    IsolatedSessionSummary,
)
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.services.filesystem import Filesystem
from opensandbox.services.isolated import (
    IsolationService,
    IsolationServiceMixin,
    IsolationSession,
)

logger = logging.getLogger(__name__)


def _decode_sse_event_line(line: str) -> EventNode | None:
    if not line.strip():
        return None
    if line.startswith((":", "event:", "id:", "retry:")):
        return None
    data = line[5:].strip() if line.startswith("data:") else line
    if not data:
        return None
    try:
        event_dict = json.loads(data)
        return EventNode(**event_dict)
    except Exception as e:
        logger.error(f"Failed to parse SSE line: {line}", exc_info=e)
        return None


def _infer_exit_code(execution: Execution) -> int | None:
    if execution.error is not None:
        try:
            return int(execution.error.value)
        except (TypeError, ValueError):
            return None
    if execution.complete is not None:
        return 0
    return None


class IsolationSessionHandle(IsolationSession):
    """Async handle to a single isolated session."""

    def __init__(self, info: IsolatedSessionInfo, adapter: "IsolatedSessionsAdapter"):
        self._info = info
        self._adapter = adapter
        self._files: Filesystem | None = None

    @property
    def session_id(self) -> str:
        return self._info.session_id

    @property
    def info(self) -> IsolatedSessionInfo:
        return self._info

    @property
    def files(self) -> Filesystem:
        if self._files is None:
            self._files = IsolatedFilesystemAdapter(
                self._adapter.connection_config,
                self._adapter.execd_endpoint,
                self._info.session_id,
            )
        return self._files

    async def run(
        self,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        return await self._adapter._run(
            self._info.session_id, code, opts=opts, handlers=handlers
        )

    async def get(self) -> IsolatedSessionState:
        return await self._adapter._get(self._info.session_id)

    async def delete(self) -> None:
        return await self._adapter._delete(self._info.session_id)


class IsolatedSessionsAdapter(IsolationServiceMixin, IsolationService):
    """Async adapter for isolated session endpoints (/v1/isolated/*).

    ``run_once``/``session`` are inherited from :class:`IsolationServiceMixin`.
    """

    CREATE_PATH = "/v1/isolated/session"
    SESSION_PATH = "/v1/isolated/session/{session_id}"
    RUN_PATH = "/v1/isolated/session/{session_id}/run"
    SESSIONS_PATH = "/v1/isolated/sessions"
    CAPABILITIES_PATH = "/v1/isolated/capabilities"

    def __init__(
        self,
        connection_config: ConnectionConfig,
        execd_endpoint: SandboxEndpoint,
    ) -> None:
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint

        protocol = self.connection_config.protocol
        base_url = f"{protocol}://{self.execd_endpoint.endpoint}"
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

        sse_headers = {
            **headers,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
        }
        self._sse_client = httpx.AsyncClient(
            headers=sse_headers,
            timeout=httpx.Timeout(
                connect=timeout_seconds,
                read=None,
                write=timeout_seconds,
                pool=None,
            ),
            transport=self.connection_config.transport,
        )

    def _get_url(self, path: str) -> str:
        protocol = self.connection_config.protocol
        return f"{protocol}://{self.execd_endpoint.endpoint}{path}"

    async def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSessionHandle:
        try:
            url = self._get_url(self.CREATE_PATH)
            body = request.model_dump(exclude_none=True)
            response = await self._httpx_client.post(url, json=body)
            if response.status_code not in (200, 201):
                raise SandboxApiException(
                    message=f"create isolated session failed. Status: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
            data = response.json()
            info = IsolatedSessionInfo(**data)
            return IsolationSessionHandle(info, self)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def _get(self, session_id: str) -> IsolatedSessionState:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        try:
            url = self._get_url(self.SESSION_PATH.format(session_id=session_id))
            response = await self._httpx_client.get(url)
            if response.status_code != 200:
                raise SandboxApiException(
                    message=f"get isolated session failed. Status: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
            data = response.json()
            return IsolatedSessionState(**data)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def _run(
        self,
        session_id: str,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlers | None = None,
    ) -> Execution:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        if not (code and code.strip()):
            raise InvalidArgumentException("code cannot be empty")

        opts = opts or IsolatedRunOpts()
        json_body: dict = {"code": code}
        if opts.envs:
            json_body["envs"] = opts.envs
        if opts.timeout_seconds is not None:
            json_body["timeout_seconds"] = opts.timeout_seconds

        url = self._get_url(self.RUN_PATH.format(session_id=session_id))

        try:
            execution = Execution(id=None, execution_count=None, result=[], error=None)
            client = self._sse_client

            async with client.stream("POST", url, json=json_body) as response:
                if response.status_code != 200:
                    await response.aread()
                    raise SandboxApiException(
                        message=f"run in isolated session failed. Status: {response.status_code}",
                        status_code=response.status_code,
                        request_id=extract_request_id(response.headers),
                    )

                dispatcher = ExecutionEventDispatcher(execution, handlers)
                async for line in response.aiter_lines():
                    event_node = _decode_sse_event_line(line)
                    if event_node is None:
                        continue
                    await dispatcher.dispatch(event_node)

            execution.exit_code = _infer_exit_code(execution)
            return execution
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def _delete(self, session_id: str) -> None:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        try:
            url = self._get_url(self.SESSION_PATH.format(session_id=session_id))
            response = await self._httpx_client.delete(url)
            if response.status_code not in (200, 204):
                raise SandboxApiException(
                    message=f"delete isolated session failed. Status: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def list(self) -> list[IsolatedSessionSummary]:
        try:
            url = self._get_url(self.SESSIONS_PATH)
            response = await self._httpx_client.get(url)
            if response.status_code != 200:
                raise SandboxApiException(
                    message=f"list isolated sessions failed. Status: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
            data = response.json()
            return [
                IsolatedSessionSummary(**item) for item in data.get("sessions", [])
            ]
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def capabilities(self) -> IsolatedCapabilities:
        try:
            url = self._get_url(self.CAPABILITIES_PATH)
            response = await self._httpx_client.get(url)
            if response.status_code != 200:
                raise SandboxApiException(
                    message=f"get capabilities failed. Status: {response.status_code}",
                    status_code=response.status_code,
                    request_id=extract_request_id(response.headers),
                )
            data = response.json()
            return IsolatedCapabilities(**data)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e
