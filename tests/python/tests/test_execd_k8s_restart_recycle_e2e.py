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
K8s Restart-recycle e2e against an init-mode execd pod (OSEP-0018).

Verifies the ``restart_default.go`` "contract compatible" claim end to end:
a Pool whose pod template runs execd as PID 1 (``bootstrap.sh`` +
``EXECD_INIT=1`` + keepalive) uses the Restart recycle strategy; when the
BatchSandbox is released, the controller pod-execs ``kill 1`` inside the
container, execd forwards SIGTERM to the workload and exits, and the
kubelet restarts the container (restartCount increases) while the pod stays
alive — the pool keeps working for the next allocation.

Requires a Kind cluster with the controller and the execd image loaded
(scripts/python-k8s-execd-init-e2e.sh), plus kubectl on PATH.
"""

import logging
import os
import subprocess
import time

import pytest

from tests.base_e2e_test import is_kubernetes_runtime

logger = logging.getLogger(__name__)

E2E_NAMESPACE = os.getenv("OPENSANDBOX_E2E_NAMESPACE", "opensandbox-e2e")
EXECD_IMG = os.getenv("OPENSANDBOX_EXECD_IMAGE", "opensandbox/execd:e2e-local")

POOL_NAME = "execd-init-restart-recycle"
BS_NAME = "execd-init-restart-recycle-bs"


def _kubectl(*args: str, timeout: int = 60, input: str | None = None) -> str:
    out = subprocess.run(
        ["kubectl", *args],
        check=True, capture_output=True, text=True, timeout=timeout, input=input,
    )
    return out.stdout.strip()


@pytest.mark.skipif(
    not is_kubernetes_runtime(),
    reason="Pool + Restart recycle requires the Kubernetes runtime",
)
class TestExecdK8sRestartRecycleE2E:
    @pytest.fixture(scope="class", autouse=True)
    def pool(self):
        """Pool whose pod template runs execd as PID 1 with a keepalive, and
        a Restart recycle strategy."""
        pool_yaml = f"""apiVersion: sandbox.opensandbox.io/v1alpha1
kind: Pool
metadata:
  name: {POOL_NAME}
  namespace: {E2E_NAMESPACE}
spec:
  template:
    spec:
      containers:
        - name: sandbox-container
          image: {EXECD_IMG}
          command: ["/bootstrap.sh", "sleep", "3600"]
          env:
            - name: EXECD_INIT
              value: "1"
            # The execd image keeps the binary at /execd (no execd-installer
            # on the Pool path); bootstrap.sh defaults to
            # /opt/opensandbox/execd, so point it at the real path.
            - name: EXECD
              value: "/execd"
            - name: OPENSANDBOX_ID
              value: "restart-recycle-sbx"
  capacitySpec:
    bufferMax: 2
    bufferMin: 1
    poolMax: 2
    poolMin: 1
  recycleStrategy:
    type: Restart
"""
        _kubectl("apply", "-f", "-", input=pool_yaml)
        try:
            self._wait_pool_pods()
            yield
        finally:
            _kubectl("delete", "pool", POOL_NAME, "-n", E2E_NAMESPACE, "--ignore-not-found=true", timeout=120)

    def _wait_pool_pods(self, timeout: int = 180) -> list[str]:
        deadline = time.monotonic() + timeout
        pods: list[str] = []
        while time.monotonic() < deadline:
            pods = _kubectl(
                "get", "pods",
                "-n", E2E_NAMESPACE,
                "-l", f"sandbox.opensandbox.io/pool-name={POOL_NAME}",
                "-o", "jsonpath={.items[*].metadata.name}",
            ).split()
            running = self._pod_phase(pods)
            if pods and running:
                return pods
            time.sleep(3)
        pytest.fail(f"pool pods did not become Running: {pods}")

    def _pod_phase(self, pods: list[str]) -> bool:
        for name in pods:
            phase = _kubectl(
                "get", "pod", name, "-n", E2E_NAMESPACE,
                "-o", "jsonpath={.status.phase}",
            )
            if phase != "Running":
                return False
        return bool(pods)

    def _container_restart_count(self, pod: str) -> int:
        out = _kubectl(
            "get", "pod", pod, "-n", E2E_NAMESPACE,
            "-o", "jsonpath={.status.containerStatuses[0].restartCount}",
        )
        return int(out or 0)

    def test_pool_pod_runs_execd_as_pid1(self, pool) -> None:
        pod = self._wait_pool_pods()[0]
        comm = _kubectl(
            "exec", pod, "-n", E2E_NAMESPACE, "-c", "sandbox-container",
            "--", "cat", "/proc/1/comm",
        ).strip()
        assert comm == "execd", f"pool pod PID 1 = {comm}, want execd"

    def test_restart_recycle_restarts_container_via_kill1(self, pool) -> None:
        pod = self._wait_pool_pods()[0]
        before = self._container_restart_count(pod)

        # Allocate a BatchSandbox from the pool, then release it — the
        # controller runs the Restart recycle (pod exec `kill 1`).
        bs_yaml = f"""apiVersion: sandbox.opensandbox.io/v1alpha1
kind: BatchSandbox
metadata:
  name: {BS_NAME}
  namespace: {E2E_NAMESPACE}
spec:
  replicas: 1
  poolRef: {POOL_NAME}
"""
        _kubectl("apply", "-f", "-", input=bs_yaml)
        try:
            # Wait for allocation to land on the pool pod.
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                alloc = _kubectl(
                    "get", "batchsandbox", BS_NAME, "-n", E2E_NAMESPACE,
                    "-o", "jsonpath={.metadata.annotations.sandbox\\.opensandbox\\.io/alloc-status}",
                )
                if alloc:
                    break
                time.sleep(3)
            else:
                pytest.fail("BatchSandbox never allocated from the pool")

            _kubectl("delete", "batchsandbox", BS_NAME, "-n", E2E_NAMESPACE, timeout=120)

            # The recycle pod-execs `kill 1`: init-mode execd forwards SIGTERM
            # to the keepalive and exits, the kubelet restarts the container.
            deadline = time.monotonic() + 180
            while time.monotonic() < deadline:
                if self._container_restart_count(pod) > before:
                    break
                time.sleep(3)
            else:
                pytest.fail(
                    f"container restartCount did not increase after Restart "
                    f"recycle (before={before}, pod={pod})"
                )
            # Pod must survive the recycle and come back Running with execd
            # as PID 1 again — the pool stays usable.
            deadline = time.monotonic() + 120
            while time.monotonic() < deadline:
                if self._pod_phase([pod]) and self._container_restart_count(pod) > before:
                    break
                time.sleep(3)
            else:
                pytest.fail(f"pod {pod} did not return Running after recycle")
            comm = _kubectl(
                "exec", pod, "-n", E2E_NAMESPACE, "-c", "sandbox-container",
                "--", "cat", "/proc/1/comm",
            ).strip()
            assert comm == "execd", f"post-recycle PID 1 = {comm}, want execd"
        finally:
            _kubectl("delete", "batchsandbox", BS_NAME, "-n", E2E_NAMESPACE, "--ignore-not-found=true", timeout=120)
