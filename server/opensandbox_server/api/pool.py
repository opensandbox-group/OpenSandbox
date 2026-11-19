# Copyright 2025 The OpenSandbox Authors
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

import logging
import threading
from typing import TYPE_CHECKING, Optional

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

if TYPE_CHECKING:
    from opensandbox_server.services.k8s.pool_service import PoolService

router = APIRouter(tags=["Pools"])
logger = logging.getLogger(__name__)

_POOL_NOT_K8S_DETAIL = {
    "code": SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED,
    "message": "Pool management is only available when runtime.type is 'kubernetes'.",
}


def _pool_not_supported_for_provider(provider_name: str) -> dict[str, str]:
    return {
        "code": SandboxErrorCodes.K8S_POOL_NOT_SUPPORTED,
        "message": (
            f"Pool management is not supported by workload provider '{provider_name}'."
        ),
    }


_service: Optional["PoolService"] = None
_pool_unsupported = False
_service_lock = threading.Lock()


def _get_pool_service() -> "PoolService":
    """
    Lazily create the PoolService via the configured WorkloadProvider, raising
    501 if the runtime is not Kubernetes or the provider does not manage pools.

    This deferred approach means the pool router can be registered unconditionally
    in main.py; non-k8s or unsupported-provider deployments receive a clear 501.

    The service is created once and shared: building a K8sClient reloads the
    kubeconfig and rewrites the process-global default configuration, and each
    PoolService opens its own API client. The routes are sync handlers running
    on a thread pool, so creation is locked (as in the fast-sandbox service).
    """
    global _service, _pool_unsupported
    from opensandbox_server.config import AppConfig
    from opensandbox_server.services.k8s.client import K8sClient
    from opensandbox_server.services.k8s.provider_factory import (
        create_workload_provider,
        get_workload_provider_class,
    )
    from opensandbox_server.services.k8s.workload_provider import WorkloadProvider

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

    provider_type = getattr(config.kubernetes, "workload_provider", None)
    try:
        provider_cls = get_workload_provider_class(provider_type)
    except ValueError as exc:
        raise HTTPException(
            status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
            detail={
                "code": SandboxErrorCodes.K8S_INITIALIZATION_ERROR,
                "message": f"Invalid workload provider configuration: {exc}",
            },
        ) from exc

    if provider_cls.create_pool_service is WorkloadProvider.create_pool_service:
        raise HTTPException(
            status_code=status.HTTP_501_NOT_IMPLEMENTED,
            detail=_pool_not_supported_for_provider(
                provider_type or provider_cls.__name__
            ),
        )

    with _service_lock:
        if _service is None and not _pool_unsupported:
            app_cfg = config if isinstance(config, AppConfig) else None
            k8s_client = K8sClient(config.kubernetes)
            try:
                provider = create_workload_provider(
                    provider_type=provider_type, k8s_client=k8s_client, app_config=app_cfg
                )
            except (ValueError, FileNotFoundError, RuntimeError) as exc:
                logger.exception(f"Failed to initialize workload provider {provider_cls.__name__}")
                raise HTTPException(
                    status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                    detail={
                        "code": SandboxErrorCodes.K8S_INITIALIZATION_ERROR,
                        "message": f"Failed to initialize workload provider {provider_cls.__name__}.",
                    },
                ) from exc
            _service = provider.create_pool_service(
                config.kubernetes.namespace or "default"
            )
            _pool_unsupported = _service is None
        if _service is None:
            raise HTTPException(
                status_code=status.HTTP_501_NOT_IMPLEMENTED,
                detail=_pool_not_supported_for_provider(
                    provider_type or provider_cls.__name__
                ),
            )
        return _service


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
    """
    pool_service = _get_pool_service()
    pool_service.delete_pool(pool_name)
    return Response(status_code=status.HTTP_204_NO_CONTENT)
