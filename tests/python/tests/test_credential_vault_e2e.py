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
"""Credential Vault E2E tests against a local OpenSandbox server."""

from __future__ import annotations

import json
import os
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.models.sandboxes import (
    Credential,
    CredentialBinding,
    CredentialProxyConfig,
    NetworkPolicy,
    NetworkRule,
    SandboxImageSpec,
)

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

TARGET_HOST = os.getenv(
    "OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_HOST",
    "credential-vault-e2e.opensandbox.test",
)
TARGET_IP = os.getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP", "")
E2E_LABEL_KEY = os.getenv(
    "OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_KEY", "opensandbox.e2e"
)
E2E_LABEL_VALUE = os.getenv(
    "OPENSANDBOX_CREDENTIAL_VAULT_E2E_LABEL_VALUE", "credential-vault"
)

SECRET_VALUES = {
    "bearer-token": "vault-bearer-token",
    "basic-token": "dXNlcjpwYXNz",
    "api-key-token": "vault-api-key-token",
    "client-id": "vault-client-id",
    "client-secret": "vault-client-secret",
    "runtime-token": "vault-runtime-token",
    "runtime-token-replaced": "vault-runtime-token-replaced",
}


@pytest.fixture(scope="module")
def credential_vault_target_ip() -> str:
    if not TARGET_IP:
        pytest.skip("Set OPENSANDBOX_CREDENTIAL_VAULT_E2E_TARGET_IP to run this E2E")
    return TARGET_IP


def test_credential_vault_injects_all_auth_types(
    credential_vault_target_ip: str,
) -> None:
    cfg, sandbox = _create_credential_proxy_sandbox(credential_vault_target_ip)
    try:
        state = sandbox.credential_vault.create(
            credentials=[
                Credential(name=name, source={"value": value})
                for name, value in SECRET_VALUES.items()
            ],
            bindings=[
                _binding(
                    "bearer",
                    "/bearer",
                    {"type": "bearer", "credential": "bearer-token"},
                ),
                _binding(
                    "basic",
                    "/basic",
                    {"type": "basic", "credential": "basic-token"},
                ),
                _binding(
                    "api-key",
                    "/api-key",
                    {
                        "type": "apiKey",
                        "name": "X-Api-Key",
                        "credential": "api-key-token",
                    },
                ),
                _binding(
                    "custom-headers",
                    "/custom-headers",
                    {
                        "type": "customHeaders",
                        "headers": [
                            {"name": "X-Client-Id", "credential": "client-id"},
                            {"name": "X-Client-Secret", "credential": "client-secret"},
                        ],
                    },
                ),
            ],
        )

        state_payload = state.model_dump_json(by_alias=True)
        for secret in SECRET_VALUES.values():
            assert secret not in state_payload
        assert {binding.auth.type for binding in state.bindings if binding.auth} == {
            "bearer",
            "basic",
            "apiKey",
            "customHeaders",
        }

        for path in ["/bearer", "/basic", "/api-key", "/custom-headers"]:
            response = _curl_json(sandbox, credential_vault_target_ip, path)
            assert response["ok"] is True
            assert response["case"] == path.lstrip("/")
            assert response["missingOrInvalid"] == []
    finally:
        _close_sandbox(cfg, sandbox)


