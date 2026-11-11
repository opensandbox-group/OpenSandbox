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
Synchronous health service interface.

Defines the contract for **blocking** health checks against a sandbox instance.
This is the sync counterpart of :mod:`opensandbox.services.health`.
"""

from typing import Protocol


class HealthSync(Protocol):
    """
    Health check service for sandbox environments (sync).

    This service provides lightweight checks to verify that a sandbox (and its execd service)
    is reachable and responsive.
    """

    def ping(self, sandbox_id: str) -> bool:
        """
        Ping the sandbox execd service to verify liveness.

        Args:
            sandbox_id: Unique identifier of the sandbox.

        Returns:
            True if the sandbox responds successfully, False otherwise.

        Raises:
            SandboxException: If the underlying request fails in a non-recoverable way.
        """
        ...
