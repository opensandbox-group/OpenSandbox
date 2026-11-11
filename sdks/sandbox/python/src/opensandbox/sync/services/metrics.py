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
Synchronous metrics service interface.

Defines the contract for **blocking** metrics retrieval from a sandbox instance.
This is the sync counterpart of :mod:`opensandbox.services.metrics`.
"""

from typing import Protocol

from opensandbox.models.sandboxes import SandboxMetrics


class MetricsSync(Protocol):
    """
    Metrics retrieval service for sandbox environments (sync).

    This service provides resource usage statistics (CPU, memory, etc.) for a running sandbox.
    """

    def get_metrics(self, sandbox_id: str) -> SandboxMetrics:
        """
        Retrieve current sandbox metrics for the given sandbox id.

        Args:
            sandbox_id: Unique identifier of the sandbox.

        Returns:
            Current sandbox metrics including CPU/memory and other usage information.

        Raises:
            SandboxException: If the operation fails.
        """
        ...
