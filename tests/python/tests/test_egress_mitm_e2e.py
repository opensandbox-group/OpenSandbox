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
"""Egress x transparent-MITM E2E (docker runtime, dns+nft).

The MITM REDIRECT rewrites intercepted dports to the mitmproxy listener
before the nft chain runs; without an exemption the rewritten port hits the
port floor or the default-deny tail and blackholes allowed HTTPS.

- Scenario A: reachable.ports.cap = 80/443 + vault + default-allow — HTTPS
  works and non-whitelisted ports stay blocked (issue #2423).
- Scenario B: no floor + allow-443/deny-all + vault — HTTPS works.

The floor file is docker-exec'd into the sidecar and picked up by the
~1-minute always-rules reload the tests poll for. Skips without
OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IP (set by scripts/egress-mitm-e2e-target.sh).
"""

from __future__ import annotations

import os
import subprocess
import time
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.models.sandboxes import (
    CredentialProxyConfig,
    NetworkPolicy,
    NetworkRule,
    SandboxImageSpec,
)

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    is_kubernetes_runtime,
)

TARGET_IP = os.getenv("OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IP", "")
E2E_LABEL_KEY = os.getenv(
    "OPENSANDBOX_EGRESS_MITM_E2E_LABEL_KEY", "opensandbox.e2e"
)
E2E_LABEL_VALUE = os.getenv(
    "OPENSANDBOX_EGRESS_MITM_E2E_LABEL_VALUE", "egress-mitm"
)
# Not 80/443: neither MITM-intercepted nor whitelisted by the floor.
PROBE_PORT = 8080
# Always-rules reload runs at most once per minute.
FLOOR_RELOAD_TIMEOUT_S = 150


@pytest.fixture(scope="module")
def mitm_target_ip() -> str:
    if not TARGET_IP:
        pytest.skip("Set OPENSANDBOX_EGRESS_MITM_E2E_TARGET_IP to run this E2E")
    if is_kubernetes_runtime():
        pytest.skip("floor injection uses docker exec into the egress sidecar")
    return TARGET_IP


@pytest.mark.timeout(420)
def test_port_floor_with_vault_keeps_https_working(
    mitm_target_ip: str,
) -> None:
    """Issue #2423: the cap whitelist (80/443) must not blackhole MITM'd HTTPS.

    The REDIRECT-rewritten dport lands in the floor complement, so pre-fix
    every intercepted flow timed out under a default-allow policy.
    """
    cfg, sandbox = _create_sandbox(
        network_policy=NetworkPolicy(defaultAction="allow"),
    )
    try:
        _write_reachable_ports_cap(sandbox.id, ["80", "443"])

        # Precondition: probe target reachable before the floor lands.
        assert _wait_until_tcp(
            sandbox, mitm_target_ip, PROBE_PORT, expect_connected=True, timeout_s=60
        ), f"probe target {mitm_target_ip}:{PROBE_PORT} unreachable even before the floor"

        # Floor takes effect via the ~1-minute always-rules reload.
        assert _wait_until_tcp(
            sandbox, mitm_target_ip, PROBE_PORT, expect_connected=False
        ), (
            "floor never took effect: "
            f"non-whitelisted port ({PROBE_PORT}) stayed reachable (cap file not reloaded?)"
        )

        _assert_https_via_mitm(sandbox, "https://example.com", "Example Domain")
    finally:
        _close_sandbox(cfg, sandbox)


