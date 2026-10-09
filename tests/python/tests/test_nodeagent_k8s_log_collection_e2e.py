#
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
#
"""Nightly E2E: the node-agent collects a real sandbox's container logs.

Runs against the full stack deployed by ``scripts/nodeagent-k8s-smoke.sh``
(controller + server + node-agent file sink on Kind). A sandbox is created
through the server, its entrypoint prints a unique marker to the container
stdout, and the test asserts the marker reaches the node-local file-sink
directory. After the sandbox is deleted the stream must be finalized with a
``sandbox.finalized.*.json`` marker naming the sandbox.
"""

from __future__ import annotations

import json
import os
import subprocess
import time
import uuid
from collections.abc import Callable
from datetime import timedelta
from typing import TypeVar

import pytest
from opensandbox import SandboxSync

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

CLUSTER_ID = os.getenv("NODEAGENT_SMOKE_CLUSTER_ID", "nightly-smoke")
DATA_ROOT = os.getenv("NODEAGENT_SMOKE_DATA_ROOT", "/var/lib/opensandbox/nodeagent-data")
KIND_NODE = os.getenv("NODEAGENT_SMOKE_KIND_NODE", "")

# Only scripts/nodeagent-k8s-smoke.sh deploys the node-agent file sink and
# exports the kind node name to inspect; everywhere else (docker bridge,
# mini-e2e) the required stack is absent, so skip instead of failing.
pytestmark = pytest.mark.skipif(
    not KIND_NODE,
    reason="requires the node-agent full-stack smoke environment "
    "(NODEAGENT_SMOKE_KIND_NODE from scripts/nodeagent-k8s-smoke.sh)",
)

COLLECT_TIMEOUT = timedelta(minutes=4)
FINALIZE_TIMEOUT = timedelta(minutes=4)
POLL_INTERVAL = timedelta(seconds=2)

T = TypeVar("T")


def _node_command(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["docker", "exec", KIND_NODE, *args],
        capture_output=True,
        text=True,
        timeout=60,
    )


def _marker_collected(marker: str) -> bool:
    result = _node_command("grep", "-R", "-l", "-F", marker, DATA_ROOT)
    return bool(result.stdout.strip())


def _finalized_marker(sandbox_id: str) -> str:
    result = _node_command(
        "find", DATA_ROOT, "-type", "f", "-name", "sandbox.finalized.*.json"
    )
    candidates = [line for line in result.stdout.splitlines() if sandbox_id in line]
    return candidates[0] if candidates else ""


def _eventually(
    description: str,
    condition: Callable[[], bool],
    timeout: timedelta,
    interval: timedelta = POLL_INTERVAL,
) -> None:
    deadline = time.monotonic() + timeout.total_seconds()
    while not condition():
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {description}")
        time.sleep(interval.total_seconds())


def _eventually_value(
    description: str,
    producer: Callable[[], T],
    timeout: timedelta,
    interval: timedelta = POLL_INTERVAL,
) -> T:
    deadline = time.monotonic() + timeout.total_seconds()
    while True:
        value = producer()
        if value:
            return value
        if time.monotonic() > deadline:
            raise AssertionError(f"timed out waiting for {description}")
        time.sleep(interval.total_seconds())


@pytest.mark.e2e
class TestNodeAgentK8sLogCollectionE2E:
    """Full-stack node-agent collection of a real server-created sandbox."""

    @pytest.mark.timeout(600)
    def test_real_sandbox_stdout_collected_and_finalized(self) -> None:
        marker = f"nodeagent-smoke-{uuid.uuid4().hex}"
        sandbox = SandboxSync.create(
            get_sandbox_image(),
            entrypoint=["sh", "-c", f"echo {marker}; exec tail -f /dev/null"],
            env={"E2E_TEST": "true"},
            metadata={"suite": "nodeagent-k8s-log-collection-e2e"},
            resource=get_e2e_sandbox_resource(),
            connection_config=create_connection_config_sync(),
            timeout=timedelta(minutes=15),
        )
        try:
            assert sandbox.is_healthy()

            result = sandbox.commands.run("echo exec-ok")
            assert result.error is None
            assert result.logs.stdout[0].text == "exec-ok"

            _eventually(
                "sandbox stdout marker reaching the node-agent file sink",
                lambda: _marker_collected(marker),
                COLLECT_TIMEOUT,
            )
        finally:
            sandbox.kill()
            sandbox.close()

        marker_file = _eventually_value(
            "finalization marker for the deleted sandbox",
            lambda: _finalized_marker(sandbox.id),
            FINALIZE_TIMEOUT,
        )
        node_file = _node_command("cat", marker_file)
        payload = json.loads(node_file.stdout)
        assert payload["resource"]["sandbox_id"] == sandbox.id
        assert CLUSTER_ID in marker_file
