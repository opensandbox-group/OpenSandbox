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
Kubernetes-based implementation of SandboxService.

This module provides a Kubernetes implementation of the sandbox service interface,
using Kubernetes resources for sandbox lifecycle management.
"""

import asyncio
import logging
import time
from datetime import datetime, timezone
from typing import Optional, Dict, Any

from fastapi import HTTPException, status

from opensandbox_server.extensions import (
    apply_access_renew_extend_seconds_to_mapping,
    apply_extensions_to_annotations,
)
from opensandbox_server.extensions.keys import ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY
from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    Endpoint,
    ListSandboxesRequest,
    ListSandboxesResponse,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxStatus,
)
from opensandbox_server.config import AppConfig, INGRESS_MODE_GATEWAY, SecureAccessConfig, get_config
from opensandbox_server.services.constants import (
    SANDBOX_ID_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.endpoint_auth import generate_egress_token, generate_secure_access_token
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.helpers import format_ingress_endpoint
from opensandbox_server.services.k8s.create_helpers import _build_create_workload_context
from opensandbox_server.services.k8s.error_helpers import _build_k8s_api_error
from opensandbox_server.services.k8s.k8s_diagnostics import K8sDiagnosticsMixin
from opensandbox_server.services.k8s.endpoint_resolver import _attach_egress_auth_headers, _attach_secure_access_headers
from opensandbox_server.services.k8s.list_helpers import _build_list_sandboxes_response
from opensandbox_server.services.k8s.status_helpers import (
    _is_unschedulable_status,
    _normalize_create_status,
)
from opensandbox_server.services.k8s.workload_mapper import (
    _build_sandbox_from_workload,
    _extract_platform_from_workload,
)
from opensandbox_server.services.signing import (
    build_canonical_bytes,
    compute_signature,
    encode_expires_b36,
)
from opensandbox_server.services.k8s.workload_access import (
    _delete_workload_or_404,
    _get_workload_or_404,
)
from opensandbox_server.services.sandbox_service import SandboxService
from opensandbox_server.services.validators import (
    ensure_entrypoint,
    ensure_egress_configured,
    ensure_future_expiration,
    ensure_metadata_labels,
    ensure_platform_valid,
    ensure_timeout_within_limit,
    ensure_valid_host_path,
    ensure_volumes_valid,
)
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.provider_factory import create_workload_provider
from opensandbox_server.services.snapshot_restore import resolve_sandbox_image_from_request

logger = logging.getLogger(__name__)


class KubernetesSandboxService(K8sDiagnosticsMixin, SandboxService, ExtensionService):
    """
    Kubernetes-based implementation of SandboxService.
    
    This class implements sandbox lifecycle operations using Kubernetes resources.
    """
    
    def __init__(self, config: Optional[AppConfig] = None):
        """
        Initialize Kubernetes sandbox service.
        
        Args:
            config: Application configuration
            
        Raises:
            HTTPException: If initialization fails
        """
        self.app_config = config or get_config()
        runtime_config = self.app_config.runtime
        
        if runtime_config.type != "kubernetes":
            raise ValueError("KubernetesSandboxService requires runtime.type = 'kubernetes'")
        
        if not self.app_config.kubernetes:
            raise ValueError("Kubernetes configuration is required")

        self.ingress_config = self.app_config.ingress

        self.namespace = self.app_config.kubernetes.namespace
        self.execd_image = runtime_config.execd_image

        try:
            self.k8s_client = K8sClient(self.app_config.kubernetes)
            logger.info("Kubernetes client initialized successfully")
        except Exception as e:
            logger.error(f"Failed to initialize Kubernetes client: {e}")
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.K8S_INITIALIZATION_ERROR,
                    "message": f"Failed to initialize Kubernetes client: {str(e)}",
                },
            ) from e

        provider_type = self.app_config.kubernetes.workload_provider
        try:
            self.workload_provider = create_workload_provider(
                provider_type=provider_type,
                k8s_client=self.k8s_client,
                app_config=self.app_config,
            )
            logger.info(
                f"Initialized workload provider: {self.workload_provider.__class__.__name__}"
            )
        except ValueError as e:
            logger.error(f"Failed to create workload provider: {e}")
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.K8S_INITIALIZATION_ERROR,
                    "message": f"Invalid workload provider configuration: {str(e)}",
                },
            ) from e

        logger.info(
            "KubernetesSandboxService initialized: namespace=%s, execd_image=%s",
            self.namespace,
            self.execd_image,
        )
    
    async def _wait_for_sandbox_ready(
        self,
        sandbox_id: str,
        timeout_seconds: int = 60,
        poll_interval_seconds: float = 1.0,
    ) -> Dict[str, Any]:
        """
        Wait for Pod to be Running and have an IP address.
        
        Args:
            sandbox_id: Sandbox ID
            timeout_seconds: Maximum time to wait in seconds
            poll_interval_seconds: Time between polling attempts
            
        Returns:
            Workload dict when Pod is Running with IP
            
        Raises:
            HTTPException: If timeout or Pod fails
        """
        logger.info(
            f"Waiting for sandbox {sandbox_id} to be Running with IP (timeout: {timeout_seconds}s)"
        )
        
        start_time = time.time()
        last_state = None
        last_message = None
        
        while time.time() - start_time < timeout_seconds:
            try:
                workload = await asyncio.to_thread(
                    self.workload_provider.get_workload,
                    sandbox_id=sandbox_id,
                    namespace=self.namespace,
                )
                
                if not workload:
                    logger.debug(f"Workload not found yet for sandbox {sandbox_id}")
                    await asyncio.sleep(poll_interval_seconds)
                    continue
                
                status_info = _normalize_create_status(
                    self.workload_provider.get_status(workload)
                )
                current_state = status_info["state"]
                current_message = status_info["message"]

                if current_state != last_state or current_message != last_message:
                    logger.info(
                        f"Sandbox {sandbox_id} state: {current_state} - {current_message}"
                    )
                    last_state = current_state
                    last_message = current_message

                if current_state in ("Running", "Allocated"):
                    return workload
                if _is_unschedulable_status(status_info):
                    raise HTTPException(
                        status_code=status.HTTP_400_BAD_REQUEST,
                        detail={
                            "code": SandboxErrorCodes.INVALID_PARAMETER,
                            "message": (
                                f"Sandbox {sandbox_id} is unschedulable: "
                                f"{current_message or status_info.get('reason') or 'no scheduler details'}"
                            ),
                        },
                    )

            except HTTPException:
                raise
            except Exception as e:
                logger.warning(
                    f"Error checking sandbox {sandbox_id} status: {e}",
                    exc_info=True
                )

            await asyncio.sleep(poll_interval_seconds)

        elapsed = time.time() - start_time
        raise HTTPException(
            status_code=status.HTTP_504_GATEWAY_TIMEOUT,
            detail={
                "code": SandboxErrorCodes.K8S_POD_READY_TIMEOUT,
                "message": (
                    f"Timeout waiting for sandbox {sandbox_id} to be Running with IP. "
                    f"Elapsed: {elapsed:.1f}s, Last state: {last_state}"
                ),
            },
        )

    def _ensure_network_policy_support(self, request: CreateSandboxRequest) -> None:
        """
        Validate that network policy can be honored under the current runtime config.
        
        This validates that egress.image is configured when network_policy is provided.
        """
        ensure_egress_configured(request.network_policy, self.app_config.egress)

    def _ensure_image_auth_support(self, request: CreateSandboxRequest) -> None:
        """
        Validate image auth support for the current workload provider.

        Raises HTTP 400 if the provider does not support per-request image auth.
        """
        if request.image is None or request.image.auth is None:
            return
        if self.workload_provider.supports_image_auth():
            return
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "image.auth is not supported by the current workload provider. "
                    "Use imagePullSecrets via Kubernetes ServiceAccount or sandbox template."
                ),
            },
        )

    def _ensure_secure_access_support(self, request: CreateSandboxRequest) -> None:
        """Validate that secure access can be enforced for the configured exposure mode."""
        if not request.secure_access:
            return
        if self.ingress_config and self.ingress_config.mode == INGRESS_MODE_GATEWAY:
            return
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "secureAccess is currently supported only for Kubernetes sandboxes exposed "
                    "through ingress.mode='gateway'. Configure ingress gateway mode or disable secureAccess."
                ),
            },
        )

    def _ensure_pvc_volumes(self, volumes: list) -> None:
        """
        Ensure that PVC volumes exist before creating the workload.

        For each volume with a ``pvc`` backend, check whether the
        PersistentVolumeClaim already exists in the target namespace.
        If not, create it using the provisioning hints from the PVC model.

        Degrades gracefully: if the service account lacks RBAC permissions
        for PVC operations (403), the check is skipped and volume resolution
        is left to the kubelet at pod scheduling time.
        """
        from kubernetes.client import V1PersistentVolume, V1PersistentVolumeClaim, V1ObjectMeta
        from kubernetes.client import ApiException

        default_size = self.app_config.storage.volume_default_size

        seen_claims: set[str] = set()
        for vol in volumes:
            if vol.pvc is None or not vol.pvc.create_if_not_exists:
                continue
            claim_name = vol.pvc.claim_name
            if claim_name in seen_claims:
                continue
            seen_claims.add(claim_name)

            try:
                existing = self.k8s_client.get_pvc(self.namespace, claim_name)
            except ApiException as e:
                if e.status == 403:
                    logger.warning(
                        f"No RBAC permission to read PVC '{claim_name}', skipping auto-create. "
                        "Grant 'get' and 'create' on 'persistentvolumeclaims' to enable."
                    )
                    return  # Skip all remaining PVCs — same SA, same permissions
                raise
            if existing is not None:
                logger.debug(f"PVC '{claim_name}' already exists in namespace '{self.namespace}'")
                continue

            storage = vol.pvc.storage or default_size
            access_modes = vol.pvc.access_modes or ["ReadWriteOnce"]
            storage_class = vol.pvc.storage_class  # None = cluster default

            pv_body = None
            if vol.pvc.pv is not None:
                allowed_paths = self.app_config.storage.allowed_host_paths
                pv_spec_raw = vol.pvc.pv
                for source_key, path_key in (("hostPath", "path"), ("local", "path")):
                    source = pv_spec_raw.get(source_key)
                    if isinstance(source, dict) and path_key in source:
                        ensure_valid_host_path(source[path_key], allowed_paths)

                pv_name = claim_name
                if self.namespace is not None:
                    pv_name = f"{claim_name}-{self.namespace}"
                if len(pv_name) > 253:
                    raise HTTPException(
                        status_code=status.HTTP_400_BAD_REQUEST,
                        detail={
                            "code": SandboxErrorCodes.INVALID_PARAMETER,
                            "message": (
                                f"Generated PV name '{pv_name}' exceeds Kubernetes' "
                                f"253-character limit ({len(pv_name)} chars). "
                                f"Use a shorter claimName."
                            ),
                        },
                    )
                spec = pv_spec_raw
                spec["claimRef"] = {
                    "name": claim_name,
                    "namespace": self.namespace,
                }
                spec["accessModes"] = access_modes
                spec["capacity"] = {"storage": storage}
                if storage_class is not None:
                    spec["storageClassName"] = storage_class
                else:
                    spec["storageClassName"] = ""
                pv_body = V1PersistentVolume(
                    metadata=V1ObjectMeta(name=pv_name),
                    spec=spec,
                )
                if storage_class is None:
                    storage_class = ""

            pvc_body = V1PersistentVolumeClaim(
                metadata=V1ObjectMeta(
                    name=claim_name,
                    namespace=self.namespace
                ),
                spec={
                    "accessModes": access_modes,
                    "resources": {"requests": {"storage": storage}},
                },
            )
            if storage_class is not None:
                pvc_body.spec["storageClassName"] = storage_class

            if pv_body is not None:
                try:
                    self.k8s_client.create_pv(pv_body)
                    logger.info(f"Auto-created PV '{pv_body.metadata.name}'")
                except ApiException as e:
                    if e.status == 409:
                        logger.info(f"PV '{pv_body.metadata.name}' already exists, proceeding with PVC creation")
                    elif e.status in (400, 422):
                        raise HTTPException(
                            status_code=status.HTTP_400_BAD_REQUEST,
                            detail={
                                "code": SandboxErrorCodes.INVALID_PARAMETER,
                                "message": f"Invalid PV spec for '{claim_name}': {e.reason}",
                            },
                        ) from e
                    else:
                        raise

            try:
                self.k8s_client.create_pvc(self.namespace, pvc_body)
                logger.info(
                    f"Auto-created PVC '{claim_name}' (size={storage}, class={storage_class or '<default>'}) "
                    f"in namespace '{self.namespace}'"
                )
            except ApiException as e:
                if e.status == 409:
                    logger.info(f"PVC '{claim_name}' was created concurrently, proceeding")
                elif e.status == 403:
                    logger.warning(
                        f"No RBAC permission to create PVC '{claim_name}', skipping. "
                        "The PVC must be pre-created or RBAC must be updated."
                    )
                elif e.status in (400, 422):
                    # Invalid PVC spec from user-provided hints
                    # (e.g. accessModes, storage). These are client errors,
                    # not retryable server faults.
                    raise HTTPException(
                        status_code=status.HTTP_400_BAD_REQUEST,
                        detail={
                            "code": SandboxErrorCodes.INVALID_PARAMETER,
                            "message": f"Invalid PVC spec for '{claim_name}': {e.reason}",
                        },
                    ) from e
                else:
                    logger.error(f"Failed to create PVC '{claim_name}': {e}")
                    raise HTTPException(
                        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                        detail={
                            "code": SandboxErrorCodes.INTERNAL_ERROR,
                            "message": f"Failed to auto-create PVC '{claim_name}': {e.reason}",
                        },
                    ) from e

    async def create_sandbox(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        """
        Create a new sandbox using Kubernetes Pod.

        Wait for the Pod to be Running and have an IP address before returning.
        
        Args:
            request: Sandbox creation request.
            
        Returns:
            CreateSandboxResponse: Created sandbox information with Running state
            
        Raises:
            HTTPException: If creation fails, timeout, or invalid parameters
        """
        has_pool_ref = bool((request.extensions or {}).get("poolRef", "").strip())

        if not has_pool_ref:
            request = resolve_sandbox_image_from_request(request)
            ensure_entrypoint(request.entrypoint or [])
        ensure_metadata_labels(request.metadata)
        ensure_platform_valid(request.platform)
        ensure_timeout_within_limit(
            request.timeout,
            self.app_config.server.max_sandbox_timeout_seconds,
        )
        self._ensure_secure_access_support(request)
        self._ensure_network_policy_support(request)
        self._ensure_image_auth_support(request)

        sandbox_id = self.generate_sandbox_id()

        created_at = datetime.now(timezone.utc)
        context = _build_create_workload_context(
            app_config=self.app_config,
            request=request,
            sandbox_id=sandbox_id,
            created_at=created_at,
            egress_token_factory=generate_egress_token,
            secure_access_token_factory=generate_secure_access_token,
        )
        
        try:
            apply_access_renew_extend_seconds_to_mapping(context.annotations, request.extensions)
            apply_extensions_to_annotations(context.annotations, request.extensions)

            ensure_volumes_valid(
                request.volumes,
                self.app_config.storage.allowed_host_paths,
            )

            # Auto-create PVCs that don't exist yet
            if request.volumes:
                await asyncio.to_thread(self._ensure_pvc_volumes, request.volumes)

            # Create workload
            workload_info = await asyncio.to_thread(
                self.workload_provider.create_workload,
                sandbox_id=sandbox_id,
                namespace=self.namespace,
                image_spec=request.image,
                entrypoint=request.entrypoint,
                env=request.env or {},
                resource_limits=context.resource_limits,
                labels=context.labels,
                annotations=context.annotations or None,
                expires_at=context.expires_at,
                execd_image=self.execd_image,
                extensions=request.extensions,
                network_policy=request.network_policy,
                egress_image=context.egress_image,
                egress_auth_token=context.egress_auth_token,
                egress_mode=context.egress_mode,
                volumes=request.volumes,
                platform=request.platform,
            )
            
            logger.info(
                "Created sandbox: id=%s, workload=%s",
                sandbox_id,
                workload_info.get("name"),
            )

            try:
                workload = await self._wait_for_sandbox_ready(
                    sandbox_id=sandbox_id,
                    timeout_seconds=self.app_config.kubernetes.sandbox_create_timeout_seconds,
                    poll_interval_seconds=self.app_config.kubernetes.sandbox_create_poll_interval_seconds,
                )
                
                status_info = _normalize_create_status(
                    self.workload_provider.get_status(workload)
                )
                effective_platform = _extract_platform_from_workload(workload)

                return CreateSandboxResponse(
                    id=sandbox_id,
                    status=SandboxStatus(
                        state=status_info["state"],
                        reason=status_info["reason"],
                        message=status_info["message"],
                        last_transition_at=status_info["last_transition_at"],
                    ),
                    created_at=created_at,
                    expires_at=context.expires_at,
                    metadata=request.metadata,
                    entrypoint=request.entrypoint,
                    platform=effective_platform or request.platform,
                )
                
            except HTTPException as e:
                try:
                    logger.error(f"Creation failed, cleaning up sandbox {sandbox_id}: {e}")
                    await asyncio.to_thread(
                        self.workload_provider.delete_workload,
                        sandbox_id,
                        self.namespace,
                    )
                except Exception as cleanup_ex:
                    logger.error(f"Failed to cleanup sandbox {sandbox_id}", exc_info=cleanup_ex)
                raise
            
        except HTTPException:
            raise
        except ValueError as e:
            logger.error(f"Invalid parameters for sandbox creation: {e}")
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": str(e),
                },
            ) from e
        except Exception as e:
            logger.error(f"Error creating sandbox: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_API_ERROR,
                    "message": f"Failed to create sandbox: {str(e)}",
                },
            ) from e

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """
        Get sandbox by ID.

        Args:
            sandbox_id: Unique sandbox identifier

        Returns:
            Sandbox: Sandbox information

        Raises:
            HTTPException: If sandbox not found
        """
        try:
            workload = _get_workload_or_404(
                self.workload_provider,
                self.namespace,
                sandbox_id,
            )
            return _build_sandbox_from_workload(workload, self.workload_provider)
            
        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Error getting sandbox {sandbox_id}: {e}")
            raise _build_k8s_api_error("get sandbox", e) from e
    
    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        """
        List sandboxes with filtering and pagination.
        
        Args:
            request: List request with filters and pagination
            
        Returns:
            ListSandboxesResponse: Paginated list of sandboxes
        """
        try:
            label_selector = SANDBOX_ID_LABEL
            workloads = self.workload_provider.list_workloads(
                namespace=self.namespace,
                label_selector=label_selector,
            )
            sandboxes = [
                _build_sandbox_from_workload(w, self.workload_provider)
                for w in workloads
            ]
            
            return _build_list_sandboxes_response(sandboxes, request)
            
        except Exception as e:
            logger.error(f"Error listing sandboxes: {e}")
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_API_ERROR,
                    "message": f"Failed to list sandboxes: {str(e)}",
                },
            ) from e
    
    def delete_sandbox(self, sandbox_id: str) -> None:
        """
        Delete a sandbox.

        Args:
            sandbox_id: Unique sandbox identifier

        Raises:
            HTTPException: If deletion fails
        """
        try:
            _delete_workload_or_404(
                self.workload_provider,
                self.namespace,
                sandbox_id,
            )
            logger.info(f"Deleted sandbox: {sandbox_id}")

        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Error deleting sandbox {sandbox_id}: {e}")
            raise _build_k8s_api_error("delete sandbox", e) from e
    
    def pause_sandbox(self, sandbox_id: str) -> None:
        """
        Pause sandbox by delegating to the workload provider.
        """
        try:
            self.workload_provider.pause_sandbox(sandbox_id, self.namespace)
        except NotImplementedError:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_STATE,
                    "message": "Pause is not supported for this sandbox type",
                },
            )
        except ValueError as e:
            msg = str(e)
            if "not found" in msg:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_SANDBOX_NOT_FOUND,
                        "message": msg,
                    },
                )
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.INVALID_STATE,
                    "message": msg,
                },
            )
        except Exception as e:
            logger.error("Failed to pause sandbox %s: %s", sandbox_id, e)
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_API_ERROR,
                    "message": f"Failed to pause sandbox: {e}",
                },
            )

    def resume_sandbox(self, sandbox_id: str) -> None:
        """
        Resume sandbox by delegating to the workload provider.
        """
        try:
            self.workload_provider.resume_sandbox(sandbox_id, self.namespace)
        except NotImplementedError:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_STATE,
                    "message": "Resume is not supported for this sandbox type",
                },
            )
        except ValueError as e:
            msg = str(e)
            if "not found" in msg:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_SANDBOX_NOT_FOUND,
                        "message": msg,
                    },
                )
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.INVALID_STATE,
                    "message": msg,
                },
            )
        except Exception as e:
            logger.error("Failed to resume sandbox %s: %s", sandbox_id, e)
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.K8S_API_ERROR,
                    "message": f"Failed to resume sandbox: {e}",
                },
            )

    def get_access_renew_extend_seconds(self, sandbox_id: str) -> Optional[int]:
        workload = self.workload_provider.get_workload(
            sandbox_id=sandbox_id,
            namespace=self.namespace,
        )
        if not workload:
            return None
        if isinstance(workload, dict):
            annotations = workload.get("metadata", {}).get("annotations") or {}
        else:
            md = getattr(workload, "metadata", None)
            raw_ann = getattr(md, "annotations", None) if md else None
            annotations = raw_ann if isinstance(raw_ann, dict) else {}
        raw = annotations.get(ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY)
        if raw is None or not str(raw).strip():
            return None
        try:
            return int(str(raw).strip())
        except ValueError:
            return None

    def renew_expiration(
        self,
        sandbox_id: str,
        request: RenewSandboxExpirationRequest,
    ) -> RenewSandboxExpirationResponse:
        """
        Renew sandbox expiration time.
        
        Updates both the BatchSandbox spec.expireTime and label for consistency.
        
        Args:
            sandbox_id: Unique sandbox identifier
            request: Renewal request with new expiration time
            
        Returns:
            RenewSandboxExpirationResponse: Updated expiration time
            
        Raises:
            HTTPException: If renewal fails
        """
        new_expiration = ensure_future_expiration(request.expires_at)
        
        try:
            workload = _get_workload_or_404(
                self.workload_provider,
                self.namespace,
                sandbox_id,
            )

            current_expiration = self.workload_provider.get_expiration(workload)
            if current_expiration is None:
                raise HTTPException(
                    status_code=status.HTTP_409_CONFLICT,
                    detail={
                        "code": SandboxErrorCodes.INVALID_EXPIRATION,
                        "message": f"Sandbox {sandbox_id} does not have automatic expiration enabled.",
                    },
                )

            self.workload_provider.update_expiration(
                sandbox_id=sandbox_id,
                namespace=self.namespace,
                expires_at=new_expiration,
            )
            
            logger.info(
                f"Renewed sandbox {sandbox_id} expiration to {new_expiration}"
            )
            
            return RenewSandboxExpirationResponse(
                expires_at=new_expiration
            )
            
        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Error renewing expiration for {sandbox_id}: {e}")
            raise _build_k8s_api_error("renew expiration", e) from e

    def patch_sandbox_metadata(self, sandbox_id: str, patch: PatchSandboxMetadataRequest) -> Sandbox:
        """Patch sandbox metadata via JSON Merge Patch (RFC 7396). Does not restart the sandbox."""
        workload = _get_workload_or_404(
            self.workload_provider,
            self.namespace,
            sandbox_id,
        )

        if isinstance(workload, dict):
            labels = dict(workload.get("metadata", {}).get("labels") or {})
            name = workload["metadata"]["name"]
        else:
            labels = dict(getattr(workload.metadata, "labels", None) or {})
            name = workload.metadata.name

        new_labels = self._apply_metadata_patch(labels, patch)

        # JSON merge patch (RFC 7396) on metadata.labels treats keys absent
        # from the body as kept. To delete a label we must send the key with
        # an explicit null. Build the merge body from the desired final labels
        # plus null markers for keys removed by this patch.
        label_patch: Dict[str, Optional[str]] = dict(new_labels)
        for key, value in patch.items():
            if value is None:
                label_patch[key] = None

        try:
            updated = self.workload_provider.patch_labels(
                name=name,
                namespace=self.namespace,
                labels=label_patch,
            )
        except Exception as e:
            logger.error("Error patching labels for sandbox %s: %s", sandbox_id, e)
            raise _build_k8s_api_error("patch sandbox labels", e) from e

        return _build_sandbox_from_workload(updated, self.workload_provider)

    def get_endpoint(
        self,
        sandbox_id: str,
        port: int,
        resolve_internal: bool = False,
        expires: Optional[int] = None,
    ) -> Endpoint:
        """
        Get sandbox access endpoint.

        Args:
            sandbox_id: Unique sandbox identifier
            port: Port number
            resolve_internal: Ignored for Kubernetes (always returns Pod IP)
            expires: Unix epoch seconds for a signed route token.
                Requires ingress gateway mode with secure_access keys configured.

        Returns:
            Endpoint: Endpoint information

        Raises:
            HTTPException: If endpoint not available or signed routes unsupported
        """
        self.validate_port(port)

        if expires is not None:
            if expires < 0:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": "expires must be a non-negative Unix timestamp (uint64).",
                    },
                )
            now = int(time.time())
            if expires <= now:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": f"expires ({expires}) must be greater than current time ({now}).",
                    },
                )
            if expires > 18446744073709551615:
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_PARAMETER,
                        "message": "expires exceeds uint64 maximum value.",
                    },
                )

        try:
            workload = _get_workload_or_404(
                self.workload_provider,
                self.namespace,
                sandbox_id,
            )

            if expires is not None:
                endpoint = self._build_signed_endpoint(sandbox_id, port, expires)
            else:
                endpoint = self.workload_provider.get_endpoint_info(workload, port, sandbox_id)

            if not endpoint:
                raise HTTPException(
                    status_code=status.HTTP_404_NOT_FOUND,
                    detail={
                        "code": SandboxErrorCodes.K8S_POD_IP_NOT_AVAILABLE,
                        "message": "Pod IP is not yet available. The Pod may still be starting.",
                    },
                )
            if expires is None:
                _attach_secure_access_headers(endpoint, workload)
            _attach_egress_auth_headers(endpoint, workload)
            return endpoint

        except HTTPException:
            raise
        except Exception as e:
            logger.error(f"Error getting endpoint for {sandbox_id}:{port}: {e}")
            raise _build_k8s_api_error("get endpoint", e) from e

    def _build_signed_endpoint(self, sandbox_id: str, port: int, expires: int) -> Endpoint:
        """Build a signed ingress endpoint per OSEP-0011."""
        secure_cfg = self._get_secure_access_config()

        expires_b36 = encode_expires_b36(expires)
        secret = secure_cfg.get_active_secret_bytes()
        active_key = secure_cfg.active_key
        canonical = build_canonical_bytes(sandbox_id, port, expires_b36)
        signature = compute_signature(secret, active_key, canonical)

        endpoint = format_ingress_endpoint(
            self.ingress_config, sandbox_id, port,
            expires_b36=expires_b36, signature=signature,
        )
        if endpoint is None:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Signed routes are only available when ingress is in gateway mode. "
                        "Configure ingress gateway or omit the expires parameter."
                    ),
                },
            )
        return endpoint

    def _get_secure_access_config(self) -> SecureAccessConfig:
        """Return the secure_access config or raise 400 if not configured."""
        if not self.ingress_config or self.ingress_config.mode != INGRESS_MODE_GATEWAY:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Signed routes require ingress.mode = 'gateway'. "
                        "Configure ingress gateway or omit the expires parameter."
                    ),
                },
            )
        secure = self.ingress_config.secure_access
        if secure is None:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        "Signed routes require ingress.secure_access to be configured "
                        "with signing keys. Configure secure_access or omit the expires parameter."
                    ),
                },
            )
        return secure
