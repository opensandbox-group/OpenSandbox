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

import pytest
from unittest.mock import MagicMock, patch

from opensandbox_server.services.constants import (
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    SANDBOX_EMBEDDING_PROXY_PORT_LABEL,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
)
from opensandbox_server.services.docker import DockerSandboxService
from opensandbox_server.config import AppConfig, RuntimeConfig, DockerConfig, ServerConfig

@pytest.fixture
def mock_docker_service():
    """Create a DockerSandboxService with mocked docker client."""
    # Setup base config
    config = AppConfig(
        server=ServerConfig(port=8080, host="0.0.0.0"),
        runtime=RuntimeConfig(type="docker", execd_image="test/execd:latest"),
        router=None,
        docker=DockerConfig(network_mode="bridge"),
    )

    with patch("docker.from_env") as mock_docker:
        mock_client = MagicMock()
        mock_docker.return_value = mock_client

        # Initialize service
        service = DockerSandboxService(config=config)
        # Inject the mock client directly to ensure we control it
        service.docker_client = mock_client

        yield service, mock_client

def test_get_endpoint_host_mode(mock_docker_service):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "host"
    service.network_mode = "host"

    mock_container = MagicMock()
    mock_container.attrs = {"State": {"Running": True}}
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="10.0.0.1"):
        endpoint = service.get_endpoint("sbx-123", 8080, resolve_internal=False)
        assert endpoint.endpoint == "10.0.0.1:8080"

    endpoint_internal = service.get_endpoint("sbx-123", 8080, resolve_internal=True)
    assert endpoint_internal.endpoint == "127.0.0.1:8080"


def test_get_endpoint_bridge_http_port(mock_docker_service):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        "opensandbox.io/embedding-proxy-port": "50002",
        "opensandbox.io/http-port": "50001",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "172.17.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="192.168.1.100"):
        endpoint = service.get_endpoint("sbx-123", 8080, resolve_internal=False)

    assert endpoint.endpoint == "192.168.1.100:50001"


def test_get_endpoint_bridge_other_port_via_execd(mock_docker_service):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        "opensandbox.io/embedding-proxy-port": "50002",
        "opensandbox.io/http-port": "50001",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "172.17.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="192.168.1.100"):
        endpoint = service.get_endpoint("sbx-123", 6000, resolve_internal=False)

    assert endpoint.endpoint == "192.168.1.100:50002/proxy/6000"


def test_get_endpoint_bridge_egress_port_includes_auth_header(mock_docker_service):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        "opensandbox.io/embedding-proxy-port": "50002",
        "opensandbox.io/http-port": "50001",
        "opensandbox.io/egress-auth-token": "egress-token",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "172.17.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="192.168.1.100"):
        endpoint = service.get_endpoint("sbx-123", 18080, resolve_internal=False)

    assert endpoint.endpoint == "192.168.1.100:50002/proxy/18080"
    assert endpoint.headers == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}


def test_get_endpoint_bridge_non_egress_port_still_includes_instance_auth_header(
    mock_docker_service,
):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        SANDBOX_EMBEDDING_PROXY_PORT_LABEL: "50002",
        SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "172.17.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="192.168.1.100"):
        endpoint = service.get_endpoint("sbx-123", 44772, resolve_internal=False)

    assert endpoint.endpoint == "192.168.1.100:50002/proxy/44772"
    assert endpoint.headers == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}

def test_get_endpoint_bridge_internal_resolution(mock_docker_service):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "NetworkSettings": {"IPAddress": "10.0.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    endpoint = service.get_endpoint("sbx-123", 8080, resolve_internal=True)
    assert endpoint.endpoint == "10.0.0.5:8080"


def test_get_endpoint_bridge_internal_resolution_with_egress_sidecar_falls_back_to_host_mapped_endpoint(
    mock_docker_service,
):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        SANDBOX_EMBEDDING_PROXY_PORT_LABEL: "50002",
        SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": ""},
    }
    mock_client.containers.list.return_value = [mock_container]

    endpoint = service.get_endpoint("sbx-123", 18080, resolve_internal=True)

    assert endpoint.endpoint == "127.0.0.1:50002/proxy/18080"
    assert endpoint.headers == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}


def test_get_endpoint_bridge_internal_resolution_with_egress_sidecar_ignores_container_ip(
    mock_docker_service,
):
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        SANDBOX_EMBEDDING_PROXY_PORT_LABEL: "50002",
        SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "10.0.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    endpoint = service.get_endpoint("sbx-123", 18080, resolve_internal=True)

    assert endpoint.endpoint == "127.0.0.1:50002/proxy/18080"
    assert endpoint.headers == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}


