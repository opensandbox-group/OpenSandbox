#
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
#
"""
E2E tests for the server pool allocation path.

The Pool CR (``tests/python/tests/support/server-pool.yaml``) is installed
directly with kubectl; its pod template runs execd as PID 1
(``/bootstrap.sh sleep 3600`` with ``EXECD_INIT=1``), so pooled sandboxes can
execute SDK commands through execd. The test then verifies the server-driven
allocation path: sandboxes created through the SDK with
``extensions.poolRef`` are allocated from the pre-warmed pool, pool status
counters move accordingly, and execd interaction works on the pooled pod.

This is complementary to the SDK-side pool covered by
``test_sandbox_pool_e2e_sync.py``, which never creates a Pool CR.

Requires a Kind cluster with the controller deployed and the execd image
loaded as ``opensandbox/execd:e2e-local`` (scripts/python-k8s-e2e*.sh),
plus kubectl on PATH.
"""

import logging
import os
import subprocess
import time
from pathlib import Path

import pytest
from opensandbox import SandboxSync

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    is_kubernetes_runtime,
)

logger = logging.getLogger(__name__)

E2E_NAMESPACE = os.getenv("OPENSANDBOX_E2E_NAMESPACE", "opensandbox-e2e")
POOL_NAME = "e2e-server-pool"
POOL_YAML = Path(__file__).parent / "support" / "server-pool.yaml"

WARM_TIMEOUT_SECONDS = 180
SANDBOX_READY_TIMEOUT_SECONDS = 120
RELEASE_TIMEOUT_SECONDS = 120


def _kubectl(*args: str, timeout: int = 60, input: str | None = None) -> str:
    out = subprocess.run(
        ["kubectl", *args],
        check=True, capture_output=True, text=True, timeout=timeout, input=input,
    )
    return out.stdout.strip()


def _apply_pool() -> None:
    manifest = POOL_YAML.read_text()
    if E2E_NAMESPACE != "opensandbox-e2e":
        manifest = manifest.replace("namespace: opensandbox-e2e", f"namespace: {E2E_NAMESPACE}")
    _kubectl("apply", "-f", "-", input=manifest)


def _pool_status_field(field: str) -> str:
    return _kubectl(
        "get", "pool", POOL_NAME, "-n", E2E_NAMESPACE,
        "-o", f"jsonpath={{.status.{field}}}",
    )


def _eventually(description: str, predicate, timeout_seconds: int = 60) -> None:
    deadline = time.monotonic() + timeout_seconds
    last_exc: Exception | None = None
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except Exception as exc:  # noqa: BLE001 - poll until timeout
            last_exc = exc
        time.sleep(3)
    if last_exc is not None:
        raise AssertionError(f"timed out waiting for {description}: {last_exc}") from last_exc
    raise AssertionError(f"timed out waiting for {description}")


@pytest.mark.e2e
@pytest.mark.skipif(
    not is_kubernetes_runtime(),
    reason="Server pools require the Kubernetes runtime (Pool CRD)",
)
class TestServerPoolLifecycleE2ESync:
    """Allocation + execd interaction of a kubectl-installed Pool."""

    @pytest.fixture(scope="class", autouse=True)
    def server_pool(self) -> dict:
        """Apply the Pool CR, wait for warm buffer pods, and clean up."""
        _apply_pool()
        sandboxes: list[SandboxSync] = []
        try:
            _eventually(
                "pool warms up idle buffer pods",
                lambda: _pool_status_field("available") not in ("", "0"),
                timeout_seconds=WARM_TIMEOUT_SECONDS,
            )
            yield {"sandboxes": sandboxes}
        finally:
            for sandbox in sandboxes:
                try:
                    sandbox.destroy()
                except Exception as exc:  # noqa: BLE001 - best-effort teardown
                    logger.warning(
                        "failed to destroy sandbox %s during teardown: %s",
                        sandbox.id, exc,
                    )
            _kubectl(
                "delete", "pool", POOL_NAME, "-n", E2E_NAMESPACE,
                "--ignore-not-found=true", timeout=120,
            )

    def _create_pooled_sandbox(self, pool: dict) -> SandboxSync:
        sandbox = SandboxSync.create(
            image=get_sandbox_image(),
            resource=get_e2e_sandbox_resource(),
            timeout=None,
            extensions={"poolRef": POOL_NAME},
            connection_config=create_connection_config_sync(),
        )
        pool["sandboxes"].append(sandbox)

        _eventually(
            f"sandbox {sandbox.id} reaches Running",
            lambda: sandbox.get_info().status.state == "Running",
            timeout_seconds=SANDBOX_READY_TIMEOUT_SECONDS,
        )
        return sandbox

    def test_01_pool_warms_buffer(self, server_pool) -> None:
        assert int(_pool_status_field("available")) >= 1
        assert int(_pool_status_field("total")) >= 1

    def test_02_allocate_pooled_sandbox_and_run_command_via_execd(
        self, server_pool,
    ) -> None:
        allocated_before = int(_pool_status_field("allocated"))

        sandbox = self._create_pooled_sandbox(server_pool)

        _eventually(
            f"pool allocation grows after allocating {sandbox.id}",
            lambda: int(_pool_status_field("allocated")) >= allocated_before + 1,
            timeout_seconds=SANDBOX_READY_TIMEOUT_SECONDS,
        )

        marker = f"pool-execd-{int(time.time() * 1000)}"
        result = sandbox.commands.run(f"echo {marker}")
        assert result.error is None
        assert len(result.logs.stdout) == 1
        assert result.logs.stdout[0].text == marker

    def test_03_destroy_releases_pod_back_to_pool(self, server_pool) -> None:
        sandbox = self._create_pooled_sandbox(server_pool)

        allocated_before = int(_pool_status_field("allocated"))
        assert allocated_before >= 1

        sandbox.destroy()
        server_pool["sandboxes"].remove(sandbox)

        _eventually(
            "pool allocation drops after sandbox destroy",
            lambda: int(_pool_status_field("allocated")) == allocated_before - 1,
            timeout_seconds=RELEASE_TIMEOUT_SECONDS,
        )
        assert int(_pool_status_field("available")) >= 1, (
            "buffer should refill after release"
        )
