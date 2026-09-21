#
# Copyright 2026 The OpenSandbox Authors
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
Synchronous lifecycle control-plane network policy adapter.

Sync counterpart of :mod:`opensandbox.adapters.network_policy_adapter`:
implements the Egress protocol against the lifecycle server
(``/sandboxes/{sandboxId}/networkpolicy``) instead of the sandbox-side
egress sidecar.
"""

import logging

import httpx

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
from opensandbox.adapters.network_policy_adapter import (
    _CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
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
from opensandbox.sync.services.egress import EgressSync

logger = logging.getLogger(__name__)


class NetworkPolicyAdapterSync(EgressSync):
    """Sync egress policy operations routed through the lifecycle control plane."""

    def __init__(
        self, connection_config: ConnectionConfigSync, sandbox_id: str
    ) -> None:
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
        self._httpx_client = httpx.Client(
            base_url=self.connection_config.get_base_url(),
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

    def _get_policy_payload(self):
        from opensandbox.api.lifecycle.api.sandboxes import (
            get_sandbox_network_policy,
        )
        from opensandbox.api.lifecycle.models import PolicyStatusResponse
        from opensandbox.api.lifecycle.types import Unset

        response_obj = get_sandbox_network_policy.sync_detailed(
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

    def get_policy(self) -> NetworkPolicy:
        try:
            return NetworkPolicy.model_validate(self._get_policy_payload().to_dict())
        except Exception as e:
            logger.warning(
                f"Failed to get network policy for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def patch_rules(self, rules: list[NetworkRule]) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                patch_sandbox_network_policy,
            )

            response_obj = patch_sandbox_network_policy.sync_detailed(
                client=self._client,
                sandbox_id=self.sandbox_id,
                body=SandboxModelConverter.to_api_network_rules(rules),
            )
            handle_api_error(
                response_obj, f"Patch network policy for sandbox {self.sandbox_id}"
            )
        except Exception as e:
            logger.warning(
                f"Failed to patch network policy for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_rules(self, targets: list[str]) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                delete_sandbox_network_policy_rules,
            )

            response_obj = delete_sandbox_network_policy_rules.sync_detailed(
                client=self._client,
                sandbox_id=self.sandbox_id,
                body=list(targets),
            )
            handle_api_error(
                response_obj,
                f"Delete network policy rules for sandbox {self.sandbox_id}",
            )
        except Exception as e:
            logger.warning(
                f"Failed to delete network policy rules for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def _unsupported(self, operation: str) -> SandboxException:
        return SandboxException(
            f"{operation}: {_CREDENTIAL_VAULT_UNSUPPORTED_MESSAGE}"
        )

    def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        raise self._unsupported("Credential Vault creation")

    def get(self) -> CredentialVaultState:
        raise self._unsupported("Credential Vault read")

    def patch(
        self,
        *,
        expected_revision: int | None = None,
        credentials: CredentialMutationSet | dict[str, object] | None = None,
        bindings: CredentialBindingMutationSet | dict[str, object] | None = None,
    ) -> CredentialVaultState:
        raise self._unsupported("Credential Vault patch")

    def delete(self) -> None:
        raise self._unsupported("Credential Vault deletion")

    def list_credentials(self) -> list[CredentialMetadata]:
        raise self._unsupported("Credential Vault credential listing")

    def get_credential(self, name: str) -> CredentialMetadata:
        raise self._unsupported("Credential Vault credential read")

    def list_bindings(self) -> list[CredentialBindingMetadata]:
        raise self._unsupported("Credential Vault binding listing")

    def get_binding(self, name: str) -> CredentialBindingMetadata:
        raise self._unsupported("Credential Vault binding read")
