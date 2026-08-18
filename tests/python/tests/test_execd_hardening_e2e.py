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
E2E tests for the server-path hardening floor (OSEP-0018 R-i).

Requires a server running with ``runtime.execd_run_as_init = true``. Docker
bridge (scripts/python-execd-hardening-e2e.sh) injects the hardened TOML via
server config::

    [docker]
    sandbox_env = { EXECD_ISOLATION_CONFIG = "/etc/opensandbox/isolation.toml" }
    sandbox_binds = [
        "/tmp/opensandbox-e2e/workspace:/workspace",
        "/tmp/opensandbox-e2e/isolation.hardened.toml:/etc/opensandbox/isolation.toml",
    ]

Kubernetes (scripts/python-k8s-execd-init-e2e.sh) delivers the TOML in a
ConfigMap mounted by the e2e batchsandbox template and points execd at it per
request (``EXECD_ISOLATION_CONFIG`` env + the workspace PVC mounted at
/mnt/workspace-exec; /workspace itself is a runtime-provided noexec tmpfs on
k8s).

Verifies through the SDK, with the whole server -> sandbox -> execd path
running with ``[hardening]``/``[landlock]`` enabled:

- every execd-spawned path (entrypoint + /command) runs reduced:
  no effective caps, bounding set trimmed, seccomp filter mode, no_new_privs
- execd's config env is stripped from the workload (EXECD_ISOLATION_CONFIG
  and EXECD_ACCESS_TOKEN absent from /command; EXECD_ACCESS_TOKEN absent
  from the entrypoint)
- Landlock confinement: /tmp writable, /etc/passwd not writable, the
  bind-mounted workspace writable AND executable (exercises the launcher's
  mount expansion), /proc/1/environ denied
- GET /v1/isolated/capabilities reports init_mode=pid1 with
  cap_drop/seccomp active and landlock active|unsupported

A second class covers the fail-open degradation with CAP_SETPCAP dropped
from the container ceiling (run by the script's phase 2, gated on
OPENSANDBOX_HARDENING_DEGRADATION=true):

- cap_drop reports degraded with a concrete reason; seccomp/landlock stay
  active
- the floor still applies (CapEff=0, seccomp, NNP) but the bounding set is
  NOT trimmed (fail-open: workloads keep the container ceiling's bounding
  set)

