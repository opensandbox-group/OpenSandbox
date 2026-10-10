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

from opensandbox.adapters.converter.sandbox_model_converter import SandboxModelConverter
from opensandbox.api.lifecycle.models.create_sandbox_response import (
    CreateSandboxResponse,
)
from opensandbox.api.lifecycle.models.sandbox import Sandbox


@pytest.mark.parametrize("value", ["missing", None, "sha256:" + "a" * 64])
def test_runtime_digest_survives_information_and_creation_conversion(value):
    payload = {
        "id": "sandbox-1",
        "image": {"uri": "python:3.11"},
        "status": {"state": "Running"},
        "entrypoint": ["python"],
        "createdAt": "2026-10-09T00:00:00Z",
    }
    if value != "missing":
        payload["resolvedImageDigest"] = value
    expected = None if value == "missing" else value
    info = SandboxModelConverter.to_sandbox_info(Sandbox.from_dict(payload))
    created = SandboxModelConverter.to_sandbox_create_response(
        CreateSandboxResponse.from_dict(payload)
    )
    assert info.resolved_image_digest == expected
    assert created.resolved_image_digest == expected
    assert info.image.image == "python:3.11"
