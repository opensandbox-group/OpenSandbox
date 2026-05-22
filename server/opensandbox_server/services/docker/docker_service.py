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
Docker-based implementation of SandboxService.

This module provides a Docker implementation of the sandbox service interface,
using Docker containers for sandbox lifecycle management.
"""

from __future__ import annotations

import inspect
import json
import logging
import math
import os
import time
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from threading import Lock, Timer
from typing import Any, Dict, Optional

import docker
from docker.errors import DockerException
from fastapi import HTTPException, status

from opensandbox_server.extensions import (
    ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
)
from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    ImageSpec,
    ListSandboxesRequest,
    ListSandboxesResponse,
    PaginationInfo,
    PatchSandboxMetadataRequest,
    RenewSandboxExpirationRequest,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxStatus,
    PlatformSpec,
)
from opensandbox_server.config import AppConfig, get_config
from opensandbox_server.services.docker.docker_diagnostics import DockerDiagnosticsMixin
from opensandbox_server.services.docker.runtime import (
    DockerRuntimeMixin,
)
from opensandbox_server.services.docker.volumes import DockerVolumesMixin
from opensandbox_server.services.docker.networking import (
    EGRESS_SIDECAR_LABEL,
    HOST_NETWORK_MODE,
    DockerNetworkingMixin,
)
from opensandbox_server.services.docker.container_ops import DockerContainerOpsMixin
from opensandbox_server.services.docker.metadata import DockerMetadataStore
from opensandbox_server.services.docker.port_allocator import (
    allocate_port_bindings,
    normalize_port_bindings,
)
from opensandbox_server.services.windows_common import (
    inject_windows_resource_limits_env,
    inject_windows_user_ports,
    is_windows_platform,
    validate_windows_resource_limits,
)
from opensandbox_server.services.docker.windows_profile import (
    apply_windows_runtime_host_config_defaults,
    validate_windows_runtime_prerequisites,
)
from opensandbox_server.services.extension_service import ExtensionService
from opensandbox_server.services.constants import (
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_EMBEDDING_PROXY_PORT_LABEL,
    SANDBOX_EXPIRES_AT_LABEL,
    SANDBOX_HTTP_PORT_LABEL,
    SANDBOX_ID_LABEL,
    SANDBOX_MANAGED_VOLUMES_LABEL,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SANDBOX_OSSFS_MOUNTS_LABEL,
    SANDBOX_SNAPSHOT_ID_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.endpoint_auth import (
    generate_egress_token,
)
from opensandbox_server.services.helpers import (
    matches_filter,
    parse_timestamp,
)
from opensandbox_server.services.docker.ossfs_mixin import OSSFSMixin
from opensandbox_server.services.sandbox_service import SandboxService
from opensandbox_server.services.runtime_resolver import SecureRuntimeResolver
from opensandbox_server.services.snapshot_restore import resolve_sandbox_image_from_request
from opensandbox_server.services.validators import (
    calculate_expiration_or_raise,
    ensure_entrypoint,
    ensure_future_expiration,
    ensure_metadata_labels,
    ensure_platform_valid,
    ensure_timeout_within_limit,
)
logger = logging.getLogger(__name__)

PENDING_FAILURE_TTL_SECONDS = int(os.environ.get("PENDING_FAILURE_TTL", "3600"))


@dataclass
class PendingSandbox:
    request: CreateSandboxRequest
    created_at: datetime
    expires_at: Optional[datetime]
    status: SandboxStatus


class DockerSandboxService(DockerDiagnosticsMixin, DockerRuntimeMixin, DockerVolumesMixin, DockerNetworkingMixin, DockerContainerOpsMixin, OSSFSMixin, SandboxService, ExtensionService):
    """
    Docker-based implementation of SandboxService.

    This class implements sandbox lifecycle operations using Docker containers.
    """

    def __init__(self, config: Optional[AppConfig] = None):
        """
        Initialize Docker sandbox service.

        Initializes Docker service from environment variables.
        The service will read configuration from:
        - DOCKER_HOST: Docker daemon URL (e.g., 'unix://var/run/docker.sock' or 'tcp://127.0.0.1:2376')
        - DOCKER_TLS_CERTDIR: Directory containing TLS certificates
        - Other Docker environment variables as needed

        Note: Connection is not verified at initialization time.
        Connection errors will be raised when Docker operations are performed.
        """
        self.app_config = config or get_config()
        runtime_config = self.app_config.runtime
        if runtime_config.type != "docker":
            raise ValueError("DockerSandboxService requires runtime.type = 'docker'.")

        self.execd_image = runtime_config.execd_image
        self.network_mode = (self.app_config.docker.network_mode or HOST_NETWORK_MODE).lower()
        self._execd_archive_cache: Dict[str, bytes] = {}
        self._windows_profile_cache: Dict[str, bytes] = {}
        self._daemon_platform: Optional[PlatformSpec] = None
        self._metadata_store = DockerMetadataStore()
        self._api_timeout = self._resolve_api_timeout()
        try:
            # Initialize Docker service from environment variables
            client_kwargs = {}
            try:
                signature = inspect.signature(docker.from_env)
                if "timeout" in signature.parameters:
                    client_kwargs["timeout"] = self._api_timeout
            except (ValueError, TypeError):
                logger.debug(
                    "Unable to introspect docker.from_env signature; using default parameters."
                )
            self.docker_client = docker.from_env(**client_kwargs)
            if not client_kwargs:
                try:
                    self.docker_client.api.timeout = self._api_timeout
                except AttributeError:
                    logger.debug("Docker client API does not expose timeout attribute.")
            logger.info("Docker service initialized from environment")
        except Exception as e:  # noqa: BLE001
            # Common failure mode on macOS/dev machines: Docker daemon not running or socket path wrong.
            hint = ""
            msg = str(e)
            if isinstance(e, FileNotFoundError) or "No such file or directory" in msg:
                docker_host = os.environ.get("DOCKER_HOST", "")
                hint = (
                    " Docker daemon seems unavailable (unix socket not found). "
                    "Make sure Docker Desktop (or Colima/Rancher Desktop) is running. "
                    "If you use Colima on macOS, you may need to set "
                    "DOCKER_HOST=unix://${HOME}/.colima/default/docker.sock before starting the server. "
                    f"(current DOCKER_HOST='{docker_host}')"
                )
            raise HTTPException(
                status_code=status.HTTP_503_SERVICE_UNAVAILABLE,
                detail={
                    "code": SandboxErrorCodes.DOCKER_INITIALIZATION_ERROR,
                    "message": f"Failed to initialize Docker service: {str(e)}.{hint}",
                },
            )
        self._expiration_lock = Lock()
        self._execd_archive_lock = Lock()
        self._sandbox_expirations: Dict[str, datetime] = {}
        self._expiration_timers: Dict[str, Timer] = {}
        self._pending_sandboxes: Dict[str, PendingSandbox] = {}
        self._pending_lock = Lock()
        self._pending_cleanup_timers: Dict[str, Timer] = {}
        self._ossfs_mount_lock = Lock()
        self._ossfs_mount_ref_counts: Dict[str, int] = {}
        self._restore_existing_sandboxes()

        # Initialize secure runtime resolver
        self.resolver = SecureRuntimeResolver(self.app_config)
        self.docker_runtime = self.resolver.get_docker_runtime()

    def _resolve_api_timeout(self) -> int:
        """Docker API timeout in seconds: [docker].api_timeout if set, else default 180."""
        cfg = self.app_config.docker.api_timeout
        if cfg is not None and cfg >= 1:
            return cfg
        return 180

    @contextmanager
    def _docker_operation(self, action: str, sandbox_id: Optional[str] = None):
        """Context manager to log duration for Docker API calls."""
        op_id = sandbox_id or "shared"
        start = time.perf_counter()
        try:
            yield
        except Exception as exc:
            elapsed_ms = (time.perf_counter() - start) * 1000
            logger.warning(
                "sandbox=%s | action=%s | duration=%.2f | error=%s",
                op_id,
                action,
                elapsed_ms,
                exc,
            )
            raise
        else:
            elapsed_ms = (time.perf_counter() - start) * 1000
            logger.info(
                "sandbox=%s | action=%s | duration=%.2f",
                op_id,
                action,
                elapsed_ms,
            )

    def _get_container_by_sandbox_id(self, sandbox_id: str):
        """Helper to fetch the Docker container associated with a sandbox ID."""
        label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        try:
            containers = self.docker_client.containers.list(
                all=True, filters={"label": label_selector}
            )
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.CONTAINER_QUERY_FAILED,
                    "message": f"Failed to query sandbox containers: {str(exc)}",
                },
            ) from exc

        if not containers:
            raise HTTPException(
                status_code=status.HTTP_404_NOT_FOUND,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_NOT_FOUND,
                    "message": f"Sandbox {sandbox_id} not found.",
                },
            )

        return containers[0]

    def _schedule_expiration(
        self,
        sandbox_id: str,
        expires_at: datetime,
        *,
        update_expiration: bool = True,
        **expire_kwargs,
    ) -> None:
        """Schedule automatic sandbox termination at expiration time."""
        # Delay might already be negative if the timer should fire immediately
        delay = max(0.0, (expires_at - datetime.now(timezone.utc)).total_seconds())
        timer = Timer(
            delay,
            self._expire_sandbox,
            args=(sandbox_id,),
            kwargs=expire_kwargs or None,
        )
        timer.daemon = True
        with self._expiration_lock:
            # Replace existing timer (if any) so renew operations take effect immediately
            existing = self._expiration_timers.pop(sandbox_id, None)
            if existing:
                existing.cancel()
            if update_expiration:
                self._sandbox_expirations[sandbox_id] = expires_at
            self._expiration_timers[sandbox_id] = timer
        timer.start()

    def _remove_expiration_tracking(self, sandbox_id: str) -> None:
        """Remove expiration tracking and cancel any pending timers."""
        with self._expiration_lock:
            timer = self._expiration_timers.pop(sandbox_id, None)
            if timer:
                timer.cancel()
            self._sandbox_expirations.pop(sandbox_id, None)

    @staticmethod
    def _has_manual_cleanup(labels: Dict[str, str]) -> bool:
        """Return True when labels indicate manual cleanup mode."""
        return labels.get(SANDBOX_MANUAL_CLEANUP_LABEL, "").lower() == "true"

    def _get_tracked_expiration(
        self,
        sandbox_id: str,
        labels: Dict[str, str],
    ) -> Optional[datetime]:
        """Return the known expiration timestamp for the sandbox."""
        with self._expiration_lock:
            tracked = self._sandbox_expirations.get(sandbox_id)
        if tracked:
            return tracked
        label_value = labels.get(SANDBOX_EXPIRES_AT_LABEL)
        if label_value:
            return parse_timestamp(label_value)
        return None

    def _expire_sandbox(
        self,
        sandbox_id: str,
        fallback_mount_keys: Optional[list[str]] = None,
    ) -> None:
        """Timer callback to terminate expired sandboxes."""
        mount_keys: list[str] = []
        try:
            container = self._get_container_by_sandbox_id(sandbox_id)
        except HTTPException as exc:
            if exc.status_code == status.HTTP_404_NOT_FOUND:
                self._remove_expiration_tracking(sandbox_id)
                self._cleanup_windows_oem_volume(sandbox_id, None)
                if fallback_mount_keys:
                    self._release_ossfs_mounts(fallback_mount_keys)
            else:
                with self._expiration_lock:
                    current_expires = self._sandbox_expirations.get(sandbox_id)
                now = datetime.now(timezone.utc)
                if current_expires and current_expires > now:
                    logger.info(
                        "Sandbox %s expiration was renewed; skipping retry.",
                        sandbox_id,
                    )
                else:
                    logger.warning(
                        "Failed to fetch sandbox %s for expiration: %s — "
                        "scheduling retry in 30s",
                        sandbox_id,
                        exc.detail,
                    )
                    retry_at = now + timedelta(seconds=30)
                    self._schedule_expiration(
                        sandbox_id,
                        retry_at,
                        update_expiration=False,
                        fallback_mount_keys=fallback_mount_keys,
                    )
            return

        with self._expiration_lock:
            current_expires = self._sandbox_expirations.get(sandbox_id)
        if current_expires and current_expires > datetime.now(timezone.utc):
            logger.info(
                "Sandbox %s was renewed (expires %s); aborting expiration.",
                sandbox_id,
                current_expires,
            )
            return

        labels = container.attrs.get("Config", {}).get("Labels") or {}
        mount_keys_raw = labels.get(SANDBOX_OSSFS_MOUNTS_LABEL, "[]")
        try:
            parsed_mount_keys = json.loads(mount_keys_raw)
            if isinstance(parsed_mount_keys, list):
                mount_keys = [key for key in parsed_mount_keys if isinstance(key, str) and key]
        except (TypeError, json.JSONDecodeError):
            mount_keys = []

        try:
            state = container.attrs.get("State", {})
            if state.get("Running", False):
                container.kill()
        except DockerException as exc:
            logger.warning("Failed to stop expired sandbox %s: %s", sandbox_id, exc)

        try:
            container.remove(force=True)
        except DockerException as exc:
            logger.warning("Failed to remove expired sandbox %s: %s", sandbox_id, exc)

        self._remove_expiration_tracking(sandbox_id)
        # Ensure sidecar is also cleaned up on expiration
        self._cleanup_egress_sidecar(sandbox_id)
        self._cleanup_windows_oem_volume(sandbox_id, labels)
        self._release_ossfs_mounts(mount_keys)
        self._metadata_store.delete(sandbox_id)

    def _restore_existing_sandboxes(self) -> None:
        """On startup, rebuild expiration timers for containers already running."""
        try:
            containers = self.docker_client.containers.list(all=True)
        except DockerException as exc:
            logger.warning("Failed to restore existing sandboxes: %s", exc)
            return

        restored = 0
        seen_sidecars: set[str] = set()
        restored_mount_refs: dict[str, int] = {}
        expired_entries: list[tuple[str, list[str]]] = []
        now = datetime.now(timezone.utc)

        def _parse_and_accumulate_mount_refs(labels: dict) -> list[str]:
            mount_keys_raw = labels.get(SANDBOX_OSSFS_MOUNTS_LABEL, "[]")
            try:
                parsed = json.loads(mount_keys_raw)
            except (TypeError, json.JSONDecodeError):
                parsed = []
            keys: list[str] = []
            if isinstance(parsed, list):
                for key in parsed:
                    if isinstance(key, str) and key:
                        keys.append(key)
                        restored_mount_refs[key] = restored_mount_refs.get(key, 0) + 1
            return keys

        # Pass 1: collect ref counts for ALL sandbox containers (alive + expired)
        # and schedule timers for alive ones.  Expired sandboxes are deferred to
        # pass 2 so that ref counts are fully populated before any release.
        for container in containers:
            labels = container.attrs.get("Config", {}).get("Labels") or {}
            sidecar_for = labels.get(EGRESS_SIDECAR_LABEL)
            if sidecar_for:
                seen_sidecars.add(sidecar_for)
                continue

            sandbox_id = labels.get(SANDBOX_ID_LABEL)
            if not sandbox_id:
                continue

            mount_keys = _parse_and_accumulate_mount_refs(labels)

            expires_label = labels.get(SANDBOX_EXPIRES_AT_LABEL)
            if expires_label:
                expires_at = parse_timestamp(expires_label)
            elif self._has_manual_cleanup(labels):
                restored += 1
                continue
            else:
                logger.warning(
                    "Sandbox %s missing expires-at label; skipping expiration scheduling.",
                    sandbox_id,
                )
                continue

            if expires_at <= now:
                logger.info("Sandbox %s already expired; terminating now.", sandbox_id)
                expired_entries.append((sandbox_id, mount_keys))
                continue

            self._schedule_expiration(sandbox_id, expires_at)
            restored += 1

        # Populate ref counts before expiring anything so _release_ossfs_mount
        # can properly decrement and unmount.
        with self._ossfs_mount_lock:
            self._ossfs_mount_ref_counts = restored_mount_refs

        # Pass 2: expire deferred sandboxes (ref counts are now available).
        # Cached mount keys are passed as fallback so that mounts are still
        # released even if the container vanishes between pass 1 and pass 2.
        for sandbox_id, cached_mount_keys in expired_entries:
            self._expire_sandbox(sandbox_id, fallback_mount_keys=cached_mount_keys)

        # Cleanup orphan sidecars (no matching sandbox container)
        for orphan_id in seen_sidecars:
            try:
                self._get_container_by_sandbox_id(orphan_id)
            except HTTPException as exc:
                if exc.status_code == status.HTTP_404_NOT_FOUND:
                    self._cleanup_egress_sidecar(orphan_id)
                else:
                    logger.warning(
                        "Failed to check sandbox %s for orphan sidecar cleanup: %s", orphan_id, exc
                    )

        if restored:
            logger.info("Restored expiration timers for %d sandbox(es).", restored)

    def _container_to_sandbox(self, container, sandbox_id: Optional[str] = None) -> Sandbox:
        labels = container.attrs.get("Config", {}).get("Labels") or {}
        resolved_id = sandbox_id or labels.get(SANDBOX_ID_LABEL)
        if not resolved_id:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_NOT_FOUND,
                    "message": "Container missing sandbox ID label.",
                },
            )

        status_section = container.attrs.get("State", {})
        status_value = (status_section.get("Status") or container.status or "").lower()
        running = status_section.get("Running", False)
        paused = status_section.get("Paused", False)
        restarting = status_section.get("Restarting", False)
        exit_code = status_section.get("ExitCode")
        finished_at = status_section.get("FinishedAt")

        if running and not paused:
            state = "Running"
            reason = "CONTAINER_RUNNING"
            message = "Sandbox container is running."
        elif paused:
            state = "Paused"
            reason = "CONTAINER_PAUSED"
            message = "Sandbox container is paused."
        elif restarting:
            state = "Running"
            reason = "CONTAINER_RESTARTING"
            message = "Sandbox container is restarting."
        elif status_value in {"created", "starting"}:
            state = "Pending"
            reason = "CONTAINER_STARTING"
            message = "Sandbox container is starting."
        elif status_value in {"exited", "dead"}:
            if exit_code == 0:
                state = "Terminated"
                reason = "CONTAINER_EXITED"
                message = "Sandbox container exited successfully."
            else:
                state = "Failed"
                reason = "CONTAINER_EXITED_ERROR"
                message = f"Sandbox container exited with code {exit_code}."
        else:
            state = "Unknown"
            reason = "CONTAINER_STATE_UNKNOWN"
            message = f"Sandbox container is in state '{status_value or 'unknown'}'."

        metadata = self._metadata_store.get(resolved_id, labels)
        entrypoint = container.attrs.get("Config", {}).get("Cmd") or []
        if isinstance(entrypoint, str):
            entrypoint = [entrypoint]
        image_tags = container.image.tags
        image_uri = image_tags[0] if image_tags else container.image.short_id
        snapshot_id = labels.get(SANDBOX_SNAPSHOT_ID_LABEL)
        image_spec = None if snapshot_id else ImageSpec(uri=image_uri)

        created_at = parse_timestamp(container.attrs.get("Created"))
        last_transition_at = (
            parse_timestamp(finished_at)
            if finished_at and finished_at != "0001-01-01T00:00:00Z"
            else created_at
        )
        expires_at = self._get_tracked_expiration(resolved_id, labels)

        status_info = SandboxStatus(
            state=state,
            reason=reason,
            message=message,
            last_transition_at=last_transition_at,
        )
        platform_spec = self._resolve_platform_for_container(container, labels)

        return Sandbox(
            id=resolved_id,
            image=image_spec,
            snapshotId=snapshot_id,
            platform=platform_spec,
            status=status_info,
            metadata=metadata,
            entrypoint=entrypoint,
            expiresAt=expires_at,
            createdAt=created_at,
        )

    def _prepare_creation_context(
        self,
        request: CreateSandboxRequest,
    ) -> tuple[str, datetime, Optional[datetime]]:
        sandbox_id = self.generate_sandbox_id()
        created_at = datetime.now(timezone.utc)
        expires_at = None
        if request.timeout is not None:
            expires_at = calculate_expiration_or_raise(created_at, request.timeout)
        return sandbox_id, created_at, expires_at

    async def create_sandbox(self, request: CreateSandboxRequest) -> CreateSandboxResponse:
        """
        Create a new sandbox from a container image using Docker.

        Args:
            request: Sandbox creation request

        Returns:
            CreateSandboxResponse: Created sandbox information

        Raises:
            HTTPException: If sandbox creation fails
        """
        if (request.extensions or {}).get("poolRef", "").strip():
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": "SANDBOX::UNSUPPORTED_POOL_REF",
                    "message": "poolRef is not supported by the Docker provider. Use Kubernetes BatchSandbox provider instead.",
                },
            )
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
        self._validate_network_exists()
        pvc_inspect_cache, auto_created_volumes = self._validate_volumes(request)
        sandbox_id, created_at, expires_at = self._prepare_creation_context(request)
        return self._provision_sandbox(
            sandbox_id, request, created_at, expires_at, pvc_inspect_cache, auto_created_volumes,
        )

    def _async_provision_worker(
        self,
        sandbox_id: str,
        request: CreateSandboxRequest,
        created_at: datetime,
        expires_at: Optional[datetime],
        pvc_inspect_cache: Optional[dict[str, dict]] = None,
    ) -> None:
        try:
            self._provision_sandbox(sandbox_id, request, created_at, expires_at, pvc_inspect_cache)
        except HTTPException as exc:
            message = exc.detail.get("message") if isinstance(exc.detail, dict) else str(exc)
            self._mark_pending_failed(sandbox_id, message or "Sandbox provisioning failed.")
            self._cleanup_failed_containers(sandbox_id)
            self._schedule_pending_cleanup(sandbox_id)
        except Exception as exc:  # noqa: BLE001
            logger.exception("Unexpected error provisioning sandbox %s: %s", sandbox_id, exc)
            self._mark_pending_failed(sandbox_id, str(exc))
            self._cleanup_failed_containers(sandbox_id)
            self._schedule_pending_cleanup(sandbox_id)
        else:
            self._remove_pending_sandbox(sandbox_id)

    def _mark_pending_failed(self, sandbox_id: str, message: str) -> None:
        with self._pending_lock:
            pending = self._pending_sandboxes.get(sandbox_id)
            if not pending:
                return
            pending.status = SandboxStatus(
                state="Failed",
                reason="PROVISIONING_ERROR",
                message=message,
                last_transition_at=datetime.now(timezone.utc),
            )

    def _cleanup_failed_containers(self, sandbox_id: str) -> None:
        """
        Best-effort cleanup for containers left behind after a failed provision.
        """
        label_selector = f"{SANDBOX_ID_LABEL}={sandbox_id}"
        try:
            containers = self.docker_client.containers.list(
                all=True, filters={"label": label_selector}
            )
        except DockerException as exc:
            logger.warning("sandbox=%s | cleanup listing failed containers: %s", sandbox_id, exc)
            self._cleanup_egress_sidecar(sandbox_id)
            return

        for container in containers:
            labels = container.attrs.get("Config", {}).get("Labels") or {}
            mount_keys_raw = labels.get(SANDBOX_OSSFS_MOUNTS_LABEL, "[]")
            try:
                mount_keys: list[str] = json.loads(mount_keys_raw)
            except (TypeError, json.JSONDecodeError):
                mount_keys = []
            try:
                with self._docker_operation("cleanup failed sandbox container", sandbox_id):
                    container.remove(force=True)
            except DockerException as exc:
                logger.warning(
                    "sandbox=%s | failed to remove leftover container %s: %s",
                    sandbox_id,
                    container.id,
                    exc,
                )
            finally:
                self._release_ossfs_mounts(mount_keys)
        # Always attempt to cleanup sidecar as well
        self._cleanup_egress_sidecar(sandbox_id)

    def _remove_pending_sandbox(self, sandbox_id: str) -> None:
        with self._pending_lock:
            timer = self._pending_cleanup_timers.pop(sandbox_id, None)
            if timer:
                timer.cancel()
            self._pending_sandboxes.pop(sandbox_id, None)

    def _get_pending_sandbox(self, sandbox_id: str) -> Optional[PendingSandbox]:
        with self._pending_lock:
            pending = self._pending_sandboxes.get(sandbox_id)
            return pending

    def _iter_pending_sandboxes(self) -> list[tuple[str, PendingSandbox]]:
        with self._pending_lock:
            return list(self._pending_sandboxes.items())

    @staticmethod
    def _pending_to_sandbox(sandbox_id: str, pending: PendingSandbox) -> Sandbox:
        snapshot_id = getattr(pending.request, "snapshot_id", None)
        if not isinstance(snapshot_id, str) or not snapshot_id:
            snapshot_id = None
        return Sandbox(
            id=sandbox_id,
            image=None if snapshot_id else pending.request.image,
            snapshotId=snapshot_id,
            platform=pending.request.platform,
            status=pending.status,
            metadata=pending.request.metadata,
            entrypoint=pending.request.entrypoint,
            expiresAt=pending.expires_at,
            createdAt=pending.created_at,
        )

    def _schedule_pending_cleanup(self, sandbox_id: str) -> None:
        def _cleanup():
            self._remove_pending_sandbox(sandbox_id)

        timer = Timer(PENDING_FAILURE_TTL_SECONDS, _cleanup)
        timer.daemon = True
        with self._pending_lock:
            existing = self._pending_cleanup_timers.pop(sandbox_id, None)
            if existing:
                existing.cancel()
            self._pending_cleanup_timers[sandbox_id] = timer
        timer.start()

    def _provision_sandbox(
        self,
        sandbox_id: str,
        request: CreateSandboxRequest,
        created_at: datetime,
        expires_at: Optional[datetime],
        pvc_inspect_cache: Optional[dict[str, dict]] = None,
        auto_created_volumes: Optional[list[str]] = None,
    ) -> CreateSandboxResponse:
        labels, environment = self._build_labels_and_env(sandbox_id, request, expires_at)
        if auto_created_volumes:
            labels[SANDBOX_MANAGED_VOLUMES_LABEL] = json.dumps(
                auto_created_volumes, separators=(",", ":"),
            )
        image_uri, auth_config = self._resolve_image_auth(request, sandbox_id)
        mem_limit, nano_cpus, gpu_count = self._resolve_resource_limits(request)
        egress_token: Optional[str] = None
        requested_windows_profile = is_windows_platform(request.platform)

        if requested_windows_profile:
            validate_windows_resource_limits((request.resource_limits.root if request.resource_limits else None) or {})
            validate_windows_runtime_prerequisites()

        # Prepare OSSFS mounts first so binds can reference mounted host paths.
        ossfs_mount_keys = self._prepare_ossfs_mounts(request.volumes)
        if ossfs_mount_keys:
            labels[SANDBOX_OSSFS_MOUNTS_LABEL] = json.dumps(
                ossfs_mount_keys,
                separators=(",", ":"),
            )

        sidecar_container = None
        try:
            # For dockur/windows profile, resourceLimits are translated to
            # guest envs (RAM_SIZE/CPU_CORES/DISK_SIZE). Avoid applying
            # container cgroup memory/cpu limits to the outer Linux container,
            # which can OOM-kill QEMU during installation/runtime. GPU
            # passthrough is likewise suppressed: the Windows guest runs inside
            # QEMU and would not see GPUs exposed to the outer container.
            effective_mem_limit = None if requested_windows_profile else mem_limit
            effective_nano_cpus = None if requested_windows_profile else nano_cpus
            effective_gpu_count = None if requested_windows_profile else gpu_count

            # Build volume bind mounts from request volumes.
            # pvc_inspect_cache carries Docker volume inspect data from the
            # validation phase, avoiding a redundant API call.
            volume_binds = self._build_volume_binds(request.volumes, pvc_inspect_cache)

            host_config_kwargs: Dict[str, Any]
            exposed_ports: Optional[list[str]] = ["44772", "8080"]
            if requested_windows_profile:
                # dockur/windows exposes RDP and noVNC/web UI on these ports.
                # https://github.com/dockur/windows/blob/master/Dockerfile
                exposed_ports.extend(["3389/tcp", "3389/udp", "8006/tcp"])
            container_exposed_ports: Optional[list[str]] = exposed_ports

            if request.network_policy:
                egress_token = generate_egress_token()
                labels[SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY] = egress_token
                sidecar_port_bindings = allocate_port_bindings(exposed_ports)
                host_execd_port = sidecar_port_bindings["44772"][1]
                host_http_port = sidecar_port_bindings["8080"][1]
                extra_sidecar_port_bindings = {
                    port: binding
                    for port, binding in sidecar_port_bindings.items()
                    if port not in {"44772", "8080"}
                }
                sidecar_container = self._start_egress_sidecar(
                    sandbox_id=sandbox_id,
                    network_policy=request.network_policy,
                    egress_token=egress_token,
                    host_execd_port=host_execd_port,
                    host_http_port=host_http_port,
                    extra_port_bindings=extra_sidecar_port_bindings,
                )
                labels[SANDBOX_EMBEDDING_PROXY_PORT_LABEL] = str(host_execd_port)
                labels[SANDBOX_HTTP_PORT_LABEL] = str(host_http_port)
                host_config_kwargs = self._base_host_config_kwargs(
                    effective_mem_limit, effective_nano_cpus, f"container:{sidecar_container.id}",
                    gpu_count=effective_gpu_count,
                )
                # Container network namespace is shared with sidecar. Docker rejects
                # exposing ports on the main container in "container:<id>" mode.
                container_exposed_ports = None
                # Drop NET_ADMIN for the main container; only the sidecar should keep it
                cap_drop = set(host_config_kwargs.get("cap_drop") or [])
                cap_drop.add("NET_ADMIN")
                if cap_drop:
                    host_config_kwargs["cap_drop"] = list(cap_drop)
            else:
                host_config_kwargs = self._base_host_config_kwargs(
                    effective_mem_limit, effective_nano_cpus, self.network_mode,
                    gpu_count=effective_gpu_count,
                )
                if self.network_mode != HOST_NETWORK_MODE:
                    port_bindings = allocate_port_bindings(exposed_ports)
                    host_execd_port = port_bindings["44772"][1]
                    host_http_port = port_bindings["8080"][1]
                    host_config_kwargs["port_bindings"] = normalize_port_bindings(port_bindings)
                    labels[SANDBOX_EMBEDDING_PROXY_PORT_LABEL] = str(host_execd_port)
                    labels[SANDBOX_HTTP_PORT_LABEL] = str(host_http_port)
                else:
                    exposed_ports = None

            # Inject volume bind mounts into Docker host config
            if volume_binds:
                host_config_kwargs["binds"] = volume_binds
            if requested_windows_profile:
                host_config_kwargs = apply_windows_runtime_host_config_defaults(
                    host_config_kwargs,
                    sandbox_id,
                )
                environment = inject_windows_resource_limits_env(
                    environment,
                    (request.resource_limits.root if request.resource_limits else None) or {},
                )
                environment = inject_windows_user_ports(environment, exposed_ports)

            created_container = self._create_and_start_container(
                sandbox_id,
                image_uri,
                request.entrypoint,
                labels,
                environment,
                host_config_kwargs,
                container_exposed_ports,
                request.platform,
            )
        except Exception:
            if sidecar_container is not None:
                try:
                    sidecar_container.remove(force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup egress sidecar for sandbox %s: %s",
                        sandbox_id,
                        cleanup_exc,
                    )
            self._release_ossfs_mounts(ossfs_mount_keys)
            self._cleanup_managed_volumes(sandbox_id, auto_created_volumes or [])
            raise

        status_info = SandboxStatus(
            state="Running",
            reason="CONTAINER_RUNNING",
            message="Sandbox container started successfully.",
            last_transition_at=created_at,
        )

        if expires_at is not None:
            self._schedule_expiration(sandbox_id, expires_at)

        effective_platform = self._resolve_platform_for_container(created_container, labels)
        return CreateSandboxResponse(
            id=sandbox_id,
            status=status_info,
            metadata=request.metadata,
            platform=effective_platform or request.platform,
            expiresAt=expires_at,
            createdAt=created_at,
            entrypoint=request.entrypoint,
        )

    def list_sandboxes(self, request: ListSandboxesRequest) -> ListSandboxesResponse:
        """
        List sandboxes with optional filtering and pagination.
        """
        try:
            containers = self.docker_client.containers.list(
                all=True,
                filters={"label": [SANDBOX_ID_LABEL]},
            )
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.CONTAINER_QUERY_FAILED,
                    "message": f"Failed to query sandbox containers: {str(exc)}",
                },
            ) from exc

        sandboxes_by_id: dict[str, Sandbox] = {}
        container_ids: set[str] = set()
        for container in containers:
            labels = container.attrs.get("Config", {}).get("Labels") or {}
            sandbox_id = labels.get(SANDBOX_ID_LABEL)
            if not sandbox_id:
                continue
            sandbox_obj = self._container_to_sandbox(container, sandbox_id)
            container_ids.add(sandbox_id)
            if matches_filter(sandbox_obj, request.filter):
                sandboxes_by_id[sandbox_id] = sandbox_obj

        for sandbox_id, pending in self._iter_pending_sandboxes():
            if sandbox_id in container_ids:
                # If a real container exists, prefer its state regardless of filter outcome.
                continue
            sandbox_obj = self._pending_to_sandbox(sandbox_id, pending)
            if matches_filter(sandbox_obj, request.filter):
                sandboxes_by_id[sandbox_id] = sandbox_obj

        sandboxes: list[Sandbox] = list(sandboxes_by_id.values())

        sandboxes.sort(key=lambda s: s.created_at or datetime.min, reverse=True)

        if request.pagination:
            page = request.pagination.page
            page_size = request.pagination.page_size
        else:
            page = 1
            page_size = 20

        total_items = len(sandboxes)
        total_pages = math.ceil(total_items / page_size) if total_items else 0
        start_index = (page - 1) * page_size
        end_index = start_index + page_size
        items = sandboxes[start_index:end_index]
        has_next_page = page < total_pages

        pagination_info = PaginationInfo(
            page=page,
            page_size=page_size,
            total_items=total_items,
            total_pages=total_pages,
            has_next_page=has_next_page,
        )

        return ListSandboxesResponse(items=items, pagination=pagination_info)

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """
        Fetch a sandbox by id.

        Args:
            sandbox_id: Unique sandbox identifier

        Returns:
            Sandbox: Complete sandbox information

        Raises:
            HTTPException: If sandbox not found
        """
        # Prefer real container state; fall back to pending record only if no container exists.
        try:
            container = self._get_container_by_sandbox_id(sandbox_id)
        except HTTPException as exc:
            if exc.status_code != status.HTTP_404_NOT_FOUND:
                raise
            pending = self._get_pending_sandbox(sandbox_id)
            if pending:
                return self._pending_to_sandbox(sandbox_id, pending)
            raise
        return self._container_to_sandbox(container, sandbox_id)

    def delete_sandbox(self, sandbox_id: str) -> None:
        """
        Delete a sandbox using Docker.

        Args:
            sandbox_id: Unique sandbox identifier

        Raises:
            HTTPException: If sandbox not found or deletion fails
        """
        container = self._get_container_by_sandbox_id(sandbox_id)
        labels = container.attrs.get("Config", {}).get("Labels") or {}
        mount_keys_raw = labels.get(SANDBOX_OSSFS_MOUNTS_LABEL, "[]")
        try:
            mount_keys: list[str] = json.loads(mount_keys_raw)
        except (TypeError, json.JSONDecodeError):
            mount_keys = []
        managed_volumes_raw = labels.get(SANDBOX_MANAGED_VOLUMES_LABEL, "[]")
        try:
            managed_volumes: list[str] = json.loads(managed_volumes_raw)
        except (TypeError, json.JSONDecodeError):
            managed_volumes = []
        try:
            try:
                with self._docker_operation("kill sandbox container", sandbox_id):
                    container.kill()
            except DockerException as exc:
                # Ignore error if container is already stopped
                if "is not running" not in str(exc).lower():
                    raise
            with self._docker_operation("remove sandbox container", sandbox_id):
                container.remove(force=True)
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_DELETE_FAILED,
                    "message": f"Failed to delete sandbox container: {str(exc)}",
                },
            ) from exc
        finally:
            self._remove_expiration_tracking(sandbox_id)
            self._cleanup_egress_sidecar(sandbox_id)
            self._cleanup_windows_oem_volume(sandbox_id, labels)
            self._release_ossfs_mounts(mount_keys)
            self._cleanup_managed_volumes(sandbox_id, managed_volumes)
            self._metadata_store.delete(sandbox_id)

    def pause_sandbox(self, sandbox_id: str) -> None:
        """
        Pause a running sandbox using Docker.

        Args:
            sandbox_id: Unique sandbox identifier

        Raises:
            HTTPException: If sandbox not found or cannot be paused
        """
        container = self._get_container_by_sandbox_id(sandbox_id)
        state = container.attrs.get("State", {})
        if not state.get("Running", False):
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_NOT_RUNNING,
                    "message": "Sandbox is not in a running state.",
                },
            )

        try:
            with self._docker_operation("pause sandbox container", sandbox_id):
                container.pause()
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_PAUSE_FAILED,
                    "message": f"Failed to pause sandbox container: {str(exc)}",
                },
            ) from exc

    def resume_sandbox(self, sandbox_id: str) -> None:
        """
        Resume a paused sandbox using Docker.

        Args:
            sandbox_id: Unique sandbox identifier

        Raises:
            HTTPException: If sandbox not found or cannot be resumed
        """
        container = self._get_container_by_sandbox_id(sandbox_id)
        state = container.attrs.get("State", {})
        if not state.get("Paused", False):
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_NOT_PAUSED,
                    "message": "Sandbox is not in a paused state.",
                },
            )

        try:
            with self._docker_operation("resume sandbox container", sandbox_id):
                container.unpause()
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.SANDBOX_RESUME_FAILED,
                    "message": f"Failed to resume sandbox container: {str(exc)}",
                },
            ) from exc

    def get_access_renew_extend_seconds(self, sandbox_id: str) -> Optional[int]:
        try:
            container = self._get_container_by_sandbox_id(sandbox_id)
        except HTTPException:
            return None
        labels = container.attrs.get("Config", {}).get("Labels") or {}
        raw = labels.get(ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY)
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

        Args:
            sandbox_id: Unique sandbox identifier
            request: Renewal request with new expiration time

        Returns:
            RenewSandboxExpirationResponse: Updated expiration time

        Raises:
            HTTPException: If sandbox not found or renewal fails
        """
        container = self._get_container_by_sandbox_id(sandbox_id)
        new_expiration = ensure_future_expiration(request.expires_at)

        labels = container.attrs.get("Config", {}).get("Labels") or {}
        if self._has_manual_cleanup(labels):
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.INVALID_EXPIRATION,
                    "message": f"Sandbox {sandbox_id} does not have automatic expiration enabled.",
                },
            )
        if self._get_tracked_expiration(sandbox_id, labels) is None:
            raise HTTPException(
                status_code=status.HTTP_409_CONFLICT,
                detail={
                    "code": SandboxErrorCodes.INVALID_EXPIRATION,
                    "message": (
                        f"Sandbox {sandbox_id} is missing expiration metadata and cannot be renewed safely."
                    ),
                },
            )

        # Persist the new timeout in memory; it will also be respected on restart via _restore_existing_sandboxes
        self._schedule_expiration(sandbox_id, new_expiration)
        labels[SANDBOX_EXPIRES_AT_LABEL] = new_expiration.isoformat()
        try:
            with self._docker_operation("update sandbox labels", sandbox_id):
                self._update_container_labels(container, labels)
        except (DockerException, TypeError) as exc:
            logger.warning("Failed to refresh labels for sandbox %s: %s", sandbox_id, exc)

        return RenewSandboxExpirationResponse(expires_at=new_expiration)

    # Patch sandbox metadata

    def patch_sandbox_metadata(self, sandbox_id: str, patch: PatchSandboxMetadataRequest) -> Sandbox:
        """Patch sandbox metadata via JSON Merge Patch (RFC 7396). Docker cannot update labels on running containers, so metadata is persisted to file."""
        from opensandbox_server.services.validators import ensure_metadata_labels

        container = self._get_container_by_sandbox_id(sandbox_id)
        labels = dict(container.attrs.get("Config", {}).get("Labels") or {})

        # Reject reserved keys
        for key in patch:
            if SandboxService._is_system_label(key):
                raise HTTPException(
                    status_code=400,
                    detail={
                        "code": "INVALID_METADATA_LABEL",
                        "message": f"Metadata key '{key}' is reserved (opensandbox.io/ prefix).",
                    },
                )

        # Validate only incoming patch values
        patch_additions = {k: str(v) for k, v in patch.items() if v is not None}
        if patch_additions:
            ensure_metadata_labels(patch_additions)

        self._metadata_store.patch(sandbox_id, labels, patch)
        return self._container_to_sandbox(container, sandbox_id)
