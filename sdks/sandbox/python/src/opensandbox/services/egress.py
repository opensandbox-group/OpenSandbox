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

from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialBindingMetadata,
    CredentialBindingMutationSet,
    CredentialMetadata,
    CredentialMutationSet,
    CredentialVaultState,
    NetworkPolicy,
    NetworkRule,
)


class CredentialVault(Protocol):
    """Sandbox-scoped Credential Vault facade."""

    async def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        """Create a sandbox-local Credential Vault."""
        ...

    async def get(self) -> CredentialVaultState:
        """Get sanitized Credential Vault state."""
        ...

    async def patch(
        self,
        *,
        expected_revision: int | None = None,
        credentials: CredentialMutationSet | dict[str, object] | None = None,
        bindings: CredentialBindingMutationSet | dict[str, object] | None = None,
    ) -> CredentialVaultState:
        """Atomically patch sandbox-local credentials and bindings."""
        ...

    async def delete(self) -> None:
        """Delete the sandbox-local Credential Vault."""
        ...

    async def list_credentials(self) -> list[CredentialMetadata]:
        """List sanitized credential metadata."""
        ...

    async def get_credential(self, name: str) -> CredentialMetadata:
        """Get sanitized metadata for one credential."""
        ...

    async def list_bindings(self) -> list[CredentialBindingMetadata]:
        """List sanitized binding metadata."""
        ...

    async def get_binding(self, name: str) -> CredentialBindingMetadata:
        """Get sanitized metadata for one binding."""
        ...


class Egress(CredentialVault, Protocol):
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

    async def delete_rules(self, targets: list[str]) -> None:
        """
        Delete egress rules by target via the sidecar policy API.

        Each entry is a FQDN or wildcard domain. Matching rules are removed
        from the currently enforced policy. Targets not present in the policy
        are silently ignored (idempotent). The current defaultAction is
        preserved.

        Raises:
            SandboxException: if the operation fails
        """
        ...
