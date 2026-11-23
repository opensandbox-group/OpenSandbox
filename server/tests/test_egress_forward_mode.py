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

"""OSEP-0030 egress forward mode: config, request guards, Docker attachment."""

from unittest.mock import MagicMock, patch

import pytest
from fastapi import FastAPI, HTTPException, status
from fastapi.testclient import TestClient
from pydantic import SecretStr, ValidationError

from opensandbox_server.api import network_policy as network_policy_api
from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CredentialProxyConfig,
    ImageSpec,
    NetworkPolicy,
    PlatformSpec,
    ResourceLimits,
)
from opensandbox_server.config import (
    EGRESS_MODE_DNS_NFT,
    EGRESS_MODE_FORWARD,
    AppConfig,
    DockerConfig,
    EgressConfig,
    EgressUpstreamProxyConfig,
    IngressConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
    SecureRuntimeConfig,
    ServerConfig,
)
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.docker import DockerSandboxService
from opensandbox_server.services.helpers import (
    forward_mode_egress_env,
    validate_upstream_proxy_request,
)

GATEWAY = EgressUpstreamProxyConfig(
    url="https://gateway.example.com:3000",
    authorization=SecretStr("Bearer gw-token"),
)


def _forward_egress(**overrides) -> EgressConfig:
    fields = {"image": "egress:latest", "mode": EGRESS_MODE_FORWARD, "upstream_proxy": GATEWAY}
    fields.update(overrides)
    return EgressConfig(**fields)


def _docker_app_config(**overrides) -> AppConfig:
    fields = {
        "server": ServerConfig(),
        "runtime": RuntimeConfig(type="docker", execd_image="ghcr.io/opensandbox/platform:latest"),
        "ingress": IngressConfig(mode="direct"),
        "docker": DockerConfig(network_mode="bridge"),
        "egress": _forward_egress(),
    }
    fields.update(overrides)
    return AppConfig(**fields)


@pytest.fixture(autouse=True)
def isolated_metadata_store(monkeypatch, tmp_path):
    monkeypatch.setattr(
        "opensandbox_server.services.docker.metadata.DEFAULT_STORE_DIR", tmp_path / "metadata"
    )


# --- config -----------------------------------------------------------------


def test_forward_mode_config_accepts_docker_bridge():
    cfg = _docker_app_config(egress=_forward_egress(dns_upstreams=["10.96.0.53", "[fd00::53]:53"]))
    assert cfg.egress.mode == EGRESS_MODE_FORWARD
    assert forward_mode_egress_env(cfg.egress) == {
        "OPENSANDBOX_EGRESS_DNS_UPSTREAM": "10.96.0.53,[fd00::53]:53"
    }


def test_forward_mode_requires_authenticated_upstream_proxy():
    with pytest.raises(ValidationError, match="upstream_proxy.authorization"):
        _forward_egress(upstream_proxy=None)
    with pytest.raises(ValidationError, match="upstream_proxy.authorization"):
        _forward_egress(upstream_proxy=EgressUpstreamProxyConfig(url="http://gw.example.com:3128"))
    with pytest.raises(ValidationError, match="egress.image"):
        _forward_egress(image=None)


@pytest.mark.parametrize("entry", ["dns.example.com", "10.0.0.1:0", "10.0.0.1:abc", "[fd00::1", ""])
def test_forward_mode_dns_upstreams_must_be_literal_ips(entry):
    with pytest.raises(ValidationError, match="dns_upstreams"):
        _forward_egress(dns_upstreams=[entry])


def test_dns_upstreams_requires_forward_mode():
    with pytest.raises(ValidationError, match='requires egress.mode = "forward"'):
        EgressConfig(image="egress:latest", mode=EGRESS_MODE_DNS_NFT, dns_upstreams=["10.0.0.53"])


@pytest.mark.parametrize(
    "overrides, message",
    [
        ({"docker": DockerConfig()}, 'network_mode = "bridge"'),
        ({"docker": DockerConfig(network_mode="none")}, 'network_mode = "bridge"'),
        (
            {"secure_runtime": SecureRuntimeConfig(type="gvisor", docker_runtime="runsc")},
            "gvisor",
        ),
        (
            {
                "runtime": RuntimeConfig(type="kubernetes", execd_image="execd:latest"),
                "kubernetes": KubernetesRuntimeConfig(),
            },
            "not yet supported",
        ),
    ],
)
def test_forward_mode_rejects_unsupported_runtimes(overrides, message):
    with pytest.raises(ValidationError, match=message):
        _docker_app_config(**overrides)


