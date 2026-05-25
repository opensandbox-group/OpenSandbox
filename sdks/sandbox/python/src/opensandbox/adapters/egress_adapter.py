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
Direct egress sidecar adapter implementation.
"""

import logging

import httpx

from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule, SandboxEndpoint
from opensandbox.services.egress import Egress

logger = logging.getLogger(__name__)


class EgressAdapter(Egress):
    """Direct egress sidecar adapter using the generated egress client."""

    def __init__(self, connection_config: ConnectionConfig, endpoint: SandboxEndpoint) -> None:
        self.connection_config = connection_config
        self.endpoint = endpoint
        from opensandbox.api.egress import Client

        base_url = f"{self.connection_config.protocol}://{self.endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.endpoint.headers,
        }

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

    async def get_policy(self) -> NetworkPolicy:
        try:
            from opensandbox.api.egress.api.policy import get_policy
            from opensandbox.api.egress.models.network_policy import (
                NetworkPolicy as ApiNetworkPolicy,
            )
            from opensandbox.api.egress.models.policy_status_response import (
                PolicyStatusResponse,
            )
            from opensandbox.api.egress.types import Unset

            response_obj = await get_policy.asyncio_detailed(client=self._client)
            handle_api_error(response_obj, "Get egress policy")
            parsed = require_parsed(response_obj, PolicyStatusResponse, "Get egress policy")
            policy = parsed.policy
            if isinstance(policy, Unset):
                raise ValueError("Egress policy response missing policy payload")
            if not isinstance(policy, ApiNetworkPolicy):
                raise TypeError(f"Expected NetworkPolicy, got {type(policy).__name__}")
            return NetworkPolicy.model_validate(policy.to_dict())
        except Exception as e:
            logger.error("Failed to get egress policy from endpoint %s", self.endpoint.endpoint, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def patch_rules(self, rules: list[NetworkRule]) -> None:
        try:
            from opensandbox.api.egress.api.policy import patch_policy
            from opensandbox.api.egress.models.network_rule import (
                NetworkRule as ApiNetworkRule,
            )
            from opensandbox.api.egress.models.network_rule_action import (
                NetworkRuleAction,
            )

            response_obj = await patch_policy.asyncio_detailed(
                client=self._client,
                body=[
                    ApiNetworkRule(
                        action=NetworkRuleAction(rule.action),
                        target=rule.target,
                    )
                    for rule in rules
                ],
            )
            handle_api_error(response_obj, "Patch egress rules")
        except Exception as e:
            logger.error("Failed to patch egress policy via endpoint %s", self.endpoint.endpoint, exc_info=e)
            raise ExceptionConverter.to_sandbox_exception(e) from e
