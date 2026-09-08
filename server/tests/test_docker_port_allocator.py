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

import pytest
from fastapi import HTTPException

from opensandbox_server.services.docker import port_allocator


@pytest.fixture(autouse=True)
def _clear_port_reservations():
    with port_allocator._RESERVATION_LOCK:
        port_allocator._RESERVED_HOST_PORTS.clear()
    yield
    with port_allocator._RESERVATION_LOCK:
        port_allocator._RESERVED_HOST_PORTS.clear()


class _FakeSocket:
    def __init__(self, bound_addresses):
        self._bound_addresses = bound_addresses

    def __enter__(self):
        return self

    def __exit__(self, exc_type, exc_value, traceback):
        return None

    def setsockopt(self, level, optname, value):
        return None

    def bind(self, address):
        self._bound_addresses.append(address)


def test_allocate_host_port_probes_docker_publish_address(monkeypatch) -> None:
    bound_addresses: list[tuple[str, int]] = []

    monkeypatch.setattr(port_allocator.random, "randint", lambda min_port, max_port: 45678)
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket(bound_addresses),
    )

    port = port_allocator.allocate_host_port(min_port=45678, max_port=45678, attempts=1)

    assert port == 45678
    assert bound_addresses == [(port_allocator.PORT_PROBE_HOST, 45678)]


def test_allocate_port_bindings_keep_docker_publish_host(monkeypatch) -> None:
    monkeypatch.setattr(port_allocator, "allocate_host_port", lambda min_port=40000, max_port=60000, attempts=50: 45678)

    bindings = port_allocator.allocate_port_bindings(["8080"])

    try:
        assert bindings == {"8080": (port_allocator.DOCKER_PUBLISH_HOST, 45678)}
    finally:
        port_allocator.release_port_bindings(bindings)


def test_allocate_host_port_respects_custom_range(monkeypatch) -> None:
    """allocate_host_port uses passed min/max instead of defaults."""
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: 41000)
    bound_addresses: list[tuple[str, int]] = []
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket(bound_addresses),
    )

    port = port_allocator.allocate_host_port(min_port=41000, max_port=41100, attempts=1)

    assert port == 41000
    assert bound_addresses == [(port_allocator.PORT_PROBE_HOST, 41000)]


def test_allocate_port_bindings_passes_custom_range(monkeypatch) -> None:
    """allocate_port_bindings forwards custom range to allocate_host_port."""
    calls: list[tuple[int, int, int]] = []

    def tracked_allocate(min_port=40000, max_port=60000, attempts=50):
        calls.append((min_port, max_port, attempts))
        return 41000

    monkeypatch.setattr(port_allocator, "allocate_host_port", tracked_allocate)

    bindings = port_allocator.allocate_port_bindings(
        ["8080"],
        min_port=41000,
        max_port=41100,
    )

    try:
        assert calls == [(41000, 41100, 50)]
        assert bindings == {"8080": (port_allocator.DOCKER_PUBLISH_HOST, 41000)}
    finally:
        port_allocator.release_port_bindings(bindings)


def test_port_remains_reserved_until_bindings_are_released(monkeypatch) -> None:
    candidates = iter([45678, 45678, 45679, 45678])
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: next(candidates))
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket([]),
    )

    first: dict[str, tuple[str, int]] = {}
    second: dict[str, tuple[str, int]] = {}
    third: dict[str, tuple[str, int]] = {}
    try:
        first = port_allocator.allocate_port_bindings(["8080"])
        second = port_allocator.allocate_port_bindings(["8080"])
        port_allocator.release_port_bindings(first)
        third = port_allocator.allocate_port_bindings(["8080"])

        assert first["8080"][1] == 45678
        assert second["8080"][1] == 45679
        assert third["8080"][1] == 45678
    finally:
        port_allocator.release_port_bindings(first)
        port_allocator.release_port_bindings(second)
        port_allocator.release_port_bindings(third)


def test_partial_allocation_failure_releases_reserved_ports(monkeypatch) -> None:
    monkeypatch.setattr(port_allocator.random, "randint", lambda a, b: 45678)
    monkeypatch.setattr(
        port_allocator.socket,
        "socket",
        lambda family, sock_type: _FakeSocket([]),
    )

    with pytest.raises(HTTPException):
        port_allocator.allocate_port_bindings(
            ["44772", "8080"],
            min_port=45678,
            max_port=45678,
        )

    bindings = port_allocator.allocate_port_bindings(
        ["8080"],
        min_port=45678,
        max_port=45678,
    )
    try:
        assert bindings["8080"][1] == 45678
    finally:
        port_allocator.release_port_bindings(bindings)
