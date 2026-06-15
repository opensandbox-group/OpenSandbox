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
import httpx
import pytest

from opensandbox.config import ConnectionConfig


def test_protocol_validation() -> None:
    ConnectionConfig(protocol="http")
    ConnectionConfig(protocol="https")

    with pytest.raises(ValueError):
        ConnectionConfig(protocol="ftp")  # type: ignore[arg-type]


def test_get_base_url_with_domain_and_protocol() -> None:
    # get_base_url() must NOT append /v1 — doing so would route requests to the
    # blocking /v1/sandboxes endpoint and cause 504 Gateway Timeouts (issue #591).
    cfg = ConnectionConfig(domain="example.com:1234", protocol="https")
    assert cfg.get_base_url() == "https://example.com:1234"


def test_get_base_url_domain_can_include_scheme() -> None:
    cfg = ConnectionConfig(domain="https://example.com:9999", protocol="http")
    assert cfg.get_base_url() == "https://example.com:9999"


def test_get_base_url_no_v1_prefix_direct_mode() -> None:
    """Direct mode: base URL must not contain /v1 so SDK hits POST /sandboxes (async, 202)."""
    cfg = ConnectionConfig(domain="sandbox-api.example.com", use_server_proxy=False)
    url = cfg.get_base_url()
    assert "/v1" not in url
    assert url == "http://sandbox-api.example.com"


def test_get_base_url_no_v1_prefix_proxy_mode() -> None:
    """Proxy mode: base URL must not contain /v1 to avoid hitting the blocking endpoint."""
    cfg = ConnectionConfig(
        domain="http://sandbox-api.example.com",
        use_server_proxy=True,
    )
    url = cfg.get_base_url()
    assert "/v1" not in url
    assert url == "http://sandbox-api.example.com"


def test_get_base_url_trailing_slash_stripped() -> None:
    """Trailing slashes on the domain are stripped to prevent double-slash in paths."""
    cfg = ConnectionConfig(domain="http://sandbox-api.example.com/")
    assert cfg.get_base_url() == "http://sandbox-api.example.com"


def test_get_base_url_no_scheme_trailing_slash_stripped() -> None:
    """Trailing slash is stripped even when domain has no explicit scheme."""
    cfg = ConnectionConfig(domain="sandbox-api.example.com/")
    assert cfg.get_base_url() == "http://sandbox-api.example.com"


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
