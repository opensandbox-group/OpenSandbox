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
E2E tests for execd-as-init mode (OSEP-0018).

Requires a server running with ``runtime.execd_run_as_init = true`` (the
dedicated real-e2e and kubernetes-nightly jobs set it). Verifies the
container-level init contract through the SDK:

- execd is PID 1 and the workload's parent
- orphaned children are reaped (no zombie accumulation under PID 1)
- a fork-heavy workload keeps the process table bounded
- in-namespace ``kill -9 1`` is inert (kernel signal shield)
- application signals (HUP/USR1/USR2/WINCH) are forwarded to the entrypoint
- the entrypoint's exit code propagates to the container/runtime (docker
  bridge; on Kubernetes the test skips — BatchSandbox does not surface the
  container exit code or a terminal lifecycle state, OSEP-0018 R-l)
- an in-namespace ``kill 1`` (SIGTERM) still stops the sandbox — interim
  behavior pin for OSEP-0018 §3 (R-a: trusted out-of-band stop channel);
  on Kubernetes the pin asserts execd becomes unreachable instead of a
  lifecycle state transition
- the workload cannot read execd's environment (``/proc/1/environ`` denied by
  non-dumpable, independent of Landlock)
- ``GET /v1/isolated/capabilities`` reports ``hardening.init_mode = pid1``
  and ``hardening.signal_shield = true``
