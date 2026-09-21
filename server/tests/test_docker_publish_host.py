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

"""[docker].publish_host: the address bridge-mode sandbox ports are published on."""

import errno

import pytest

from opensandbox_server.config import DockerConfig
from opensandbox_server.services.docker import port_allocator


class _FakeSocket:
    def __init__(self, bound_addresses, refuse=None):
        self._bound_addresses = bound_addresses
        self._refuse = refuse or {}

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        return None

    def setsockopt(self, level, optname, value):
        return None

    def bind(self, address):
        self._bound_addresses.append(address)
        code = self._refuse.get(address[0])
        if code is not None:
            raise OSError(code, "refused by the test")


def test_publish_host_defaults_to_every_interface() -> None:
    assert DockerConfig().publish_host == "0.0.0.0"
    assert DockerConfig(publish_host="  ").publish_host == "0.0.0.0"


def test_publish_host_accepts_an_ip_address() -> None:
    assert DockerConfig(publish_host="127.0.0.1").publish_host == "127.0.0.1"
    assert DockerConfig(publish_host=" 172.17.0.1 ").publish_host == "172.17.0.1"


def test_publish_host_rejects_a_name() -> None:
    # Docker publishes on addresses, not names: a name would fail at container start, per sandbox.
    with pytest.raises(ValueError, match="publish_host must be an IP address"):
        DockerConfig(publish_host="host.docker.internal")


def test_publish_host_rejects_ipv6() -> None:
    # The allocator probes with an AF_INET socket: an IPv6 literal would pass ip_address() and then
    # fail every probe with gaierror (not EADDRNOTAVAIL, so no wildcard fallback) — every sandbox
    # creation a 500. Refused at config load instead.
    for v6 in ("::", "::1", "fe80::1"):
        with pytest.raises(ValueError, match="publish_host must be an IPv4 address"):
            DockerConfig(publish_host=v6)


def test_allocate_port_bindings_publish_on_the_configured_host(monkeypatch) -> None:
    probes: list[str] = []

    def tracked_allocate(min_port=40000, max_port=60000, attempts=50, probe_host=port_allocator.PORT_PROBE_HOST):
        probes.append(probe_host)
        return 45678

    monkeypatch.setattr(port_allocator, "allocate_host_port", tracked_allocate)

    bindings = port_allocator.allocate_port_bindings(["8080"], publish_host="127.0.0.1")

    assert bindings == {"8080": ("127.0.0.1", 45678)}
    assert probes == ["127.0.0.1"]


def test_allocate_host_port_probes_the_publish_host(monkeypatch) -> None:
    bound: list[tuple[str, int]] = []
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: 45678)
    monkeypatch.setattr(port_allocator.socket, "socket", lambda family, sock_type: _FakeSocket(bound))

    port = port_allocator.allocate_host_port(min_port=45678, max_port=45678, attempts=1, probe_host="127.0.0.1")

    assert port == 45678
    assert bound == [("127.0.0.1", 45678)]


def test_allocate_host_port_falls_back_to_the_wildcard_when_the_publish_host_is_not_local(monkeypatch) -> None:
    """The server runs in a container and publishes on the host's bridge gateway: that address is
    not one of the container's, so the probe cannot bind it and probes the wildcard instead."""
    bound: list[tuple[str, int]] = []
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: 45678)
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket(bound, refuse={"172.17.0.1": errno.EADDRNOTAVAIL}),
    )

    port = port_allocator.allocate_host_port(min_port=45678, max_port=45678, attempts=1, probe_host="172.17.0.1")

    assert port == 45678
    assert bound == [("172.17.0.1", 45678), (port_allocator.PORT_PROBE_HOST, 45678)]


def test_allocate_host_port_treats_a_taken_publish_host_port_as_taken(monkeypatch) -> None:
    bound: list[tuple[str, int]] = []
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: 45678)
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket(bound, refuse={"127.0.0.1": errno.EADDRINUSE}),
    )

    assert port_allocator.allocate_host_port(min_port=45678, max_port=45678, attempts=1, probe_host="127.0.0.1") is None
    assert bound == [("127.0.0.1", 45678)]
