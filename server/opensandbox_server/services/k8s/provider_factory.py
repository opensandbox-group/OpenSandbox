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

"""
Factory for creating WorkloadProvider instances.
"""

import logging
from typing import Dict, Type, Optional

from opensandbox_server.config import AppConfig
from opensandbox_server.services.k8s.workload_provider import WorkloadProvider
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.agent_sandbox_provider import AgentSandboxProvider
from opensandbox_server.services.k8s.client import K8sClient

logger = logging.getLogger(__name__)

PROVIDER_TYPE_BATCHSANDBOX = "batchsandbox"
PROVIDER_TYPE_AGENT_SANDBOX = "agent-sandbox"

_PROVIDER_REGISTRY: Dict[str, Type[WorkloadProvider]] = {
    PROVIDER_TYPE_BATCHSANDBOX: BatchSandboxProvider,
    PROVIDER_TYPE_AGENT_SANDBOX: AgentSandboxProvider,
}


def create_workload_provider(
    provider_type: str | None,
    k8s_client: K8sClient,
    app_config: Optional[AppConfig] = None,
) -> WorkloadProvider:
    """Create a workload provider instance by provider type."""
    if provider_type is None:
        if not _PROVIDER_REGISTRY:
            raise ValueError(
                "No workload providers are registered. "
                "Cannot create a default provider."
            )
        provider_type = next(iter(_PROVIDER_REGISTRY.keys()))
        logger.info(f"No provider specified, using default: {provider_type}")

    provider_type_lower = provider_type.lower()

    if provider_type_lower not in _PROVIDER_REGISTRY:
        available = ", ".join(_PROVIDER_REGISTRY.keys())
        raise ValueError(
            f"Unsupported workload provider type '{provider_type}'. "
            f"Available providers: {available}"
        )

    provider_class = _PROVIDER_REGISTRY[provider_type_lower]
    logger.info(f"Creating workload provider: {provider_class.__name__}")

    if provider_type_lower in (PROVIDER_TYPE_BATCHSANDBOX, PROVIDER_TYPE_AGENT_SANDBOX):
        return provider_class(k8s_client, app_config=app_config)

    return provider_class(k8s_client)


def register_provider(name: str, provider_class: Type[WorkloadProvider]) -> None:
    """Register a custom workload provider implementation."""
    if not issubclass(provider_class, WorkloadProvider):
        raise TypeError(
            f"Provider class must inherit from WorkloadProvider, "
            f"got {provider_class.__name__}"
        )
    
    name_lower = name.lower()
    if name_lower in _PROVIDER_REGISTRY:
        logger.warning(
            f"Overwriting existing provider registration: {name_lower}"
        )
    
    _PROVIDER_REGISTRY[name_lower] = provider_class
    logger.info(f"Registered workload provider: {name_lower} -> {provider_class.__name__}")


def list_available_providers() -> list[str]:
    """List registered provider types."""
    return sorted(_PROVIDER_REGISTRY.keys())
