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
"""
E2E tests for the server-path hardening floor (execd-as-init, OSEP-0018).

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
running with ``[hardening]``/``[landlock]`` enabled. Classes (gating env
where a leg supplies it):

- TestHardeningE2E — the floor end to end: reduced entrypoint, /command
  and bash sessions (caps, bounding set, seccomp, NNP, unchanged UID),
  env strip, Landlock, capabilities endpoint
- TestHardeningDegradationE2E (OPENSANDBOX_HARDENING_DEGRADATION) —
  ceiling without CAP_SETPCAP: cap_drop degraded, bounding set untrimmed,
  the rest of the floor active
- TestIsolatedSessionHardeningE2E — bwrap sessions compose with the floor
- TestHardeningCustomPolicyE2E — [seccomp] deny override + ambient
  keep_capabilities (CapEff=0x2000)
- TestHardeningDriftE2E (OPENSANDBOX_HARDENING_DRIFT) — floor enabled with
  execd_run_as_init=false: layers degraded with EXECD_INIT guidance
- TestHardeningDefaultOffE2E (OPENSANDBOX_HARDENING_DEFAULT_OFF) — no
  TOML, no EXECD_INIT: every layer disabled, workload unaffected
- TestHardeningKeepCapsConflictE2E
  (OPENSANDBOX_HARDENING_KEEP_CAPS_CONFLICT) — kept cap outside the
  ceiling: the launch succeeds fully reduced
- TestHardeningDefaultCeilingE2E (OPENSANDBOX_HARDENING_DEFAULT_CEILING) —
  Docker's default seccomp profile: Landlock active, not unsupported
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

# The entrypoint strip list: credentials + lifecycle transport are stripped,
# the Jupyter/envs configuration image entrypoint scripts need is kept.
# /command uses the full COMMAND_ENV_STRIPPED blacklist.
ENTRYPOINT_BOOTSTRAP_ENV_STRIPPED = [
    "EXECD_ACCESS_TOKEN",
    "OPENSANDBOX_LIFECYCLE",
    "EXECD_LIFECYCLE_CONFIG",
    "EXECD_RUNTIME_INIT",
]
ENTRYPOINT_BOOTSTRAP_ENV_KEPT = ["JUPYTER_HOST", "JUPYTER_TOKEN", "EXECD_ENVS"]
COMMAND_ENV_STRIPPED = [
    "EXECD_ACCESS_TOKEN",
    "EXECD_ISOLATION_CONFIG",
    "JUPYTER_HOST",
    "JUPYTER_TOKEN",
    "EXECD_ENVS",
    "OPENSANDBOX_LIFECYCLE",
    "EXECD_LIFECYCLE_CONFIG",
    "EXECD_RUNTIME_INIT",
]

# Injected via request env so the strip assertions see real names (non-vacuous
# on both runtimes). Values are inert: this suite overrides the image
# entrypoint and bootstrap.sh only touches the EXECD_ENVS path. The lifecycle
# names are not injected — execd parses them at startup — their strip is
# pinned by the Go unit TestHardeningEnvStrip.
COMMAND_ENV_INJECTED = {
    "JUPYTER_HOST": "http://127.0.0.1:9",
    "JUPYTER_TOKEN": "dummy-jupyter-token",
    "EXECD_ENVS": "/tmp/.hardening-e2e-envs",
}

# Kubernetes: the hardened TOML travels in the opensandbox-e2e-execd-isolation
# ConfigMap, mounted by the e2e batchsandbox template at this path.
K8S_EXECD_ISOLATION_CONFIG = "/etc/opensandbox/execd-isolation/isolation.hardened.toml"

# Kubernetes: the custom-policy TOML (R-q) is a second key in the same
# ConfigMap, so it lands next to the hardened one without a template change.
K8S_EXECD_CUSTOM_CONFIG = "/etc/opensandbox/execd-isolation/isolation.custom.toml"

# Kubernetes: the e2e PVC is backed by a hostPath PV on the kind node. The PV
# used to live under the node's /tmp, which is a noexec tmpfs, so every mount
# of the PVC was not executable (writes/reads fine, exec EACCES regardless of
# Landlock). The e2e harness now places the PV on the node rootfs
# (/var/opensandbox-e2e, scripts/common/kubernetes-e2e.sh); the workspace is
# mounted at /mnt/workspace-exec, which sits in the allowed_writable Landlock
# set so the mount-expansion rule grants write+exec.
K8S_WORKSPACE_EXEC = "/mnt/workspace-exec"


def _hardened_sandbox_options() -> dict:
    """Point execd at the hardened TOML (docker: server-injected; k8s:
    per-request env into the template-mounted ConfigMap) and carry the
    COMMAND_ENV_INJECTED names; k8s also mounts the workspace PVC at
    K8S_WORKSPACE_EXEC to exercise Landlock's mount expansion."""
    env = dict(COMMAND_ENV_INJECTED)
    if not is_kubernetes_runtime():
        return {"env": env}
    env["EXECD_ISOLATION_CONFIG"] = K8S_EXECD_ISOLATION_CONFIG
    return {
        "env": env,
        "volumes": [
            Volume(
                name="hardening-workspace",
                pvc=PVC(claimName=get_test_pvc_name()),
                mountPath=K8S_WORKSPACE_EXEC,
            ),
        ],
    }


