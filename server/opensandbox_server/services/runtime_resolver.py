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
Secure runtime resolver for translating secure runtime configuration
to backend-specific parameters (Docker --runtime, Kubernetes RuntimeClass).

This module provides:
- SecureRuntimeResolver: Translates AppConfig to runtime parameters
- validate_secure_runtime_on_startup: Validates runtime availability at server startup
"""

from __future__ import annotations

import asyncio
import logging
from typing import TYPE_CHECKING, Optional

from kubernetes.client.exceptions import ApiException

logger = logging.getLogger(__name__)

if TYPE_CHECKING:
    from docker import DockerClient
    from opensandbox_server.config import AppConfig, SecureRuntimeConfig
    from opensandbox_server.services.k8s.client import K8sClient


class SecureRuntimeResolver:
    """
    Resolver for secure container runtime configuration.

    Translates server-level secure_runtime configuration into
    backend-specific parameters:
    - Docker: OCI runtime name (e.g., "runsc", "kata-runtime")
    - Kubernetes: RuntimeClass name (e.g., "gvisor", "kata-qemu")
    """

    # Default runtime mappings
    DEFAULT_DOCKER_RUNTIMES = {
        "gvisor": "runsc",
        "kata": "kata-runtime",
    }

    DEFAULT_K8S_RUNTIME_CLASSES = {
        "gvisor": "gvisor",
        "kata": "kata-qemu",
        "firecracker": "kata-fc",
    }

    def __init__(self, config: AppConfig):
        """
        Initialize the resolver with application configuration.

        Args:
            config: Application configuration containing secure_runtime settings
        """
        self.secure_runtime: Optional[SecureRuntimeConfig] = getattr(
            config, "secure_runtime", None
        )
        self.runtime_mode = config.runtime.type  # "docker" or "kubernetes"

    def is_enabled(self) -> bool:
        """Check if secure runtime is configured and enabled."""
        return (
            self.secure_runtime is not None
            and self.secure_runtime.type != ""
        )

    def get_docker_runtime(self) -> Optional[str]:
        """
        Get the Docker OCI runtime name for secure containers.

        Returns the configured docker_runtime if set, otherwise uses
        the default mapping for the secure runtime type.

        Returns:
            OCI runtime name (e.g., "runsc", "kata-runtime") or None
        """
        if not self.is_enabled():
            return None

        if self.secure_runtime is None:
            return None

        # Use explicit docker_runtime if configured
        if self.secure_runtime.docker_runtime:
            return self.secure_runtime.docker_runtime

        # Fall back to default mapping
        runtime_type = self.secure_runtime.type
        return self.DEFAULT_DOCKER_RUNTIMES.get(runtime_type)

    def get_k8s_runtime_class(self) -> Optional[str]:
        """
        Get the Kubernetes RuntimeClass name for secure containers.

        Returns the configured k8s_runtime_class if set, otherwise uses
        the default mapping for the secure runtime type.

        Returns:
            RuntimeClass name (e.g., "gvisor", "kata-qemu") or None
        """
        if not self.is_enabled():
            return None

        if self.secure_runtime is None:
            return None

        # Use explicit k8s_runtime_class if configured
        if self.secure_runtime.k8s_runtime_class:
            return self.secure_runtime.k8s_runtime_class

        # Fall back to default mapping
        runtime_type = self.secure_runtime.type
        return self.DEFAULT_K8S_RUNTIME_CLASSES.get(runtime_type)


async def validate_secure_runtime_on_startup(
    config: AppConfig,
    docker_client: Optional["DockerClient"] = None,
    k8s_client: Optional["K8sClient"] = None,
) -> None:
    """
    Validate that configured secure runtimes are available at startup.

    This function performs fail-fast validation to ensure the server
    starts with a valid secure runtime configuration. It checks:
    - Docker runtimes: Verifies the runtime exists in Docker daemon
    - Kubernetes RuntimeClasses: Verifies the RuntimeClass exists in cluster

    Args:
        config: Application configuration
        docker_client: Optional Docker client for runtime validation
        k8s_client: Optional K8s client wrapper for RuntimeClass validation

    Raises:
        ValueError: If a configured secure runtime is not available
        Exception: For other validation errors
    """
    resolver = SecureRuntimeResolver(config)

    if not resolver.is_enabled():
        logger.info("Secure runtime is not configured.")
        return

    if config.runtime.type == "docker":
        await _validate_docker_runtime(resolver, docker_client)
    elif config.runtime.type == "kubernetes":
        await _validate_k8s_runtime_class(resolver, k8s_client, config)
    else:
        logger.warning(
            "Secure runtime validation skipped for unknown runtime type: %s",
            config.runtime.type,
        )


async def _validate_docker_runtime(
    resolver: SecureRuntimeResolver,
    docker_client: Optional["DockerClient"],
) -> None:
    """Validate that the Docker OCI runtime exists."""
    runtime_name = resolver.get_docker_runtime()

    if not runtime_name:
        logger.info("No Docker runtime configured for secure containers.")
        return

    logger.info("Validating Docker OCI runtime: %s", runtime_name)

    if docker_client is None:
        logger.warning(
            "Docker client not available; skipping runtime validation. "
            "Runtime '%s' will be used but not validated.",
            runtime_name,
        )
        return

    try:
        # Get list of available runtimes from Docker daemon
        # Docker stores runtimes in daemon configuration
        info = docker_client.info()
        runtimes = info.get("Runtimes", {})

        if runtime_name not in runtimes:
            available = ", ".join(runtimes.keys()) if runtimes else "none"
            raise ValueError(
                f"Configured Docker runtime '{runtime_name}' is not available. "
                f"Available runtimes: {available}. "
                f"Please install and configure the runtime before starting the server."
            )

        logger.info(
            "Docker OCI runtime '%s' is available: %s",
            runtime_name,
            runtimes.get(runtime_name, {}),
        )
    except Exception as exc:
        logger.error("Failed to validate Docker runtime: %s", exc)
        raise


async def _validate_k8s_runtime_class(
    resolver: SecureRuntimeResolver,
    k8s_client: Optional["K8sClient"],
    config: AppConfig,
) -> None:
    """Validate that the Kubernetes RuntimeClass exists."""
    runtime_class_name = resolver.get_k8s_runtime_class()

    if not runtime_class_name:
        logger.info("No Kubernetes RuntimeClass configured for secure containers.")
        return

    logger.info("Validating Kubernetes RuntimeClass: %s", runtime_class_name)

    if k8s_client is None:
        logger.warning(
            "Kubernetes client not available; skipping RuntimeClass validation. "
            "RuntimeClass '%s' will be used but not validated.",
            runtime_class_name,
        )
        return

    try:
        loop = asyncio.get_event_loop()
        await loop.run_in_executor(None, k8s_client.read_runtime_class, runtime_class_name)
        logger.info("Kubernetes RuntimeClass '%s' is available.", runtime_class_name)
    except ApiException as exc:
        if exc.status == 404:
            raise ValueError(
                f"Configured Kubernetes RuntimeClass '{runtime_class_name}' does not exist. "
                f"Please create the RuntimeClass before starting the server."
            ) from exc
        logger.error("Failed to validate RuntimeClass: %s", exc)
        raise
    except Exception as exc:
        logger.error("Failed to validate RuntimeClass: %s", exc)
        raise


__all__ = [
    "SecureRuntimeResolver",
    "validate_secure_runtime_on_startup",
]
