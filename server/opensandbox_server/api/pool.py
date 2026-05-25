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
API routes for Pool resource management.

Pools are pre-warmed sets of sandbox pods that reduce cold-start latency.
These endpoints are only available when the runtime is configured as 'kubernetes'.
"""

from typing import Optional

from fastapi import APIRouter, Header, status
from fastapi.exceptions import HTTPException
from fastapi.responses import Response

from opensandbox_server.api.schema import (
    CreatePoolRequest,
    ErrorResponse,
    ListPoolsResponse,
    PoolResponse,
    UpdatePoolRequest,
)
from opensandbox_server.config import get_config
from opensandbox_server.services.constants import SandboxErrorCodes

router = APIRouter(tags=["Pools"])

_POOL_NOT_K8S_DETAIL = {
    "code": SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED,
    "message": "Pool management is only available when runtime.type is 'kubernetes'.",
}


def _get_pool_service():
    """
    Lazily create the PoolService, raising 501 if the runtime is not Kubernetes.

    This deferred approach means the pool router can be registered unconditionally
    in main.py; non-k8s deployments simply receive a clear 501 on every call.
    """
    from opensandbox_server.services.k8s.client import K8sClient
    from opensandbox_server.services.k8s.pool_service import PoolService

    config = get_config()
    if config.runtime.type != "kubernetes":
        raise HTTPException(
            status_code=status.HTTP_501_NOT_IMPLEMENTED,
            detail=_POOL_NOT_K8S_DETAIL,
        )

    if not config.kubernetes:
        raise HTTPException(
            status_code=status.HTTP_501_NOT_IMPLEMENTED,
            detail=_POOL_NOT_K8S_DETAIL,
        )

    k8s_client = K8sClient(config.kubernetes)
    return PoolService(k8s_client, namespace=config.kubernetes.namespace)


# ============================================================================
# Pool CRUD Endpoints
# ============================================================================

@router.post(
    "/pools",
    response_model=PoolResponse,
    response_model_exclude_none=True,
    status_code=status.HTTP_201_CREATED,
    responses={
        201: {"description": "Pool created successfully"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        409: {"model": ErrorResponse, "description": "A pool with the same name already exists"},
        501: {"model": ErrorResponse, "description": "Pool management is not supported in this runtime"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
def create_pool(
    request: CreatePoolRequest,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
) -> PoolResponse:
    """
    Create a pre-warmed resource pool.

    Creates a Pool CRD resource that manages a set of pre-warmed pods.
    Once created, sandboxes can reference the pool via ``extensions.poolRef``
    during sandbox creation to benefit from reduced cold-start latency.

    Args:
        request: Pool creation request including name, pod template, and capacity spec.
        x_request_id: Optional request tracing identifier.

    Returns:
        PoolResponse: The newly created pool.
    """
    pool_service = _get_pool_service()
    return pool_service.create_pool(request)


@router.get(
    "/pools",
    response_model=ListPoolsResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "List of pools"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        501: {"model": ErrorResponse, "description": "Pool management is not supported in this runtime"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
def list_pools(
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
) -> ListPoolsResponse:
    """
    List all pre-warmed resource pools.

    Returns all Pool resources in the configured namespace.

    Args:
        x_request_id: Optional request tracing identifier.

    Returns:
        ListPoolsResponse: Collection of all pools.
    """
    pool_service = _get_pool_service()
    return pool_service.list_pools()


@router.get(
    "/pools/{pool_name}",
    response_model=PoolResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Pool retrieved successfully"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        404: {"model": ErrorResponse, "description": "The requested pool does not exist"},
        501: {"model": ErrorResponse, "description": "Pool management is not supported in this runtime"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
def get_pool(
    pool_name: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
) -> PoolResponse:
    """
    Retrieve a pool by name.

    Args:
        pool_name: Name of the pool to retrieve.
        x_request_id: Optional request tracing identifier.

    Returns:
        PoolResponse: Current state of the pool including runtime status.
    """
    pool_service = _get_pool_service()
    return pool_service.get_pool(pool_name)


@router.put(
    "/pools/{pool_name}",
    response_model=PoolResponse,
    response_model_exclude_none=True,
    responses={
        200: {"description": "Pool capacity updated successfully"},
        400: {"model": ErrorResponse, "description": "The request was invalid or malformed"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        404: {"model": ErrorResponse, "description": "The requested pool does not exist"},
        501: {"model": ErrorResponse, "description": "Pool management is not supported in this runtime"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
def update_pool(
    pool_name: str,
    request: UpdatePoolRequest,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
) -> PoolResponse:
    """
    Update pool capacity configuration.

    Only ``capacitySpec`` (bufferMax, bufferMin, poolMax, poolMin) can be
    modified after creation. To change the pod template, delete and recreate
    the pool.

    Args:
        pool_name: Name of the pool to update.
        request: Update request with the new capacity spec.
        x_request_id: Optional request tracing identifier.

    Returns:
        PoolResponse: Updated pool state.
    """
    pool_service = _get_pool_service()
    return pool_service.update_pool(pool_name, request)


@router.delete(
    "/pools/{pool_name}",
    status_code=status.HTTP_204_NO_CONTENT,
    responses={
        204: {"description": "Pool deleted successfully"},
        401: {"model": ErrorResponse, "description": "Authentication credentials are missing or invalid"},
        404: {"model": ErrorResponse, "description": "The requested pool does not exist"},
        501: {"model": ErrorResponse, "description": "Pool management is not supported in this runtime"},
        500: {"model": ErrorResponse, "description": "An unexpected server error occurred"},
    },
)
def delete_pool(
    pool_name: str,
    x_request_id: Optional[str] = Header(None, alias="X-Request-ID"),
) -> Response:
    """
    Delete a pool.

    Removes the Pool CRD resource. Pre-warmed pods managed by the pool will
    be terminated by the pool controller.

    Args:
        pool_name: Name of the pool to delete.
        x_request_id: Optional request tracing identifier.

    Returns:
        Response: 204 No Content.
    """
    pool_service = _get_pool_service()
    pool_service.delete_pool(pool_name)
    return Response(status_code=status.HTTP_204_NO_CONTENT)