def _custom_policy_sandbox_options() -> dict:
    """Sandbox create kwargs that point execd at the custom-policy TOML
    (R-q). Docker: the phase-3 server config binds it at the default path, so
    no request args are needed. Kubernetes: point the request env at the
    second ConfigMap key (no workspace mount needed — no Landlock here).
    """
    if not is_kubernetes_runtime():
        return {}
    return {
        "env": {"EXECD_ISOLATION_CONFIG": K8S_EXECD_CUSTOM_CONFIG},
    }

_HARDENING_REPORT: dict[str, HardeningStatus] = {}


def _hardening_report(sandbox: SandboxSync, refresh: bool = False) -> HardeningStatus:
    """Probe execd's capabilities endpoint via the SDK model and cache per
    sandbox (multiple sandbox classes share one pytest invocation on k8s, so
    a process-global cache would leak one sandbox's report into another).
    ``refresh=True`` forces a live probe (used where the assertion must
    observe the endpoint AFTER a state change, e.g. session teardown).
    Consuming ``IsolatedCapabilities.hardening`` also pins the spec -> SDK
    -> implementation alignment of the hardening object."""
    if refresh or sandbox.id not in _HARDENING_REPORT:
        caps = sandbox.isolation.capabilities()
        if caps.hardening is None:
            pytest.fail("capabilities endpoint returned no hardening object")
        _HARDENING_REPORT[sandbox.id] = caps.hardening
    return _HARDENING_REPORT[sandbox.id]


def _landlock_state(sandbox: SandboxSync) -> str:
    report = _hardening_report(sandbox)
    assert report.landlock is not None
    return report.landlock.state


