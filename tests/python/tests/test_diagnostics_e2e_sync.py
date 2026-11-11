# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Real-server diagnostics coverage for the synchronous public SDK."""

import json
import logging
import os
import subprocess
import sys
import time
from datetime import timedelta
from uuid import uuid4

import pytest
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.sync import SandboxManagerSync, SandboxSync

from tests.base_e2e_test import (
    TEST_API_KEY,
    TEST_DOMAIN,
    TEST_PROTOCOL,
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)
MARKER = "opensandbox-diagnostics-e2e"


def run_cli(tmp_path, *args):
    env = {**os.environ, "OPEN_SANDBOX_API_KEY": TEST_API_KEY}
    for key in (
        "http_proxy",
        "https_proxy",
        "HTTP_PROXY",
        "HTTPS_PROXY",
        "ALL_PROXY",
        "all_proxy",
    ):
        env.pop(key, None)
    return subprocess.run(
        [
            sys.executable,
            "-m",
            "opensandbox_cli",
            "--config",
            str(tmp_path / "osb.toml"),
            "--domain",
            TEST_DOMAIN,
            "--protocol",
            TEST_PROTOCOL,
            "--no-color",
            "diagnostics",
            *args,
        ],
        env=env,
        capture_output=True,
        text=True,
        timeout=45,
        check=False,
    )


def assert_content(result, sandbox_id, kind, scope):
    assert result.sandbox_id == sandbox_id
    assert result.kind == kind
    assert result.scope == scope
    assert result.delivery == "inline"
    assert result.content_type.startswith("text/plain")
    assert isinstance(result.content, str)
    assert isinstance(result.truncated, bool)


@pytest.fixture(scope="module")
def diagnostic_clients():
    config = create_connection_config_sync()
    manager = SandboxManagerSync.create(connection_config=config)
    sandbox = None
    try:
        sandbox = SandboxSync.create(
            image=get_sandbox_image(),
            connection_config=config,
            resource=get_e2e_sandbox_resource(),
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=90),
            entrypoint=["sh", "-c", f"echo {MARKER}; sleep 300"],
            env={"EXECD_API_GRACE_SHUTDOWN": "1s"},
        )
        yield manager, sandbox
    finally:
        try:
            if sandbox is not None:
                try:
                    sandbox.kill()
                finally:
                    sandbox.close()
        finally:
            manager.close()
            if config.transport is not None:
                config.transport.close()


def test_context_from_manager_and_sandbox(diagnostic_clients):
    manager, sandbox = diagnostic_clients
    context = manager.get_diagnostic_context(sandbox.id)
    current = sandbox.get_diagnostic_context()
    assert context.sandbox_id == current.sandbox_id == sandbox.id
    assert context.backend_type == current.backend_type
    assert context.backend_identifiers == current.backend_identifiers
    assert context.state
    if context.backend_type == "docker":
        assert context.backend_identifiers["containerId"]
        assert context.backend_identifiers["containerName"]
    else:
        assert context.backend_type == "kubernetes"
        assert context.backend_identifiers["namespace"]
        assert context.backend_identifiers["workloadName"]
    logger.info("diagnostic context: %s", context.model_dump())


def test_cli_context_logs_and_events(diagnostic_clients, tmp_path):
    manager, sandbox = diagnostic_clients
    context = run_cli(tmp_path, "context", sandbox.id, "-o", "json")
    assert context.returncode == 0, context.stderr
    actual = json.loads(context.stdout)
    expected = manager.get_diagnostic_context(sandbox.id)
    assert actual["sandbox_id"] == expected.sandbox_id
    assert actual["backend_type"] == expected.backend_type
    assert actual["backend_identifiers"] == expected.backend_identifiers
    deadline = time.monotonic() + 30
    while MARKER not in (
        manager.get_diagnostic_logs(sandbox.id, "container").content or ""
    ):
        assert time.monotonic() < deadline, "entrypoint log missing before CLI check"
        time.sleep(0.5)
    logs = run_cli(tmp_path, "logs", sandbox.id, "--scope", "container", "-o", "raw")
    assert logs.returncode == 0, logs.stderr
    assert MARKER in logs.stdout
    events = run_cli(tmp_path, "events", sandbox.id, "--scope", "runtime", "-o", "json")
    assert events.returncode == 0, events.stderr
    assert_content(
        DiagnosticContent(**json.loads(events.stdout)),
        sandbox.id,
        "events",
        "runtime",
    )
    missing = run_cli(tmp_path, "context", str(uuid4()), "-o", "json")
    assert missing.returncode != 0


@pytest.mark.parametrize("scope", ["container", "all"])
def test_logs_contain_entrypoint_output(diagnostic_clients, scope):
    manager, sandbox = diagnostic_clients
    for get_logs in (
        lambda: manager.get_diagnostic_logs(sandbox.id, scope),
        lambda: sandbox.get_diagnostic_logs(scope),
    ):
        deadline = time.monotonic() + 30
        while True:
            result = get_logs()
            assert_content(result, sandbox.id, "logs", scope)
            if MARKER in result.content:
                break
            assert time.monotonic() < deadline, (
                f"entrypoint log missing: {result.content!r}"
            )
            time.sleep(0.5)


@pytest.mark.parametrize("scope", ["runtime", "all"])
def test_events_descriptors(diagnostic_clients, scope):
    manager, sandbox = diagnostic_clients
    assert_content(
        manager.get_diagnostic_events(sandbox.id, scope), sandbox.id, "events", scope
    )
    assert_content(sandbox.get_diagnostic_events(scope), sandbox.id, "events", scope)


@pytest.mark.parametrize("kind", ["logs", "events"])
def test_unsupported_scope_preserves_api_error(diagnostic_clients, kind):
    manager, sandbox = diagnostic_clients
    method = getattr(manager, f"get_diagnostic_{kind}")
    with pytest.raises(SandboxApiException) as error:
        method(sandbox.id, "unsupported-e2e-scope")
    assert error.value.status_code == 400
    assert error.value.error.code == "DIAGNOSTICS_SCOPE_UNSUPPORTED"


@pytest.mark.parametrize("kind", ["context", "logs", "events"])
def test_missing_sandbox_preserves_not_found(diagnostic_clients, kind):
    manager, _ = diagnostic_clients
    sandbox_id = str(uuid4())
    method = getattr(manager, f"get_diagnostic_{kind}")
    args = (
        (sandbox_id,)
        if kind == "context"
        else (sandbox_id, "container" if kind == "logs" else "runtime")
    )
    with pytest.raises(SandboxApiException) as error:
        method(*args)
    assert error.value.status_code == 404
