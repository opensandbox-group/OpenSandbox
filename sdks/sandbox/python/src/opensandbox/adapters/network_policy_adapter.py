#
# Copyright 2026 Alibaba Group Holding Ltd.
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
Lifecycle control-plane network policy adapter.

Implements the Egress protocol against the lifecycle server
(``/sandboxes/{sandboxId}/networkpolicy``) instead of the sandbox-side
egress sidecar. Used for sandboxes created from fsb templates, where the
policy intent is persisted on the Sandbox CR.

The lifecycle API only exposes read/replace operations, so rule merge and
delete semantics are emulated client-side (read, merge, replace).
"""

import logging

import httpx  # type: ignore[reportMissingImports]

from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxException
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
from opensandbox.services.egress import Egress

logger = logging.getLogger(__name__)

_CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE = (
    "Credential Vault requires the sandbox-side egress sidecar and is not "
    "available for sandboxes created from fsb templates."
)


def merge_network_policy_rules(
    current: NetworkPolicy, rules: list[NetworkRule]
) -> NetworkPolicy:
    """Apply sidecar patch merge semantics on top of the current policy.

    - Incoming rules take priority over existing rules with the same target.
    - Existing rules for other targets remain in place.
    - Within one patch payload, the first rule for a target wins.
    - The current defaultAction is preserved.
    """
    incoming: dict[str, NetworkRule] = {}
    for rule in rules:
        incoming.setdefault(rule.target, rule)
    merged: list[NetworkRule] = []
    for rule in current.egress or []:
        # Incoming rules replace existing rules for the same target in place.
        merged.append(incoming.pop(rule.target) if rule.target in incoming else rule)
    merged.extend(incoming.values())
    return NetworkPolicy.model_validate(
        {
            "defaultAction": current.default_action,
            "egress": merged,
        }
    )


def delete_network_policy_rules(
    current: NetworkPolicy, targets: list[str]
) -> NetworkPolicy:
    """Drop rules by target (idempotent), preserving the current defaultAction."""
    removed = set(targets)
    kept = [rule for rule in current.egress or [] if rule.target not in removed]
    return NetworkPolicy.model_validate(
        {
            "defaultAction": current.default_action,
            "egress": kept,
        }
    )


class NetworkPolicyAdapter(Egress):
    """Egress policy operations routed through the lifecycle control plane."""

    def __init__(self, connection_config: ConnectionConfig, sandbox_id: str) -> None:
        self.connection_config = connection_config
        self.sandbox_id = sandbox_id

        from opensandbox.api.lifecycle import AuthenticatedClient

        api_key = self.connection_config.get_api_key()
        timeout = httpx.Timeout(
            self.connection_config.request_timeout.total_seconds()
        )
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
        }
        if api_key:
            headers["OPEN-SANDBOX-API-KEY"] = api_key

        self._client = AuthenticatedClient(
            base_url=self.connection_config.get_base_url(),
            token=api_key or "",
            prefix="",
            auth_header_name="OPEN-SANDBOX-API-KEY",
            timeout=timeout,
        )
        self._httpx_client = httpx.AsyncClient(
            base_url=self.connection_config.get_base_url(),
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_async_httpx_client(self._httpx_client)

    async def _get_policy_payload(self):
        from opensandbox.api.lifecycle.api.sandboxes import (
            get_sandbox_network_policy,
        )
        from opensandbox.api.lifecycle.models import PolicyStatusResponse
        from opensandbox.api.lifecycle.types import Unset

        response_obj = await get_sandbox_network_policy.asyncio_detailed(
            client=self._client,
            sandbox_id=self.sandbox_id,
        )
        handle_api_error(
            response_obj, f"Get network policy for sandbox {self.sandbox_id}"
        )
        parsed = require_parsed(
            response_obj,
            PolicyStatusResponse,
            f"Get network policy for sandbox {self.sandbox_id}",
        )
        policy = parsed.policy
        if isinstance(policy, Unset):
            raise ValueError(
                f"Network policy response for sandbox {self.sandbox_id} "
                "is missing the policy payload"
            )
        return policy

    async def _replace_policy(self, policy: NetworkPolicy) -> None:
        from opensandbox.api.lifecycle.api.sandboxes import (
            replace_sandbox_network_policy,
        )
        from opensandbox.api.lifecycle.models.network_policy import (
            NetworkPolicy as ApiNetworkPolicy,
        )
        from opensandbox.api.lifecycle.types import Unset

        api_policy = SandboxModelConverter.to_api_network_policy(policy)
        if isinstance(api_policy, Unset) or not isinstance(
            api_policy, ApiNetworkPolicy
        ):
            raise ValueError("Network policy payload must not be empty")
        response_obj = await replace_sandbox_network_policy.asyncio_detailed(
            client=self._client,
            sandbox_id=self.sandbox_id,
            body=api_policy,
        )
        handle_api_error(
            response_obj, f"Replace network policy for sandbox {self.sandbox_id}"
        )

    async def get_policy(self) -> NetworkPolicy:
        try:
            return NetworkPolicy.model_validate(
                (await self._get_policy_payload()).to_dict()
            )
        except Exception as e:
            logger.warning(
                f"Failed to get network policy for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def patch_rules(self, rules: list[NetworkRule]) -> None:
        try:
            current = await self.get_policy()
            await self._replace_policy(merge_network_policy_rules(current, rules))
        except Exception as e:
            logger.warning(
                f"Failed to patch network policy for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete_rules(self, targets: list[str]) -> None:
        try:
            current = await self.get_policy()
            await self._replace_policy(delete_network_policy_rules(current, targets))
        except Exception as e:
            logger.warning(
                f"Failed to delete network policy rules for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _unsupported(self, operation: str) -> SandboxException:
        return SandboxException(
            f"{operation}: {_CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE}"
        )

    async def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        raise self._unsupported("Credential Vault creation")

    async def get(self) -> CredentialVaultState:
        raise self._unsupported("Credential Vault read")

    async def patch(
        self,
        *,
        expected_revision: int | None = None,
        credentials: CredentialMutationSet | dict[str, object] | None = None,
        bindings: CredentialBindingMutationSet | dict[str, object] | None = None,
    ) -> CredentialVaultState:
        raise self._unsupported("Credential Vault patch")

    async def delete(self) -> None:
        raise self._unsupported("Credential Vault deletion")

    async def list_credentials(self) -> list[CredentialMetadata]:
        raise self._unsupported("Credential Vault credential listing")

    async def get_credential(self, name: str) -> CredentialMetadata:
        raise self._unsupported("Credential Vault credential read")

    async def list_bindings(self) -> list[CredentialBindingMetadata]:
        raise self._unsupported("Credential Vault binding listing")

    async def get_binding(self, name: str) -> CredentialBindingMetadata:
        raise self._unsupported("Credential Vault binding read")
