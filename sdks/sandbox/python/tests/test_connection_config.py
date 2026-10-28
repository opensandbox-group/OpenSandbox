#
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
#
import ssl
from unittest.mock import Mock

import httpx
import pytest

from opensandbox.config import ConnectionConfig, ConnectionConfigSync


# Regression: the default User-Agent version is hand-maintained and must be
# bumped together with the package version. Update this expectation when releasing.
def test_default_user_agent_matches_package_version() -> None:
    assert ConnectionConfig().user_agent == "OpenSandbox-Python-SDK/1.2.0"
    assert ConnectionConfigSync().user_agent == "OpenSandbox-Python-SDK/1.2.0"


def test_protocol_validation() -> None:
    ConnectionConfig(protocol="http")
    ConnectionConfig(protocol="https")

    with pytest.raises(ValueError):
        ConnectionConfig(protocol="ftp")  # type: ignore[arg-type]


def test_get_base_url_with_domain_and_protocol() -> None:
    cfg = ConnectionConfig(domain="example.com:1234", protocol="https")
    assert cfg.get_base_url() == "https://example.com:1234/v1"


def test_get_base_url_domain_can_include_scheme() -> None:
    cfg = ConnectionConfig(domain="https://example.com:9999", protocol="http")
    assert cfg.get_base_url() == "https://example.com:9999/v1"


@pytest.mark.parametrize("config_cls", [ConnectionConfig, ConnectionConfigSync])
@pytest.mark.parametrize(
    ("domain", "expected"),
    [
        ("example.com:1234/", "https://example.com:1234/v1"),
        ("example.com:1234/v1", "https://example.com:1234/v1"),
        ("example.com:1234/v1/", "https://example.com:1234/v1"),
        ("https://example.com:9999/", "https://example.com:9999/v1"),
        ("https://example.com:9999/v1", "https://example.com:9999/v1"),
        ("https://example.com/proxy/", "https://example.com/proxy/v1"),
        ("https://example.com/proxy/v1/", "https://example.com/proxy/v1"),
    ],
)
def test_get_base_url_normalizes_trailing_slash_and_version_suffix(
    config_cls: type[ConnectionConfig] | type[ConnectionConfigSync],
    domain: str,
    expected: str,
) -> None:
    cfg = config_cls(domain=domain, protocol="https")
    assert cfg.get_base_url() == expected


def test_get_base_url_normalizes_domain_from_env(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("OPEN_SANDBOX_DOMAIN", "http://localhost:8080/")
    assert ConnectionConfig().get_base_url() == "http://localhost:8080/v1"
    assert ConnectionConfigSync().get_base_url() == "http://localhost:8080/v1"


@pytest.mark.asyncio
async def test_close_transport_if_owned_default_transport() -> None:
    cfg = ConnectionConfig().with_transport_if_missing()
    # default transport should be closable and owned
    await cfg.close_transport_if_owned()


@pytest.mark.asyncio
async def test_close_transport_if_owned_does_not_close_user_transport() -> None:
    class CustomTransport(httpx.AsyncBaseTransport):
        def __init__(self) -> None:
            self.closed = False

        async def handle_async_request(self, request: httpx.Request) -> httpx.Response:  # pragma: no cover
            raise RuntimeError("not used")

        async def aclose(self) -> None:
            self.closed = True

    t = CustomTransport()
    cfg = ConnectionConfig(transport=t)
    await cfg.close_transport_if_owned()
    assert t.closed is False


@pytest.mark.parametrize("config_cls", [ConnectionConfig, ConnectionConfigSync])
async def test_reuses_ssl_context_without_sharing_owned_transports(
    config_cls: type[ConnectionConfig] | type[ConnectionConfigSync],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    create_context = Mock(wraps=httpx.create_ssl_context)
    transport_name = (
        "AsyncHTTPTransport" if config_cls is ConnectionConfig else "HTTPTransport"
    )
    create_transport = Mock(wraps=getattr(httpx, transport_name))
    monkeypatch.setattr(httpx, "create_ssl_context", create_context)
    monkeypatch.setattr(httpx, transport_name, create_transport)
    config = config_cls()
    assert create_context.call_count == 0

    first = config.with_transport_if_missing()
    second = config.with_transport_if_missing()
    try:
        create_context.assert_called_once_with()
        contexts = [call.kwargs["verify"] for call in create_transport.call_args_list]
        assert contexts[0] is contexts[1]
        assert contexts[0].verify_mode == ssl.CERT_REQUIRED
        assert contexts[0].check_hostname
        assert config.transport is None
        assert first.transport is not second.transport
        assert first.owns_transport and second.owns_transport
    finally:
        for resource in (first, second):
            if isinstance(resource, ConnectionConfig):
                await resource.close_transport_if_owned()
            else:
                resource.close_transport_if_owned()


@pytest.mark.parametrize("config_cls", [ConnectionConfig, ConnectionConfigSync])
async def test_new_config_reloads_environment_trust_store(
    config_cls: type[ConnectionConfig] | type[ConnectionConfigSync],
    monkeypatch: pytest.MonkeyPatch,
    tmp_path,
) -> None:
    config = config_cls()
    first = config.with_transport_if_missing()
    monkeypatch.setenv("SSL_CERT_FILE", str(tmp_path / "missing-ca.pem"))
    second = config.with_transport_if_missing()
    try:
        with pytest.raises(FileNotFoundError):
            config_cls().with_transport_if_missing()
    finally:
        for resource in (first, second):
            if isinstance(resource, ConnectionConfig):
                await resource.close_transport_if_owned()
            else:
                resource.close_transport_if_owned()