def test_get_endpoint_bridge_internal_resolution_with_egress_sidecar_uses_proxy_host_not_eip(
    mock_docker_service,
):
    service, mock_client = mock_docker_service
    service.app_config.server.host = "0.0.0.0"
    service.app_config.server.eip = "203.0.113.10"
    service.app_config.docker.network_mode = "bridge"
    service.network_mode = "bridge"

    labels = {
        SANDBOX_EMBEDDING_PROXY_PORT_LABEL: "50002",
        SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": ""},
    }
    mock_client.containers.list.return_value = [mock_container]

    endpoint = service.get_endpoint("sbx-123", 18080, resolve_internal=True)

    assert endpoint.endpoint == "127.0.0.1:50002/proxy/18080"
    assert endpoint.headers == {OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token"}


def test_get_endpoint_bridge_uses_docker_host_ip_when_server_in_container():
    """When server runs in container (host=0.0.0.0), endpoint uses [docker].host_ip."""
    config = AppConfig(
        server=ServerConfig(port=8080, host="0.0.0.0"),
        runtime=RuntimeConfig(type="docker", execd_image="test/execd:latest"),
        router=None,
        docker=DockerConfig(network_mode="bridge", host_ip="10.57.1.91"),
    )
    with patch("docker.from_env") as mock_docker:
        mock_client = MagicMock()
        mock_docker.return_value = mock_client
        service = DockerSandboxService(config=config)
        service.docker_client = mock_client

    labels = {
        "opensandbox.io/embedding-proxy-port": "40109",
        "opensandbox.io/http-port": "50001",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {"IPAddress": "172.17.0.5"},
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.docker.networking._running_inside_docker_container", return_value=True):
        endpoint = service.get_endpoint("sbx-123", 44772, resolve_internal=False)

    assert endpoint.endpoint == "10.57.1.91:40109/proxy/44772"


def test_get_endpoint_user_defined_network_external(mock_docker_service):
    """External endpoint for a user-defined network uses host port bindings, same as bridge."""
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "my-app-net"
    service.network_mode = "my-app-net"

    labels = {
        "opensandbox.io/embedding-proxy-port": "51000",
        "opensandbox.io/http-port": "51001",
    }
    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "Config": {"Labels": labels},
        "NetworkSettings": {
            "IPAddress": "",
            "Networks": {"my-app-net": {"IPAddress": "192.168.100.5"}},
        },
    }
    mock_client.containers.list.return_value = [mock_container]

    with patch("opensandbox_server.services.sandbox_service.SandboxService._resolve_bind_ip", return_value="10.0.1.1"):
        ep_http = service.get_endpoint("sbx-123", 8080, resolve_internal=False)
        ep_proxy = service.get_endpoint("sbx-123", 5000, resolve_internal=False)

    assert ep_http.endpoint == "10.0.1.1:51001"
    assert ep_proxy.endpoint == "10.0.1.1:51000/proxy/5000"


def test_get_endpoint_user_defined_network_internal_prefers_configured_network(mock_docker_service):
    """resolve_internal=True on a user-defined network returns the IP from that specific network."""
    service, mock_client = mock_docker_service
    service.app_config.docker.network_mode = "my-app-net"
    service.network_mode = "my-app-net"

    mock_container = MagicMock()
    mock_container.attrs = {
        "State": {"Running": True},
        "NetworkSettings": {
            # top-level IPAddress is empty for user-defined networks
            "IPAddress": "",
            "Networks": {
                "bridge": {"IPAddress": "172.17.0.3"},
                "my-app-net": {"IPAddress": "192.168.100.5"},
            },
        },
    }
    mock_client.containers.list.return_value = [mock_container]

    endpoint = service.get_endpoint("sbx-123", 8080, resolve_internal=True)

    # Must use the IP from the configured network, not the default bridge entry
    assert endpoint.endpoint == "192.168.100.5:8080"


def test_extract_bridge_ip_falls_back_when_named_network_ip_missing(mock_docker_service):
    """_extract_bridge_ip falls back to any available network IP when the named entry is empty."""
    service, _ = mock_docker_service
    service.network_mode = "my-app-net"

    mock_container = MagicMock()
    mock_container.attrs = {
        "NetworkSettings": {
            "IPAddress": "",
            "Networks": {
                "my-app-net": {"IPAddress": ""},   # empty — simulate container still attaching
                "bridge": {"IPAddress": "172.17.0.9"},
            },
        },
    }

    ip = service._extract_bridge_ip(mock_container)
    assert ip == "172.17.0.9"
