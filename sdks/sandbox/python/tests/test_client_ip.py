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
import ipaddress

import pytest

from opensandbox.config import ConnectionConfig, ConnectionConfigSync, client_ip

CLIENT_IP_HEADER = client_ip.CLIENT_IP_HEADER


@pytest.fixture(autouse=True)
def _reset_client_ip_cache():
    """Ensure each test starts and ends with a clean detection cache."""
    client_ip._cached_ip = None
    yield
    client_ip._cached_ip = None


def _stub_detection(monkeypatch: pytest.MonkeyPatch, ip: str) -> None:
    client_ip._cached_ip = None
    monkeypatch.setattr(client_ip, "detect_outbound_ip", lambda: ip)


def test_apply_client_ip_sets_header_when_absent(monkeypatch: pytest.MonkeyPatch) -> None:
    _stub_detection(monkeypatch, "10.1.2.3")
    headers: dict[str, str] = {}
    client_ip.apply_client_ip(headers)
    assert headers[CLIENT_IP_HEADER] == "10.1.2.3"


def test_apply_client_ip_does_not_overwrite_user_value(monkeypatch: pytest.MonkeyPatch) -> None:
    _stub_detection(monkeypatch, "10.1.2.3")
    headers = {"open-sandbox-client-ip": "192.168.0.9"}
    client_ip.apply_client_ip(headers)
    assert headers["open-sandbox-client-ip"] == "192.168.0.9"
    assert CLIENT_IP_HEADER not in headers


def test_apply_client_ip_noop_when_undetectable(monkeypatch: pytest.MonkeyPatch) -> None:
    _stub_detection(monkeypatch, "")
    headers: dict[str, str] = {}
    client_ip.apply_client_ip(headers)
    assert CLIENT_IP_HEADER not in headers


def test_detect_outbound_ip_returns_valid_or_empty() -> None:
    # Best-effort: empty in a network-less environment, otherwise a valid,
    # non-loopback, non-link-local, non-unspecified IP.
    ip = client_ip.detect_outbound_ip()
    if ip == "":
        return
    addr = ipaddress.ip_address(ip)
    assert not addr.is_loopback
    assert not addr.is_link_local
    assert not addr.is_unspecified


def test_select_client_ip_prefers_named_nic_and_skips_virtual() -> None:
    nics = [
        ("docker0", ["172.17.0.1"]),  # virtual, skip
        ("utun3", ["10.8.0.3"]),  # VPN, skip
        ("en0", ["10.1.1.1"]),  # main NIC
        ("eth1", ["192.168.5.5"]),
    ]
    assert client_ip._select_client_ip(nics) == "10.1.1.1"


def test_select_client_ip_falls_back_to_private_when_no_named_nic() -> None:
    nics = [
        ("docker0", ["172.17.0.1"]),  # virtual, skip
        ("custom9", ["8.8.4.4"]),  # public, lower preference
        ("wlan9", ["10.0.0.50"]),  # private, preferred
    ]
    assert client_ip._select_client_ip(nics) == "10.0.0.50"


def test_select_client_ip_skips_loopback_and_link_local() -> None:
    nics = [("eth0", ["127.0.0.1", "169.254.1.1", "10.1.2.3"])]
    assert client_ip._select_client_ip(nics) == "10.1.2.3"


def test_select_client_ip_empty_when_no_usable() -> None:
    nics = [("docker0", ["172.17.0.1"]), ("eth0", ["169.254.1.1"])]
    assert client_ip._select_client_ip(nics) == ""


def test_connection_config_injects_client_ip(monkeypatch: pytest.MonkeyPatch) -> None:
    _stub_detection(monkeypatch, "10.9.8.7")
    assert ConnectionConfig().headers[CLIENT_IP_HEADER] == "10.9.8.7"
    client_ip._cached_ip = None
    assert ConnectionConfigSync().headers[CLIENT_IP_HEADER] == "10.9.8.7"


def test_connection_config_omits_client_ip_when_undetectable(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_detection(monkeypatch, "")
    assert CLIENT_IP_HEADER not in ConnectionConfig().headers


def test_connection_config_respects_user_provided_header(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    _stub_detection(monkeypatch, "10.9.8.7")
    cfg = ConnectionConfig(headers={CLIENT_IP_HEADER: "203.0.113.1"})
    assert cfg.headers[CLIENT_IP_HEADER] == "203.0.113.1"
