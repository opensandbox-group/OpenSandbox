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
Egress service interface.

Protocol for direct egress sidecar operations.
"""

from typing import Protocol

from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule


class Egress(Protocol):
    """Direct runtime egress policy service."""

    async def get_policy(self) -> NetworkPolicy:
        """
        Retrieve the current egress policy from the sidecar.

        Raises:
            SandboxException: if the operation fails
        """
        ...

    async def patch_rules(self, rules: list[NetworkRule]) -> None:
        """
        Patch egress rules via the sidecar policy API.

        Merge semantics:
        - Incoming rules take priority over existing rules with the same target.
        - Existing rules for other targets remain in place.
        - Within one patch payload, the first rule for a target wins.
        - The current defaultAction is preserved.

        Raises:
            SandboxException: if the operation fails
        """
        ...
