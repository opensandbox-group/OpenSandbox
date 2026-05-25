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
Network management mixin for Docker sandboxes.

Provides network validation, endpoint resolution, egress sidecar lifecycle,
and bridge IP extraction. Mixed into DockerSandboxService.
"""

from __future__ import annotations

import json
import logging
import os
import socket
from typing import Any, Optional

from docker.errors import DockerException, NotFound as DockerNotFound
from fastapi import HTTPException, status

from opensandbox_server.api.schema import Endpoint, NetworkPolicy
from opensandbox_server.services.constants import (
    EGRESS_MODE_ENV,
    EGRESS_RULES_ENV,
    OPENSANDBOX_EGRESS_TOKEN,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_EMBEDDING_PROXY_PORT_LABEL,
    SANDBOX_HTTP_PORT_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.services.docker.port_allocator import (
    normalize_container_port_spec,
    normalize_port_bindings,
)
from opensandbox_server.services.endpoint_auth import (
    build_egress_auth_headers,
    merge_endpoint_headers,
)
from opensandbox_server.services.validators import ensure_egress_configured

logger = logging.getLogger(__name__)


def _running_inside_docker_container() -> bool:
    """Return True if the current process is running inside a Docker container."""
    return os.path.exists("/.dockerenv")


HOST_NETWORK_MODE = "host"
BRIDGE_NETWORK_MODE = "bridge"
EGRESS_SIDECAR_LABEL = "opensandbox.io/egress-sidecar-for"


class DockerNetworkingMixin:
    """Mixin providing network validation, endpoint resolution, and egress sidecar."""

    def _is_user_defined_network(self) -> bool:
        """Return True when network_mode is a named user-defined network (not host/bridge/none/container:*)."""
        return (
            self.network_mode not in {HOST_NETWORK_MODE, BRIDGE_NETWORK_MODE, "none"}
            and not self.network_mode.startswith("container:")
        )

    def _validate_network_exists(self) -> None:
        """Verify the configured user-defined Docker network exists before creating a sandbox."""
        if not self._is_user_defined_network():
            return
        try:
            self.docker_client.networks.get(self.network_mode)
        except DockerNotFound:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        f"Docker network '{self.network_mode}' does not exist. "
                        "Create it first with 'docker network create <name>'."
                    ),
                },
            )
        except DockerException as exc:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                    "message": f"Failed to inspect Docker network '{self.network_mode}': {exc}",
                },
            ) from exc

    def _ensure_network_policy_support(self, request) -> None:
        """
        Validate that network policy can be honored under the current runtime config.

        This includes Docker-specific checks (network_mode) and common checks (egress.image).
        """
        if not request.network_policy:
            return

        # Docker-specific validation: network_mode must be bridge
        if self.network_mode == HOST_NETWORK_MODE:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": "networkPolicy is not supported when docker network_mode=host.",
                },
            )

        # User-defined networks cannot be combined with networkPolicy: the egress sidecar
        # always runs on the default bridge, which would silently discard the configured network.
        if self._is_user_defined_network():
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PARAMETER,
                    "message": (
                        f"networkPolicy is not supported when docker network_mode='{self.network_mode}' "
                        "(user-defined network). Use network_mode='bridge' to enable network policy enforcement."
                    ),
                },
            )

        # Common validation: egress.image must be configured
        ensure_egress_configured(request.network_policy, self.app_config.egress)

    def _ensure_secure_access_support(self, request) -> None:
        """Validate that secure access can be honored under the current Docker runtime."""
        if not request.secure_access:
            return

        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    "secureAccess is not supported when runtime.type='docker'. "
                    "Use the Kubernetes runtime to create secured sandboxes."
                ),
            },
        )

    def get_endpoint(self, sandbox_id: str, port: int, resolve_internal: bool = False,
                     expires: Optional[int] = None) -> Endpoint:
        """
        Get sandbox access endpoint.

        Args:
            sandbox_id: Unique sandbox identifier
            port: Port number where the service is listening inside the sandbox
            resolve_internal: If True, return the internal container IP (for proxy), ignoring router config.
            expires: Not supported by Docker runtime.

        Returns:
            Endpoint: Public endpoint URL

        Raises:
            HTTPException: If sandbox not found, endpoint not available,
                or expires is provided (Docker does not support signed routes).
        """
        if expires is not None:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.API_NOT_SUPPORTED,
                    "message": (
                        "Signed routes (expires parameter) are not supported when "
                        "runtime.type='docker'. Use the Kubernetes runtime for signed routes."
                    ),
                },
            )

        try:
            self.validate_port(port)
        except ValueError as exc:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_PORT,
                    "message": str(exc),
                },
            ) from exc

        if resolve_internal:
            container = self._get_container_by_sandbox_id(sandbox_id)
            labels = container.attrs.get("Config", {}).get("Labels") or {}
            # Sandboxes created with egress sidecar share the sidecar network namespace, so the
            # main container's private IP is not a stable proxy target. In that case, treat the
            # server-proxy target as the server-local host-mapped endpoint instead of a container IP.
            if labels.get(SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY):
                return self._resolve_host_mapped_endpoint(
                    self._resolve_proxy_host(),
                    labels,
                    port,
                )
            return self._resolve_internal_endpoint(container, port)

        public_host = self._resolve_public_host()

        if self.network_mode == HOST_NETWORK_MODE:
            endpoint = Endpoint(endpoint=f"{public_host}:{port}")
            container = self._get_container_by_sandbox_id(sandbox_id)
            labels = container.attrs.get("Config", {}).get("Labels") or {}
            self._attach_egress_auth_headers(endpoint, labels)
            return endpoint

        # non-host mode (bridge / user-defined network)
        container = self._get_container_by_sandbox_id(sandbox_id)
        labels = container.attrs.get("Config", {}).get("Labels") or {}
        return self._resolve_host_mapped_endpoint(public_host, labels, port)

    def _resolve_host_mapped_endpoint(
        self,
        public_host: str,
        labels: dict[str, str],
        port: int,
    ) -> Endpoint:
        execd_host_port = self._parse_host_port_label(
            labels.get(SANDBOX_EMBEDDING_PROXY_PORT_LABEL),
            SANDBOX_EMBEDDING_PROXY_PORT_LABEL,
        )
        http_host_port = self._parse_host_port_label(
            labels.get(SANDBOX_HTTP_PORT_LABEL),
            SANDBOX_HTTP_PORT_LABEL,
        )

        if port == 8080:
            if http_host_port is None:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.NETWORK_MODE_ENDPOINT_UNAVAILABLE,
                        "message": "Missing host port mapping for container port 8080.",
                    },
                )
            endpoint = Endpoint(endpoint=f"{public_host}:{http_host_port}")
            self._attach_egress_auth_headers(endpoint, labels)
            return endpoint

        if execd_host_port is None:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.NETWORK_MODE_ENDPOINT_UNAVAILABLE,
                    "message": "Missing host port mapping for execd proxy port 44772.",
                },
            )

        endpoint = Endpoint(endpoint=f"{public_host}:{execd_host_port}/proxy/{port}")
        self._attach_egress_auth_headers(endpoint, labels)
        return endpoint

    def _attach_egress_auth_headers(
        self,
        endpoint: Endpoint,
        labels: dict[str, str],
    ) -> None:
        token = labels.get(SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY)
        if not token:
            return
        endpoint.headers = merge_endpoint_headers(
            endpoint.headers,
            build_egress_auth_headers(token),
        )

    def _get_docker_host_ip(self) -> Optional[str]:
        """When running inside a container, return [docker].host_ip for endpoint URLs (if set)."""
        ip = (self.app_config.docker.host_ip or "").strip()
        return ip or None

    def _resolve_public_host(self) -> str:
        """Resolve the host used in endpoint URLs. If [server].eip is set, use it directly without resolving host."""
        eip_cfg = (self.app_config.server.eip or "").strip()
        if eip_cfg:
            return eip_cfg
        host_cfg = (self.app_config.server.host or "").strip()
        host_key = host_cfg.lower()
        if host_key in {"", "0.0.0.0", "::"}:
            if _running_inside_docker_container():
                host_ip = self._get_docker_host_ip()
                if host_ip:
                    return host_ip
            return self._resolve_bind_ip(socket.AF_INET)
        return host_cfg

    def _resolve_proxy_host(self) -> str:
        """Resolve the server-local host used for proxying to host-mapped Docker endpoints.

        This intentionally does not use ``server.eip`` because the proxy target must be reachable
        from the server process itself, even in deployments without hairpin access to the public EIP.
        """
        host_cfg = (self.app_config.server.host or "").strip()
        host_key = host_cfg.lower()
        if host_key in {"", "0.0.0.0", "::"}:
            if _running_inside_docker_container():
                host_ip = self._get_docker_host_ip()
                if host_ip:
                    return host_ip
            return "127.0.0.1"
        return host_cfg

    def _resolve_internal_endpoint(self, container, port: int) -> Endpoint:
        """Return the internal endpoint used when bypassing host mapping."""
        if self.network_mode == HOST_NETWORK_MODE:
            return Endpoint(endpoint=f"127.0.0.1:{port}")

        ip_address = self._extract_bridge_ip(container)
        return Endpoint(endpoint=f"{ip_address}:{port}")

    # ---------------------------
    # Common helpers for creation
    # ---------------------------

    def _cleanup_egress_sidecar(self, sandbox_id: str) -> None:
        """
        Remove egress sidecar associated with sandbox_id (best effort).
        """
        try:
            containers = self.docker_client.containers.list(
                all=True, filters={"label": f"{EGRESS_SIDECAR_LABEL}={sandbox_id}"}
            )
        except DockerException as exc:
            logger.warning("sandbox=%s | failed to list egress sidecar: %s", sandbox_id, exc)
            return

        for container in containers:
            try:
                with self._docker_operation("cleanup egress sidecar", sandbox_id):
                    container.remove(force=True)
            except DockerException as exc:
                logger.warning(
                    "sandbox=%s | failed to remove egress sidecar %s: %s",
                    sandbox_id,
                    container.id,
                    exc,
                )

    def _start_egress_sidecar(
        self,
        sandbox_id: str,
        network_policy: NetworkPolicy,
        egress_token: str,
        host_execd_port: int,
        host_http_port: int,
        extra_port_bindings: Optional[dict[str, tuple[str, int]]] = None,
    ):
        sidecar_name = f"sandbox-egress-{sandbox_id}"
        sidecar_labels = {
            EGRESS_SIDECAR_LABEL: sandbox_id,
        }

        # Ensure sidecar image is available before create/start.
        egress_image = self.app_config.egress.image if self.app_config.egress else None
        if not egress_image:
            raise ValueError("egress.image must be configured when networkPolicy is provided.")
        self._ensure_image_available(egress_image, None, sandbox_id)

        policy_payload = json.dumps(network_policy.model_dump(by_alias=True, exclude_none=True))
        assert self.app_config.egress is not None  # validated by ensure_egress_configured with networkPolicy
        egress_mode = self.app_config.egress.mode
        sidecar_env = [
            f"{EGRESS_RULES_ENV}={policy_payload}",
            f"{EGRESS_MODE_ENV}={egress_mode}",
            f"{OPENSANDBOX_EGRESS_TOKEN}={egress_token}",
        ]

        sidecar_port_bindings: dict[str, tuple[str, int]] = {
            "44772": ("0.0.0.0", host_execd_port),
            "8080": ("0.0.0.0", host_http_port),
        }
        if extra_port_bindings:
            sidecar_port_bindings.update(extra_port_bindings)

        sidecar_host_config_kwargs: dict[str, Any] = {
            "network_mode": BRIDGE_NETWORK_MODE,
            "cap_add": ["NET_ADMIN"],
            "port_bindings": normalize_port_bindings(sidecar_port_bindings),
        }
        if self.app_config.egress.disable_ipv6:
            # Optional: disable IPv6 in the shared namespace when egress.disable_ipv6 is set.
            sidecar_host_config_kwargs["sysctls"] = {
                "net.ipv6.conf.all.disable_ipv6": 1,
                "net.ipv6.conf.default.disable_ipv6": 1,
                "net.ipv6.conf.lo.disable_ipv6": 1,
            }

        sidecar_host_config = self.docker_client.api.create_host_config(
            **sidecar_host_config_kwargs
        )

        sidecar_container = None
        sidecar_container_id: Optional[str] = None
        try:
            with self._docker_operation("create egress sidecar", sandbox_id):
                sidecar_resp = self.docker_client.api.create_container(
                    image=egress_image,
                    name=sidecar_name,
                    host_config=sidecar_host_config,
                    labels=sidecar_labels,
                    environment=sidecar_env,
                    # Expose the ports that have host bindings so Docker publishes them in bridge mode.
                    ports=[normalize_container_port_spec(p) for p in sidecar_port_bindings.keys()],
                )
            sidecar_container_id = sidecar_resp.get("Id")
            if not sidecar_container_id:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                        "message": "Docker did not return an egress sidecar container ID.",
                    },
                )
            sidecar_container = self.docker_client.containers.get(sidecar_container_id)
            with self._docker_operation("start egress sidecar", sandbox_id):
                sidecar_container.start()
            return sidecar_container
        except Exception as exc:
            if sidecar_container is not None:
                try:
                    with self._docker_operation("cleanup egress sidecar", sandbox_id):
                        sidecar_container.remove(force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup egress sidecar for sandbox %s: %s",
                        sandbox_id,
                        cleanup_exc,
                    )
            elif sidecar_container_id:
                try:
                    with self._docker_operation("cleanup egress sidecar (API)", sandbox_id):
                        self.docker_client.api.remove_container(sidecar_container_id, force=True)
                except DockerException as cleanup_exc:
                    logger.warning(
                        "Failed to cleanup egress sidecar for sandbox %s: %s",
                        sandbox_id,
                        cleanup_exc,
                    )
            if isinstance(exc, HTTPException):
                raise exc
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                    "message": "Egress sidecar container failed to start.",
                },
            ) from exc

    @staticmethod
    def _parse_host_port_label(value: Optional[str], label_name: str) -> Optional[int]:
        if not value:
            return None
        try:
            port = int(value)
            if port <= 0 or port > 65535:
                raise ValueError
            return port
        except ValueError:
            logger.warning("Invalid port label %s=%s", label_name, value)
            return None

    def _extract_bridge_ip(self, container) -> str:
        """Extract the IP address assigned to a container on a bridge or user-defined network.

        For user-defined networks, the top-level ``NetworkSettings.IPAddress`` is empty;
        the IP lives under ``NetworkSettings.Networks[<network-name>].IPAddress``.
        This method prefers the configured ``network_mode`` entry when it is a user-defined
        network, then falls back to any non-empty entry for robustness.
        """
        network_settings = container.attrs.get("NetworkSettings", {}) or {}
        networks = network_settings.get("Networks", {}) or {}
        ip_address: Optional[str] = None

        if self._is_user_defined_network():
            # Prefer the explicit network entry for the configured named network.
            net_conf = networks.get(self.network_mode) or {}
            ip_address = net_conf.get("IPAddress") or None

        if not ip_address:
            # Default bridge path (or fallback): check the top-level IPAddress first.
            ip_address = network_settings.get("IPAddress") or None

        if not ip_address:
            # Last resort: iterate all network entries and take the first populated IP.
            for net_conf in networks.values():
                if net_conf and net_conf.get("IPAddress"):
                    ip_address = net_conf.get("IPAddress")
                    break

        if not ip_address:
            raise HTTPException(
                status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                detail={
                    "code": SandboxErrorCodes.NETWORK_MODE_ENDPOINT_UNAVAILABLE,
                    "message": "Container is running but has no assigned IP address.",
                },
            )
        return ip_address