@pytest.mark.timeout(360)
def test_allow_443_deny_all_with_vault_keeps_https_working(
    mitm_target_ip: str,
) -> None:
    """allow-443/deny-all + MITM must let HTTPS out; other ports stay blocked.

    Pre-fix the rewritten dport matched no allow rule and hit the final
    default-deny drop.
    """
    cfg, sandbox = _create_sandbox(
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", ports=[443])],
        ),
    )
    try:
        # Gate: MITM path live (pre-fix this timed out on the rewritten dport).
        assert _wait_until_tcp(
            sandbox, "1.1.1.1", 443, expect_connected=True, timeout_s=60
        ), "TCP/443 was blackholed by the MITM redirect under allow-443/deny-all"

        # Target listens on PROBE_PORT, so the block proves deny-all bites.
        assert _wait_until_tcp(
            sandbox, mitm_target_ip, PROBE_PORT, expect_connected=False, timeout_s=45
        ), "deny-all policy did not block a non-allowed port"

        _assert_https_via_mitm(
            sandbox,
            (
                "'https://cloudflare-dns.com/dns-query?name=example.com&type=A'"
                " --resolve cloudflare-dns.com:443:1.1.1.1"
                " --header 'accept: application/dns-json'"
            ),
            '"Status":0',
        )
    finally:
        _close_sandbox(cfg, sandbox)


def _create_sandbox(network_policy: NetworkPolicy) -> tuple[object, SandboxSync]:
    cfg = create_connection_config_sync()
    sandbox = SandboxSync.create(
        image=SandboxImageSpec(get_sandbox_image()),
        resource=get_e2e_sandbox_resource(),
        connection_config=cfg,
        timeout=timedelta(minutes=5),
        ready_timeout=timedelta(seconds=60),
        network_policy=network_policy,
        credential_proxy=CredentialProxyConfig(enabled=True),
        metadata={E2E_LABEL_KEY: E2E_LABEL_VALUE},
    )
    return cfg, sandbox


def _close_sandbox(cfg: object, sandbox: SandboxSync) -> None:
    try:
        sandbox.kill()
    finally:
        sandbox.close()
        try:
            cfg.transport.close()
        except Exception:
            # Best-effort teardown: do not fail the test if transport is already closed
            # or cannot be closed during cleanup.
            pass


def _write_reachable_ports_cap(sandbox_id: str, ports: list[str]) -> None:
    """Write the floor file into the sidecar; picked up by the ~1-minute reload.
    """
    body = "\\n".join(ports) + "\\n"
    _sidecar_exec(
        sandbox_id,
        "mkdir -p /var/egress/rules && "
        f"printf '{body}' > /var/egress/rules/reachable.ports.cap && "
        "cat /var/egress/rules/reachable.ports.cap",
    )


def _sidecar_exec(sandbox_id: str, script: str) -> str:
    container = f"sandbox-egress-{sandbox_id}"
    result = subprocess.run(
        ["docker", "exec", container, "sh", "-c", script],
        capture_output=True,
        text=True,
        timeout=30,
    )
    assert result.returncode == 0, (
        f"docker exec {container} failed: "
        f"{result.stderr.strip() or result.stdout.strip()}"
    )
    return result.stdout


def _tcp_connects(sandbox: SandboxSync, host: str, port: int) -> bool:
    probe = sandbox.commands.run(
        f"timeout 6 bash -c 'echo > /dev/tcp/{host}/{port}' 2>/dev/null"
    )
    return probe.error is None and probe.exit_code == 0


def _wait_until_tcp(
    sandbox: SandboxSync,
    host: str,
    port: int,
    *,
    expect_connected: bool,
    timeout_s: int = FLOOR_RELOAD_TIMEOUT_S,
) -> bool:
    """Poll until the probe matches the expectation or the budget expires."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if _tcp_connects(sandbox, host, port) == expect_connected:
            return True
        time.sleep(5)
    return False


def _assert_https_via_mitm(sandbox: SandboxSync, url_and_args: str, expect: str) -> None:
    """Retry a curl until the expected marker shows up; TLS verification stays on.

    The sandbox trusts the mitm CA installed by the execd bootstrap, and
    mitmproxy verifies the upstream against public roots.
    """
    last = ""
    for _ in range(3):
        result = sandbox.commands.run(
            f"curl --silent --show-error --connect-timeout 8 --max-time 25 {url_and_args}"
        )
        stdout = "".join(part.text for part in result.logs.stdout)
        if result.error is None and result.exit_code == 0 and expect in stdout:
            return
        last = str(result.error) if result.error else f"exit={result.exit_code} stdout={stdout!r}"
        time.sleep(5)
    pytest.fail(f"HTTPS through the MITM did not succeed: {last}")
