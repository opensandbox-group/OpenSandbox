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
Health service adapter implementation.

Implementation of HealthService that adapts openapi-python-client generated HealthApi.
This adapter provides health check functionality for sandboxes.
"""

import logging

import httpx

from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.services.health import Health

logger = logging.getLogger(__name__)


class HealthAdapter(Health):
    """
    Implementation of HealthService for sandbox health monitoring.

    This adapter provides health check functionality to verify sandbox
    availability and responsiveness using the openapi-python-client
    generated API client.
    """

    def __init__(
        self,
        connection_config: ConnectionConfig,
        execd_endpoint: SandboxEndpoint,
    ) -> None:
        """
        Initialize the health service adapter.

        Args:
            connection_config: Connection configuration (shared transport, headers, timeouts)
            execd_endpoint: Endpoint for execd service
        """
        self.connection_config = connection_config
        self.execd_endpoint = execd_endpoint
        from opensandbox.api.execd import Client

        protocol = self.connection_config.protocol
        base_url = f"{protocol}://{self.execd_endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)

        headers = self.execd_endpoint.build_request_headers(self.connection_config)
        self._client = Client(
            base_url=base_url,
            timeout=timeout,
        )

        self._httpx_client = httpx.AsyncClient(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_async_httpx_client(self._httpx_client)

    async def _get_client(self):
        """Return the client for execd API."""
        return self._client

    async def ping(self, sandbox_id: str) -> bool:
        """Check if a sandbox is alive and responsive.

        Args:
            sandbox_id: Unique identifier of the sandbox to check

        Returns:
            True if the sandbox is healthy and responsive, False otherwise
        """
        try:
            from opensandbox.adapters.converter.response_handler import (
                handle_api_error,
            )
            from opensandbox.api.execd.api.health import ping

            client = await self._get_client()
            response_obj = await ping.asyncio_detailed(client=client)

            handle_api_error(response_obj, "Ping")
            return True

        except Exception as e:
            logger.debug(f"Health check failed for sandbox {sandbox_id}: {e}")
            return False