@pytest.mark.parametrize("drop_caps", [[], ["NET_ADMIN"], ["NET_RAW"]])
def test_forward_mode_requires_dropping_net_admin_and_net_raw(drop_caps):
    with pytest.raises(ValidationError, match="drop_capabilities"):
        _docker_app_config(
            docker=DockerConfig(network_mode="bridge", drop_capabilities=drop_caps)
        )


@pytest.mark.parametrize(
    "drop_caps",
    [["ALL"], ["CAP_NET_ADMIN", "CAP_NET_RAW"]],
)
def test_forward_mode_accepts_all_or_prefixed_cap_drops(drop_caps):
    cfg = _docker_app_config(
        docker=DockerConfig(network_mode="bridge", drop_capabilities=drop_caps)
    )
    assert cfg.egress is not None and cfg.egress.mode == EGRESS_MODE_FORWARD


def test_non_forward_upstream_proxy_still_requires_dns_nft():
    with pytest.raises(ValidationError, match='egress.mode = "dns\\+nft"'):
        EgressConfig(image="egress:latest", upstream_proxy=GATEWAY)


# --- request guards -----------------------------------------------------------


@pytest.mark.parametrize(
    "kwargs, message",
    [
        ({"has_network_policy": True}, "networkPolicy"),
        ({"credential_proxy_enabled": True}, "credentialProxy"),
        ({"egress_env": {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "false"}}, "MITMPROXY_TRANSPARENT"),
        ({"egress_env": {"OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE": "1"}}, "MITMPROXY_SSL_INSECURE"),
    ],
)
def test_forward_mode_rejects_local_policy_and_mitm(kwargs, message):
    args = {"has_network_policy": False, "credential_proxy_enabled": False, "egress_env": {}}
    args.update(kwargs)
    with pytest.raises(ValueError, match=message):
        validate_upstream_proxy_request(_forward_egress(), **args)


def test_forward_mode_allows_plain_request():
    validate_upstream_proxy_request(
        _forward_egress(),
        has_network_policy=False,
        credential_proxy_enabled=False,
        egress_env={"OPENSANDBOX_EGRESS_LOG_LEVEL": "debug"},
    )


@pytest.mark.parametrize("method", ["put", "patch"])
def test_network_policy_mutation_rejected_in_forward_mode(monkeypatch, method):
    proxied = MagicMock()
    monkeypatch.setattr(network_policy_api, "_proxy_http_request", proxied)
    monkeypatch.setattr(network_policy_api, "get_config", lambda: _docker_app_config())
    app = FastAPI()
    app.include_router(network_policy_api.router)
    body = {"defaultAction": "allow", "egress": []} if method == "put" else []

    resp = getattr(TestClient(app), method)("/sandboxes/sbx-1/networkpolicy", json=body)

    assert resp.status_code == status.HTTP_400_BAD_REQUEST
    assert resp.json()["detail"]["code"] == SandboxErrorCodes.INVALID_PARAMETER
    proxied.assert_not_called()


# --- Docker attachment --------------------------------------------------------


def _mock_docker_client(mock_docker):
    mock_client = MagicMock()
    mock_client.containers.list.return_value = []
    mock_client.api.create_host_config.side_effect = lambda **kwargs: kwargs
    mock_client.api.create_container.side_effect = [{"Id": "sidecar-id"}, {"Id": "main-id"}]
    mock_client.containers.get.side_effect = [MagicMock(id="sidecar-id"), MagicMock(id="main-id")]
    mock_docker.from_env.return_value = mock_client
    return mock_client


def _create_request(**overrides) -> CreateSandboxRequest:
    fields = {
        "image": ImageSpec(uri="python:3.11"),
        "timeout": 120,
        "resourceLimits": ResourceLimits(root={}),
        "env": {"APP": "1"},
        "metadata": {},
        "entrypoint": ["python"],
    }
    fields.update(overrides)
    return CreateSandboxRequest(**fields)


