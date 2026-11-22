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
Synchronous egress service interface.
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


class CredentialVaultSync(Protocol):
    """Blocking sandbox-scoped Credential Vault facade."""

    def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        """Create a sandbox-local Credential Vault."""
        ...

    def get(self) -> CredentialVaultState:
        """Get sanitized Credential Vault state."""
        ...

    def patch(
        self,
        *,
        expected_revision: int | None = None,
        credentials: CredentialMutationSet | dict[str, object] | None = None,
        bindings: CredentialBindingMutationSet | dict[str, object] | None = None,
    ) -> CredentialVaultState:
        """Atomically patch sandbox-local credentials and bindings."""
        ...

    def delete(self) -> None:
        """Delete the sandbox-local Credential Vault."""
        ...

    def list_credentials(self) -> list[CredentialMetadata]:
        """List sanitized credential metadata."""
        ...

    def get_credential(self, name: str) -> CredentialMetadata:
        """Get sanitized metadata for one credential."""
        ...

    def list_bindings(self) -> list[CredentialBindingMetadata]:
        """List sanitized binding metadata."""
        ...

    def get_binding(self, name: str) -> CredentialBindingMetadata:
        """Get sanitized metadata for one binding."""
        ...


class EgressSync(CredentialVaultSync, Protocol):
    """Blocking direct runtime egress policy service."""

    def get_policy(self) -> NetworkPolicy:
        """Retrieve the current egress policy from the sidecar."""
        ...

    def patch_rules(self, rules: list[NetworkRule]) -> None:
        """Replace matching (case-insensitive target, sorted ports) scopes; first patch rule wins, defaultAction stays."""
        ...

    def delete_rules(self, targets: list[str]) -> None:
        """Delete by target (domain, wildcard, IP, CIDR, or empty for port-only rules); misses/defaultAction stay unchanged."""
        ...
