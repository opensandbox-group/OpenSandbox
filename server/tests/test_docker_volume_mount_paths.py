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

import pytest
from fastapi import HTTPException

from opensandbox_server.api.schema import Host, Volume
from opensandbox_server.services.constants import SandboxErrorCodes
from opensandbox_server.services.docker.volumes import _validate_docker_mount_paths


def _volume(name: str, mount_path: str, host_path: str = "/tmp/data") -> Volume:
    return Volume(
        name=name,
        host=Host(path=host_path),
        mount_path=mount_path,
        read_only=False,
    )


def test_rejects_root_mount_path():
    with pytest.raises(HTTPException) as exc_info:
        _validate_docker_mount_paths([_volume("data", "/")])

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH
    assert "root" in exc_info.value.detail["message"].lower()


@pytest.mark.parametrize(
    ("first", "second"),
    [("/d1", "/d1"), ("/d1", "/d1/")],
    ids=["exact-duplicate", "trailing-slash-duplicate"],
)
def test_rejects_duplicate_normalized_mount_paths(first: str, second: str):
    volumes = [_volume("first", first), _volume("second", second)]

    with pytest.raises(HTTPException) as exc_info:
        _validate_docker_mount_paths(volumes)

    assert exc_info.value.status_code == 400
    assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_MOUNT_PATH
    assert "duplicate" in exc_info.value.detail["message"].lower()


def test_allows_nested_mount_paths():
    _validate_docker_mount_paths(
        [_volume("parent", "/d1"), _volume("child", "/d1/inner")]
    )


def test_allows_same_source_at_distinct_mount_paths():
    _validate_docker_mount_paths(
        [
            _volume("first", "/d1", host_path="/tmp/shared"),
            _volume("second", "/d2", host_path="/tmp/shared"),
        ]
    )