async def _create(service, request, ready_error=None):
    with (
        patch(
            "opensandbox_server.services.docker.docker_service.generate_egress_token",
            return_value="egress-token",
        ),
        patch(
            "opensandbox_server.services.docker.docker_service.allocate_port_bindings",
            return_value={
                "44772": ("0.0.0.0", 44772),
                "8080": ("0.0.0.0", 8080),
                "18080": ("0.0.0.0", 18080),
            },
        ),
        patch.object(service, "_ensure_image_available"),
        patch.object(service, "_prepare_sandbox_runtime"),
        patch.object(service, "_wait_for_egress_sidecar_ready", side_effect=ready_error) as wait_ready,
    ):
        await service.create_sandbox(request)
    return wait_ready


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_forward_mode_attaches_egress_without_network_policy(mock_docker):
    mock_client = _mock_docker_client(mock_docker)
    cfg = _docker_app_config(egress=_forward_egress(dns_upstreams=["10.96.0.53"]))
    service = DockerSandboxService(config=cfg)

    wait_ready = await _create(service, _create_request())

    assert mock_client.api.create_container.call_count == 2
    sidecar_kwargs = mock_client.api.create_container.call_args_list[0].kwargs
    main_kwargs = mock_client.api.create_container.call_args_list[1].kwargs
    sidecar_env = sidecar_kwargs["environment"]
    assert "OPENSANDBOX_EGRESS_MODE=forward" in sidecar_env
    assert "OPENSANDBOX_EGRESS_UPSTREAM_PROXY=https://gateway.example.com:3000" in sidecar_env
    assert "OPENSANDBOX_EGRESS_UPSTREAM_PROXY_AUTH=Bearer gw-token" in sidecar_env
    assert "OPENSANDBOX_EGRESS_DNS_UPSTREAM=10.96.0.53" in sidecar_env
    assert any(e.startswith("OPENSANDBOX_EGRESS_SANDBOX_ID=") for e in sidecar_env)
    assert not any(e.startswith("OPENSANDBOX_EGRESS_RULES=") for e in sidecar_env)
    # Workload starts only after the sidecar reports ready.
    wait_ready.assert_called_once()

    main_host_config = main_kwargs["host_config"]
    assert main_host_config["network_mode"] == "container:sidecar-id"
    assert {"NET_ADMIN", "NET_RAW"} <= set(main_host_config["cap_drop"])
    # The workload receives no egress credentials or gateway config.
    assert not any(e.startswith("OPENSANDBOX_EGRESS_") for e in main_kwargs["environment"])


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
@pytest.mark.parametrize(
    "overrides",
    [
        {"networkPolicy": NetworkPolicy(default_action="deny", egress=[])},
        {
            "networkPolicy": NetworkPolicy(default_action="deny", egress=[]),
            "credentialProxy": CredentialProxyConfig(enabled=True),
        },
        {"env": {"OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT": "true"}},
    ],
)
async def test_forward_mode_create_rejects_opt_out(mock_docker, overrides):
    mock_client = _mock_docker_client(mock_docker)
    service = DockerSandboxService(config=_docker_app_config())

    with pytest.raises(HTTPException) as exc:
        await _create(service, _create_request(**overrides))

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    mock_client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
@pytest.mark.parametrize(
    "env_key",
    [
        "OPENSANDBOX_EGRESS_SANDBOX_ID",
        "OPENSANDBOX_EGRESS_SANDBOX_NAMESPACE",
        "OPENSANDBOX_EGRESS_TENANT_NAME",
    ],
)
async def test_forward_mode_rejects_reserved_identity_env(mock_docker, env_key):
    mock_client = _mock_docker_client(mock_docker)
    service = DockerSandboxService(config=_docker_app_config())

    with pytest.raises(HTTPException) as exc:
        await _create(service, _create_request(env={env_key: "spoofed"}))

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    mock_client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_forward_mode_rejects_windows_profile(mock_docker):
    mock_client = _mock_docker_client(mock_docker)
    service = DockerSandboxService(config=_docker_app_config())

    with pytest.raises(HTTPException) as exc:
        await _create(
            service,
            _create_request(platform=PlatformSpec(os="windows", arch="amd64")),
        )

    assert exc.value.status_code == status.HTTP_400_BAD_REQUEST
    assert "Windows" in exc.value.detail["message"]
    mock_client.api.create_container.assert_not_called()


@pytest.mark.asyncio
@patch("opensandbox_server.services.docker.docker_service.docker")
async def test_forward_mode_unreachable_gateway_fails_create(mock_docker):
    """Egress /healthz stays 503 while the gateway is unreachable: creation
    fails, the sidecar is removed and the workload never starts."""
    mock_client = _mock_docker_client(mock_docker)
    sidecar = MagicMock(id="sidecar-id")
    mock_client.containers.get.side_effect = [sidecar]
    service = DockerSandboxService(config=_docker_app_config())
    not_ready = HTTPException(
        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
        detail={"code": SandboxErrorCodes.CONTAINER_START_FAILED, "message": "not ready"},
    )

    with pytest.raises(HTTPException) as exc:
        await _create(service, _create_request(), ready_error=not_ready)

    assert exc.value.status_code == status.HTTP_500_INTERNAL_SERVER_ERROR
    assert mock_client.api.create_container.call_count == 1  # sidecar only
    sidecar.remove.assert_called_with(force=True)
