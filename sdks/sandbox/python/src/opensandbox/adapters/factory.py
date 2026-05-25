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
Service factory for creating adapter instances.

Factory for creating service adapter instances that provide access to
sandbox operations including command execution, file system management,
health monitoring, and metrics collection.

All HTTP clients created by adapters share the same `ConnectionConfig.transport`
to ensure consistent pooling/proxy/retry behavior across services.
"""

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.diagnostics_adapter import DiagnosticsAdapter
from opensandbox.adapters.egress_adapter import EgressAdapter
from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.health_adapter import HealthAdapter
from opensandbox.adapters.metrics_adapter import MetricsAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.services.command import Commands
from opensandbox.services.diagnostics import Diagnostics
from opensandbox.services.egress import Egress
from opensandbox.services.filesystem import Filesystem
from opensandbox.services.health import Health
from opensandbox.services.metrics import Metrics
from opensandbox.services.sandbox import Sandboxes


class AdapterFactory:
    """
    Factory responsible for creating service instances.

    This factory encapsulates the instantiation logic of specific service adapters.
    Each adapter creates its own httpx clients, but they all share the same transport
    instance coming from the provided ConnectionConfig.

    Usage:
        config = ConnectionConfig(...)
        factory = AdapterFactory(config)
    """

    def __init__(self, connection_config: ConnectionConfig) -> None:
        """
        Initialize the service factory.

        Args:
            connection_config: Shared connection configuration, including transport.
        """
        self.connection_config = connection_config

    def create_sandbox_service(self) -> Sandboxes:
        """Create a sandbox management service for lifecycle operations.

        Returns:
            Service for creating, managing, and monitoring sandbox instances
        """
        return SandboxesAdapter(self.connection_config)

    def create_diagnostics_service(self) -> Diagnostics:
        """Create a diagnostics service for sandbox troubleshooting operations."""
        return DiagnosticsAdapter(self.connection_config)

    def create_filesystem_service(self, endpoint: SandboxEndpoint) -> Filesystem:
        """Create a filesystem service for file and directory operations.

        Args:
            endpoint: Sandbox endpoint information for file operations

        Returns:
            Service for file system management within the sandbox
        """
        return FilesystemAdapter(self.connection_config, endpoint)

    def create_command_service(self, endpoint: SandboxEndpoint) -> Commands:
        """Create a command execution service for running shell commands.

        Args:
            endpoint: Sandbox endpoint information for command execution

        Returns:
            Service for executing commands within the sandbox
        """
        return CommandsAdapter(self.connection_config, endpoint)

    def create_egress_service(self, endpoint: SandboxEndpoint) -> Egress:
        """Create a direct egress service for runtime egress policy operations."""
        return EgressAdapter(self.connection_config, endpoint)

    def create_health_service(self, endpoint: SandboxEndpoint) -> Health:
        """Create a health monitoring service for sandbox status checks.

        Args:
            endpoint: Sandbox endpoint information for health checks

        Returns:
            Service for monitoring sandbox health and availability
        """
        return HealthAdapter(self.connection_config, endpoint)

    def create_metrics_service(self, endpoint: SandboxEndpoint) -> Metrics:
        """Create a metrics collection service for resource monitoring.

        Args:
            endpoint: Sandbox endpoint information for metrics collection

        Returns:
            Service for collecting sandbox resource usage metrics
        """
        return MetricsAdapter(self.connection_config, endpoint)
