# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Server-routed Credential Vault API (OSEP-0022), dual-mode like networkpolicy.

``fsb-`` sandboxes relay to the egress-held vault over the FastPath handler
relay (FastSandboxService); every other backend proxies the sidecar's
``/credential-vault`` surface (18080, ``internal=True`` so the egress-auth
token rides along), preserving ``specs/egress-api.yaml`` semantics verbatim.
"""

import asyncio
from urllib.parse import quote

from fastapi import APIRouter, Request
from fastapi.responses import Response

from opensandbox_server.api import lifecycle
from opensandbox_server.api.proxy import _proxy_http_request
from opensandbox_server.api.schema import (
    CredentialBindingListResponse,
    CredentialBindingMetadata,
    CredentialListResponse,
    CredentialMetadata,
    CredentialVaultCreateRequest,
    CredentialVaultMutationRequest,
    CredentialVaultState,
)
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.fast_sandbox import FastSandboxService

router = APIRouter(tags=["Sandboxes"])


def _fsb_service():
    service = lifecycle.sandbox_service
    if not isinstance(service, (FastSandboxService, CompositeSandboxService)):
        from fastapi import HTTPException

        raise HTTPException(404, detail="Fsb sandbox not found.")
    return service


def _quote_segment(name: str) -> str:
    return quote(name, safe="")


@router.post("/sandboxes/{sandbox_id}/credential-vault", status_code=201, response_model=CredentialVaultState, response_model_exclude_none=True)
async def create_credential_vault(
    request: Request, sandbox_id: str, body: CredentialVaultCreateRequest
):
    """Create the initial Credential Vault revision (create-only)."""
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().create_credential_vault, sandbox_id, body)
    return await _proxy_http_request(request, sandbox_id, 18080, "credential-vault", internal=True)


@router.get("/sandboxes/{sandbox_id}/credential-vault", response_model=CredentialVaultState, response_model_exclude_none=True)
async def get_credential_vault(request: Request, sandbox_id: str):
    """Sanitized Credential Vault state (values never appear)."""
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().get_credential_vault, sandbox_id)
    return await _proxy_http_request(request, sandbox_id, 18080, "credential-vault", internal=True)


@router.patch("/sandboxes/{sandbox_id}/credential-vault", response_model=CredentialVaultState, response_model_exclude_none=True)
async def patch_credential_vault(
    request: Request, sandbox_id: str, body: CredentialVaultMutationRequest
):
    """Atomically mutate credentials and bindings (server-side merge commit)."""
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().patch_credential_vault, sandbox_id, body)
    return await _proxy_http_request(request, sandbox_id, 18080, "credential-vault", internal=True)


@router.delete("/sandboxes/{sandbox_id}/credential-vault", status_code=204)
async def delete_credential_vault(request: Request, sandbox_id: str):
    if sandbox_id.startswith("fsb-"):
        await asyncio.to_thread(_fsb_service().delete_credential_vault, sandbox_id)
        return Response(status_code=204)
    return await _proxy_http_request(request, sandbox_id, 18080, "credential-vault", internal=True)


@router.get("/sandboxes/{sandbox_id}/credential-vault/credentials", response_model=CredentialListResponse, response_model_exclude_none=True)
async def list_credential_vault_credentials(request: Request, sandbox_id: str):
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(
            _fsb_service().list_credential_vault_credentials, sandbox_id
        )
    return await _proxy_http_request(
        request, sandbox_id, 18080, "credential-vault/credentials", internal=True
    )


@router.get("/sandboxes/{sandbox_id}/credential-vault/credentials/{credential_name}", response_model=CredentialMetadata, response_model_exclude_none=True)
async def get_credential_vault_credential(
    request: Request, sandbox_id: str, credential_name: str
):
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(
            _fsb_service().get_credential_vault_credential, sandbox_id, credential_name
        )
    return await _proxy_http_request(
        request,
        sandbox_id,
        18080,
        f"credential-vault/credentials/{_quote_segment(credential_name)}",
        internal=True,
    )


@router.get("/sandboxes/{sandbox_id}/credential-vault/bindings", response_model=CredentialBindingListResponse, response_model_exclude_none=True)
async def list_credential_vault_bindings(request: Request, sandbox_id: str):
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().list_credential_vault_bindings, sandbox_id)
    return await _proxy_http_request(
        request, sandbox_id, 18080, "credential-vault/bindings", internal=True
    )


@router.get("/sandboxes/{sandbox_id}/credential-vault/bindings/{binding_name}", response_model=CredentialBindingMetadata, response_model_exclude_none=True)
async def get_credential_vault_binding(request: Request, sandbox_id: str, binding_name: str):
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(
            _fsb_service().get_credential_vault_binding, sandbox_id, binding_name
        )
    return await _proxy_http_request(
        request,
        sandbox_id,
        18080,
        f"credential-vault/bindings/{_quote_segment(binding_name)}",
        internal=True,
    )
