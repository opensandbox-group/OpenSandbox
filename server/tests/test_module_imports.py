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

"""Modules import on their own, not only after ``opensandbox_server.main``.

The test session imports ``main`` first (see conftest.py), which hides an
import cycle, so the imports run in a fresh interpreter.
"""

import subprocess
import sys

# Like conftest.py, keep the Docker service that lifecycle builds at import off
# a real daemon, which CI hosts such as the Windows runners do not have.
_NO_DOCKER = "import docker; from unittest.mock import MagicMock; docker.from_env = MagicMock; "

_MODULES = [
    "opensandbox_server.services",
    "opensandbox_server.services.fast_sandbox.service",
    "opensandbox_server.services.snapshot_service",
    "opensandbox_server.api.lifecycle",
    "opensandbox_server.integrations.idle_pause",
]


def test_modules_import_in_a_fresh_interpreter() -> None:
    result = subprocess.run(
        [sys.executable, "-c", _NO_DOCKER + "\n".join(f"import {m}" for m in _MODULES)],
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert result.returncode == 0, result.stderr