def test_credential_vault_runtime_mutation_adds_replaces_and_deletes_binding(
    credential_vault_target_ip: str,
) -> None:
    cfg, sandbox = _create_credential_proxy_sandbox(credential_vault_target_ip)
    try:
        state = sandbox.credential_vault.create(credentials=[], bindings=[])
        assert state.revision == 1
        assert state.credentials == []
        assert state.bindings == []

        state = sandbox.credential_vault.patch(
            expected_revision=state.revision,
            credentials={
                "add": [
                    {
                        "name": "runtime-token",
                        "source": {"value": SECRET_VALUES["runtime-token"]},
                    }
                ]
            },
            bindings={
                "add": [
                    _binding(
                        "runtime-added",
                        "/runtime-added",
                        {
                            "type": "apiKey",
                            "name": "X-Runtime-Token",
                            "credential": "runtime-token",
                        },
                    )
                ]
            },
        )
        assert state.revision == 2
        assert [credential.name for credential in state.credentials] == ["runtime-token"]
        assert [binding.name for binding in state.bindings] == ["runtime-added"]
        assert SECRET_VALUES["runtime-token"] not in state.model_dump_json(by_alias=True)

        response = _curl_json(sandbox, credential_vault_target_ip, "/runtime-added")
        assert response["ok"] is True
        assert response["case"] == "runtime-added"
        assert response["missingOrInvalid"] == []

        state = sandbox.credential_vault.patch(
            expected_revision=state.revision,
            bindings={"delete": ["runtime-added"]},
        )
        assert state.revision == 3
        assert state.bindings == []

        state = sandbox.credential_vault.patch(
            expected_revision=state.revision,
            credentials={
                "replace": [
                    {
                        "name": "runtime-token",
                        "source": {"value": SECRET_VALUES["runtime-token-replaced"]},
                    }
                ]
            },
            bindings={
                "add": [
                    _binding(
                        "runtime-replaced",
                        "/runtime-replaced",
                        {
                            "type": "apiKey",
                            "name": "X-Runtime-Token",
                            "credential": "runtime-token",
                        },
                    )
                ]
            },
        )
        assert state.revision == 4
        assert [credential.name for credential in state.credentials] == ["runtime-token"]
        assert [binding.name for binding in state.bindings] == ["runtime-replaced"]
        state_payload = state.model_dump_json(by_alias=True)
        assert SECRET_VALUES["runtime-token"] not in state_payload
        assert SECRET_VALUES["runtime-token-replaced"] not in state_payload

        response = _curl_json(sandbox, credential_vault_target_ip, "/runtime-replaced")
        assert response["ok"] is True
        assert response["case"] == "runtime-replaced"
        assert response["missingOrInvalid"] == []

        response = _curl_json(
            sandbox,
            credential_vault_target_ip,
            "/runtime-added",
            fail_on_http_error=False,
        )
        assert response["ok"] is False
        assert response["case"] == "runtime-added"
        assert response["missingOrInvalid"] == ["x-runtime-token"]

        state = sandbox.credential_vault.patch(
            expected_revision=state.revision,
            bindings={"delete": ["runtime-replaced"]},
        )
        assert state.revision == 5
        assert state.bindings == []

        state = sandbox.credential_vault.patch(
            expected_revision=state.revision,
            credentials={"delete": ["runtime-token"]},
        )
        assert state.revision == 6
        assert state.credentials == []
    finally:
        _close_sandbox(cfg, sandbox)


def _create_credential_proxy_sandbox(target_ip: str) -> tuple[object, SandboxSync]:
    cfg = create_connection_config_sync()
    sandbox = SandboxSync.create(
        image=SandboxImageSpec(
            os.getenv("OPENSANDBOX_CREDENTIAL_VAULT_E2E_SANDBOX_IMAGE", get_sandbox_image())
        ),
        resource=get_e2e_sandbox_resource(),
        connection_config=cfg,
        timeout=timedelta(minutes=5),
        ready_timeout=timedelta(seconds=60),
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[
                NetworkRule(action="allow", target=TARGET_HOST),
                NetworkRule(action="allow", target=target_ip),
            ],
        ),
        credential_proxy=CredentialProxyConfig(enabled=True),
        metadata={E2E_LABEL_KEY: E2E_LABEL_VALUE},
    )
    return cfg, sandbox


def _close_sandbox(cfg: object, sandbox: SandboxSync) -> None:
    try:
        sandbox.kill()
    finally:
        sandbox.close()
        try:
            cfg.transport.close()
        except Exception:
            # Best-effort teardown: do not fail the test if transport is already closed
            # or cannot be closed during cleanup.
            pass


def _binding(name: str, path: str, auth: dict[str, object]) -> CredentialBinding:
    return CredentialBinding(
        name=name,
        match={
            "schemes": ["http"],
            "ports": [80],
            "hosts": [TARGET_HOST],
            "methods": ["GET"],
            "paths": [path],
        },
        auth=auth,
    )


def _curl_json(
    sandbox: SandboxSync,
    target_ip: str,
    path: str,
    *,
    fail_on_http_error: bool = True,
) -> dict[str, object]:
    fail_flag = "--fail " if fail_on_http_error else ""
    command = (
        f"curl {fail_flag}--silent --show-error "
        "--connect-timeout 5 --max-time 20 "
        f"--resolve {TARGET_HOST}:80:{target_ip} "
        f"http://{TARGET_HOST}{path}"
    )
    for secret in SECRET_VALUES.values():
        assert secret not in command

    result = sandbox.commands.run(command)
    assert result.error is None, result.error
    stdout = "".join(part.text for part in result.logs.stdout)
    assert stdout
    return json.loads(stdout)
