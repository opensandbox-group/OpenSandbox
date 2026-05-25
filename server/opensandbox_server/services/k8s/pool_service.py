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

"""Kubernetes Pool service for pre-warmed sandbox resource pools."""

import logging
from typing import Any, Dict, List, Optional

from fastapi import HTTPException, status
from kubernetes.client import ApiException

from opensandbox_server.api.schema import (
    CreatePoolRequest,
    ListPoolsResponse,
    PoolCapacitySpec,
    PoolResponse,
    PoolStatus,
    UpdatePoolRequest,
)
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.k8s.client import K8sClient

logger = logging.getLogger(__name__)

_GROUP = "sandbox.opensandbox.io"
_VERSION = "v1alpha1"
_PLURAL = "pools"


class PoolService:
    """Service for managing Pool CRD resources in Kubernetes."""

    def __init__(self, k8s_client: K8sClient, namespace: str) -> None:
        """Initialize PoolService."""
        self._custom_api = k8s_client.get_custom_objects_api()
        self._namespace = namespace

    def _build_pool_manifest(
        self,
        name: str,
        namespace: str,
        template: Dict[str, Any],
        capacity_spec: PoolCapacitySpec,
    ) -> Dict[str, Any]:
        """Build a Pool CRD manifest dict."""
        return {
            "apiVersion": f"{_GROUP}/{_VERSION}",
            "kind": "Pool",
            "metadata": {
                "name": name,
                "namespace": namespace,
            },
            "spec": {
                "template": template,
                "capacitySpec": {
                    "bufferMax": capacity_spec.buffer_max,
                    "bufferMin": capacity_spec.buffer_min,
                    "poolMax": capacity_spec.pool_max,
                    "poolMin": capacity_spec.pool_min,
                },
            },
        }

    def _pool_from_raw(self, raw: Dict[str, Any]) -> PoolResponse:
        """Convert a raw Pool CRD dict to a PoolResponse model."""
        metadata = raw.get("metadata", {})
        spec = raw.get("spec", {})
        raw_status = raw.get("status")

        capacity = spec.get("capacitySpec", {})
        capacity_spec = PoolCapacitySpec(
            bufferMax=capacity.get("bufferMax", 0),
            bufferMin=capacity.get("bufferMin", 0),
            poolMax=capacity.get("poolMax", 0),
            poolMin=capacity.get("poolMin", 0),
        )

        pool_status: Optional[PoolStatus] = None
        if raw_status:
            pool_status = PoolStatus(
                total=raw_status.get("total", 0),
                allocated=raw_status.get("allocated", 0),
                available=raw_status.get("available", 0),
                revision=raw_status.get("revision", ""),
            )

        return PoolResponse(
            name=metadata.get("name", ""),
            capacitySpec=capacity_spec,
            status=pool_status,
            createdAt=metadata.get("creationTimestamp"),
        )

    def create_pool(self, request: CreatePoolRequest) -> PoolResponse:
        """Create a new Pool resource."""
        manifest = self._build_pool_manifest(
            name=request.name,
            namespace=self._namespace,
            template=request.template,
            capacity_spec=request.capacity_spec,
        )

        try:
            created = self._custom_api.create_namespaced_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                body=manifest,
            )
            logger.info(f"Created pool: name={request.name}, namespace={self._namespace}")
            return self._pool_from_raw(created)

        except ApiException as e:
            if e.status == 409:
                raise HTTPException(
                    status_code=status.HTTP_409_CONFLICT,
                    detail={
                        "code": SandboxErrorCodes.K8S_POOL_ALREADY_EXISTS,
                        "message": f"Pool '{request.name}' already exists.",
                    },
                ) from e
            logger.error(f"Kubernetes API error creating pool {request.name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to create pool: {e.reason}",
                },
            ) from e
        except Exception as e:
            logger.error(f"Unexpected error creating pool {request.name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to create pool: {e}",
                },
            ) from e

    def get_pool(self, pool_name: str) -> PoolResponse:
        """Retrieve a Pool by name."""
        try:
            raw = self._custom_api.get_namespaced_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                name=pool_name,
            )
            return self._pool_from_raw(raw)

        except ApiException as e:
            if e.status == 404:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                        "message": f"Pool '{pool_name}' not found.",
                    },
                ) from e
            logger.error(f"Kubernetes API error getting pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to get pool: {e.reason}",
                },
            ) from e
        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Unexpected error getting pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to get pool: {e}",
                },
            ) from e

    def list_pools(self) -> ListPoolsResponse:
        """List all Pools in the configured namespace."""
        try:
            result = self._custom_api.list_namespaced_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
            )
            items: List[PoolResponse] = [
                self._pool_from_raw(item) for item in result.get("items", [])
            ]
            return ListPoolsResponse(items=items)

        except ApiException as e:
            if e.status == 404:
                # CRD not installed — return empty list gracefully
                logger.warning("Pool CRD not found (404); returning empty list.")
                return ListPoolsResponse(items=[])
            logger.error(f"Kubernetes API error listing pools: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to list pools: {e.reason}",
                },
            ) from e
        except Exception as e:
            logger.error(f"Unexpected error listing pools: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to list pools: {e}",
                },
            ) from e

    def update_pool(self, pool_name: str, request: UpdatePoolRequest) -> PoolResponse:
        """Update the capacity configuration of an existing Pool."""
        patch_body = {
            "spec": {
                "capacitySpec": {
                    "bufferMax": request.capacity_spec.buffer_max,
                    "bufferMin": request.capacity_spec.buffer_min,
                    "poolMax": request.capacity_spec.pool_max,
                    "poolMin": request.capacity_spec.pool_min,
                }
            }
        }

        try:
            updated = self._custom_api.patch_namespaced_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                name=pool_name,
                body=patch_body,
            )
            logger.info(f"Updated pool capacity: name={pool_name}")
            return self._pool_from_raw(updated)

        except ApiException as e:
            if e.status == 404:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                        "message": f"Pool '{pool_name}' not found.",
                    },
                ) from e
            logger.error(f"Kubernetes API error updating pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to update pool: {e.reason}",
                },
            ) from e
        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Unexpected error updating pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to update pool: {e}",
                },
            ) from e

    def delete_pool(self, pool_name: str) -> None:
        """Delete a Pool resource."""
        try:
            self._custom_api.delete_namespaced_custom_object(
                group=_GROUP,
                version=_VERSION,
                namespace=self._namespace,
                plural=_PLURAL,
                name=pool_name,
                grace_period_seconds=0,
            )
            logger.info(f"Deleted pool: name={pool_name}, namespace={self._namespace}")

        except ApiException as e:
            if e.status == 404:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_POOL_NOT_FOUND,
                        "message": f"Pool '{pool_name}' not found.",
                    },
                ) from e
            logger.error(f"Kubernetes API error deleting pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to delete pool: {e.reason}",
                },
            ) from e
        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Unexpected error deleting pool {pool_name}: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_POOL_API_ERROR,
                    "message": f"Failed to delete pool: {e}",
                },
            ) from e
