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

"""Shared test fixtures."""

from __future__ import annotations

from unittest.mock import MagicMock

import pytest
from click.testing import CliRunner

from opensandbox_cli.output import OutputFormatter


@pytest.fixture()
def runner() -> CliRunner:
    return CliRunner()


@pytest.fixture()
def mock_manager() -> MagicMock:
    return MagicMock()


@pytest.fixture()
def mock_sandbox() -> MagicMock:
    return MagicMock()


@pytest.fixture()
def mock_client_context(mock_manager: MagicMock, mock_sandbox: MagicMock) -> MagicMock:
    """A mock ClientContext that avoids real SDK/HTTP calls."""
    ctx = MagicMock()
    ctx.resolved_config = {
        "api_key": "test-key",
        "domain": "localhost:8080",
        "protocol": "http",
        "request_timeout": 30,
        "color": False,
        "default_image": None,
        "default_timeout": None,
    }
    ctx.output = OutputFormatter("json", color=False)
    def _make_output(fmt: str) -> OutputFormatter:
        formatter = OutputFormatter(fmt, color=False)
        ctx.output = formatter
        return formatter
    ctx.make_output.side_effect = _make_output
    ctx.get_manager.return_value = mock_manager
    ctx.connect_sandbox.return_value = mock_sandbox
    ctx.connection_config = MagicMock()
    ctx.close = MagicMock()
    return ctx
