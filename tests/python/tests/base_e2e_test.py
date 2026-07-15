#
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
#
"""
Base class for E2E tests providing common setup and configuration.
"""

import os
from datetime import timedelta

import httpx
from opensandbox.config import ConnectionConfig, ConnectionConfigSync

DEFAULT_DOMAIN = "localhost:8080"
DEFAULT_PROTOCOL = "http"
DEFAULT_API_KEY = "e2e-test"
DEFAULT_IMAGE = "opensandbox/code-interpreter:latest"
DEFAULT_RUNTIME = "docker"
DEFAULT_USE_SERVER_PROXY = "false"
DEFAULT_PVC_NAME = "opensandbox-e2e-pvc-test"
DEFAULT_HOST_VOLUME_DIR = "/tmp/opensandbox-e2e/host-volume-test"

TEST_DOMAIN = os.getenv("OPENSANDBOX_TEST_DOMAIN", DEFAULT_DOMAIN)
TEST_PROTOCOL = os.getenv("OPENSANDBOX_TEST_PROTOCOL", DEFAULT_PROTOCOL)
TEST_API_KEY = os.getenv("OPENSANDBOX_TEST_API_KEY", DEFAULT_API_KEY)
TEST_IMAGE = os.getenv("OPENSANDBOX_SANDBOX_DEFAULT_IMAGE", DEFAULT_IMAGE)
TEST_RUNTIME = os.getenv("OPENSANDBOX_E2E_RUNTIME", DEFAULT_RUNTIME).lower()
TEST_USE_SERVER_PROXY = os.getenv(
    "OPENSANDBOX_TEST_USE_SERVER_PROXY", DEFAULT_USE_SERVER_PROXY
).lower() in {"1", "true", "yes", "on"}
TEST_PVC_NAME = os.getenv("OPENSANDBOX_TEST_PVC_NAME", DEFAULT_PVC_NAME)
TEST_HOST_VOLUME_DIR = os.getenv(
    "OPENSANDBOX_TEST_HOST_VOLUME_DIR", DEFAULT_HOST_VOLUME_DIR
)

# When ``true`` (case-insensitive, per Java ``Boolean.parseBoolean``), the shared
# E2E sandbox is created with ``secure_access=True`` (gateway/ingress can enforce
# ``OpenSandbox-Secure-Access``). Default off for local docker-style runs.
OPENSANDBOX_TEST_SECURE_ACCESS_VERIFIABLE = "OPENSANDBOX_TEST_SECURE_ACCESS_VERIFIABLE"


def get_sandbox_image() -> str:
    """Get the default sandbox image for E2E tests."""
    return TEST_IMAGE


def get_e2e_sandbox_resource() -> dict[str, str]:
    """
    CPU/memory for ``Sandbox.create`` / ``SandboxSync.create`` in E2E tests.

    Read from ``OPENSANDBOX_E2E_SANDBOX_CPU`` and ``OPENSANDBOX_E2E_SANDBOX_MEMORY``.
    When unset, matches the SDK default (1 CPU, 2Gi memory).

    Kubernetes E2E entry scripts (``scripts/python-k8s-e2e*.sh``) export smaller
    defaults for Kind; override via the same env vars.
    """
    return {
        "cpu": os.getenv("OPENSANDBOX_E2E_SANDBOX_CPU", "1"),
        "memory": os.getenv("OPENSANDBOX_E2E_SANDBOX_MEMORY", "2Gi"),
    }


def is_kubernetes_runtime() -> bool:
    """Whether the current E2E run targets the Kubernetes backend."""
    return TEST_RUNTIME == "kubernetes"


def is_secure_access_verifiable() -> bool:
    """
    True when ``OPENSANDBOX_TEST_SECURE_ACCESS_VERIFIABLE`` is ``"true"`` after
    strip, case-insensitive (same as Java E2E ``Boolean.parseBoolean``).
    """
    v = os.getenv(OPENSANDBOX_TEST_SECURE_ACCESS_VERIFIABLE)
    if v is None:
        return False
    return v.strip().lower() == "true"


def should_use_server_proxy() -> bool:
    """Whether SDK calls should proxy execd traffic through the server."""
    return TEST_USE_SERVER_PROXY


def get_test_pvc_name() -> str:
    """Get the PVC name used by runtime E2E tests."""
    return TEST_PVC_NAME


def get_test_host_volume_dir() -> str:
    """Get the host directory used by host-volume E2E tests."""
    return TEST_HOST_VOLUME_DIR


def create_connection_config() -> ConnectionConfig:
    """Create async ConnectionConfig for E2E tests."""
    return ConnectionConfig(
        domain=TEST_DOMAIN,
        api_key=TEST_API_KEY,
        request_timeout=timedelta(minutes=3),
        protocol=TEST_PROTOCOL,
        use_server_proxy=should_use_server_proxy(),
    )


def create_connection_config_server_proxy() -> ConnectionConfig:
    """Create async ConnectionConfig for E2E tests using server-proxied endpoints."""
    return ConnectionConfig(
        domain=TEST_DOMAIN,
        api_key=TEST_API_KEY,
        request_timeout=timedelta(minutes=3),
        protocol=TEST_PROTOCOL,
        use_server_proxy=True,
    )


def create_connection_config_sync() -> ConnectionConfigSync:
    """Create sync ConnectionConfig for E2E tests."""
    return ConnectionConfigSync(
        domain=TEST_DOMAIN,
        api_key=TEST_API_KEY,
        request_timeout=timedelta(minutes=3),
        transport=httpx.HTTPTransport(
            limits=httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=15,
            )
        ),
        protocol=TEST_PROTOCOL,
        use_server_proxy=should_use_server_proxy(),
    )


def create_connection_config_sync_server_proxy() -> ConnectionConfigSync:
    """Create sync ConnectionConfig for E2E tests using server-proxied endpoints."""
    return ConnectionConfigSync(
        domain=TEST_DOMAIN,
        api_key=TEST_API_KEY,
        request_timeout=timedelta(minutes=3),
        transport=httpx.HTTPTransport(
            limits=httpx.Limits(
                max_connections=100,
                max_keepalive_connections=20,
                keepalive_expiry=15,
            )
        ),
        protocol=TEST_PROTOCOL,
        use_server_proxy=True,
    )
