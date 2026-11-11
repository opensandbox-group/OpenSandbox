#
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
#
"""
Factory for creating code interpreter services.

Provides a centralized way to create and configure code execution services
with proper dependency injection and configuration management.
"""

from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.adapters.code_adapter import CodesAdapter
from code_interpreter.services.code import Codes


class AdapterFactory:
    """
    Factory for creating code interpreter service instances.

    This factory handles the creation of code execution services with proper
    configuration and dependency injection, ensuring all services have access
    to the required HTTP client and endpoint configuration.
    """

    def __init__(self, connection_config: ConnectionConfig) -> None:
        """
        Initialize the factory with shared connection configuration.

        Args:
            connection_config: Shared connection configuration (transport, headers, timeouts)
        """
        self.connection_config = connection_config

    def create_code_execution_service(self, endpoint: SandboxEndpoint) -> Codes:
        """
        Create a code execution service for the specified endpoint.

        Args:
            endpoint: Sandbox endpoint for code execution services.

        Returns:
            Configured code service instance.
        """
        return CodesAdapter(endpoint, self.connection_config)
