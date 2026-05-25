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

from opensandbox_server.services.docker import port_allocator


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
    monkeypatch.setattr(port_allocator, "allocate_host_port", lambda: 45678)

    bindings = port_allocator.allocate_port_bindings(["8080"])

    assert bindings == {"8080": (port_allocator.DOCKER_PUBLISH_HOST, 45678)}
