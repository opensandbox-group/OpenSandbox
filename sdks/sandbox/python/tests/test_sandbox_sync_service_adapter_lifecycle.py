#
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
#
from __future__ import annotations

import pytest

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync


def test_sync_get_sandbox_endpoint_logs_warning_and_not_error_on_failure(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    def _boom(*, sandbox_id, port, client, use_server_proxy=False):
        raise RuntimeError("endpoint exploded")

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandboxes_sandbox_id_endpoints_port.sync_detailed",
        _boom,
    )

    adapter = SandboxesAdapterSync(ConnectionConfigSync())

    with caplog.at_level(
        "WARNING", logger="opensandbox.sync.adapters.sandboxes_adapter"
    ):
        with pytest.raises(Exception) as exc_info:
            adapter.get_sandbox_endpoint("sbx-1", 8080)

    assert "endpoint exploded" in str(exc_info.value)
    assert not [r for r in caplog.records if r.levelname == "ERROR"]
    debug_messages = [
        r.getMessage() for r in caplog.records if r.levelname == "WARNING"
    ]
    assert any(
        "Failed to retrieve sandbox endpoint for sandbox sbx-1" in msg
        for msg in debug_messages
    )


def test_sync_resume_sandbox_logs_warning_and_not_error_on_failure(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    def _boom(*, client, sandbox_id):
        raise RuntimeError("resume exploded")

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes_sandbox_id_resume.sync_detailed",
        _boom,
    )

    adapter = SandboxesAdapterSync(ConnectionConfigSync())

    with caplog.at_level(
        "WARNING", logger="opensandbox.sync.adapters.sandboxes_adapter"
    ):
        with pytest.raises(Exception) as exc_info:
            adapter.resume_sandbox("sbx-2")

    assert "resume exploded" in str(exc_info.value)
    assert not [r for r in caplog.records if r.levelname == "ERROR"]
    debug_messages = [
        r.getMessage() for r in caplog.records if r.levelname == "WARNING"
    ]
    assert any(
        "Failed to resume sandbox sbx-2: resume exploded" in msg
        for msg in debug_messages
    )
    assert all(r.exc_info is None for r in caplog.records if r.levelname == "WARNING")
