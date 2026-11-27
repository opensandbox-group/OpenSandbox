# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

"""Read/replace/patch policy intent without exposing the Fastlet's loopback handler."""

import asyncio

from fastapi import APIRouter, HTTPException, Request

from opensandbox_server.api import lifecycle
from opensandbox_server.api.proxy import _proxy_http_request
from opensandbox_server.api.schema import NetworkPolicy, NetworkRule
from opensandbox_server.config import get_config
from opensandbox_server.services.composite_service import CompositeSandboxService
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.fast_sandbox import FastSandboxService
from opensandbox_server.services.helpers import is_egress_forward_mode

router = APIRouter(tags=["Sandboxes"])


def _fsb_service():
    service = lifecycle.sandbox_service
    if not isinstance(service, (FastSandboxService, CompositeSandboxService)):
        raise HTTPException(
            404,
            detail={
                "code": SandboxErrorCodes.FSB_SANDBOX_NOT_FOUND,
                "message": "Fsb sandbox not found.",
            },
        )
    return service


def _reject_in_forward_mode() -> None:
    """Forward mode (OSEP-0030) has no local policy to replace or patch."""
    if is_egress_forward_mode(get_config().egress):
        raise HTTPException(
            400,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    'networkPolicy cannot be replaced or patched when egress.mode = "forward"; '
                    "egress policy is enforced by the upstream gateway."
                ),
            },
        )


@router.get("/sandboxes/{sandbox_id}/networkpolicy")
async def get_network_policy(request: Request, sandbox_id: str):
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().get_network_policy, sandbox_id)
    return await _proxy_http_request(request, sandbox_id, 18080, "policy", internal=True)


@router.put("/sandboxes/{sandbox_id}/networkpolicy")
async def replace_network_policy(request: Request, sandbox_id: str, policy: NetworkPolicy):
    _reject_in_forward_mode()
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().replace_network_policy, sandbox_id, policy)
    return await _proxy_http_request(request, sandbox_id, 18080, "policy", internal=True)


@router.patch("/sandboxes/{sandbox_id}/networkpolicy")
async def patch_network_policy(request: Request, sandbox_id: str, rules: list[NetworkRule]):
    """Replace matching (case-insensitive target, sorted ports) scopes; first patch rule wins, defaultAction stays."""
    _reject_in_forward_mode()
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().patch_network_policy, sandbox_id, rules)
    return await _proxy_http_request(request, sandbox_id, 18080, "policy", internal=True)


@router.delete("/sandboxes/{sandbox_id}/networkpolicy")
async def delete_network_policy(request: Request, sandbox_id: str, targets: list[str]):
    """Remove rules by target (idempotent); the current defaultAction is preserved."""
    if sandbox_id.startswith("fsb-"):
        return await asyncio.to_thread(_fsb_service().delete_network_policy_rules, sandbox_id, targets)
    return await _proxy_http_request(request, sandbox_id, 18080, "policy", internal=True)
