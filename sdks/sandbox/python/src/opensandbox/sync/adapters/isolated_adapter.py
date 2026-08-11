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
    TAIL_CURSOR_HEADER,
    _build_attach_info,
    _build_session_state,
)
from opensandbox.adapters.sse import iter_sse_events
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import InvalidArgumentException
from opensandbox.models.execd import Execution
from opensandbox.models.execd_sync import ExecutionHandlersSync
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest,
    IsolatedBackgroundRun,
    IsolatedCapabilities,
    IsolatedRunLogs,
    IsolatedRunOpts,
    IsolatedRunStatus,
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


def _decode_sse_event_data(data: str) -> EventNode | None:
    if not data.strip():
        return None
    try:
        event_dict = json.loads(data)
        return EventNode(**event_dict)
    except Exception as e:
        logger.error(f"Failed to parse SSE event data: {data}", exc_info=e)
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

    def run_background(
        self,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
    ) -> IsolatedBackgroundRun:
        return self._adapter._run_background(
            self._info.session_id, code, opts=opts
        )

    def run_status(self, run_id: str) -> IsolatedRunStatus:
        return self._adapter._run_status(self._info.session_id, run_id)

    def run_logs(self, run_id: str, cursor: int = 0) -> IsolatedRunLogs:
        return self._adapter._run_logs(
            self._info.session_id, run_id, cursor=cursor
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
    RUN_STATUS_PATH = "/v1/isolated/session/{session_id}/runs/{run_id}"
    RUN_LOGS_PATH = "/v1/isolated/session/{session_id}/runs/{run_id}/logs"
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
                for event in iter_sse_events(response):
                    event_node = _decode_sse_event_data(event.data)
                    if event_node is None:
                        continue
                    dispatcher.dispatch(event_node)

            execution.exit_code = _infer_exit_code(execution)
            return execution
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _run_background(
        self,
        session_id: str,
        code: str,
        *,
        opts: IsolatedRunOpts | None = None,
    ) -> IsolatedBackgroundRun:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        if not (code and code.strip()):
            raise InvalidArgumentException("code cannot be empty")

        opts = opts or IsolatedRunOpts()
        json_body: dict = {"code": code, "background": True}
        if opts.envs:
            json_body["envs"] = opts.envs
        # timeout_seconds is foreground-only and deliberately not sent.

        url = self._get_url(self.RUN_PATH.format(session_id=session_id))

        try:
            response = self._httpx_client.post(url, json=json_body)
            if response.status_code != 202:
                raise build_api_exception_from_httpx(
                    response, "run background in isolated session"
                )
            return IsolatedBackgroundRun(**response.json())
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _run_status(
        self, session_id: str, run_id: str
    ) -> IsolatedRunStatus:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        if not (run_id and run_id.strip()):
            raise InvalidArgumentException("run_id cannot be empty")
        try:
            url = self._get_url(
                self.RUN_STATUS_PATH.format(session_id=session_id, run_id=run_id)
            )
            response = self._httpx_client.get(url)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "get isolated run status"
                )
            return IsolatedRunStatus(**response.json())
        except Exception as e:
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _run_logs(
        self,
        session_id: str,
        run_id: str,
        *,
        cursor: int = 0,
    ) -> IsolatedRunLogs:
        if not (session_id and session_id.strip()):
            raise InvalidArgumentException("session_id cannot be empty")
        if not (run_id and run_id.strip()):
            raise InvalidArgumentException("run_id cannot be empty")
        if cursor < 0:
            raise InvalidArgumentException("cursor cannot be negative")
        try:
            url = self._get_url(
                self.RUN_LOGS_PATH.format(session_id=session_id, run_id=run_id)
            )
            params = {"cursor": cursor} if cursor else None
            response = self._httpx_client.get(url, params=params)
            if response.status_code != 200:
                raise build_api_exception_from_httpx(
                    response, "get isolated run logs"
                )
            next_cursor = response.headers.get(TAIL_CURSOR_HEADER)
            if next_cursor is not None:
                try:
                    cursor_value = int(next_cursor)
                except (TypeError, ValueError):
                    cursor_value = cursor + len(response.content)
            else:
                cursor_value = cursor + len(response.content)
            return IsolatedRunLogs(text=response.text, cursor=cursor_value)
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
