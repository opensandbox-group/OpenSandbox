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

from pathlib import Path

import yaml

from opensandbox_server.config import AppConfig

try:  # Python 3.11+
    import tomllib  # type: ignore[attr-defined]
except ModuleNotFoundError:  # Python 3.10 fallback
    import tomli as tomllib  # type: ignore[import]


def test_compose_example_routes_proxy_through_host_mappings() -> None:
    compose_path = Path(__file__).resolve().parents[1] / "docker-compose.example.yaml"
    compose = yaml.safe_load(compose_path.read_text(encoding="utf-8"))

    config_content = compose["configs"]["opensandbox-config"]["content"]
    config = AppConfig.model_validate(tomllib.loads(config_content))

    assert config.runtime.type == "docker"
    assert config.docker.network_mode == "bridge"
    assert config.proxy.resolve_internal is False
    assert config.docker.host_ip == "host.docker.internal"

    expected_host_mapping = "host.docker.internal:host-gateway"
    for service_name in ("opensandbox-server", "sdk-client"):
        assert expected_host_mapping in compose["services"][service_name]["extra_hosts"]


def test_compose_example_pins_a_released_server_image() -> None:
    repo_root = Path(__file__).resolve().parents[2]
    compose = yaml.safe_load(
        (repo_root / "server/docker-compose.example.yaml").read_text(encoding="utf-8")
    )
    image = compose["services"]["opensandbox-server"]["image"]
    repository, tag = image.rsplit(":", 1)

    assert repository == "opensandbox/server"
    assert tag.startswith("release-"), "Use a fixed umbrella release with the proxy fixes"
    version = tag.removeprefix("release-")
    release = yaml.safe_load(
        (repo_root / "docs/releases" / f"{version}.yaml").read_text(encoding="utf-8")
    )
    assert release["metadata"]["channel"] == "stable"
    assert release["images"]["server"]["tag"] == tag