A third class covers bwrap isolated sessions under init mode + the floor
(OSEP-0018 R-o): sessions run with the bwrap namespace + seccomp/NNP floor
and the credential env strip, and the hardening report stays intact around
session create/run/delete.
"""

import logging
import os
import time

import pytest
from opensandbox import SandboxSync
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest,
    HardeningStatus,
    IsolatedWorkspaceSpec,
)
from opensandbox.models.sandboxes import PVC, Volume

from tests.base_e2e_test import (
    get_test_pvc_name,
    is_kubernetes_runtime,
)
from tests.test_execd_init_e2e import (
    _create_sandbox,
    _destroy,
    _run_command,
)

logger = logging.getLogger(__name__)

WORKSPACE_HOST = os.environ.get(
    "OPENSANDBOX_HARDENING_WORKSPACE_HOST", "/tmp/opensandbox-e2e/workspace"
)

# The entrypoint is launched with bootstrapEnv: EXECD_ACCESS_TOKEN is
# stripped, everything else (incl. EXECD_ISOLATION_CONFIG) is kept for
# image entrypoint scripts. /command uses the full config blacklist.
ENTRYPOINT_BOOTSTRAP_ENV_STRIPPED = ["EXECD_ACCESS_TOKEN"]
COMMAND_ENV_STRIPPED = [
    "EXECD_ACCESS_TOKEN",
    "EXECD_ISOLATION_CONFIG",
    "JUPYTER_HOST",
    "JUPYTER_TOKEN",
    "EXECD_ENVS",
]

# Kubernetes: the hardened TOML travels in the opensandbox-e2e-execd-isolation
# ConfigMap, mounted by the e2e batchsandbox template at this path.
K8S_EXECD_ISOLATION_CONFIG = "/etc/opensandbox/execd-isolation/isolation.hardened.toml"

# Kubernetes: the e2e PVC is backed by a hostPath PV on the kind node. The PV
# used to live under the node's /tmp, which is a noexec tmpfs, so every mount
# of the PVC was not executable (writes/reads fine, exec EACCES regardless of
# Landlock). The e2e harness now places the PV on the node rootfs
# (/var/opensandbox-e2e, scripts/common/kubernetes-e2e.sh); the workspace is
# mounted at /mnt/workspace-exec, which sits in the allowed_writable Landlock
# set so the mount-expansion rule grants write+exec.
K8S_WORKSPACE_EXEC = "/mnt/workspace-exec"


def _hardened_sandbox_options() -> dict:
    """Sandbox create kwargs that point execd at the hardened TOML.

    Docker: the server injects EXECD_ISOLATION_CONFIG via [docker] sandbox_env
    (config-level bind mount of isolation.hardened.toml at
    /etc/opensandbox/isolation.toml) — no request args needed. Kubernetes: the
    TOML arrives via the template-mounted ConfigMap, so the request env points
    execd at it; the workspace PVC is mounted at /mnt/workspace-exec so the
    Landlock bind-mount expansion is exercised like the docker bind mount
    (/workspace itself is a runtime-provided noexec tmpfs on k8s).
    """
    if not is_kubernetes_runtime():
        return {}
    return {
        "env": {"EXECD_ISOLATION_CONFIG": K8S_EXECD_ISOLATION_CONFIG},
        "volumes": [
            Volume(
                name="hardening-workspace",
                pvc=PVC(claimName=get_test_pvc_name()),
                mountPath=K8S_WORKSPACE_EXEC,
            ),
        ],
    }

_HARDENING_REPORT: HardeningStatus | None = None


def _hardening_report(sandbox: SandboxSync, refresh: bool = False) -> HardeningStatus:
    """Probe execd's capabilities endpoint via the SDK model and cache.
    ``refresh=True`` forces a live probe (used where the assertion must
    observe the endpoint AFTER a state change, e.g. session teardown).
    Consuming ``IsolatedCapabilities.hardening`` also pins the spec -> SDK
    -> implementation alignment of the hardening object (OSEP-0018 R-r)."""
    global _HARDENING_REPORT
    if _HARDENING_REPORT is None or refresh:
        caps = sandbox.isolation.capabilities()
        if caps.hardening is None:
            pytest.fail("capabilities endpoint returned no hardening object")
        _HARDENING_REPORT = caps.hardening
    return _HARDENING_REPORT


def _landlock_state(sandbox: SandboxSync) -> str:
    report = _hardening_report(sandbox)
    assert report.landlock is not None
    return report.landlock.state


def _status_fields(sandbox: SandboxSync, fields: list[str]) -> dict:
    """Parse selected /proc/self/status fields of the /command shell.

    Two constraints: (1) the read happens with the shell's own read loop,
    NOT with forked helpers — under Landlock the ruleset only grants the
    launcher's own /proc/<pid> (documented OSEP-0018 limitation), so a
    forked grep/cat would get EACCES on its own /proc/self; (2) execd's
    /command SSE output strips newlines, so the values must come out as a
    single line (space-separated key=value pairs).
    """
    arms = "\n".join(
        f'      {name}:*) {name}="${{line#*:\t}}" ;;' for name in fields
    )
    echo = " ".join(f'"{n}=${n}"' for n in fields)
    script = (
        'while IFS= read -r line; do\n'
        '  case "$line" in\n'
        f"{arms}\n"
        '  esac\n'
        'done < /proc/self/status\n'
        f'echo {echo}'
    )
    out = _run_command(sandbox, script)
    parsed: dict[str, str] = {}
    for token in out.split():
        key, _, value = token.partition("=")
        parsed[key.strip()] = value.strip()
    return parsed


def _read_entrypoint_dump(sbx: SandboxSync) -> str:
    """Fetch the entrypoint's status + env dump written to /workspace.

    Docker: read the host side of the bind mount. Kubernetes: the dump lands
    in the workspace PVC (mounted at /workspace), which is not host-readable
    from the runner — read it back through the SDK files API instead.
    """
    path = f"/workspace/state-{sbx.id}.txt"
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        if not is_kubernetes_runtime():
            host_path = os.path.join(WORKSPACE_HOST, f"state-{sbx.id}.txt")
            if os.path.exists(host_path):
                with open(host_path, encoding="utf-8") as f:
                    return f.read()
        else:
            try:
                return sbx.files.read_file(path)
            except Exception:  # noqa: BLE001  # file may not be flushed yet
                pass
        time.sleep(1)
    pytest.fail(f"entrypoint dump {path} never appeared (runtime kubernetes={is_kubernetes_runtime()})")


def _parse_entrypoint_dump(content: str) -> tuple[dict, dict]:
    """Split the entrypoint dump into status fields and an env dict."""
    status_part, _, env_part = content.partition("=== env ===")
    status: dict[str, str] = {}
    for line in status_part.splitlines():
        key, _, value = line.partition(":")
        status[key.strip()] = value.strip()
    env = dict(line.split("=", 1) for line in env_part.splitlines() if "=" in line)
    return status, env


class TestHardeningE2E:
    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        # Entrypoint: dump its own /proc/self/status + env to the
        # bind-mounted workspace (host-readable), then stay alive. The dump
        # is written once at startup, before any /command churn, so it
        # reflects exactly the launcher-applied floor. The status is read
        # with the shell's own loop, NOT `cat`: under Landlock a forked
        # descendant resolves its own /proc/<pid>, which the inherited
        # ruleset does not grant (documented OSEP-0018 limitation), so a
        # forked helper would get EACCES.
        sbx = _create_sandbox(
            entrypoint=[
                "sh",
                "-c",
                "out=/workspace/state-$OPENSANDBOX_ID.txt; "
                "{ echo '=== status ==='; "
                "while IFS= read -r line; do echo \"$line\"; done < /proc/self/status; "
                "echo '=== env ==='; env | sort; } > \"$out\" 2>&1; "
                "while :; do sleep 1; done",
            ],
            tag="execd-hardening-e2e",
            **_hardened_sandbox_options(),
        )
        logger.info("✓ hardening sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_capabilities_endpoint_reports_hardening(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        assert report.signal_shield is True
        assert report.cap_drop is not None and report.cap_drop.state == "active", (
            report.cap_drop
        )
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )
        assert report.landlock is not None
        assert report.landlock.state in ("active", "unsupported"), report.landlock
        assert report.ebpf is not None and report.ebpf.state == "disabled", report.ebpf
        logger.info(
            "hardening report: init_mode=%s cap_drop=%s seccomp=%s landlock=%s",
            report.init_mode,
            report.cap_drop,
            report.seccomp,
            report.landlock,
        )

    def test_command_path_is_reduced(self, sandbox) -> None:
        # /command children go through the same launcher prelude as the
        # entrypoint: zero effective caps, bounding set trimmed, seccomp
        # filter mode, no_new_privs.
        status = _status_fields(sandbox, ["CapEff", "CapBnd", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["CapBnd"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status

    def test_command_path_strips_execd_config_env(self, sandbox) -> None:
        env = _run_command(sandbox, "env")
        for name in COMMAND_ENV_STRIPPED:
            assert f"{name}=" not in env, f"/command env leaked {name}"

    def test_entrypoint_is_reduced_and_env_stripped(self, sandbox) -> None:
        status, env = _parse_entrypoint_dump(_read_entrypoint_dump(sandbox))
        assert status["CapEff"] == "0000000000000000", status
        assert status["CapBnd"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status
        for name in ENTRYPOINT_BOOTSTRAP_ENV_STRIPPED:
            assert name not in env, f"entrypoint env leaked {name}"

    def test_workload_cannot_read_execd_environ(self, sandbox) -> None:
        # PR_SET_DUMPABLE shield: same-uid workload without CAP_SYS_PTRACE
        # cannot read execd's environment even before Landlock.
        result = sandbox.commands.run("cat /proc/1/environ")
        assert result.error is not None, "reading execd's /proc/1/environ must be denied"
        stderr = "".join(msg.text for msg in result.logs.stderr)
        assert "Permission denied" in stderr or "Operation not permitted" in stderr

    def test_tmp_is_writable(self, sandbox) -> None:
        if _landlock_state(sandbox) != "active":
            pytest.skip("landlock not active on this kernel")
        _run_command(sandbox, "echo ok > /tmp/hardening-e2e-write && rm /tmp/hardening-e2e-write")

    def test_etc_passwd_is_not_writable(self, sandbox) -> None:
        if _landlock_state(sandbox) != "active":
            pytest.skip("landlock not active on this kernel")
        result = sandbox.commands.run("echo x >> /etc/passwd")
        assert result.error is not None, "writing /etc/passwd must be denied by landlock"

    def test_workspace_bind_mount_writable_and_executable(self, sandbox) -> None:
        # The workspace is a separate mount from /: executing a script from it
        # exercises the launcher's mount expansion (the grants beneath the
        # mount point must be merged onto it), and writing to it exercises
        # the workspace read/write rule. On k8s the exec workspace is
        # /mnt/workspace-exec (the PVC); the e2e PV lives on the node rootfs
        # since the previous hostPath location (/tmp) was a noexec tmpfs.
        if _landlock_state(sandbox) != "active":
            pytest.skip("landlock not active on this kernel")
        workspace = K8S_WORKSPACE_EXEC if is_kubernetes_runtime() else "/workspace"
        script = f"{workspace}/hardening-e2e.sh"
        if is_kubernetes_runtime():
            # k8s regression probe: confirm the PVC mount is really present
            # and executable. /proc must be read with the shell's own loop:
            # the Landlock /proc/self rule pins the launcher's pid, so forked
            # helpers get EACCES on their own procfs (documented OSEP-0018
            # limitation).
            diag = _run_command(
                sandbox,
                f"id; "
                f"printf '#!/bin/sh\\necho workspace-exec-ok\\n' > {script}"
                f" && chmod +x {script}; "
                "while IFS= read -r line; do case \"$line\" in "
                f"*workspace-exec*) echo \"$line\" ;; esac; "
                "done < /proc/self/mounts; "
                f"stat -c '%A %a %U:%G %n' {script}",
            )
            logger.info("workspace exec diagnostics:\n%s", diag)
        _run_command(
            sandbox,
            f"printf '#!/bin/sh\\necho workspace-exec-ok\\n' > {script}"
            f" && chmod +x {script} && {script}",
        )


@pytest.mark.skipif(
    os.environ.get("OPENSANDBOX_HARDENING_DEGRADATION") != "true",
    reason="requires the degradation server (CAP_SETPCAP dropped); run via "
    "scripts/python-execd-hardening-e2e.sh phase 2",
)
@pytest.mark.skipif(
    is_kubernetes_runtime(),
    reason="CAP_SETPCAP ceiling degradation is docker-only (kubernetes "
    "securityContext caps are not tuned in the k8s e2e)",
)
class TestHardeningDegradationE2E:
    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(tag="execd-hardening-degradation-e2e")
        logger.info("✓ degradation sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_cap_drop_reports_degraded_with_reason(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        assert report.cap_drop is not None
        cap_drop = report.cap_drop
        assert cap_drop.state == "degraded", cap_drop
        assert cap_drop.message is not None and "SETPCAP" in cap_drop.message, cap_drop
        # The remaining layers must not cascade: fail-open is per layer.
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )
        assert report.landlock is not None
        assert report.landlock.state in ("active", "unsupported"), report.landlock

    def test_floor_still_applies_without_setpcap(self, sandbox) -> None:
        # Bounding-set trim is skipped without CAP_SETPCAP, but capset (drop
        # own caps), seccomp and NNP still apply — the workload is reduced
        # even in the degraded state.
        status = _status_fields(sandbox, ["CapEff", "CapBnd", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status
        # Fail-open: the bounding set keeps the container ceiling caps.
        assert status["CapBnd"] != "0000000000000000", status


class TestIsolatedSessionHardeningE2E:
    """bwrap isolated sessions under init mode + the hardening floor
    (OSEP-0018 R-o).

    The sandbox runs with ``[hardening]``/``[landlock]`` enabled and execd as
    PID 1, so the whole server -> sandbox -> execd -> launcher -> bwrap chain
    is active. bwrap itself is launcher-exempt (``withoutHardening``: its
    workload is already reduced by bwrap's own seccomp + namespaces), so this
    pins that the isolated-session path composes with the floor: sessions
    run, their workload carries bwrap's seccomp/NNP floor and the credential
    env strip, and the sandbox-level hardening report stays intact around
    session create/run/delete.
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        # The isolation extension grants the container ceiling CAP_SYS_ADMIN
        # (bwrap needs it to build namespaces); the floor still applies to
        # every user-code child, and bwrap remains launcher-exempt.
        sbx = _create_sandbox(
            extensions={"bootstrap.execd.isolation": "enable"},
            tag="execd-hardening-isolated-e2e",
        )
        logger.info("✓ hardening+isolated sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def _create_session(self, sandbox):
        return sandbox.isolation.create(
            CreateIsolatedSessionRequest(
                workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
            )
        )

    def test_capabilities_available_with_hardening(self, sandbox) -> None:
        caps = sandbox.isolation.capabilities()
        assert caps.available, caps.message
        assert caps.isolator == "bwrap"
        # The floor must not be disturbed by the isolation extension or by
        # probing bwrap inside the hardened sandbox.
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        assert report.cap_drop is not None and report.cap_drop.state == "active", (
            report.cap_drop
        )
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )

    def test_isolated_session_workload_has_floor(self, sandbox) -> None:
        # Inside the bwrap namespace the workload carries bwrap's own floor:
        # the shared seccomp denylist in filter mode, no_new_privs, and the
        # credential env strip (bwrap --unsetenv blacklist). Read the fields
        # with the session shell's own loop so no forked helper is involved.
        session = self._create_session(sandbox)
        try:
            code = (
                "while IFS= read -r line; do "
                "case \"$line\" in "
                "Seccomp:*) echo \"sec=${line#*:\t}\" ;; "
                "NoNewPrivs:*) echo \"nnp=${line#*:\t}\" ;; "
                "esac; done < /proc/self/status\n"
                "if env | grep -q '^EXECD_ACCESS_TOKEN='; then "
                "echo token_leaked; else echo token_stripped; fi"
            )
            result = session.run(code)
            assert "sec=2" in result.text, result.text
            assert "nnp=1" in result.text, result.text
            assert "token_stripped" in result.text, result.text
        finally:
            session.delete()

    def test_isolated_session_pid_isolation(self, sandbox) -> None:
        session = self._create_session(sandbox)
        try:
            result = session.run("echo $$")
            pid = int(result.text.strip())
            assert pid <= 2, f"session pid = {pid}, want PID 1 or 2 in the namespace"
        finally:
            session.delete()

    def test_isolated_session_state_persists(self, sandbox) -> None:
        session = self._create_session(sandbox)
        try:
            session.run("export PERSIST_HARDENED=abc123")
            result = session.run("echo $PERSIST_HARDENED")
            assert "abc123" in result.text, result.text
        finally:
            session.delete()

    def test_session_delete_while_workload_busy(self, sandbox) -> None:
        # A backgrounded workload inside the session must be torn down with
        # the session under reaper dispatch; the sandbox stays healthy and
        # the floor stays active afterwards.
        session = self._create_session(sandbox)
        session.run("sleep 30 &")
        session.delete()

        # Fresh probe: the process-global cache was populated by earlier
        # tests, so a cached report would make this assertion vacuous.
        report = _hardening_report(sandbox, refresh=True)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        status = _status_fields(sandbox, ["CapEff", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status
