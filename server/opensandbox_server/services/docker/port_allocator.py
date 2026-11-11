# Copyright 2026 The OpenSandbox Authors
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

from __future__ import annotations

import errno
import random
import socket
from threading import Lock
from typing import Dict, Optional

from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes

DOCKER_PUBLISH_HOST = "0.0.0.0"
# The probe is a short-lived availability check and must match Docker's
# publish scope; probing only localhost can miss ports bound on other host
# interfaces that Docker would later fail to publish.
PORT_PROBE_HOST = DOCKER_PUBLISH_HOST
# Keep ports claimed across the gap between the socket probe and Docker start so
# concurrent sandbox creations in this server process cannot select the same port.
_RESERVED_HOST_PORTS: set[int] = set()
_RESERVATION_LOCK = Lock()
MAX_PORT_PUBLISH_ATTEMPTS = 3


def is_port_publish_error(exc: Exception) -> bool:
    """Return True when Docker reports an unavailable / reserved / conflicting host port."""
    if isinstance(exc, HTTPException) and isinstance(exc.detail, dict):
        message = str(exc.detail.get("message", "")).lower()
    else:
        message = str(exc).lower()

    if getattr(exc, "__cause__", None) is not None:
        message = f"{message} {str(exc.__cause__).lower()}"

    return (
        "port is already allocated" in message
        or "ports are not available" in message
        or "address already in use" in message
        or ("bind:" in message and "forbidden" in message)
        or ("bind:" in message and "permission denied" in message)
    )


def normalize_container_port_spec(port_spec: str) -> str:
    token = str(port_spec).strip()
    if token.endswith("/tcp"):
        return token[:-4]
    return token


def normalize_port_bindings(
    port_bindings: dict[str, tuple[str, int]],
) -> dict[str, tuple[str, int]]:
    """
    Normalize binding keys to docker-py canonical forms.

    Docker port bindings accept "port" for tcp and "port/udp" for udp.
    """
    normalized: dict[str, tuple[str, int]] = {}
    for container_port, binding in port_bindings.items():
        normalized_key = normalize_container_port_spec(container_port)
        normalized[normalized_key] = binding
    return normalized


def _port_is_free(port: int, probe_host: str) -> bool:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            # This does not listen for or accept connections; it mirrors the
            # later Docker publish binding to catch host-wide port conflicts.
            # codeql[py/bind-socket-all-network-interfaces]
            sock.bind((probe_host, port))
        except OSError as exc:
            if exc.errno == errno.EADDRNOTAVAIL and probe_host != PORT_PROBE_HOST:
                # The publish host is not an address of this network namespace (the
                # server runs in a container and publishes on the host's bridge
                # gateway): the closest probe left is the wildcard, as before.
                return _port_is_free(port, PORT_PROBE_HOST)
            return False
    return True


def allocate_host_port(
    min_port: int = 40000,
    max_port: int = 60000,
    attempts: int = 50,
    probe_host: str = PORT_PROBE_HOST,
) -> Optional[int]:
    """Find a free TCP port in the range.

    ``probe_host`` is the address Docker will publish on ([docker].publish_host),
    so the probe and the later binding see the same conflicts.
    """
    for _ in range(attempts):
        port = random.randint(min_port, max_port)
        if _port_is_free(port, probe_host):
            return port
    return None


def _reserve_host_port(
    min_port: int = 40000,
    max_port: int = 60000,
    attempts: int = 50,
    probe_host: str = PORT_PROBE_HOST,
) -> Optional[int]:
    for _ in range(attempts):
        port = allocate_host_port(
            min_port=min_port,
            max_port=max_port,
            attempts=attempts,
            probe_host=probe_host,
        )
        if port is None:
            return None
        with _RESERVATION_LOCK:
            if port in _RESERVED_HOST_PORTS:
                continue
            _RESERVED_HOST_PORTS.add(port)
            return port
    return None


def allocate_port_bindings(
    container_ports: list[str],
    min_port: int = 40000,
    max_port: int = 60000,
    publish_host: str = DOCKER_PUBLISH_HOST,
) -> Dict[str, tuple[str, int]]:
    """Allocate distinct random host ports for each container port spec.

    Every binding is published on ``publish_host`` ([docker].publish_host):
    0.0.0.0 by default, or one address to keep sandbox ports off the other
    interfaces of the host.
    """
    bindings: Dict[str, tuple[str, int]] = {}
    completed = False
    try:
        for container_port in container_ports:
            host_port = _reserve_host_port(min_port=min_port, max_port=max_port, probe_host=publish_host)
            if host_port is None:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                        "message": "Failed to allocate host ports for sandbox container.",
                    },
                )
            bindings[container_port] = (publish_host, host_port)
        completed = True
        return bindings
    finally:
        if not completed:
            release_port_bindings(bindings)


def release_port_bindings(port_bindings: dict[str, tuple[str, int]]) -> None:
    """Release host ports reserved by allocate_port_bindings."""
    with _RESERVATION_LOCK:
        _RESERVED_HOST_PORTS.difference_update(
            host_port for _, host_port in port_bindings.values()
        )