"""

import json
import logging
import time
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.sandboxes import SandboxImageSpec

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    is_kubernetes_runtime,
)

logger = logging.getLogger(__name__)

EXECD_CAPABILITIES_URL = "http://127.0.0.1:44772/v1/isolated/capabilities"


def _run_command(sandbox, command: str) -> str:
    """Run a command and return its combined stdout."""
    result = sandbox.commands.run(command, opts=RunCommandOpts())
    assert result.error is None, f"command failed: {result.error}"
    return "".join(msg.text for msg in result.logs.stdout)


def _zombie_count(sandbox) -> int:
    """Count processes in state Z whose parent is PID 1."""
    out = _run_command(
        sandbox,
        "z=0; for p in /proc/[0-9]*; do "
        "stat=$(cat \"$p/stat\" 2>/dev/null) || continue; "
        "stat=${stat#*)}; set -- $stat; "
        "[ \"$1\" = Z ] && [ \"$2\" = 1 ] && z=$((z+1)); done; echo $z",
    )
    return int(out.strip())


def _process_count(sandbox) -> int:
    return int(_run_command(sandbox, "ls -d /proc/[0-9]* | wc -l").strip())


def _create_sandbox(
    entrypoint: list[str] | None = None,
    tag: str = "execd-init-e2e",
    extensions: dict[str, str] | None = None,
    env: dict[str, str] | None = None,
    volumes: list | None = None,
) -> SandboxSync:
    connection_config = create_connection_config_sync()
    return SandboxSync.create(
        image=SandboxImageSpec(get_sandbox_image()),
        resource=get_e2e_sandbox_resource(),
        connection_config=connection_config,
        timeout=timedelta(minutes=5),
        ready_timeout=timedelta(seconds=60),
        entrypoint=entrypoint,
        extensions=extensions,
        env=env,
        volumes=volumes,
        metadata={"tag": tag},
    )


def _destroy(sandbox) -> None:
    try:
        sandbox.kill()
    except Exception as exc:  # noqa: BLE001
        logger.warning("Teardown: sandbox.kill() failed: %s", exc, exc_info=True)
    try:
        sandbox.close()
    except Exception as exc:  # noqa: BLE001
        logger.warning("Teardown: sandbox.close() failed: %s", exc, exc_info=True)


class TestExecdInitE2E:
    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(tag="execd-init-e2e")
        logger.info("✓ execd-init sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_pid1_is_execd(self, sandbox) -> None:
        assert _run_command(sandbox, "cat /proc/1/comm").strip() == "execd"

    def test_workload_is_direct_child_of_execd(self, sandbox) -> None:
        # /proc/$$/stat field 4 is the parent pid; the run-command shell is
        # a direct child of execd (PID 1).
        ppid = _run_command(sandbox, "awk '{print $4}' /proc/$$/stat").strip()
        assert ppid == "1", f"workload ppid = {ppid}, want 1"

    def test_orphans_are_reaped(self, sandbox) -> None:
        # Background children reparent to PID 1 and must be reaped by execd.
        _run_command(sandbox, "for i in $(seq 1 5); do ( sleep 0.1 ) & done")
        time.sleep(2)
        zombies = _zombie_count(sandbox)
        assert zombies == 0, f"zombies under pid 1: {zombies}"

    def test_kill9_pid1_is_inert(self, sandbox) -> None:
        assert "alive" in _run_command(sandbox, "kill -9 1; echo alive")

    def test_workload_cannot_read_execd_environ(self, sandbox) -> None:
        result = sandbox.commands.run("cat /proc/1/environ", opts=RunCommandOpts())
        assert result.error is not None, "reading execd's /proc/1/environ must be denied"
        stderr = "".join(msg.text for msg in result.logs.stderr)
        assert "Permission denied" in stderr or "Operation not permitted" in stderr

    @pytest.fixture(scope="module")
    def signal_sandbox(self):
        """Sandbox whose entrypoint traps HUP/USR1/USR2/WINCH — used to
        observe forwarding of the whole application-signal set."""
        sbx = _create_sandbox(
            entrypoint=[
                "sh",
                "-c",
                "trap 'echo got-hup >> /tmp/execd-hup.log' HUP; "
                "trap 'echo got-usr1 >> /tmp/execd-usr1.log' USR1; "
                "trap 'echo got-usr2 >> /tmp/execd-usr2.log' USR2; "
                "trap 'echo got-winch >> /tmp/execd-winch.log' WINCH; "
                "while :; do sleep 1; done",
            ],
            tag="execd-init-e2e-signal",
        )
        yield sbx
        _destroy(sbx)

    @pytest.fixture(scope="module")
    def kill1_sandbox(self):
        """Sandbox destroyed by its own in-namespace ``kill 1``."""
        sbx = _create_sandbox(tag="execd-init-e2e-kill1")
        yield sbx
        _destroy(sbx)

    def test_application_signals_forwarded_to_entrypoint(self, signal_sandbox) -> None:
        # In-namespace HUP/USR1/USR2/WINCH to PID 1 are delivered (execd
        # installs handlers, so the kernel signal shield does not apply) and
        # forwarded to the entrypoint process group. The /command shell runs
        # in its own process group and must not receive them.
        out = _run_command(
            signal_sandbox,
            "sleep 1; kill -HUP 1; kill -USR1 1; kill -USR2 1; kill -WINCH 1; "
            "sleep 2; cat /tmp/execd-hup.log /tmp/execd-usr1.log "
            "/tmp/execd-usr2.log /tmp/execd-winch.log",
        )
        for marker in ("got-hup", "got-usr1", "got-usr2", "got-winch"):
            assert marker in out, f"forwarded signal marker {marker} missing: {out}"

    def test_entrypoint_exit_code_propagates(self) -> None:
        # When the user entrypoint exits, execd exits with the same status so
        # Docker/kubelet observe it (OSEP-0018 §2 "entrypoint owns the
        # container lifecycle"). The sleep gives the sandbox time to become
        # ready before the entrypoint exits.
        if is_kubernetes_runtime():
            # BatchSandbox stays Pending after the pod completes and does not
            # surface the container exit code (OSEP-0018 R-l): the lifecycle
            # state-transition assertion below is docker-runtime-specific.
            pytest.skip(
                "BatchSandbox does not surface the container exit code "
                "(OSEP-0018 R-l)"
            )
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "sleep 20; exit 42"],
            tag="execd-init-e2e-exit42",
        )
        try:
            deadline = time.monotonic() + 75
            state = None
            while time.monotonic() < deadline:
                state = sbx.get_info().status.state
                if state in {"Failed", "Terminated"}:
                    break
                time.sleep(2)
            if state not in {"Failed", "Terminated"}:
                pytest.fail(f"entrypoint-exit sandbox stuck in state {state}")
            info = sbx.get_info()
            assert info.status.state == "Failed", info.status
            assert info.status.message and "exited with code 42" in info.status.message, (
                info.status.message
            )
        finally:
            _destroy(sbx)

    def test_in_namespace_sigterm_kill1_stops_sandbox(self, kill1_sandbox) -> None:
        # Interim-behavior pin (OSEP-0018 §3, R-a): today an in-namespace
        # `kill 1` SIGTERM reaches execd's forwarding loop and stops the
        # sandbox, matching the pre-OSEP bootstrap behavior. Once the trusted
        # out-of-band stop channel lands, execd must ignore in-namespace
        # SIGTERM and this test flips to asserting the sandbox stays Running.
        try:
            kill1_sandbox.commands.run(
                "kill 1; sleep 5; echo alive", opts=RunCommandOpts()
            )
        except Exception:  # noqa: BLE001  # the execd connection dies mid-stream
            pass
        if is_kubernetes_runtime():
            self._assert_execd_unreachable(kill1_sandbox)
            return
        deadline = time.monotonic() + 45
        state = None
        while time.monotonic() < deadline:
            state = kill1_sandbox.get_info().status.state
            if state in {"Failed", "Terminated"}:
                return
            time.sleep(2)
        pytest.fail(f"sandbox did not stop after in-namespace kill 1 (state={state})")

    def _assert_execd_unreachable(self, kill1_sandbox) -> None:
        """k8s leg of the kill-1 pin: the pod exits after ``kill 1`` but
        BatchSandbox stays Pending (it never transitions to a terminal
        lifecycle state — OSEP-0018 R-l), so assert the observable effect
        instead: execd is gone and the sandbox is unusable.
        """
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            try:
                kill1_sandbox.commands.run("echo alive", opts=RunCommandOpts())
            except Exception:  # noqa: BLE001  # execd unreachable -> sandbox stopped
                # Confirm on a fresh attempt so a single stale-pooled-
                # connection error (the kill-1 proxy stream dies mid-flight)
                # cannot false-pass while execd is still alive.
                time.sleep(1)
                try:
                    kill1_sandbox.commands.run("echo alive", opts=RunCommandOpts())
                except Exception:  # noqa: BLE001
                    return
                continue
            time.sleep(2)
        pytest.fail("execd still reachable after in-namespace kill 1")

    def test_fork_heavy_keeps_process_table_bounded(self, sandbox) -> None:
        # Sustained fork churn: short-lived background children (reparented
        # orphans) plus a few long sleepers. The reaper must keep the process
        # table bounded and zombie-free over many cycles.
        baseline = _process_count(sandbox)
        for _ in range(20):
            _run_command(sandbox, "for i in $(seq 1 5); do ( sleep 0.1 ) & done; sleep 0.3")
        _run_command(sandbox, "sleep 15 & sleep 15 & sleep 15 &")
        time.sleep(2)
        zombies = _zombie_count(sandbox)
        assert zombies == 0, f"zombies under pid 1: {zombies}"
        total = _process_count(sandbox)
        assert total <= baseline + 10, f"process table grew: {baseline} -> {total}"

    def test_hardening_reports_pid1(self, sandbox) -> None:
        # execd's /command SSE output strips newlines, so the probe emits a
        # single JSON object instead of multi-line prints.
        probe = (
            "python3 -c \"import json,urllib.request;"
            f"h=json.load(urllib.request.urlopen('{EXECD_CAPABILITIES_URL}'))['hardening'];"
            "print(json.dumps({'init_mode': h['init_mode'], 'signal_shield': h['signal_shield']}))\""
        )
        report = json.loads(_run_command(sandbox, probe))
        assert report["init_mode"] == "pid1", f"hardening.init_mode = {report['init_mode']}"
        assert report["signal_shield"] is True, (
            f"hardening.signal_shield = {report['signal_shield']}"
        )
