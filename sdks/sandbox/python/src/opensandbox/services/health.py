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
Health service interface.

Protocol for sandbox health monitoring operations.
"""

from typing import Protocol


class Health(Protocol):
    """
    Health monitoring service for sandbox environments.

    This service provides health checking and monitoring capabilities
    for sandbox instances.
    """

    async def ping(self, sandbox_id: str) -> bool:
        """
        Check if a sandbox is alive and responsive.

        Args:
            sandbox_id: Unique identifier of the sandbox

        Returns:
            True if the sandbox is healthy, False otherwise

        Raises:
            SandboxException: if the operation fails
        """
        ...
