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

"""Container detection decides whether ``[docker].host_ip`` is used to reach
host-mapped ports. Docker creates ``/.dockerenv``; Podman creates
``/run/.containerenv`` instead and exports ``container=podman``. Treating a
Podman-hosted server as bare metal sent every egress sidecar readiness probe
to 127.0.0.1, where nothing listens (#1801)."""

import os
from types import SimpleNamespace

from opensandbox_server.services.docker import networking


def _only_these_paths_exist(monkeypatch, *paths: str) -> None:
    monkeypatch.setattr(os.path, "exists", lambda path: path in paths)


def _no_container_env(monkeypatch) -> None:
    monkeypatch.delenv("container", raising=False)
    monkeypatch.delenv("CONTAINER", raising=False)


def test_docker_marker_file(monkeypatch):
    _only_these_paths_exist(monkeypatch, "/.dockerenv")
    _no_container_env(monkeypatch)
    assert networking._running_inside_docker_container() is True


def test_podman_marker_file(monkeypatch):
    _only_these_paths_exist(monkeypatch, "/run/.containerenv")
    _no_container_env(monkeypatch)
    assert networking._running_inside_docker_container() is True


def test_container_env_var(monkeypatch):
    _only_these_paths_exist(monkeypatch)
    _no_container_env(monkeypatch)
    monkeypatch.setenv("container", "podman")
    assert networking._running_inside_docker_container() is True


def test_bare_host(monkeypatch):
    _only_these_paths_exist(monkeypatch)
    _no_container_env(monkeypatch)
    assert networking._running_inside_docker_container() is False


def test_empty_env_var_is_not_a_container(monkeypatch):
    _only_these_paths_exist(monkeypatch)
    _no_container_env(monkeypatch)
    monkeypatch.setenv("container", "")
    assert networking._running_inside_docker_container() is False


def test_unrelated_env_var_value_is_not_a_container(monkeypatch):
    """A generic CONTAINER=build on bare metal must not switch endpoint
    resolution to [docker].host_ip; only runtime-set values count."""
    _only_these_paths_exist(monkeypatch)
    _no_container_env(monkeypatch)
    monkeypatch.setenv("CONTAINER", "build")
    assert networking._running_inside_docker_container() is False
    monkeypatch.setenv("CONTAINER", "Docker")
    assert networking._running_inside_docker_container() is True


def test_proxy_host_uses_docker_host_ip_under_podman(monkeypatch):
    """The reporter's failure: server in a rootless Podman container with
    ``[docker].host_ip`` set, but the sidecar probe went to 127.0.0.1."""
    _only_these_paths_exist(monkeypatch, "/run/.containerenv")
    _no_container_env(monkeypatch)
    fake_self = SimpleNamespace(
        app_config=SimpleNamespace(server=SimpleNamespace(host="0.0.0.0")),
        _get_docker_host_ip=lambda: "host.docker.internal",
    )
    assert networking.DockerNetworkingMixin._resolve_proxy_host(fake_self) == "host.docker.internal"


def test_proxy_host_falls_back_to_loopback_on_bare_metal(monkeypatch):
    _only_these_paths_exist(monkeypatch)
    _no_container_env(monkeypatch)
    fake_self = SimpleNamespace(
        app_config=SimpleNamespace(server=SimpleNamespace(host="0.0.0.0")),
        _get_docker_host_ip=lambda: "host.docker.internal",
    )
    assert networking.DockerNetworkingMixin._resolve_proxy_host(fake_self) == "127.0.0.1"
