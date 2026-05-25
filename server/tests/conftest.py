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

import os
from pathlib import Path

import pytest
from fastapi.testclient import TestClient
from unittest.mock import MagicMock

TEST_CONFIG_PATH = Path(__file__).resolve().parent / "testdata" / "config.toml"
os.environ.setdefault("SANDBOX_CONFIG_PATH", str(TEST_CONFIG_PATH))

# Prevent real Docker connections during tests by mocking docker.from_env
import docker  # noqa: E402

_mock_docker_client = MagicMock()
_mock_docker_client.containers.list.return_value = []
docker.from_env = lambda: _mock_docker_client  # type: ignore

from opensandbox_server.main import app  # noqa: E402


@pytest.fixture(scope="session")
def test_api_key() -> str:
    return "test-api-key-12345"


@pytest.fixture(scope="function")
def client() -> TestClient:
    return TestClient(app)


@pytest.fixture(scope="function")
def auth_headers(test_api_key: str) -> dict:
    return {"OPEN-SANDBOX-API-KEY": test_api_key}


@pytest.fixture(scope="session")
def sample_sandbox_request() -> dict:
    return {
        "image": {"uri": "python:3.11"},
        "timeout": 3600,
        "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
        "env": {"DEBUG": "true", "LOG_LEVEL": "info"},
        "metadata": {"name": "Test Sandbox", "project": "test-project"},
        "entrypoint": ["python", "-c", "print('Hello from sandbox')"],
    }
