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
Synchronous PTY service adapter implementation.

Implementation of PtySync that adapts the openapi-python-client generated PTY API (sync SDK).
"""

import logging

import httpx

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.execd import PtySession, PtySessionStatus
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.services.pty import PtySync

logger = logging.getLogger(__name__)


class PtyAdapterSync(PtySync):
    """Synchronous PTY session service backed by the generated execd PTY API client."""

    def __init__(
        self,
        connection_config: ConnectionConfigSync,
        execd_endpoint: SandboxEndpoint,
    ) -> None:
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint
        from opensandbox.api.execd import Client

        base_url = f"{self.connection_config.protocol}://{self.execd_endpoint.endpoint}"
        timeout = httpx.Timeout(self.connection_config.request_timeout.total_seconds())
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.execd_endpoint.headers,
        }

        self._client = Client(base_url=base_url, timeout=timeout)
        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

    def create_session(
        self,
        cwd: str | None = None,
        command: str | None = None,
    ) -> PtySession:
        from opensandbox.adapters.converter.response_handler import (
            handle_api_error,
            require_parsed,
        )
        from opensandbox.api.execd.api.pty import create_pty_session
        from opensandbox.api.execd.models import CreatePtySessionResponse
        from opensandbox.api.execd.models.create_pty_session_request import (
            CreatePtySessionRequest,
        )
        from opensandbox.api.execd.types import UNSET

        body = CreatePtySessionRequest(
            cwd=cwd if cwd is not None else UNSET,
            command=command if command is not None else UNSET,
        )
        response_obj = create_pty_session.sync_detailed(client=self._client, body=body)
        handle_api_error(response_obj, "Create PTY session")
        parsed = require_parsed(response_obj, CreatePtySessionResponse, "Create PTY session")
        return PtySession(session_id=parsed.session_id)

    def get_session(self, session_id: str) -> PtySessionStatus:
        from opensandbox.adapters.converter.response_handler import (
            handle_api_error,
            require_parsed,
        )
        from opensandbox.api.execd.api.pty import get_pty_session
        from opensandbox.api.execd.models import PtySessionStatusResponse

        response_obj = get_pty_session.sync_detailed(session_id, client=self._client)
        handle_api_error(response_obj, "Get PTY session")
        parsed = require_parsed(response_obj, PtySessionStatusResponse, "Get PTY session")
        return PtySessionStatus(
            session_id=parsed.session_id,
            running=parsed.running,
            output_offset=parsed.output_offset,
        )

    def delete_session(self, session_id: str) -> None:
        from opensandbox.adapters.converter.response_handler import handle_api_error
        from opensandbox.api.execd.api.pty import delete_pty_session

        response_obj = delete_pty_session.sync_detailed(session_id, client=self._client)
        handle_api_error(response_obj, "Delete PTY session")
