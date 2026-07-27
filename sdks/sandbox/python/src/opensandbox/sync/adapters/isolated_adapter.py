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
Synchronous isolated session adapter implementation.
"""

import json
import logging

import httpx

from opensandbox.adapters.converter.event_node import EventNode
from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.response_handler import (
    build_api_exception_from_httpx,
)
from opensandbox.adapters.isolated_adapter import (
    _build_attach_info,
    _build_session_state,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException
from opensandbox.models.execd import Execution
from opensandbox.models.execd_sync import ExecutionHandlersSync
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest,
    IsolatedCapabilities,
    IsolatedRunOpts,
    IsolatedSessionInfo,
    IsolatedSessionState,
    IsolatedSessionSummary,
)
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.converter.execution_event_dispatcher import (
    ExecutionEventDispatcherSync,
)
from opensandbox.sync.services.isolated import (
    IsolationServiceSync,
    IsolationServiceSyncMixin,
    IsolationSessionSync,
)
from opensandbox.transport import unwrap_retry_transport

logger = logging.getLogger(__name__)


def _decode_sse_event_line(line: str) -> EventNode | None:
    if not line or not line.strip():
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


class IsolationSessionHandleSync(IsolationSessionSync):
    """Sync handle to a single isolated session."""

    def __init__(
        self, info: IsolatedSessionInfo, adapter: "IsolatedSessionsAdapterSync"
    ):
        self._info = info
        self._adapter = adapter
        self._files = None

    @property
    def session_id(self) -> str:
        return self._info.session_id

    @property
    def info(self) -> IsolatedSessionInfo:
        return self._info

    @property
    def files(self):
        if self._files is None:
            from opensandbox.sync.adapters.isolated_filesystem_adapter import (
                IsolatedFilesystemAdapterSync,
            )

            self._files = IsolatedFilesystemAdapterSync(
                self._adapter.connection_config,
                self._adapter.execd_endpoint,
                self._info.session_id,
            )
        return self._files

    def run(
        self,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
    ) -> Execution:
        return self._adapter._run(
            self._info.session_id, code, opts=opts, handlers=handlers
        )

    def get(self) -> IsolatedSessionState:
        return self._adapter._get(self._info.session_id)

    def delete(self) -> None:
        return self._adapter._delete(self._info.session_id)


class IsolatedSessionsAdapterSync(IsolationServiceSyncMixin, IsolationServiceSync):
    """Synchronous adapter for isolated session endpoints.

    ``run_once``/``session`` are inherited from
    :class:`IsolationServiceSyncMixin`.
    """

    CREATE_PATH = "/v1/isolated/session"
    SESSION_PATH = "/v1/isolated/session/{session_id}"
    RUN_PATH = "/v1/isolated/session/{session_id}/run"
    SESSIONS_PATH = "/v1/isolated/sessions"
    CAPABILITIES_PATH = "/v1/isolated/capabilities"

    def __init__(
        self,
        connection_config: ConnectionConfigSync,
        execd_endpoint: SandboxEndpoint,
    ) -> None:
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint

        base_url = f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}"
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

        sse_headers = {
            **headers,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
        }
        # SSE bootstraps bypass the retry wrapper: request bodies are
        # not replayable and a non-idempotent status opt-in would cause
        # duplicate execution on a resent SSE POST.
        self._sse_client = httpx.Client(
            headers=sse_headers,
            timeout=httpx.Timeout(
                connect=timeout_seconds,
                read=None,
                write=timeout_seconds,
                pool=None,
            ),
            transport=unwrap_retry_transport(self.connection_config.transport),
        )

    def _get_url(self, path: str) -> str:
        return (
            f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}{path}"
        )

    def create(
        self, request: CreateIsolatedSessionRequest
    ) -> IsolationSessionHandleSync:
        try:
            url = self._get_url(self.CREATE_PATH)
            body = request.model_dump(exclude_none=True)
            response = self._httpx_client.post(url, json=body)
            if response.status_code not in (200, 201):
                raise build_api_exception_from_httpx(
                    response, "create isolated session"
                )
            data = response.json()
            info = IsolatedSessionInfo(**data)
            return IsolationSessionHandleSync(info, self)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def attach(self, session_id: str) -> IsolationSessionHandleSync:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        try:
            url = self._get_url(self.SESSION_PATH.format(session_id=session_id))
            response = self._httpx_client.get(url)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "attach isolated session"
                )
            info = _build_attach_info(session_id, response.json())
            return IsolationSessionHandleSync(info, self)
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _get(self, session_id: str) -> IsolatedSessionState:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        try:
            url = self._get_url(self.SESSION_PATH.format(session_id=session_id))
            response = self._httpx_client.get(url)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "get isolated session"
                )
            return _build_session_state(response.json())
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _run(
        self,
        session_id: str,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
        handlers: ExecutionHandlersSync | None = None,
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
            dispatcher = ExecutionEventDispatcherSync(execution, handlers)

            with self._sse_client.stream("POST", url, json=json_body) as response:
                if response.status_code != 200:
                    response.read()
                    raise build_api_exception_from_httpx(
                        response, "run in isolated session"
                    )
                for line in response.iter_lines():
                    event_node = _decode_sse_event_line(line)
                    if event_node is None:
                        continue
                    dispatcher.dispatch(event_node)

            execution.exit_code = _infer_exit_code(execution)
            return execution
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _delete(self, session_id: str) -> None:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        try:
            url = self._get_url(self.SESSION_PATH.format(session_id=session_id))
            response = self._httpx_client.delete(url)
            if response.status_code not in (200, 204):
                raise build_api_exception_from_httpx(
                    response, "delete isolated session"
                )
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list(self) -> list[IsolatedSessionSummary]:
        try:
            url = self._get_url(self.SESSIONS_PATH)
            response = self._httpx_client.get(url)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "list isolated sessions"
                )
            data = response.json()
            return [
                IsolatedSessionSummary(**item) for item in data.get("sessions", [])
            ]
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def capabilities(self) -> IsolatedCapabilities:
        try:
            url = self._get_url(self.CAPABILITIES_PATH)
            response = self._httpx_client.get(url)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "get capabilities"
                )
            return IsolatedCapabilities(**response.json())
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e
