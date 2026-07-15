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

import io
import pathlib
import tarfile
from unittest.mock import MagicMock, patch

from opensandbox_server.config import AppConfig, IngressConfig, RuntimeConfig, ServerConfig
from opensandbox_server.services.docker import DockerSandboxService


def _app_config() -> AppConfig:
    return AppConfig(
        server=ServerConfig(),
        runtime=RuntimeConfig(type="docker", execd_image="ghcr.io/opensandbox/platform:latest"),
        ingress=IngressConfig(mode="direct"),
    )


def _extract_bootstrap_script(archive_bytes: bytes) -> str:
    with tarfile.open(fileobj=io.BytesIO(archive_bytes), mode="r:") as tar:
        member = tar.getmember("bootstrap.sh")
        extracted = tar.extractfile(member)
        assert extracted is not None
        return extracted.read().decode("utf-8")


def _read_full_bootstrap_sh() -> bytes:
    """Read the real execd bootstrap.sh from the components directory."""
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    bs_path = repo_root / "components" / "execd" / "bootstrap.sh"
    script_bytes = bs_path.read_bytes()

    # Wrap as a tar archive so it can be consumed by _install_bootstrap_script
    # via put_archive, just like Docker's get_archive returns.
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w") as tar:
        info = tarfile.TarInfo(name="bootstrap.sh")
        info.mode = 0o755
        info.size = len(script_bytes)
        tar.addfile(info, io.BytesIO(script_bytes))
    return buf.getvalue()


@patch("opensandbox_server.services.docker.docker_service.docker")
def test_install_bootstrap_script_uses_full_bootstrap_sh(mock_docker):
    """Verify _install_bootstrap_script writes the full bootstrap.sh from the cache.

    The test pre-populates _bootstrap_script_cache with the real bootstrap.sh
    from components/execd/bootstrap.sh and asserts that the installed content
    contains features from the full script (MITM CA handling, signal forwarding,
    etc.) that were absent from the old inline-generated shim.
    """
    mock_docker.from_env.return_value = MagicMock()
    service = DockerSandboxService(config=_app_config())

    # Pre-populate the cache as _copy_execd_to_container would.
    cache_key = service._normalize_platform_key(None)
    archive = _read_full_bootstrap_sh()
    service._bootstrap_script_cache[cache_key] = archive

    mock_container = MagicMock()

    with patch.object(service, "_ensure_directory") as mock_ensure_dir, patch.object(
        service, "_docker_operation"
    ):
        service._install_bootstrap_script(mock_container, "test-sandbox")

    mock_ensure_dir.assert_called_once()
    archive_bytes = mock_container.put_archive.call_args.kwargs["data"]
    script = _extract_bootstrap_script(archive_bytes)

    # Full bootstrap.sh features (absent from the old inline shim).
    assert "trust_mitm_ca" in script
    assert "trust_mitm_ca_jdk" in script
    assert "_forward_signal" in script
    assert "OPENSANDBOX_MERGED_CA" in script
    assert "EXECD_BOOTSTRAP_PRE_SCRIPT" in script

    # Core execd lifecycle shared by both old and new.
    assert 'EXECD="${EXECD:=/opt/opensandbox/execd}"' in script or "EXECD=" in script
    assert 'if [ -z "${EXECD_ENVS:-}" ]; then' in script
    assert 'export EXECD_ENVS' in script
