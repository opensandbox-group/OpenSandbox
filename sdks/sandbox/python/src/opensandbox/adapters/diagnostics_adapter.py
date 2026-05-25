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
"""Diagnostics service adapter implementation."""

import logging

import httpx  # type: ignore[reportMissingImports]

from opensandbox.adapters.converter.diagnostic_model_converter import (
    DiagnosticModelConverter,
)
from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.config import ConnectionConfig
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.services.diagnostics import Diagnostics

logger = logging.getLogger(__name__)


class DiagnosticsAdapter(Diagnostics):
    """Adapter for sandbox diagnostics management API operations."""

    def __init__(self, connection_config: ConnectionConfig) -> None:
        self.connection_config = connection_config
        from opensandbox.api.diagnostic import AuthenticatedClient

        api_key = self.connection_config.get_api_key()
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)

        headers = {
            "Accept": "application/json",
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
        }
        if api_key:
            headers["OPEN-SANDBOX-API-KEY"] = api_key

        self._client = AuthenticatedClient(
            base_url=self.connection_config.get_base_url(),
            token=api_key or "",
            prefix="",
            auth_header_name="OPEN-SANDBOX-API-KEY",
            timeout=timeout,
        )
        self._httpx_client = httpx.AsyncClient(
            base_url=self.connection_config.get_base_url(),
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_async_httpx_client(self._httpx_client)

    async def _get_client(self):
        return self._client

    async def get_logs(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        try:
            from opensandbox.api.diagnostic.api.diagnostics import (
                get_sandboxes_sandbox_id_diagnostics_logs,
            )
            from opensandbox.api.diagnostic.models import DiagnosticContentResponse

            response_obj = await get_sandboxes_sandbox_id_diagnostics_logs.asyncio_detailed(
                sandbox_id=sandbox_id,
                client=await self._get_client(),
                scope=scope,
            )
            handle_api_error(response_obj, f"Get diagnostic logs for sandbox {sandbox_id}")
            parsed = require_parsed(
                response_obj,
                DiagnosticContentResponse,
                f"Get diagnostic logs for sandbox {sandbox_id}",
            )
            return DiagnosticModelConverter.to_diagnostic_content(parsed)
        except Exception as e:
            logger.error("Failed to get diagnostic logs for sandbox %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_events(
        self,
        sandbox_id: str,
        scope: str,
    ) -> DiagnosticContent:
        try:
            from opensandbox.api.diagnostic.api.diagnostics import (
                get_sandboxes_sandbox_id_diagnostics_events,
            )
            from opensandbox.api.diagnostic.models import DiagnosticContentResponse

            response_obj = await get_sandboxes_sandbox_id_diagnostics_events.asyncio_detailed(
                sandbox_id=sandbox_id,
                client=await self._get_client(),
                scope=scope,
            )
            handle_api_error(response_obj, f"Get diagnostic events for sandbox {sandbox_id}")
            parsed = require_parsed(
                response_obj,
                DiagnosticContentResponse,
                f"Get diagnostic events for sandbox {sandbox_id}",
            )
            return DiagnosticModelConverter.to_diagnostic_content(parsed)
        except Exception as e:
            logger.error("Failed to get diagnostic events for sandbox %s", sandbox_id, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
