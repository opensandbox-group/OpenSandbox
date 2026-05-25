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

import os
import stat

import pytest

from opensandbox_server.services.docker.ossfs_mixin import OSSFSMixin


def test_write_ossfs_private_config_file_writes_content(tmp_path) -> None:
    config_path = tmp_path / "ossfs.conf"

    OSSFSMixin._write_ossfs_private_config_file(str(config_path), "secret")

    assert config_path.read_text(encoding="utf-8") == "secret"


@pytest.mark.skipif(os.name == "nt", reason="Windows does not expose POSIX owner-only mode bits.")
def test_write_ossfs_private_config_file_uses_owner_only_mode(tmp_path) -> None:
    config_path = tmp_path / "ossfs.conf"

    OSSFSMixin._write_ossfs_private_config_file(str(config_path), "secret")

    assert stat.S_IMODE(config_path.stat().st_mode) == 0o600
