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

"""
API routes for OpenSandbox Lifecycle API.

This module defines FastAPI routes that map to the OpenAPI specification endpoints.
All business logic is delegated to the service layer that backs each operation.
"""

import asyncio
from typing import List, Optional

from fastapi import APIRouter, Body, Header, HTTPException, Query, Request, status
from fastapi.responses import Response

from opensandbox_server.extensions import validate_extensions
from opensandbox_server.config import get_config
from opensandbox_server.api.schema import (
    CreateSnapshotRequest,
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ErrorResponse,
    ListSnapshotsRequest,
    ListSnapshotsResponse,
    ListSandboxesRequest,
    ListSandboxesResponse,
    PaginationRequest,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxFilter,
    Snapshot,
    SnapshotFilter,
)
from opensandbox_server.services.factory import create_sandbox_service
from opensandbox_server.services.snapshot_service import create_snapshot_service
from opensandbox_server.api.lifecycle_helpers import (
    apply_reserved_metadata_for_create,
    authorize_mutating_action,
    authorize_snapshot_scope,
    get_principal,
    log_mutation_audit,
    merge_list_scope_from_request,
    strip_reserved_metadata_from_patch,
)
from opensandbox_server.middleware.authorization import LifecycleAction, authorize_action, is_user_scoped

# Initialize router
router = APIRouter(tags=["Sandboxes"])

# Initialize service based on configuration from config.toml (defaults to docker)
sandbox_service = create_sandbox_service()
snapshot_service = create_snapshot_service(sandbox_service)


# ============================================================================
# Sandbox CRUD Operations
# ============================================================================

