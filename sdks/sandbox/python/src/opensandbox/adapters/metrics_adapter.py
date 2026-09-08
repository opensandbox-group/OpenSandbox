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
Metrics service adapter implementation.

Implementation of MetricsService that adapts openapi-python-client generated MetricApi.
"""

import logging

import httpx

from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.metrics_model_converter import (
    MetricsModelConverter,
)
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint, SandboxMetrics
from opensandbox.services.metrics import Metrics

logger = logging.getLogger(__name__)


class MetricsAdapter(Metrics):
    """
    Implementation of MetricsService for sandbox resource monitoring.

    This adapter provides comprehensive metrics collection and monitoring capabilities
    for sandbox environments, including CPU usage, memory consumption, and other
    performance metrics using the openapi-python-client generated API client.
    """

    def __init__(
        self,
        connection_config: ConnectionConfig,
        execd_endpoint: SandboxEndpoint,
    ) -> None:
        """
        Initialize the metrics service adapter.

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

    async def get_metrics(self, sandbox_id: str) -> SandboxMetrics:
        """Retrieve current resource usage metrics for a sandbox.

        Args:
            sandbox_id: The unique identifier of the sandbox

        Returns:
            Current metrics including CPU usage, memory consumption, and timestamp

        Raises:
            SandboxException: If metrics retrieval fails
        """
        logger.debug(f"Retrieving sandbox metrics for {sandbox_id}")

        try:
            from opensandbox.api.execd.api.metric import get_metrics

            client = await self._get_client()
            response_obj = await get_metrics.asyncio_detailed(client=client)

            handle_api_error(response_obj, "Get metrics")
            from opensandbox.api.execd.models import Metrics
            parsed = require_parsed(response_obj, Metrics, "Get metrics")
            return MetricsModelConverter.to_sandbox_metrics(parsed)

        except Exception as e:
            logger.error(f"Failed to get metrics for sandbox {sandbox_id}", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
