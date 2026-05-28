# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

"""
Lifecycle action authorization: role matrix + owner/team scope (OSEP-0006).
"""

from __future__ import annotations

import logging
from typing import TYPE_CHECKING, Optional

from fastapi import status
from fastapi.exceptions import HTTPException

from opensandbox_server.middleware.principal import Principal

logger = logging.getLogger(__name__)

if TYPE_CHECKING:
    from opensandbox_server.api.schema import Sandbox

ActionName = str


# Actions match OSEP-0006 lifecycle surface.
class LifecycleAction:
    LIST_SANDBOXES = "list_sandboxes"
    GET_SANDBOX = "get_sandbox"
    GET_ENDPOINT = "get_endpoint"
    CREATE = "create_sandbox"
    RENEW = "renew_expiration"
    DELETE = "delete_sandbox"
    PAUSE = "pause_sandbox"
    RESUME = "resume_sandbox"
    LIST_SNAPSHOTS = "list_snapshots"
    GET_SNAPSHOT = "get_snapshot"
    CREATE_SNAPSHOT = "create_snapshot"
    DELETE_SNAPSHOT = "delete_snapshot"
    PATCH_METADATA = "patch_metadata"


_READ_ONLY = {
    LifecycleAction.LIST_SANDBOXES,
    LifecycleAction.GET_SANDBOX,
    LifecycleAction.GET_ENDPOINT,
    LifecycleAction.LIST_SNAPSHOTS,
    LifecycleAction.GET_SNAPSHOT,
}
_OPERATOR = _READ_ONLY | {
    LifecycleAction.CREATE,
    LifecycleAction.RENEW,
    LifecycleAction.DELETE,
    LifecycleAction.PAUSE,
    LifecycleAction.RESUME,
    LifecycleAction.CREATE_SNAPSHOT,
    LifecycleAction.DELETE_SNAPSHOT,
    LifecycleAction.PATCH_METADATA,
}


def _actions_for_role(role: str) -> set[str] | None:
    if role == "service_admin":
        return None  # all allowed; caller checks
    if role == "operator":
        return set(_OPERATOR)
    if role == "read_only":
        return set(_READ_ONLY)
    return set()


def _scope_match(
    principal: Principal,
    owner_key: str,
    team_key: str,
    metadata: Optional[dict[str, str]],
) -> bool:
    if principal.source not in ("user",):
        return True
    meta = metadata or {}
    got_owner = (meta.get(owner_key) or "").strip()
    if got_owner != principal.canonical_owner:
        return False
    if principal.canonical_team is not None:
        if (meta.get(team_key) or "").strip() != principal.canonical_team:
            return False
    return True


def sandbox_in_scope(
    principal: Optional[Principal],
    sandbox: "Sandbox | dict",
    owner_key: str,
    team_key: str,
) -> bool:
    if principal is None or principal.is_service_admin:
        return True
    if isinstance(sandbox, dict):
        metadata = sandbox.get("metadata")
    else:
        metadata = sandbox.metadata
    return _scope_match(principal, owner_key, team_key, metadata)


def authorize_action(
    principal: Optional[Principal],
    action: str,
    *,
    owner_key: str,
    team_key: str,
    sandbox: Optional["Sandbox | dict"] = None,
) -> None:
    """
    Raise ``HTTPException``(403) if the action is not allowed for this principal, or
    the sandbox (when provided) is out of owner/team scope.
    Unauthenticated (``None``) principals are only allowed in dev mode (no API key configured);
    the lifecycle layer should treat that as allow-all to preserve legacy tests.
    """
    if principal is None:
        # Dev mode: no API key configured. Log so misconfigured production deployments are visible.
        logger.debug("authorize_action called with no principal (dev/open mode) — action=%s", action)
        return
    if principal.is_service_admin:
        return
    allowed = _actions_for_role(principal.role)
    if allowed is not None and action not in allowed:
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={
                "code": "INSUFFICIENT_ROLE",
                "message": f"Role '{principal.role}' is not allowed to perform this operation.",
            },
        )
    if sandbox is not None and not sandbox_in_scope(principal, sandbox, owner_key, team_key):
        raise HTTPException(
            status_code=status.HTTP_403_FORBIDDEN,
            detail={
                "code": "OUT_OF_SCOPE",
                "message": "The sandbox is outside the authenticated user owner/team scope.",
            },
        )


def is_user_scoped(principal: Optional[Principal]) -> bool:
    return bool(principal and principal.source == "user" and not principal.is_service_admin)
