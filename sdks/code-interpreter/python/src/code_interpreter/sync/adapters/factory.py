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
Factory for creating Code Interpreter sync services.
"""

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint

from code_interpreter.sync.adapters.code_adapter import CodesAdapterSync
from code_interpreter.sync.services.code import CodesSync


class AdapterFactorySync:
    """
    Factory for creating Code Interpreter sync service instances.

    This factory centralizes construction of sync services so they all share the same
    connection configuration (transport, headers, timeouts).
    """

    def __init__(self, connection_config: ConnectionConfigSync) -> None:
        """
        Initialize the factory with shared connection configuration (sync).

        Args:
            connection_config: Shared connection configuration (transport, headers, timeouts).
        """
        self.connection_config = connection_config

    def create_code_execution_service(self, endpoint: SandboxEndpoint) -> CodesSync:
        """
        Create a code execution service for the specified endpoint (sync).

        Args:
            endpoint: Sandbox endpoint for code execution services.

        Returns:
            Configured sync code service instance.
        """
        return CodesAdapterSync(endpoint, self.connection_config)
