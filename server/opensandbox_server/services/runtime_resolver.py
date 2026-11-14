# Copyright 2025 The OpenSandbox Authors
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
Secure runtime resolver: translates secure runtime configuration to
backend-specific parameters (Docker --runtime, Kubernetes RuntimeClass).
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
        self.secure_runtime: Optional[SecureRuntimeConfig] = getattr(
            config, "secure_runtime", None
        )

    def is_enabled(self) -> bool:
        """Check if secure runtime is configured and enabled."""
        return (
            self.secure_runtime is not None
            and self.secure_runtime.type != ""
        )

    def get_docker_runtime(self) -> Optional[str]:
        """Return the Docker OCI runtime name (configured or type default); None when disabled."""
        if not self.is_enabled():
            return None

        if self.secure_runtime is None:
            return None

        if self.secure_runtime.docker_runtime:
            return self.secure_runtime.docker_runtime

        runtime_type = self.secure_runtime.type
        return self.DEFAULT_DOCKER_RUNTIMES.get(runtime_type)

    def get_k8s_runtime_class(self) -> Optional[str]:
        """Return the Kubernetes RuntimeClass name (configured or type default); None when disabled."""
        if not self.is_enabled():
            return None

        if self.secure_runtime is None:
            return None

        if self.secure_runtime.k8s_runtime_class:
            return self.secure_runtime.k8s_runtime_class

        runtime_type = self.secure_runtime.type
        return self.DEFAULT_K8S_RUNTIME_CLASSES.get(runtime_type)


async def validate_secure_runtime_on_startup(
    config: AppConfig,
    docker_client: Optional["DockerClient"] = None,
    k8s_client: Optional["K8sClient"] = None,
) -> None:
    """
    Fail-fast startup validation: verify the configured Docker OCI runtime or
    Kubernetes RuntimeClass exists, raising if unavailable.
    """
    resolver = SecureRuntimeResolver(config)

    if not resolver.is_enabled():
        logger.info("Secure runtime is not configured.")
        return

    if config.runtime.type == "docker":
        await _validate_docker_runtime(resolver, docker_client, config)
    elif config.runtime.type == "kubernetes":
        await _validate_k8s_runtime_class(resolver, k8s_client, config)
    else:
        logger.warning(
            f"Secure runtime validation skipped for unknown runtime type: "
            f"{config.runtime.type}"
        )


async def _validate_docker_runtime(
    resolver: SecureRuntimeResolver,
    docker_client: Optional["DockerClient"],
    config: "AppConfig",
) -> None:
    """Validate that the Docker OCI runtime exists."""
    runtime_name = resolver.get_docker_runtime()

    if not runtime_name:
        logger.info("No Docker runtime configured for secure containers.")
        return

    logger.info(f"Validating Docker OCI runtime: {runtime_name}")

    if docker_client is None:
        logger.warning(
            "Docker client not available; skipping runtime validation. "
            f"Runtime '{runtime_name}' will be used but not validated."
        )
        return

    try:
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
            f"Docker OCI runtime '{runtime_name}' is available: "
            f"{runtimes.get(runtime_name, {})}"
        )
    except Exception as exc:
        logger.error(f"Failed to validate Docker runtime: {exc}")
        raise

    _warn_gvisor_egress_incompatibility(config)


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

    logger.info(f"Validating Kubernetes RuntimeClass: {runtime_class_name}")

    if k8s_client is None:
        logger.warning(
            "Kubernetes client not available; skipping RuntimeClass validation. "
            f"RuntimeClass '{runtime_class_name}' will be used but not validated."
        )
        return

    try:
        loop = asyncio.get_event_loop()
        await loop.run_in_executor(None, k8s_client.read_runtime_class, runtime_class_name)
        logger.info(f"Kubernetes RuntimeClass '{runtime_class_name}' is available.")
    except ApiException as exc:
        if exc.status == 404:
            raise ValueError(
                f"Configured Kubernetes RuntimeClass '{runtime_class_name}' does not exist. "
                f"Please create the RuntimeClass before starting the server."
            ) from exc
        logger.error(f"Failed to validate RuntimeClass: {exc}")
        raise
    except Exception as exc:
        logger.error(f"Failed to validate RuntimeClass: {exc}")
        raise

    _warn_gvisor_egress_incompatibility(config)


def _warn_gvisor_egress_incompatibility(config: "AppConfig") -> None:
    """Log a warning when gVisor is configured alongside an egress sidecar image."""
    egress_image = config.egress.image if getattr(config, "egress", None) else None
    if config.secure_runtime and config.secure_runtime.type == "gvisor" and egress_image:
        logger.warning(
            "gVisor runtime is configured with egress sidecar image. "
            "The egress sidecar's nftables-based DNS redirect and policy rules are incompatible with gVisor. "
            "Sandboxes created with network_policy will be rejected at creation time."
        )


__all__ = [
    "SecureRuntimeResolver",
    "validate_secure_runtime_on_startup",
]