@router.post(
    "/sandboxes",
    response_model=CreateSandboxResponse,
    response_model_exclude_none=True,
    status_code=status.HTTP_202_ACCEPTED,
    responses={
        202: {"description": "Sandbox creation accepted for asynchronous provisioning"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def create_sandbox(
    http_request: Request,
    body: CreateSandboxRequest,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> CreateSandboxResponse:
    """
    Create a sandbox from a container image.

    Creates a new sandbox from a container image with optional resource limits,
    environment variables, and metadata. Sandboxes are provisioned directly from
    the specified image without requiring a pre-created template.

    Args:
        body: Sandbox creation request
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        CreateSandboxResponse: Accepted sandbox creation request

    Raises:
        HTTPException: If sandbox creation scheduling fails
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.CREATE,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    body = apply_reserved_metadata_for_create(body, principal, cfg)
    validate_extensions(body.extensions)
    if body.snapshot_id and is_user_scoped(principal):
        snap = snapshot_service.get_snapshot(body.snapshot_id)
        authorize_snapshot_scope(
            principal,
            snap,
            owner_key=cfg.authz.owner_metadata_key,
            team_key=cfg.authz.team_metadata_key,
            sandbox_service=sandbox_service,
        )
    try:
        res = await sandbox_service.create_sandbox(body)
        log_mutation_audit(
            http_request, action=LifecycleAction.CREATE, sandbox_id=res.id, outcome="success"
        )
        return res
    except HTTPException as exc:
        err = exc.detail
        if isinstance(err, dict):
            code = err.get("code")
        else:
            code = None
        log_mutation_audit(
            http_request,
            action=LifecycleAction.CREATE,
            sandbox_id=None,
            outcome="error",
            error_code=code,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.CREATE,
            sandbox_id=None,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise


# Search endpoint
@router.get(
    "/sandboxes",
    response_model=ListSandboxesResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Paginated collection of sandboxes"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def list_sandboxes(
    http_request: Request,
    state: Optional[List[str]] = Query(None, description="Filter by lifecycle state. Pass multiple times for OR logic."),
    metadata: Optional[str] = Query(None, description="Arbitrary metadata key-value pairs for filtering (URL encoded)."),
    page: int = Query(1, ge=1, description="Page number for pagination"),
    page_size: int = Query(20, ge=1, le=200, alias="pageSize", description="Number of items per page"),
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> ListSandboxesResponse:
    """
    List sandboxes with optional filtering and pagination.

    List all sandboxes with optional filtering and pagination using query parameters.
    All filter conditions use AND logic. Multiple `state` parameters use OR logic within states.

    Args:
        state: Filter by lifecycle state.
        metadata: Arbitrary metadata key-value pairs for filtering.
        page: Page number for pagination.
        page_size: Number of items per page.
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        ListSandboxesResponse: Paginated list of sandboxes
    """
    # Parse metadata query string into dictionary
    metadata_dict = {}
    if metadata:
        from urllib.parse import parse_qsl
        try:
            # Parse query string format: key=value&key2=value2
            # strict_parsing=True rejects malformed segments like "a=1&broken"
            parsed = parse_qsl(metadata, keep_blank_values=True, strict_parsing=True)
            metadata_dict = dict(parsed)
        except Exception as e:
            from fastapi import HTTPException
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={"code": "INVALID_METADATA_FORMAT", "message": f"Invalid metadata format: {str(e)}"}
            )

    # Construct request object
    list_req = ListSandboxesRequest(
        filter=SandboxFilter(state=state, metadata=metadata_dict if metadata_dict else None),
        pagination=PaginationRequest(page=page, pageSize=page_size),
    )

    import logging

    logger = logging.getLogger(__name__)
    logger.info("ListSandboxes: %s", list_req.filter)

    cfg = get_config()
    principal = get_principal(http_request)
    authorize_action(
        principal,
        LifecycleAction.LIST_SANDBOXES,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    list_req = merge_list_scope_from_request(http_request, list_req, cfg)

    # Delegate to the service layer for filtering and pagination
    return sandbox_service.list_sandboxes(list_req)


@router.get(
    "/sandboxes/{sandbox_id}",
    response_model=Sandbox,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Sandbox current state and metadata"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def get_sandbox(
    http_request: Request,
    sandbox_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Sandbox:
    """
    Fetch a sandbox by id.

    Returns the complete sandbox information including image specification,
    status, metadata, and timestamps.

    Args:
        sandbox_id: Unique sandbox identifier
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        Sandbox: Complete sandbox information

    Raises:
        HTTPException: If sandbox not found or access denied
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_action(
        principal,
        LifecycleAction.GET_SANDBOX,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    box = sandbox_service.get_sandbox(sandbox_id)
    authorize_action(
        principal,
        LifecycleAction.GET_SANDBOX,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox=box,
    )
    return box


@router.patch(
    "/sandboxes/{sandbox_id}/metadata",
    response_model=Sandbox,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Metadata patched successfully. Returns the complete sandbox with updated metadata."},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def patch_sandbox_metadata(
    http_request: Request,
    sandbox_id: str,
    patch: PatchSandboxMetadataRequest = Body(...),
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Sandbox:
    """
    Patch sandbox metadata via JSON Merge Patch (RFC 7396).
    Non-null adds/replaces, null deletes, absent keeps.
    Read-modify-write without optimistic locking — concurrent PATCH may drop updates.
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.PATCH_METADATA,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    try:
        box = sandbox_service.get_sandbox(sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.PATCH_METADATA,
                sandbox_id=sandbox_id,
                outcome="not_found",
            )
        raise
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.PATCH_METADATA,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    safe_patch = strip_reserved_metadata_from_patch(
        patch,
        principal,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    try:
        result = sandbox_service.patch_sandbox_metadata(sandbox_id, safe_patch)
        log_mutation_audit(
            http_request,
            action=LifecycleAction.PATCH_METADATA,
            sandbox_id=sandbox_id,
            outcome="success",
        )
        return result
    except HTTPException as exc:
        err = exc.detail
        log_mutation_audit(
            http_request,
            action=LifecycleAction.PATCH_METADATA,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=err.get("code") if isinstance(err, dict) else None,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.PATCH_METADATA,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise


@router.delete(
    "/sandboxes/{sandbox_id}",
    status_code=status.HTTP_204_NO_CONTENT,
    responses={
        204: {"description": "Sandbox successfully deleted"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def delete_sandbox(
    http_request: Request,
    sandbox_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Response:
    """
    Delete a sandbox.

    Terminates sandbox execution. The sandbox will transition through Stopping state to Terminated.

    Args:
        sandbox_id: Unique sandbox identifier
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        Response: 204 No Content

    Raises:
        HTTPException: If sandbox not found or deletion fails
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.DELETE,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    try:
        box = sandbox_service.get_sandbox(sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.DELETE,
                sandbox_id=sandbox_id,
                outcome="not_found",
            )
        raise
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.DELETE,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    try:
        sandbox_service.delete_sandbox(sandbox_id)
        log_mutation_audit(
            http_request, action=LifecycleAction.DELETE, sandbox_id=sandbox_id, outcome="success"
        )
    except HTTPException as exc:
        err = exc.detail
        if isinstance(err, dict):
            code = err.get("code")
        else:
            code = None
        log_mutation_audit(
            http_request,
            action=LifecycleAction.DELETE,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=code,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.DELETE,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise
    return Response(status_code=status.HTTP_204_NO_CONTENT)


# ============================================================================
# Sandbox Lifecycle Operations
# ============================================================================

@router.post(
    "/sandboxes/{sandbox_id}/pause",
    status_code=status.HTTP_202_ACCEPTED,
    responses={
        202: {"description": "Pause operation accepted"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def pause_sandbox(
    http_request: Request,
    sandbox_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Response:
    """
    Pause execution while retaining state.

    Pauses a running sandbox while preserving its state.
    Poll GET /sandboxes/{sandboxId} to track state transition through Pausing and eventually Paused.

    Args:
        sandbox_id: Unique sandbox identifier
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        Response: 202 Accepted

    Raises:
        HTTPException: If sandbox not found or cannot be paused
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.PAUSE,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    try:
        box = sandbox_service.get_sandbox(sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.PAUSE,
                sandbox_id=sandbox_id,
                outcome="not_found",
            )
        raise
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.PAUSE,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    try:
        sandbox_service.pause_sandbox(sandbox_id)
        log_mutation_audit(
            http_request, action=LifecycleAction.PAUSE, sandbox_id=sandbox_id, outcome="success"
        )
    except HTTPException as exc:
        err = exc.detail
        if isinstance(err, dict):
            code = err.get("code")
        else:
            code = None
        log_mutation_audit(
            http_request,
            action=LifecycleAction.PAUSE,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=code,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.PAUSE,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise
    return Response(status_code=status.HTTP_202_ACCEPTED)


@router.post(
    "/sandboxes/{sandbox_id}/resume",
    status_code=status.HTTP_202_ACCEPTED,
    responses={
        202: {"description": "Resume operation accepted"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def resume_sandbox(
    http_request: Request,
    sandbox_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Response:
    """
    Resume a paused sandbox.

    Resumes execution of a paused sandbox.
    Poll GET /sandboxes/{sandboxId} to track state transition through Resuming and eventually Running.

    Args:
        sandbox_id: Unique sandbox identifier
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        Response: 202 Accepted

    Raises:
        HTTPException: If sandbox not found or cannot be resumed
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.RESUME,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    try:
        box = sandbox_service.get_sandbox(sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.RESUME,
                sandbox_id=sandbox_id,
                outcome="not_found",
            )
        raise
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.RESUME,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    try:
        sandbox_service.resume_sandbox(sandbox_id)
        log_mutation_audit(
            http_request, action=LifecycleAction.RESUME, sandbox_id=sandbox_id, outcome="success"
        )
    except HTTPException as exc:
        err = exc.detail
        if isinstance(err, dict):
            code = err.get("code")
        else:
            code = None
        log_mutation_audit(
            http_request,
            action=LifecycleAction.RESUME,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=code,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.RESUME,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise
    return Response(status_code=status.HTTP_202_ACCEPTED)


@router.post(
    "/sandboxes/{sandbox_id}/renew-expiration",
    response_model=RenewSandboxExpirationResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Sandbox expiration updated successfully"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def renew_sandbox_expiration(
    http_request: Request,
    sandbox_id: str,
    renew_body: RenewSandboxExpirationRequest,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> RenewSandboxExpirationResponse:
    """
    Renew sandbox expiration.

    Renews the absolute expiration time of a sandbox.
    The new expiration time must be in the future and after the current expiresAt time.

    Args:
        sandbox_id: Unique sandbox identifier
        renew_body: Renewal request with new expiration time
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        RenewSandboxExpirationResponse: Updated expiration time

    Raises:
        HTTPException: If sandbox not found or renewal fails
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.RENEW,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    try:
        box = sandbox_service.get_sandbox(sandbox_id)
    except HTTPException as exc:
        if exc.status_code == status.HTTP_404_NOT_FOUND:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.RENEW,
                sandbox_id=sandbox_id,
                outcome="not_found",
            )
        raise
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.RENEW,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    try:
        res = sandbox_service.renew_expiration(sandbox_id, renew_body)
        log_mutation_audit(
            http_request, action=LifecycleAction.RENEW, sandbox_id=sandbox_id, outcome="success"
        )
        return res
    except HTTPException as exc:
        err = exc.detail
        if isinstance(err, dict):
            code = err.get("code")
        else:
            code = None
        log_mutation_audit(
            http_request,
            action=LifecycleAction.RENEW,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=code,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.RENEW,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise


# ============================================================================
# Snapshot Operations
# ============================================================================

@router.post(
    "/sandboxes/{sandbox_id}/snapshots",
    tags=["Snapshots"],
    response_model=Snapshot,
    response_model_exclude_none=True,
    status_code=status.HTTP_202_ACCEPTED,
    responses={
        202: {"description": "Snapshot creation accepted"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The operation conflicts with the current state"},
        501: {"model": ErrorResponse, "description": "Snapshot management is not implemented yet"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def create_snapshot(
    http_request: Request,
    sandbox_id: str,
    response: Response,
    request: Optional[CreateSnapshotRequest] = None,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Snapshot:
    """
    Create a persistent point-in-time snapshot from a sandbox.
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.CREATE_SNAPSHOT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
    )
    box = sandbox_service.get_sandbox(sandbox_id)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.CREATE_SNAPSHOT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=sandbox_id,
        sandbox=box,
    )
    create_request = request or CreateSnapshotRequest()
    # Derive snapshot scope from the source sandbox's metadata so every snapshot
    # carries ownership data regardless of whether the creator is a user principal
    # or a service-admin API key.
    box_meta: dict = {}
    if isinstance(box, dict):
        box_meta = box.get("metadata") or {}
    else:
        box_meta = box.metadata or {}
    snap_access_owner = box_meta.get(cfg.authz.owner_metadata_key) or None
    snap_access_team = box_meta.get(cfg.authz.team_metadata_key) or None
    try:
        snapshot = await asyncio.to_thread(
            snapshot_service.create_snapshot,
            sandbox_id,
            create_request,
            access_owner=snap_access_owner,
            access_team=snap_access_team,
        )
        log_mutation_audit(
            http_request, action=LifecycleAction.CREATE_SNAPSHOT, sandbox_id=sandbox_id, outcome="success"
        )
    except HTTPException as exc:
        err = exc.detail
        log_mutation_audit(
            http_request,
            action=LifecycleAction.CREATE_SNAPSHOT,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code=err.get("code") if isinstance(err, dict) else None,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.CREATE_SNAPSHOT,
            sandbox_id=sandbox_id,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise
    response.headers["Location"] = f"/v1/snapshots/{snapshot.id}"
    return snapshot


@router.get(
    "/snapshots",
    tags=["Snapshots"],
    response_model=ListSnapshotsResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Paginated collection of snapshots"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        501: {"model": ErrorResponse, "description": "Snapshot management is not implemented yet"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def list_snapshots(
    http_request: Request,
    sandbox_id: Optional[str] = Query(None, alias="sandboxId", description="Filter snapshots by source sandbox identifier"),
    state: Optional[List[str]] = Query(None, description="Filter by snapshot lifecycle state. Pass multiple times for OR logic."),
    page: int = Query(1, ge=1, description="Page number for pagination"),
    page_size: int = Query(20, ge=1, le=200, alias="pageSize", description="Number of items per page"),
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> ListSnapshotsResponse:
    """
    List snapshots with optional filtering and pagination.
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_action(
        principal,
        LifecycleAction.LIST_SNAPSHOTS,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )

    snap_access_owner: Optional[str] = None
    snap_access_team: Optional[str] = None
    if is_user_scoped(principal):
        # Scope to the caller's own snapshots via stored ownership metadata.
        # A live sandbox lookup is intentionally avoided: snapshots outlive their
        # source sandbox and must remain listable after it is deleted.
        snap_access_owner = principal.canonical_owner
        snap_access_team = principal.canonical_team

    request = ListSnapshotsRequest(
        filter=SnapshotFilter(sandboxId=sandbox_id, state=state),
        pagination=PaginationRequest(page=page, pageSize=page_size),
    )
    return snapshot_service.list_snapshots(
        request,
        access_owner=snap_access_owner,
        access_team=snap_access_team,
    )


@router.get(
    "/snapshots/{snapshot_id}",
    tags=["Snapshots"],
    response_model=Snapshot,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Snapshot current state and metadata"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        501: {"model": ErrorResponse, "description": "Snapshot management is not implemented yet"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def get_snapshot(
    http_request: Request,
    snapshot_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Snapshot:
    """
    Fetch a snapshot by id.
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_action(
        principal,
        LifecycleAction.GET_SNAPSHOT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    snap = snapshot_service.get_snapshot(snapshot_id)
    authorize_snapshot_scope(
        principal,
        snap,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_service=sandbox_service,
    )
    return snap


@router.delete(
    "/snapshots/{snapshot_id}",
    tags=["Snapshots"],
    status_code=status.HTTP_204_NO_CONTENT,
    responses={
        204: {"description": "Snapshot successfully deleted"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        409: {"model": ErrorResponse, "description": "The snapshot is not in a deletable state or is still in use"},
        501: {"model": ErrorResponse, "description": "Snapshot management is not implemented yet"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def delete_snapshot(
    http_request: Request,
    snapshot_id: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Response:
    """
    Delete a snapshot by id.
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_mutating_action(
        http_request,
        principal,
        LifecycleAction.DELETE_SNAPSHOT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox_id=None,
    )
    if is_user_scoped(principal):
        try:
            snap = snapshot_service.get_snapshot(snapshot_id)
        except HTTPException as exc:
            if exc.status_code == status.HTTP_404_NOT_FOUND:
                log_mutation_audit(
                    http_request,
                    action=LifecycleAction.DELETE_SNAPSHOT,
                    sandbox_id=None,
                    outcome="not_found",
                )
            raise
        try:
            authorize_snapshot_scope(
                principal,
                snap,
                owner_key=cfg.authz.owner_metadata_key,
                team_key=cfg.authz.team_metadata_key,
                sandbox_service=sandbox_service,
            )
        except HTTPException:
            log_mutation_audit(
                http_request,
                action=LifecycleAction.DELETE_SNAPSHOT,
                sandbox_id=None,
                outcome="forbidden",
            )
            raise
    try:
        snapshot_service.delete_snapshot(snapshot_id)
        log_mutation_audit(
            http_request, action=LifecycleAction.DELETE_SNAPSHOT, sandbox_id=None, outcome="success"
        )
    except HTTPException as exc:
        err = exc.detail
        log_mutation_audit(
            http_request,
            action=LifecycleAction.DELETE_SNAPSHOT,
            sandbox_id=None,
            outcome="error",
            error_code=err.get("code") if isinstance(err, dict) else None,
        )
        raise
    except Exception:
        log_mutation_audit(
            http_request,
            action=LifecycleAction.DELETE_SNAPSHOT,
            sandbox_id=None,
            outcome="error",
            error_code="UNEXPECTED",
        )
        raise
    return Response(status_code=status.HTTP_204_NO_CONTENT)


# ============================================================================
# Sandbox Endpoints
# ============================================================================

@router.get(
    "/sandboxes/{sandbox_id}/endpoints/{port}",
    response_model=Endpoint,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Endpoint retrieved successfully"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        403: {"model": ErrorResponse, "description": "The authenticated user lacks permission for this operation"},
        404: {"model": ErrorResponse, "description": "The requested resource does not exist"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
async def get_sandbox_endpoint(
    http_request: Request,
    sandbox_id: str,
    port: int,
    use_server_proxy: bool = Query(False, description="Whether to return a server-proxied URL"),
    expires: Optional[int] = Query(None, description="Request a signed route token with this Unix epoch second expiration. Requires ingress gateway with secure_access configured."),
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID", description="Unique request identifier for tracing"),
) -> Endpoint:
    """
    Get sandbox access endpoint.

    Returns the public access endpoint URL for accessing a service running on a specific port
    within the sandbox. The service must be listening on the specified port inside the sandbox
    for the endpoint to be available.

    When the ``expires`` query parameter is provided, the endpoint is wrapped in a
    cryptographically signed route token (OSEP-0011) instead of returning a plain URL.
    This requires the ingress gateway to be configured with secure_access signing keys.

    Args:
        http_request: FastAPI request object
        sandbox_id: Unique sandbox identifier
        port: Port number where the service is listening inside the sandbox (1-65535)
        use_server_proxy: Whether to return a server-proxied URL
        expires: Unix epoch seconds for signed route token expiration. Must be a
            non-negative uint64 value. When omitted or invalid, a plain (unsigned)
            endpoint is returned.
        x_request_id: Unique request identifier for tracing (optional; server generates if omitted).

    Returns:
        Endpoint: Public endpoint URL

    Raises:
        HTTPException: If sandbox not found, endpoint not available, or signed
            routes are not supported by the runtime/configuration (400).
    """
    cfg = get_config()
    principal = get_principal(http_request)
    authorize_action(
        principal,
        LifecycleAction.GET_ENDPOINT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
    )
    box = sandbox_service.get_sandbox(sandbox_id)
    authorize_action(
        principal,
        LifecycleAction.GET_ENDPOINT,
        owner_key=cfg.authz.owner_metadata_key,
        team_key=cfg.authz.team_metadata_key,
        sandbox=box,
    )
    # Delegate to the service layer for endpoint resolution
    endpoint = sandbox_service.get_endpoint(sandbox_id, port, expires=expires)

    if use_server_proxy:
        # Prefer configured external address when available.
        base_url = str(http_request.base_url).rstrip("/")
        eip = (get_config().server.eip or "").strip().rstrip("/")
        if eip:
            base_url = eip
        base_url = base_url.replace("https://", "").replace("http://", "")
        endpoint.endpoint = f"{base_url}/sandboxes/{sandbox_id}/proxy/{port}"

    return endpoint
