# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

"""Shared helpers for lifecycle routes: scoping, reserved metadata, audit logging."""

from __future__ import annotations

import logging
from typing import Optional

from fastapi import Request, status
from fastapi.exceptions import HTTPException

from opensandbox_server.api.schema import CreateSandboxRequest, ListSandboxesRequest, SandboxFilter
from opensandbox_server.config import AppConfig
from opensandbox_server.middleware.request_id import get_request_id
from opensandbox_server.middleware.authorization import authorize_action, is_user_scoped, sandbox_in_scope
from opensandbox_server.middleware.principal import Principal

logger = logging.getLogger(__name__)


def get_principal(request: Request) -> Optional[Principal]:
    return getattr(request.state, "principal", None)


def merge_list_scope_from_request(http_request: Request, body: ListSandboxesRequest, config: AppConfig) -> ListSandboxesRequest:
    """AND server-side owner/team scope into list metadata filters for user principals."""
    return _merge_list_scope_inner(body, get_principal(http_request), config)


def _merge_list_scope_inner(
    request: ListSandboxesRequest,
    principal: Optional[Principal],
    config: AppConfig,
) -> ListSandboxesRequest:
    if not is_user_scoped(principal):
        return request
    assert principal is not None
    owner_k = config.authz.owner_metadata_key
    team_k = config.authz.team_metadata_key
    meta = dict(request.filter.metadata or {})
    meta[owner_k] = principal.canonical_owner
    if principal.canonical_team is not None:
        meta[team_k] = principal.canonical_team
    new_filter = SandboxFilter(
        state=request.filter.state,
        metadata=meta,
    )
    return ListSandboxesRequest(filter=new_filter, pagination=request.pagination)


def apply_reserved_metadata_for_create(
    req: CreateSandboxRequest,
    principal: Optional[Principal],
    config: AppConfig,
) -> CreateSandboxRequest:
    if not is_user_scoped(principal):
        return req
    assert principal is not None
    meta = dict(req.metadata or {})
    meta[config.authz.owner_metadata_key] = principal.canonical_owner
    if principal.canonical_team is not None:
        meta[config.authz.team_metadata_key] = principal.canonical_team
    return req.model_copy(update={"metadata": meta})


def authorize_snapshot_scope(
    principal: Optional[Principal],
    snapshot,
    *,
    owner_key: str,
    team_key: str,
    sandbox_service,
) -> None:
    """Enforce owner/team scope for a snapshot.

    Scope is checked using the snapshot's persisted access_owner/access_team
    fields (populated at snapshot-create time) so that the check remains valid
    even after the source sandbox has been deleted.

    For snapshots that pre-date scope metadata (access_owner is None), the check
    falls back to resolving the source sandbox.  If the source sandbox no longer
    exists we allow access: we cannot prove a mismatch and snapshots must remain
    reachable after sandbox teardown.

    Service admins and API-key-only principals always pass through.
    """
    if not is_user_scoped(principal):
        return

    _OUT_OF_SCOPE = HTTPException(
        status_code=status.HTTP_403_FORBIDDEN,
        detail={
            "code": "OUT_OF_SCOPE",
            "message": "The snapshot is outside the authenticated user owner/team scope.",
        },
    )

    if snapshot.access_owner is not None:
        # Snapshot has stored scope metadata — compare directly without a live
        # sandbox lookup, so deleted source sandboxes never block access.
        if snapshot.access_owner.strip() != principal.canonical_owner:
            raise _OUT_OF_SCOPE
        if principal.canonical_team is not None:
            if (snapshot.access_team or "").strip() != principal.canonical_team:
                raise _OUT_OF_SCOPE
        return

    # Legacy snapshot without stored scope metadata: resolve via source sandbox.
    try:
        box = sandbox_service.get_sandbox(snapshot.sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            # Source sandbox deleted and no stored scope — cannot prove a mismatch; allow.
            return
        raise  # 500 / 503 / etc. — fail closed rather than exposing data
    if not sandbox_in_scope(principal, box, owner_key, team_key):
        raise _OUT_OF_SCOPE


def authorize_mutating_action(
    request: Request,
    principal: Optional[Principal],
    action: str,
    *,
    owner_key: str,
    team_key: str,
    sandbox_id: Optional[str] = None,
    sandbox=None,
) -> None:
    """Calls authorize_action and emits a mutation_audit entry when 403 is raised."""
    try:
        authorize_action(principal, action, owner_key=owner_key, team_key=team_key, sandbox=sandbox)
    except HTTPException:
        log_mutation_audit(request, action=action, sandbox_id=sandbox_id, outcome="forbidden")
        raise


def strip_reserved_metadata_from_patch(
    patch: dict,
    principal: Optional[Principal],
    *,
    owner_key: str,
    team_key: str,
) -> dict:
    """Remove reserved scope keys from a metadata patch for non-service-admin principals.

    Prevents user principals from altering access.owner / access.team through
    the PATCH endpoint, which would let them escape their own scope boundary.
    """
    if principal is None or principal.is_service_admin:
        return patch
    return {k: v for k, v in patch.items() if k not in (owner_key, team_key)}


def log_mutation_audit(
    request: Request,
    *,
    action: str,
    sandbox_id: Optional[str],
    outcome: str,
    error_code: Optional[str] = None,
) -> None:
    principal = get_principal(request)
    rid = get_request_id() or request.headers.get("X-Request-ID") or "-"
    subj = getattr(principal, "subject", None) if principal else None
    team = getattr(principal, "canonical_team", None) if principal else None
    role = getattr(principal, "role", None) if principal else None
    src = getattr(principal, "source", None) if principal else None
    logger.info(
        "mutation_audit request_id=%s action=%s sandbox_id=%s outcome=%s error_code=%s "
        "principal_source=%s principal_subject=%s principal_team=%s principal_role=%s",
        rid,
        action,
        sandbox_id,
        outcome,
        error_code,
        src,
        subj,
        team,
        role,
    )
