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
Factory for creating sandbox service instances.

This module provides a factory function to create sandbox service implementations
based on application configuration loaded from sandbox_server.config.
"""

import logging
from typing import Optional

from opensandbox_server.config import AppConfig, get_config
from opensandbox_server.services.docker import DockerSandboxService
from opensandbox_server.services.k8s import KubernetesSandboxService
from opensandbox_server.services.sandbox_service import SandboxService

logger = logging.getLogger(__name__)


def create_sandbox_service(
    service_type: Optional[str] = None,
    config: Optional[AppConfig] = None,
) -> SandboxService:
    """
    Create a sandbox service instance based on configuration.

    Args:
        service_type: Optional override for service implementation type.
        config: Optional application configuration. Defaults to global config.

    Returns:
        SandboxService: An instance of the configured sandbox service implementation.

    Raises:
        ValueError: If the configured service type is not supported.
    """
    active_config = config or get_config()
    selected_type = (service_type or active_config.runtime.type).lower()

    logger.info("Creating sandbox service with type: %s", selected_type)

    # Service implementation registry
    # Add new implementations here as they are created
    implementations: dict[str, type[SandboxService]] = {
        "docker": DockerSandboxService,
        "kubernetes": KubernetesSandboxService,
        # Future implementations can be added here:
        # "containerd": ContainerdSandboxService,
    }

    if selected_type not in implementations:
        supported_types = ", ".join(implementations.keys())
        raise ValueError(
            f"Unsupported sandbox service type: {selected_type}. "
            f"Supported types: {supported_types}"
        )

    implementation_class = implementations[selected_type]
    return implementation_class(config=active_config)
