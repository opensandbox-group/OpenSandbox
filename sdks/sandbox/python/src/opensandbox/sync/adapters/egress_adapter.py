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
Synchronous direct egress sidecar adapter implementation.
"""

import logging
from urllib.parse import quote

import httpx

from opensandbox.adapters.converter.exception_converter import ExceptionConverter
from opensandbox.adapters.converter.response_handler import (
    handle_api_error,
    require_parsed,
)
from opensandbox.config.connection_sync import ConnectionConfigSync
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
    SandboxEndpoint,
)
from opensandbox.sync.services.egress import EgressSync

logger = logging.getLogger(__name__)


def _dump_credentials(
    items: list[Credential | dict[str, object]],
) -> list[dict[str, object]]:
    return [
        Credential.model_validate(item).model_dump(by_alias=True, exclude_none=True)
        for item in items
    ]


def _dump_bindings(
    items: list[CredentialBinding | dict[str, object]],
) -> list[dict[str, object]]:
    return [
        CredentialBinding.model_validate(item).model_dump(
            by_alias=True, exclude_none=True
        )
        for item in items
    ]


def _dump_credential_mutations(
    mutations: CredentialMutationSet | dict[str, object] | None,
) -> dict[str, object] | None:
    if mutations is None:
        return None
    parsed = CredentialMutationSet.model_validate(mutations)
    out: dict[str, object] = {}
    if parsed.add is not None:
        out["add"] = _dump_credentials(parsed.add)
    if parsed.replace is not None:
        out["replace"] = _dump_credentials(parsed.replace)
    if parsed.delete is not None:
        out["delete"] = list(parsed.delete)
    return out


def _dump_binding_mutations(
    mutations: CredentialBindingMutationSet | dict[str, object] | None,
) -> dict[str, object] | None:
    if mutations is None:
        return None
    parsed = CredentialBindingMutationSet.model_validate(mutations)
    out: dict[str, object] = {}
    if parsed.add is not None:
        out["add"] = _dump_bindings(parsed.add)
    if parsed.replace is not None:
        out["replace"] = _dump_bindings(parsed.replace)
    if parsed.delete is not None:
        out["delete"] = list(parsed.delete)
    return out


class EgressAdapterSync(EgressSync):
    """Blocking direct egress sidecar adapter using the generated egress client."""

    def __init__(
        self, connection_config: ConnectionConfigSync, endpoint: SandboxEndpoint
    ) -> None:
        self.connection_config = connection_config
        self.endpoint = endpoint
        from opensandbox.api.egress import Client

        base_url = f"{self.connection_config.protocol}://{self.endpoint.endpoint}"
        timeout_seconds = self.connection_config.request_timeout.total_seconds()
        timeout = httpx.Timeout(timeout_seconds)
        headers = {
            "User-Agent": self.connection_config.user_agent,
            **self.connection_config.headers,
            **self.endpoint.headers,
        }

        self._client = Client(
            base_url=base_url,
            timeout=timeout,
        )
        self._httpx_client = httpx.Client(
            base_url=base_url,
            headers=headers,
            timeout=timeout,
            transport=self.connection_config.transport,
        )
        self._client.set_httpx_client(self._httpx_client)

    def _request_json(
        self,
        method: str,
        path: str,
        *,
        operation: str,
        json_body: object | None = None,
    ) -> object:
        response = self._httpx_client.request(method, path, json=json_body)
        if response.status_code >= 400:
            response.raise_for_status()
        if response.status_code == 204 or not response.content:
            return None
        return response.json()

    def create(
        self,
        *,
        credentials: list[Credential | dict[str, object]],
        bindings: list[CredentialBinding | dict[str, object]],
    ) -> CredentialVaultState:
        try:
            body = {
                "credentials": _dump_credentials(credentials),
                "bindings": _dump_bindings(bindings),
            }
            payload = self._request_json(
                "POST",
                "/credential-vault",
                operation="Create credential vault",
                json_body=body,
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.error(
                f"Failed to create credential vault via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get(self) -> CredentialVaultState:
        try:
            payload = self._request_json(
                "GET",
                "/credential-vault",
                operation="Get credential vault",
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.error(
                f"Failed to get credential vault via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def patch(
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
            credential_mutations = _dump_credential_mutations(credentials)
            if credential_mutations is not None:
                body["credentials"] = credential_mutations
            binding_mutations = _dump_binding_mutations(bindings)
            if binding_mutations is not None:
                body["bindings"] = binding_mutations
            payload = self._request_json(
                "PATCH",
                "/credential-vault",
                operation="Patch credential vault",
                json_body=body,
            )
            return CredentialVaultState.model_validate(payload)
        except Exception as e:
            logger.error(
                f"Failed to patch credential vault via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete(self) -> None:
        try:
            self._request_json(
                "DELETE",
                "/credential-vault",
                operation="Delete credential vault",
            )
        except Exception as e:
            logger.error(
                f"Failed to delete credential vault via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_credentials(self) -> list[CredentialMetadata]:
        try:
            payload = self._request_json(
                "GET",
                "/credential-vault/credentials",
                operation="List credential vault credentials",
            )
            return CredentialListResponse.model_validate(payload).credentials
        except Exception as e:
            logger.error(
                f"Failed to list credential vault credentials via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_credential(self, name: str) -> CredentialMetadata:
        try:
            payload = self._request_json(
                "GET",
                f"/credential-vault/credentials/{quote(name, safe='')}",
                operation="Get credential vault credential",
            )
            return CredentialMetadata.model_validate(payload)
        except Exception as e:
            logger.error(
                f"Failed to get credential vault credential via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def list_bindings(self) -> list[CredentialBindingMetadata]:
        try:
            payload = self._request_json(
                "GET",
                "/credential-vault/bindings",
                operation="List credential vault bindings",
            )
            return CredentialBindingListResponse.model_validate(payload).bindings
        except Exception as e:
            logger.error(
                f"Failed to list credential vault bindings via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_binding(self, name: str) -> CredentialBindingMetadata:
        try:
            payload = self._request_json(
                "GET",
                f"/credential-vault/bindings/{quote(name, safe='')}",
                operation="Get credential vault binding",
            )
            return CredentialBindingMetadata.model_validate(payload)
        except Exception as e:
            logger.error(
                f"Failed to get credential vault binding via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def get_policy(self) -> NetworkPolicy:
        try:
            from opensandbox.api.egress.api.policy import get_policy
            from opensandbox.api.egress.models.network_policy import (
                NetworkPolicy as ApiNetworkPolicy,
            )
            from opensandbox.api.egress.models.policy_status_response import (
                PolicyStatusResponse,
            )
            from opensandbox.api.egress.types import Unset

            response_obj = get_policy.sync_detailed(client=self._client)
            handle_api_error(response_obj, "Get egress policy")
            parsed = require_parsed(
                response_obj, PolicyStatusResponse, "Get egress policy"
            )
            policy = parsed.policy
            if isinstance(policy, Unset):
                raise ValueError("Egress policy response missing policy payload")
            if not isinstance(policy, ApiNetworkPolicy):
                raise TypeError(f"Expected NetworkPolicy, got {type(policy).__name__}")
            return NetworkPolicy.model_validate(policy.to_dict())
        except Exception as e:
            logger.error(
                f"Failed to get egress policy from endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def patch_rules(self, rules: list[NetworkRule]) -> None:
        try:
            from opensandbox.api.egress.api.policy import patch_policy
            from opensandbox.api.egress.models.network_rule import (
                NetworkRule as ApiNetworkRule,
            )
            from opensandbox.api.egress.models.network_rule_action import (
                NetworkRuleAction,
            )

            response_obj = patch_policy.sync_detailed(
                client=self._client,
                body=[
                    ApiNetworkRule(
                        action=NetworkRuleAction(rule.action),
                        target=rule.target,
                    )
                    for rule in rules
                ],
            )
            handle_api_error(response_obj, "Patch egress rules")
        except Exception as e:
            logger.error(
                f"Failed to patch egress policy via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e

    def delete_rules(self, targets: list[str]) -> None:
        try:
            from opensandbox.api.egress.api.policy import delete_policy

            response_obj = delete_policy.sync_detailed(
                client=self._client,
                body=list(targets),
            )
            handle_api_error(response_obj, "Delete egress rules")
        except Exception as e:
            logger.error(
                f"Failed to delete egress rules via endpoint {self.endpoint.endpoint}",
                exc_info=e,
            )
            raise ExceptionConverter.to_sandbox_exception(e) from e
