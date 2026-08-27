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
Synchronous metrics service adapter implementation.
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
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint, SandboxMetrics
from opensandbox.sync.services.metrics import MetricsSync

logger = logging.getLogger(__name__)


class MetricsAdapterSync(MetricsSync):
    def __init__(
        self, connection_config: ConnectionConfigSync, execd_endpoint: SandboxEndpoint
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

    def get_metrics(self, sandbox_id: str) -> SandboxMetrics:
        try:
            from opensandbox.api.execd.api.metric import get_metrics
            from opensandbox.api.execd.models import Metrics

            response_obj = get_metrics.sync_detailed(client=self._client)
            handle_api_error(response_obj, "Get metrics")
            parsed = require_parsed(response_obj, Metrics, "Get metrics")
            return MetricsModelConverter.to_sandbox_metrics(parsed)
        except Exception as e:
            logger.error(f"Failed to get metrics for sandbox {sandbox_id}", exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