def _status_fields(sandbox: SandboxSync, fields: list[str]) -> dict:
    """Parse selected /proc/self/status fields of the /command shell.

    Two constraints: (1) the read happens with the shell's own read loop,
    NOT with forked helpers — under Landlock the ruleset only grants the
    launcher's own /proc/<pid> (a documented limitation), so a
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
        # ruleset does not grant (a documented limitation), so a
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
        # Same launcher prelude as the entrypoint; Uid asserts the floor
        # never relocates identity.
        status = _status_fields(
            sandbox, ["CapEff", "CapBnd", "Seccomp", "NoNewPrivs", "Uid"]
        )
        assert status["CapEff"] == "0000000000000000", status
        assert status["CapBnd"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status
        assert status["Uid"] == "0", status

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
        # Identity unchanged: the launcher drops privilege, not the UID.
        assert status["Uid"].split()[0] == "0", status
        for name in ENTRYPOINT_BOOTSTRAP_ENV_STRIPPED:
            assert name not in env, f"entrypoint env leaked {name}"
        # Injected names must survive here: pins the bootstrapEnv asymmetry.
        for name in ENTRYPOINT_BOOTSTRAP_ENV_KEPT:
            assert name in env, f"entrypoint env lost {name}"

    def test_bash_session_runs_through_floor(self, sandbox) -> None:
        # /session shares the /command launch primitive.
        sid = sandbox.commands.create_session()
        try:
            code = (
                "while IFS= read -r line; do "
                "case \"$line\" in "
                "CapEff:*) echo \"capeff=${line#*:\t}\" ;; "
                "Seccomp:*) echo \"sec=${line#*:\t}\" ;; "
                "NoNewPrivs:*) echo \"nnp=${line#*:\t}\" ;; "
                "esac; done < /proc/self/status"
            )
            result = sandbox.commands.run_in_session(sid, code)
            assert result.error is None, result.error
            text = "".join(msg.text for msg in result.logs.stdout)
            assert "capeff=0000000000000000" in text, text
            assert "sec=2" in text, text
            assert "nnp=1" in text, text
        finally:
            sandbox.commands.delete_session(sid)

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
            # helpers get EACCES on their own procfs (a documented
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
    reason="requires the degradation server/leg (CAP_SETPCAP dropped from the "
    "ceiling: docker phase 2 drops it via drop_capabilities, the k8s "
    "caps-drop leg drops it from the pod template)",
)
class TestHardeningDegradationE2E:
    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(
            tag="execd-hardening-degradation-e2e", **_hardened_sandbox_options()
        )
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


class TestHardeningCustomPolicyE2E:
    """Custom `[seccomp] deny` override + `keep_capabilities`.

    The sandbox runs with configs/isolation.custom.toml: hardening enabled,
    a `[seccomp] deny` override (chmod/fchmodat/fchmodat2 — REPLACES the
    built-in denylist) and keep_capabilities=["CAP_NET_RAW"]. Proves:

    - the custom deny list reaches the workload: the denied syscall fails
      with EACCES in /command
    - keep_capabilities are raised in the ambient set and survive execve:
      /command shows CapEff=0000000000002000 (CAP_NET_RAW = bit 13) with the
      bounding set trimmed to the kept caps
    - the capabilities endpoint reports the overrides as active
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        # Keep the entrypoint trivial: it also runs through the launcher with
        # the same policy, and the deny list (chmod family) must not break it.
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "while :; do sleep 1; done"],
            tag="execd-hardening-custom-policy-e2e",
            **_custom_policy_sandbox_options(),
        )
        logger.info("✓ custom-policy sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_capabilities_endpoint_reports_custom_policy(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        assert report.cap_drop is not None and report.cap_drop.state == "active", (
            report.cap_drop
        )
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )
        # The custom TOML has no [landlock] section: it must stay disabled,
        # not be dragged into the active state.
        assert report.landlock is not None and report.landlock.state == "disabled", (
            report.landlock
        )

    def test_denied_syscall_fails_in_command(self, sandbox) -> None:
        # The custom deny list replaces the built-in one; chmod family must
        # fail with EACCES while the shell itself keeps running.
        result = sandbox.commands.run(
            "echo x > /tmp/rq-probe && chmod 755 /tmp/rq-probe"
        )
        assert result.error is not None, "chmod must be denied by the custom deny list"
        stderr = "".join(msg.text for msg in result.logs.stderr)
        assert "Permission denied" in stderr or "Operation not permitted" in stderr, stderr

    def test_kept_capability_survives_execve(self, sandbox) -> None:
        # CAP_NET_RAW (bit 13) raised in the ambient set: CapEff=0x2000 in
        # /command, and the bounding set is trimmed to exactly the kept caps.
        status = _status_fields(sandbox, ["CapEff", "CapBnd", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000002000", status
        assert status["CapBnd"] == "0000000000002000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status

    def test_other_syscalls_still_work(self, sandbox) -> None:
        # The custom deny list is a deny-override, not a lockdown: non-denied
        # syscalls keep working in the same session.
        out = _run_command(sandbox, "echo custom-deny-ok")
        assert "custom-deny-ok" in out, out


@pytest.mark.skipif(
    os.environ.get("OPENSANDBOX_HARDENING_DRIFT") != "true",
    reason="requires the drift server/leg (hardened TOML with "
    "execd_run_as_init = false): docker phase 4, or the k8s execd-init leg "
    "run with E2E_EXECD_RUN_AS_INIT=false",
)
class TestHardeningDriftE2E:
    """EXECD_INIT <-> TOML drift pin.

    `[hardening] enabled` with `runtime.execd_run_as_init = false`: the
    hardening endpoint must report init_mode=none and degrade the enabled
    layers with EXECD_INIT guidance (the image entrypoint is not wrapped),
    while execd-spawned /command still runs through the floor.
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "while :; do sleep 1; done"],
            tag="execd-hardening-drift-e2e",
            **_hardened_sandbox_options(),
        )
        logger.info("✓ drift sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_init_mode_none_and_layers_degraded(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "none", f"init_mode = {report.init_mode}"
        assert report.signal_shield is False
        for layer, name in (
            (report.cap_drop, "cap_drop"),
            (report.seccomp, "seccomp"),
            (report.landlock, "landlock"),
        ):
            assert layer is not None, name
            assert layer.state == "degraded", f"{name} state = {layer.state}"
            assert layer.message is not None and "EXECD_INIT" in layer.message, (
                layer.message
            )
        assert report.ebpf is not None and report.ebpf.state == "disabled", (
            report.ebpf
        )

    def test_command_still_runs_through_floor(self, sandbox) -> None:
        # Fail-open but honest: without init mode the image entrypoint is not
        # wrapped, but execd-spawned /command still goes through the launcher
        # — the endpoint says degraded rather than pretending full coverage.
        status = _status_fields(sandbox, ["CapEff", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status


@pytest.mark.skipif(
    os.environ.get("OPENSANDBOX_HARDENING_DEFAULT_OFF") != "true",
    reason="requires the plain server/leg (no isolation TOML, "
    "execd_run_as_init = false): docker phase 5, or the k8s execd-init leg "
    "run with E2E_EXECD_RUN_AS_INIT=false",
)
class TestHardeningDefaultOffE2E:
    """Default-off pin.

    With no isolation TOML and no EXECD_INIT, execd must be exactly the
    pre-OSEP binary: the capabilities endpoint reports init_mode=none with
    every layer disabled (no drift, no fabricated states), and the workload
    is unaffected — /command runs with the container ceiling caps and no
    seccomp floor from the launcher.
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "while :; do sleep 1; done"],
            tag="execd-hardening-default-off-e2e",
        )
        logger.info("✓ default-off sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_all_layers_disabled_without_config(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "none", f"init_mode = {report.init_mode}"
        assert report.signal_shield is False
        for layer, name in (
            (report.cap_drop, "cap_drop"),
            (report.seccomp, "seccomp"),
            (report.landlock, "landlock"),
            (report.ebpf, "ebpf"),
        ):
            assert layer is not None, name
            assert layer.state == "disabled", f"{name} state = {layer.state}"
            assert layer.message is not None, f"{name} message missing"

    def test_workload_unaffected_without_hardening(self, sandbox) -> None:
        # No launcher floor. Whether the ceiling carries a seccomp profile is
        # a node concern (docker: none; k8s: containerd-dependent); the
        # invariant is "no launcher-installed filter": Seccomp != 2.
        status = _status_fields(sandbox, ["CapEff", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] != "0000000000000000", status
        assert status["Seccomp"] != "2", status
        assert status["NoNewPrivs"] == "0", status


@pytest.mark.skipif(
    os.environ.get("OPENSANDBOX_HARDENING_KEEP_CAPS_CONFLICT") != "true",
    reason="requires a leg whose ceiling lacks CAP_NET_RAW while the TOML "
    "keeps it: docker phase 3b (phase-1 drop_capabilities) or the k8s "
    "caps-drop leg (pod template capabilities.drop)",
)
class TestHardeningKeepCapsConflictE2E:
    """The custom TOML keeps CAP_NET_RAW, but this leg's ceiling dropped it:
    the launch must still succeed fully reduced (zero caps, seccomp, NNP,
    deny list enforced) instead of failing or granting partially.
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "while :; do sleep 1; done"],
            tag="execd-hardening-keepcaps-conflict-e2e",
            **_custom_policy_sandbox_options(),
        )
        logger.info("✓ keep-caps-conflict sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_endpoint_reports_floor_active(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        # cap_drop is active where the ceiling holds CAP_SETPCAP (docker
        # phase 3b) and degraded where it does not (k8s caps-drop leg).
        assert report.cap_drop is not None and report.cap_drop.state in (
            "active",
            "degraded",
        ), report.cap_drop
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )
        assert report.landlock is not None and report.landlock.state == "disabled", (
            report.landlock
        )

    def test_kept_cap_not_granted_and_floor_intact(self, sandbox) -> None:
        # Keep is per-cap best-effort: the unusable cap is dropped, not the
        # whole reduction.
        status = _status_fields(sandbox, ["CapEff", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status

    def test_custom_deny_still_enforced(self, sandbox) -> None:
        result = sandbox.commands.run(
            "echo x > /tmp/conflict-probe && chmod 755 /tmp/conflict-probe"
        )
        assert result.error is not None, "chmod must be denied by the custom deny list"
        stderr = "".join(msg.text for msg in result.logs.stderr)
        assert "Permission denied" in stderr or "Operation not permitted" in stderr, stderr


@pytest.mark.skipif(
    os.environ.get("OPENSANDBOX_HARDENING_DEFAULT_CEILING") != "true",
    reason="requires the production-default container ceiling (docker phase "
    "6: server leaves no_new_privileges at its default and no seccomp "
    "profile override, so the sandbox runs Docker's default profile)",
)
class TestHardeningDefaultCeilingE2E:
    """The hardened TOML against Docker's default seccomp profile (the other
    legs unset the container baseline): the launcher's own syscalls —
    including Landlock — must pass, so landlock reports active.
    """

    @pytest.fixture(scope="module", autouse=True)
    def sandbox(self):
        sbx = _create_sandbox(
            entrypoint=["sh", "-c", "while :; do sleep 1; done"],
            tag="execd-hardening-default-ceiling-e2e",
            **_hardened_sandbox_options(),
        )
        logger.info("✓ default-ceiling sandbox created: %s", sbx.id)
        yield sbx
        _destroy(sbx)

    def test_landlock_active_under_default_profile(self, sandbox) -> None:
        report = _hardening_report(sandbox)
        assert report.init_mode == "pid1", f"init_mode = {report.init_mode}"
        assert report.cap_drop is not None and report.cap_drop.state == "active", (
            report.cap_drop
        )
        assert report.seccomp is not None and report.seccomp.state == "active", (
            report.seccomp
        )
        # The point of this leg: Landlock syscalls pass the container's
        # default seccomp profile.
        assert report.landlock is not None and report.landlock.state == "active", (
            f"landlock must be active under the container default profile, "
            f"got: {report.landlock}"
        )

    def test_floor_applies_normally(self, sandbox) -> None:
        status = _status_fields(sandbox, ["CapEff", "Seccomp", "NoNewPrivs"])
        assert status["CapEff"] == "0000000000000000", status
        assert status["Seccomp"] == "2", status
        assert status["NoNewPrivs"] == "1", status

    def test_landlock_enforcement(self, sandbox) -> None:
        _run_command(sandbox, "echo ok > /tmp/default-ceiling-write && rm /tmp/default-ceiling-write")
        result = sandbox.commands.run("echo x >> /etc/passwd")
        assert result.error is not None, "writing /etc/passwd must be denied by landlock"


class TestIsolatedSessionHardeningE2E:
    """bwrap isolated sessions under init mode + the hardening floor.

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
        # The hardened options carry the TOML pointer: kubernetes delivers it
        # per-request, so omitting them would silently disable the floor on
        # the k8s leg.
        sbx = _create_sandbox(
            extensions={"bootstrap.execd.isolation": "enable"},
            tag="execd-hardening-isolated-e2e",
            **_hardened_sandbox_options(),
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
