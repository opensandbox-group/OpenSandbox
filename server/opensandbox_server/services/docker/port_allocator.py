# Copyright 2026 Alibaba Group Holding Ltd.
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


def allocate_host_port(
    min_port: int = 40000,
    max_port: int = 60000,
    attempts: int = 50,
) -> Optional[int]:
    """Find an available TCP port on the host within the given range."""
    for _ in range(attempts):
        port = random.randint(min_port, max_port)
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            try:
                # This does not listen for or accept connections; it mirrors the
                # later Docker publish binding to catch host-wide port conflicts.
                # codeql[py/bind-socket-all-network-interfaces]
                sock.bind((PORT_PROBE_HOST, port))
            except OSError:
                continue
            return port
    return None


def _reserve_host_port(
    min_port: int = 40000,
    max_port: int = 60000,
    attempts: int = 50,
) -> Optional[int]:
    for _ in range(attempts):
        port = allocate_host_port(
            min_port=min_port,
            max_port=max_port,
            attempts=attempts,
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
) -> Dict[str, tuple[str, int]]:
    """Allocate distinct random host ports for each container port spec."""
    bindings: Dict[str, tuple[str, int]] = {}
    completed = False
    try:
        for container_port in container_ports:
            host_port = _reserve_host_port(min_port=min_port, max_port=max_port)
            if host_port is None:
                raise HTTPException(
                    status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
                    detail={
                        "code": SandboxErrorCodes.CONTAINER_START_FAILED,
                        "message": "Failed to allocate host ports for sandbox container.",
                    },
                )
            bindings[container_port] = (DOCKER_PUBLISH_HOST, host_port)
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
