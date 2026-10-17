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
Lifecycle control-plane network policy and Credential Vault adapter.

Implements the Egress protocol against the lifecycle server
(``/sandboxes/{sandboxId}/networkpolicy`` and
``/sandboxes/{sandboxId}/credential-vault``) instead of the sandbox-side
egress sidecar. Used for sandboxes created from fsb templates, where the
policy intent is persisted on the Sandbox CR and the credential vault is
egress-held behind the server (OSEP-0022).

The lifecycle server exposes the same merge/delete rule semantics as the
sidecar policy API (PATCH merges with first-wins-per-target; DELETE removes
by target idempotently), so no client-side read-modify-write is needed. The
server-side vault PATCH is equally atomic (composed server-side over the
FastPath relay); a 412 means another writer won the revision — re-read and
retry with the fresh ``expected_revision``.
"""

import logging
from urllib.parse import quote

import httpx  # type: ignore[reportMissingImports]

from opensandbox.adapters.converter.credential_payload import (
    dump_binding_mutation_set,
    dump_bindings,
    dump_credential_mutation_set,
    dump_credentials,
)
from opensandbox.adapters.converter.exception_converter import (
    ExceptionConverter,
)
from opensandbox.adapters.converter.response_handler import (
    build_api_exception_from_httpx,
    handle_api_error,
    require_parsed,
)
from opensandbox.adapters.converter.sandbox_model_converter import (
    SandboxModelConverter,
)
from opensandbox.config import ConnectionConfig
from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialBindingListResponse,
    CredentialBindingMetadata,
    CredentialBindingMutationSet,
    CredentialListResponse,
    CredentialMetadata,
    CredentialMutationSet,
    CredentialVaultState,
    NetworkPolicy,
    NetworkRule,
)
from opensandbox.services.egress import Egress

logger = logging.getLogger(__name__)

_VAULT_PATH = "/sandboxes/{sandbox_id}/credential-vault"


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
            from opensandbox.api.lifecycle.api.sandboxes import (
                patch_sandbox_network_policy,
            )

            response_obj = await patch_sandbox_network_policy.asyncio_detailed(
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

    async def delete_rules(self, targets: list[str]) -> None:
        try:
            from opensandbox.api.lifecycle.api.sandboxes import (
                delete_sandbox_network_policy_rules,
            )

            response_obj = await delete_sandbox_network_policy_rules.asyncio_detailed(
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

    # -- Credential Vault (server-routed, OSEP-0022) -----------------------

    async def _vault_request(
        self,
        method: str,
        suffix: str,
        *,
        operation: str,
        json_body: object | None = None,
    ) -> object:
        path = _VAULT_PATH.format(sandbox_id=quote(self.sandbox_id, safe="")) + suffix
        response = await self._httpx_client.request(method, path, json=json_body)
        if response.status_code >= 400:
            logger.debug(f"{operation} failed with HTTP {response.status_code}")
            raise build_api_exception_from_httpx(response, operation)
        if response.status_code == 204 or not response.content:
            return None
        return response.json()

    def _vault_url(self, suffix: str) -> str:
        return _VAULT_PATH.format(sandbox_id=quote(self.sandbox_id, safe="")) + suffix

    async def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        try:
            payload = await self._vault_request(
                "POST",
                "",
                operation="Create credential vault",
                json_body={
                    "credentials": dump_credentials(credentials),
                    "bindings": dump_bindings(bindings),
                },
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.warning(f"Failed to create credential vault for sandbox {self.sandbox_id}: {e}")
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get(self) -> CredentialVaultState:
        try:
            payload = await self._vault_request(
                "GET", "", operation="Get credential vault"
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.warning(f"Failed to get credential vault for sandbox {self.sandbox_id}: {e}")
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def patch(
        self,
        *,
        expected_revision: int | None = None,
        credentials: CredentialMutationSet | dict[str, object] | None = None,
        bindings: CredentialBindingMutationSet | dict[str, object] | None = None,
    ) -> CredentialVaultState:
        try:
            body: dict[str, object] = {}
            if expected_revision is not None:
                body["expectedRevision"] = expected_revision
            credential_mutations = dump_credential_mutation_set(credentials)
            if credential_mutations is not None:
                body["credentials"] = credential_mutations
            binding_mutations = dump_binding_mutation_set(bindings)
            if binding_mutations is not None:
                body["bindings"] = binding_mutations
            payload = await self._vault_request(
                "PATCH",
                "",
                operation="Patch credential vault",
                json_body=body,
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.warning(f"Failed to patch credential vault for sandbox {self.sandbox_id}: {e}")
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def delete(self) -> None:
        try:
            await self._vault_request(
                "DELETE", "", operation="Delete credential vault"
            )
        except Exception as e:
            logger.warning(f"Failed to delete credential vault for sandbox {self.sandbox_id}: {e}")
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def list_credentials(self) -> list[CredentialMetadata]:
        try:
            payload = await self._vault_request(
                "GET",
                "/credentials",
                operation="List credential vault credentials",
            )
            return CredentialListResponse.model_validate(payload).credentials
        except Exception as e:
            logger.warning(
                f"Failed to list credential vault credentials for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_credential(self, name: str) -> CredentialMetadata:
        try:
            payload = await self._vault_request(
                "GET",
                f"/credentials/{quote(name, safe='')}",
                operation="Get credential vault credential",
            )
            return CredentialMetadata.model_validate(payload)
        except Exception as e:
            logger.warning(
                f"Failed to get credential vault credential for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def list_bindings(self) -> list[CredentialBindingMetadata]:
        try:
            payload = await self._vault_request(
                "GET",
                "/bindings",
                operation="List credential vault bindings",
            )
            return CredentialBindingListResponse.model_validate(payload).bindings
        except Exception as e:
            logger.warning(
                f"Failed to list credential vault bindings for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    async def get_binding(self, name: str) -> CredentialBindingMetadata:
        try:
            payload = await self._vault_request(
                "GET",
                f"/bindings/{quote(name, safe='')}",
                operation="Get credential vault binding",
            )
            return CredentialBindingMetadata.model_validate(payload)
        except Exception as e:
            logger.warning(
                f"Failed to get credential vault binding for sandbox {self.sandbox_id}: {e}"
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e
