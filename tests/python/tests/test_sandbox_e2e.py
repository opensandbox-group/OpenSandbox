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
"""
Comprehensive E2E tests for Sandbox functionality.
"""

import asyncio
import logging
import time
from datetime import timedelta
from io import BytesIO

import httpx
import pytest
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.constants import DEFAULT_EGRESS_PORT
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import (
    ExecutionComplete,
    ExecutionError,
    ExecutionHandlers,
    ExecutionInit,
    ExecutionResult,
    OutputMessage,
    RunCommandOpts,
)
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)
from opensandbox.models.sandboxes import (
    PVC,
    Host,
    NetworkPolicy,
    NetworkRule,
    SandboxImageSpec,
    Volume,
)

from tests.base_e2e_test import (
    TEST_API_KEY,
    TEST_DOMAIN,
    TEST_PROTOCOL,
    create_connection_config,
    create_connection_config_server_proxy,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    get_test_host_volume_dir,
    get_test_pvc_name,
    is_kubernetes_runtime,
    is_secure_access_verifiable,
)

logger = logging.getLogger(__name__)

# Keep in sync with server ``opensandbox_server/extensions/keys.py``
ACCESS_RENEW_EXTEND_SECONDS_KEY = "access.renew.extend.seconds"


def _now_ms() -> int:
    return int(time.time() * 1000)


def _assert_recent_timestamp_ms(ts: int, *, tolerance_ms: int = 60_000) -> None:
    assert isinstance(ts, int)
    assert ts > 0
    delta = abs(_now_ms() - ts)
    assert delta <= tolerance_ms, f"timestamp too far from now: delta={delta}ms (ts={ts})"


def _assert_times_close(created_at, modified_at, *, tolerance_seconds: float = 2.0) -> None:
    """
    Some filesystems / implementations may report created/modified with slight reordering.
    We only assert they're close, and rely on explicit update operations to validate mtime.
    """
    delta = abs((modified_at - created_at).total_seconds())
    assert delta <= tolerance_seconds, f"created/modified skew too large: {delta}s"


def _assert_modified_updated(before, after, *, min_delta_ms: int = 0, allow_skew_ms: int = 1000) -> None:
    """
    Validate modified_at moved forward after a mutating operation, allowing small clock jitter.
    """
    delta_ms = int((after - before).total_seconds() * 1000)
    assert delta_ms >= min_delta_ms - allow_skew_ms, (
        f"modified_at did not update as expected: delta_ms={delta_ms} "
        f"(min_delta_ms={min_delta_ms}, allow_skew_ms={allow_skew_ms})"
    )


@pytest.mark.asyncio
class TestSandboxE2E:
    """Comprehensive E2E tests for Sandbox functionality."""

    sandbox = None
    connection_config = None
    _setup_done = False

    @pytest.fixture(scope="class", autouse=True)
    async def _sandbox_lifecycle(self, request):
        """Create sandbox once and ALWAYS cleanup to avoid resource leaks."""
        await request.cls._ensure_sandbox_created()
        try:
            yield
        finally:
            sandbox = request.cls.sandbox
            if sandbox is not None:
                try:
                    await sandbox.kill()
                except Exception as e:
                    logger.warning("Teardown: sandbox.kill() failed: %s", e, exc_info=True)
                try:
                    await sandbox.close()
                except Exception as e:
                    logger.warning("Teardown: sandbox.close() failed: %s", e, exc_info=True)

    @classmethod
    async def _ensure_sandbox_created(cls):
        """Ensure sandbox is created before running tests."""
        if cls._setup_done:
            return

        logger.info("=" * 100)
        logger.info("SETUP: Creating sandbox")
        logger.info("=" * 100)

        cls.connection_config = create_connection_config()

        cls.sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cls.connection_config,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            metadata={"tag": "e2e-test"},
            env={
                "E2E_TEST": "true",
                "GO_VERSION": "1.25",
                "JAVA_VERSION": "21",
                "NODE_VERSION": "22",
                "PYTHON_VERSION": "3.12",
                "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms",
            },
            health_check_polling_interval=timedelta(milliseconds=500),
            secure_access=is_secure_access_verifiable(),
        )

        logger.info(f"✓ Sandbox created: {cls.sandbox.id}")
        logger.info("=" * 100)

        cls._setup_done = True

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01_sandbox_lifecycle_and_health(self):
        """Test sandbox lifecycle and health monitoring."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 1: Testing sandbox lifecycle and health monitoring")
        logger.info("=" * 80)

        logger.info("Step 1: Verify basic sandbox properties")
        assert sandbox is not None
        assert isinstance(sandbox.id, str)
        assert await sandbox.is_healthy() is True
        logger.info(f"✓ Sandbox ID: {sandbox.id}")
        logger.info("✓ Sandbox is healthy")

        logger.info("Step 2: Get sandbox information")
        info = await sandbox.get_info()
        assert info.id == sandbox.id
        # FIXME: upstream Kubernetes BatchSandbox lifecycle may still report
        # "Allocated" after execd health checks already pass. This E2E focuses
        # on end-to-end usability, so tolerate that transient state here.
        assert info.status.state in {"Running", "Allocated"}
        assert info.created_at is not None
        assert info.expires_at is not None
        assert info.expires_at > info.created_at
        # Docker runtime reports the SDK default as-is; Kubernetes may prefix bootstrap.sh.
        assert info.entrypoint[-3:] == ["tail", "-f", "/dev/null"], info.entrypoint

        duration = info.expires_at - info.created_at
        # Matches Sandbox.create(..., timeout=timedelta(minutes=5)); allow skew across runtimes.
        min_duration = timedelta(minutes=1)
        max_duration = timedelta(minutes=6)
        assert min_duration <= duration <= max_duration, (
            f"Duration {duration} should be between {min_duration} and {max_duration}"
        )

        assert info.metadata is not None
        assert info.metadata.get("tag") == "e2e-test"
        logger.info(
            "✓ Sandbox info: state=%s, created=%s, expires=%s",
            info.status.state,
            info.created_at,
            info.expires_at,
        )

        logger.info("Step 3: Get sandbox endpoint for default execd port")
        endpoint = await sandbox.get_endpoint(44772)
        assert endpoint is not None
        assert endpoint.endpoint is not None
        logger.info(f"✓ Sandbox endpoint: {endpoint.endpoint}")

        logger.info("Step 4: Get and verify metrics")
        metrics = await sandbox.get_metrics()
        assert metrics is not None
        assert metrics.cpu_count > 0
        assert 0.0 <= metrics.cpu_used_percentage <= 100.0
        assert metrics.memory_total_in_mib > 0
        assert 0.0 <= metrics.memory_used_in_mib <= metrics.memory_total_in_mib
        _assert_recent_timestamp_ms(metrics.timestamp, tolerance_ms=120_000)
        logger.info(
            "✓ CPU: %s cores, %.2f%% used",
            metrics.cpu_count,
            metrics.cpu_used_percentage,
        )
        logger.info(
            "✓ Memory: %s/%s MiB",
            int(metrics.memory_used_in_mib),
            int(metrics.memory_total_in_mib),
        )

        logger.info("Step 5: Test sandbox renewal (extend expiration time)")
        renew_response = await sandbox.renew(timedelta(minutes=20))
        assert renew_response is not None
        assert renew_response.expires_at > info.expires_at
        logger.info("✓ Sandbox expiration renewed to %s", renew_response.expires_at)

        renewed_info = await sandbox.get_info()
        assert renewed_info.expires_at > info.expires_at
        assert renewed_info.id == sandbox.id
        assert renewed_info.status.state in {"Running", "Allocated"}

        # The renew API should return the new expiration time. Allow small backend-side skew.
        assert abs((renewed_info.expires_at - renew_response.expires_at).total_seconds()) < 10

        # Renewal is "now + timeout" (SDK behavior). Validate remaining TTL is close to 5 minutes.
        now = renewed_info.expires_at.__class__.now(tz=renewed_info.expires_at.tzinfo)
        remaining = renewed_info.expires_at - now
        assert remaining > timedelta(minutes=18), f"Remaining TTL too small: {remaining}"
        assert remaining < timedelta(minutes=22), f"Remaining TTL too large: {remaining}"

        logger.info(
            "✓ Sandbox expiration updated from %s to %s",
            info.expires_at,
            renewed_info.expires_at,
        )

        logger.info("Step 6: Test access to service components")

        assert sandbox.files is not None
        assert sandbox.commands is not None
        assert sandbox.metrics is not None
        assert sandbox.connection_config is not None
        logger.info("✓ All sandbox service components are accessible")

        logger.info("Step 6b: Get signed sandbox endpoint and verify execd reachable via gateway")
        if is_secure_access_verifiable():
            unsigned_ep = await sandbox.get_endpoint(44772)
            assert unsigned_ep is not None
            assert unsigned_ep.endpoint is not None

            future_ts = int(time.time()) + 3600
            signed_ep = await sandbox.get_signed_endpoint(44772, future_ts)
            assert signed_ep is not None
            assert signed_ep.endpoint is not None
            # Signed response carries proof of route token in headers (header mode) or URL (URI mode).
            # At minimum, the signed response must differ from the unsigned baseline.
            assert signed_ep.headers != unsigned_ep.headers or signed_ep.endpoint != unsigned_ep.endpoint, (
                "Signed endpoint should differ from unsigned endpoint in headers or URL"
            )
            logger.info(f"✓ Signed endpoint obtained: {signed_ep.endpoint}")
            if signed_ep.headers:
                for k, v in signed_ep.headers.items():
                    logger.info(f"  Header: {k}: {v}")
            else:
                logger.info("  (no headers in signed response)")

            # Use the signed endpoint to make an actual request to execd /ping
            # through the ingress gateway, verifying the route token is accepted.
            # The endpoint returned by the API has no scheme — prepend protocol.
            ping_url = f"{TEST_PROTOCOL}://{signed_ep.endpoint.rstrip('/')}/ping"
            ping_headers = {**signed_ep.headers} if signed_ep.headers else {}
            logger.info(f"Signed /ping via gateway: {ping_url}")
            async with httpx.AsyncClient() as client:
                ping_resp = await client.get(ping_url, headers=ping_headers, timeout=30)
                assert ping_resp.status_code == 200, (
                    f"Signed endpoint /ping failed: HTTP {ping_resp.status_code} {ping_resp.text[:200]}"
                )
            logger.info("✓ Execd /ping succeeded through gateway with signed endpoint")

            # Expired timestamp should be rejected by the server.
            expired_ts = int(time.time()) - 3600
            try:
                await sandbox.get_signed_endpoint(44772, expired_ts)
                logger.warning("Expired timestamp was accepted (server may not validate server-side)")
            except SandboxApiException:
                logger.info("✓ Expired timestamp correctly rejected")
        else:
            logger.info("Secure access not verifiable, skipping signed endpoint tests")

        logger.info("Step 7: Connect to existing sandbox by ID")
        sandbox2 = await Sandbox.connect(
            sandbox_id=sandbox.id,
            connection_config=TestSandboxE2E.connection_config,
        )
        try:
            assert sandbox2.id == sandbox.id
            assert await sandbox2.is_healthy() is True
            connect_result = await sandbox2.commands.run("echo connect-ok")
            assert connect_result.error is None
            assert len(connect_result.logs.stdout) == 1
            assert connect_result.logs.stdout[0].text == "connect-ok"
        finally:
            await sandbox2.close()

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01b_manual_cleanup(self):
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=TestSandboxE2E.connection_config,
            timeout=None,
            ready_timeout=timedelta(seconds=30),
            metadata={"tag": "manual-e2e-test"},
        )
        try:
            info = await sandbox.get_info()
            assert info.expires_at is None
            assert info.metadata is not None
            assert info.metadata.get("tag") == "manual-e2e-test"
        finally:
            await sandbox.kill()
            await sandbox.close()

        logger.info("TEST 1 PASSED: Sandbox lifecycle and health test completed successfully")


    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01a_network_policy_create(self):
        if is_kubernetes_runtime():
            pytest.skip("Network policy is not covered in the Kubernetes runtime suite")

        logger.info("=" * 80)
        logger.info("TEST 1a: Creating sandbox with networkPolicy (async)")
        logger.info("=" * 80)

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[NetworkRule(action="allow", target="pypi.org")],
            ),
        )
        try:
            await asyncio.sleep(5)
            result = await sandbox.commands.run("curl -I https://www.github.com")
            assert result.error is not None
            result = await sandbox.commands.run("curl -I https://pypi.org")
            assert result.error is None
        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

    @pytest.mark.timeout(180)
    @pytest.mark.order(1)
    async def test_01aa_network_policy_get_and_patch(self):
        if is_kubernetes_runtime():
            pytest.skip("Network policy is not covered in the Kubernetes runtime suite")

        logger.info("=" * 80)
        logger.info("TEST 1aa: networkPolicy get/patch (async)")
        logger.info("=" * 80)

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[NetworkRule(action="allow", target="pypi.org")],
            ),
        )
        try:
            await asyncio.sleep(5)

            # Verify get egress policy right after create.
            policy = await sandbox.get_egress_policy()
            assert policy.default_action == "deny"
            assert policy.egress is not None
            assert any(rule.target == "pypi.org" and rule.action == "allow" for rule in policy.egress)

            # Baseline behavior: github blocked, pypi allowed.
            blocked = await sandbox.commands.run("curl -I https://www.github.com")
            assert blocked.error is not None
            allowed = await sandbox.commands.run("curl -I https://pypi.org")
            assert allowed.error is None

            # Patch policy: allow github, deny pypi.
            await sandbox.patch_egress_rules(
                [
                    NetworkRule(action="allow", target="www.github.com"),
                    NetworkRule(action="deny", target="pypi.org"),
                ],
            )
            await asyncio.sleep(2)

            patched_policy = await sandbox.get_egress_policy()
            assert patched_policy.egress is not None
            assert any(
                rule.target == "www.github.com" and rule.action == "allow"
                for rule in patched_policy.egress
            )
            assert any(
                rule.target == "pypi.org" and rule.action == "deny"
                for rule in patched_policy.egress
            )

            # Behavior after patch should be flipped.
            github_allowed = await sandbox.commands.run("curl -I https://www.github.com")
            assert github_allowed.error is None
            pypi_denied = await sandbox.commands.run("curl -I https://pypi.org")
            assert pypi_denied.error is not None
        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

    @pytest.mark.timeout(240)
    @pytest.mark.order(1)
    async def test_01ab_network_policy_get_and_patch_with_server_proxy(self):
        """Also covers access renew on proxy traffic (needs ``[renew_intent] enabled = true``)."""
        if is_kubernetes_runtime():
            pytest.skip("Network policy is not covered in the Kubernetes runtime suite")

        logger.info("=" * 80)
        logger.info("TEST 1ab: networkPolicy get/patch with server proxy (async)")
        logger.info("=" * 80)

        cfg = create_connection_config_server_proxy()
        assert cfg.use_server_proxy is True
        sandbox_ttl = timedelta(minutes=4)
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=sandbox_ttl,
            ready_timeout=timedelta(seconds=90),
            extensions={ACCESS_RENEW_EXTEND_SECONDS_KEY: "300"},
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[NetworkRule(action="allow", target="pypi.org")],
            ),
        )
        try:
            boot = await sandbox.get_info()
            assert boot.expires_at is not None
            # Baseline from create contract only: ready/ping may already move expires_at.
            nominal_expires_at = boot.created_at + sandbox_ttl

            await asyncio.sleep(5)

            egress_endpoint = await sandbox.get_endpoint(DEFAULT_EGRESS_PORT)
            assert f"/sandboxes/{sandbox.id}/proxy/{DEFAULT_EGRESS_PORT}" in egress_endpoint.endpoint

            policy = await sandbox.get_egress_policy()
            assert policy.default_action == "deny"
            assert policy.egress is not None
            assert any(rule.target == "pypi.org" and rule.action == "allow" for rule in policy.egress)

            blocked = await sandbox.commands.run("curl -I https://www.github.com")
            assert blocked.error is not None
            allowed = await sandbox.commands.run("curl -I https://pypi.org")
            assert allowed.error is None

            await sandbox.patch_egress_rules(
                [
                    NetworkRule(action="allow", target="www.github.com"),
                    NetworkRule(action="deny", target="pypi.org"),
                ],
            )
            await asyncio.sleep(2)

            patched_policy = await sandbox.get_egress_policy()
            assert patched_policy.egress is not None
            assert any(
                rule.target == "www.github.com" and rule.action == "allow"
                for rule in patched_policy.egress
            )
            assert any(
                rule.target == "pypi.org" and rule.action == "deny"
                for rule in patched_policy.egress
            )

            assert await sandbox.is_healthy()

            deadline = time.monotonic() + 30.0
            min_delta = timedelta(seconds=30)
            bumped = False
            while time.monotonic() < deadline:
                info = await sandbox.get_info()
                if info.expires_at is not None and info.expires_at > nominal_expires_at + min_delta:
                    bumped = True
                    logger.info(
                        "Access renew: expires_at=%s above nominal (created_at+timeout)=%s",
                        info.expires_at,
                        nominal_expires_at,
                    )
                    break
                await asyncio.sleep(2.0)
            assert bumped, (
                "expires_at did not exceed created_at + create timeout + slack after proxied traffic; "
                "set [renew_intent] enabled = true on the lifecycle server."
            )
        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01b_host_volume_mount(self):
        """Test creating a sandbox with a host volume mount."""
        if is_kubernetes_runtime():
            pytest.skip("Host path volume E2E is only covered in the Docker runtime suite")

        logger.info("=" * 80)
        logger.info("TEST 1b: Creating sandbox with host volume mount (async)")
        logger.info("=" * 80)

        host_dir = get_test_host_volume_dir()
        container_mount_path = "/mnt/host-data"

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            volumes=[
                Volume(
                    name="test-host-vol",
                    host=Host(path=host_dir),
                    mountPath=container_mount_path,
                    readOnly=False,
                ),
            ],
        )
        try:
            logger.info(f"✓ Sandbox with volume created: {sandbox.id}")

            # Step 1: Verify the host marker file is visible inside the sandbox
            # Retry: bind mount propagation can sometimes lag on first access
            logger.info("Step 1: Verify host marker file is readable inside the sandbox")
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/marker.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None, f"Failed to read marker file: {result.error}"
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "opensandbox-e2e-marker"
            logger.info("✓ Host marker file read successfully inside sandbox")

            # Step 2: Write a file from inside the sandbox to the mounted path (read-write)
            logger.info("Step 2: Write a file from inside the sandbox to the mount path")
            result = await sandbox.commands.run(
                f"echo 'written-from-sandbox' > {container_mount_path}/sandbox-output.txt"
            )
            assert result.error is None, f"Failed to write file: {result.error}"

            # Step 3: Verify the written file is readable
            # Retry: written data may not be immediately visible through bind mount
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/sandbox-output.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "written-from-sandbox"
            logger.info("✓ File written and verified inside sandbox")

            # Step 4: Verify the mount path is a proper directory
            logger.info("Step 3: Verify mount path is a directory")
            result = await sandbox.commands.run(f"test -d {container_mount_path} && echo OK")
            assert result.error is None
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "OK"
            logger.info("✓ Mount path is a valid directory")

        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        logger.info("TEST 1b PASSED: Host volume mount test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01c_host_volume_mount_readonly(self):
        """Test creating a sandbox with a read-only host volume mount."""
        if is_kubernetes_runtime():
            pytest.skip("Host path volume E2E is only covered in the Docker runtime suite")

        logger.info("=" * 80)
        logger.info("TEST 1c: Creating sandbox with read-only host volume mount (async)")
        logger.info("=" * 80)

        host_dir = get_test_host_volume_dir()
        container_mount_path = "/mnt/host-data-ro"

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            volumes=[
                Volume(
                    name="test-host-vol-ro",
                    host=Host(path=host_dir),
                    mountPath=container_mount_path,
                    readOnly=True,
                ),
            ],
        )
        try:
            logger.info(f"✓ Sandbox with read-only volume created: {sandbox.id}")

            # Step 1: Verify the host marker file is readable
            # Retry: bind mount propagation can sometimes lag on first access
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/marker.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None, f"Failed to read marker file: {result.error}"
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "opensandbox-e2e-marker"
            logger.info("✓ Host marker file read successfully in read-only mount")

            # Step 2: Verify writing is denied on read-only mount
            result = await sandbox.commands.run(
                f"touch {container_mount_path}/should-fail.txt"
            )
            assert result.error is not None, "Write should fail on read-only mount"
            logger.info("✓ Write correctly denied on read-only mount")

        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        logger.info("TEST 1c PASSED: Read-only host volume mount test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01d_pvc_named_volume_mount(self):
        """Test creating a sandbox with a PVC (Docker named volume) mount."""
        logger.info("=" * 80)
        logger.info("TEST 1d: Creating sandbox with PVC named volume mount (async)")
        logger.info("=" * 80)

        pvc_volume_name = get_test_pvc_name()
        container_mount_path = "/mnt/pvc-data"

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            volumes=[
                Volume(
                    name="test-pvc-vol",
                    pvc=PVC(claimName=pvc_volume_name),
                    mountPath=container_mount_path,
                    readOnly=False,
                ),
            ],
        )
        try:
            logger.info(f"✓ Sandbox with PVC volume created: {sandbox.id}")

            # Step 1: Verify the marker file seeded into the named volume is readable
            logger.info("Step 1: Verify PVC marker file is readable inside the sandbox")
            # Retry: bind mount propagation can sometimes lag on first access
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/marker.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None, f"Failed to read marker file: {result.error}"
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "pvc-marker-data"
            logger.info("✓ PVC marker file read successfully inside sandbox")

            # Step 2: Write a file from inside the sandbox to the named volume
            logger.info("Step 2: Write a file from inside the sandbox to the PVC mount")
            result = await sandbox.commands.run(
                f"echo 'written-to-pvc' > {container_mount_path}/pvc-output.txt"
            )
            assert result.error is None, f"Failed to write file: {result.error}"

            # Step 3: Verify the written file is readable
            # Retry: bind mount propagation can sometimes lag on first access
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/pvc-output.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "written-to-pvc"
            logger.info("✓ File written and verified inside sandbox via PVC mount")

            # Step 4: Verify the mount path is a proper directory
            result = await sandbox.commands.run(f"test -d {container_mount_path} && echo OK")
            assert result.error is None
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "OK"
            logger.info("✓ PVC mount path is a valid directory")

        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        logger.info("TEST 1d PASSED: PVC named volume mount test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01e_pvc_named_volume_mount_readonly(self):
        """Test creating a sandbox with a read-only PVC (Docker named volume) mount."""
        logger.info("=" * 80)
        logger.info("TEST 1e: Creating sandbox with read-only PVC named volume mount (async)")
        logger.info("=" * 80)

        pvc_volume_name = get_test_pvc_name()
        container_mount_path = "/mnt/pvc-data-ro"

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            volumes=[
                Volume(
                    name="test-pvc-vol-ro",
                    pvc=PVC(claimName=pvc_volume_name),
                    mountPath=container_mount_path,
                    readOnly=True,
                ),
            ],
        )
        try:
            logger.info(f"✓ Sandbox with read-only PVC volume created: {sandbox.id}")

            # Step 1: Verify the marker file is readable on read-only mount
            # Retry: bind mount propagation can sometimes lag on first access
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/marker.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None, f"Failed to read marker file: {result.error}"
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "pvc-marker-data"
            logger.info("✓ PVC marker file read successfully in read-only mount")

            # Step 2: Verify writing is denied on read-only mount
            result = await sandbox.commands.run(
                f"touch {container_mount_path}/should-fail.txt"
            )
            assert result.error is not None, "Write should fail on read-only PVC mount"
            logger.info("✓ Write correctly denied on read-only PVC mount")

        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        logger.info("TEST 1e PASSED: Read-only PVC named volume mount test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(1)
    async def test_01f_pvc_named_volume_subpath_mount(self):
        """Test creating a sandbox with a PVC named volume mount using subPath."""
        logger.info("=" * 80)
        logger.info("TEST 1f: Creating sandbox with PVC named volume subPath mount (async)")
        logger.info("=" * 80)

        pvc_volume_name = get_test_pvc_name()
        container_mount_path = "/mnt/train"

        cfg = create_connection_config()
        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cfg,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            volumes=[
                Volume(
                    name="test-pvc-subpath",
                    pvc=PVC(claimName=pvc_volume_name),
                    mountPath=container_mount_path,
                    readOnly=False,
                    subPath="datasets/train",
                ),
            ],
        )
        try:
            logger.info(f"✓ Sandbox with PVC subPath volume created: {sandbox.id}")

            # Step 1: Verify the subpath marker file is readable
            logger.info("Step 1: Verify subPath marker file is readable")
            # Retry: bind mount propagation can sometimes lag on first access
            for _attempt in range(5):
                result = await sandbox.commands.run(f"cat {container_mount_path}/marker.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(0.5)
            assert result.error is None, f"Failed to read subpath marker file: {result.error}"
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "pvc-subpath-marker"
            logger.info("✓ SubPath marker file read successfully")

            # Step 2: Verify we only see the subpath contents (not the full volume)
            logger.info("Step 2: Verify only subPath contents are visible")
            result = await sandbox.commands.run(f"ls {container_mount_path}/")
            assert result.error is None
            # Should contain marker.txt but NOT 'datasets' directory (we are inside it)
            stdout_text = "\n".join(msg.text for msg in result.logs.stdout)
            assert "marker.txt" in stdout_text
            assert "datasets" not in stdout_text
            logger.info("✓ Only subPath contents are visible inside the sandbox")

            # Step 3: Write a file and verify (retry read-back for transient SSE drops)
            logger.info("Step 3: Write and verify a file inside subPath mount")
            result = await sandbox.commands.run(
                f"echo 'subpath-write-test' > {container_mount_path}/output.txt"
            )
            assert result.error is None
            for _attempt in range(3):
                result = await sandbox.commands.run(f"cat {container_mount_path}/output.txt")
                if result.logs.stdout:
                    break
                await asyncio.sleep(1)
            assert result.error is None
            assert len(result.logs.stdout) == 1
            assert result.logs.stdout[0].text == "subpath-write-test"
            logger.info("✓ File written and verified inside subPath mount")

        finally:
            try:
                await sandbox.kill()
            except Exception:
                pass
            await sandbox.close()

        logger.info("TEST 1f PASSED: PVC subPath named volume mount test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(2)
    async def test_02_basic_command_execution(self):
        """Test basic command execution."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 2: Testing basic command execution")
        logger.info("=" * 80)

        logger.info("Step 1: Simple echo command with handlers to capture events")
        stdout_messages = []
        stderr_messages = []
        results = []
        completed_events = []
        errors = []
        init_events = []

        async def on_stdout(msg: OutputMessage):
            stdout_messages.append(msg)
            logger.info(f"Stdout: {msg.text}")

        async def on_stderr(msg: OutputMessage):
            stderr_messages.append(msg)
            logger.warning(f"Stderr: {msg.text}")

        async def on_result(result: ExecutionResult):
            results.append(result)
            logger.info(f"Result: {result.text}")

        async def on_execution_complete(complete: ExecutionComplete):
            completed_events.append(complete)
            logger.info(f"Execution completed in {complete.execution_time_in_millis} ms")

        async def on_error(error: ExecutionError):
            errors.append(error)
            logger.error(f"Error: {error.name} - {error.value}")

        async def on_init(init: ExecutionInit):
            init_events.append(init)
            logger.info(f"Execution initialized with ID: {init.id}")

        handlers = ExecutionHandlers(
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_result=on_result,
            on_execution_complete=on_execution_complete,
            on_error=on_error,
            on_init=on_init
        )

        echo_result = await sandbox.commands.run(
            "echo 'Hello OpenSandbox E2E'",
            handlers=handlers,
        )

        # Verify result
        assert echo_result is not None
        assert echo_result.id is not None and echo_result.id.strip()
        assert echo_result.error is None
        assert len(echo_result.logs.stdout) == 1
        assert echo_result.logs.stdout[0].text == "Hello OpenSandbox E2E"
        assert echo_result.logs.stdout[0].is_error is False
        _assert_recent_timestamp_ms(echo_result.logs.stdout[0].timestamp)
        assert len(echo_result.logs.stderr) == 0
        assert echo_result.exit_code == 0
        assert echo_result.complete is not None
        assert echo_result.complete.execution_time_in_millis >= 0
        _assert_recent_timestamp_ms(echo_result.complete.timestamp)

        # Verify handlers captured events
        assert len(init_events) == 1, "Execution should have exactly one init event"
        assert len(completed_events) == 1, "Execution should have exactly one completion event"
        assert init_events[0].id == echo_result.id
        _assert_recent_timestamp_ms(init_events[0].timestamp)
        _assert_recent_timestamp_ms(completed_events[0].timestamp)
        assert completed_events[0].execution_time_in_millis >= 0

        assert len(stdout_messages) == 1, "Should have captured exactly one stdout message"
        assert stdout_messages[0].text == "Hello OpenSandbox E2E"
        assert stdout_messages[0].is_error is False
        _assert_recent_timestamp_ms(stdout_messages[0].timestamp)

        assert len(errors) == 0, "Should have no errors for successful command"

        logger.info(
            "✓ Captured %s stdout, %s stderr, %s results, %s errors, %s completions, %s inits",
            len(stdout_messages),
            len(stderr_messages),
            len(results),
            len(errors),
            len(completed_events),
            len(init_events),
        )

        logger.info("Step 2: Command with working directory")
        pwd_result = await sandbox.commands.run(
            "pwd",
            opts=RunCommandOpts(working_directory="/tmp"),
        )
        assert pwd_result is not None
        assert pwd_result.id is not None and pwd_result.id.strip()
        assert pwd_result.error is None
        assert len(pwd_result.logs.stdout) == 1
        assert pwd_result.logs.stdout[0].text == "/tmp"
        assert pwd_result.logs.stdout[0].is_error is False
        _assert_recent_timestamp_ms(pwd_result.logs.stdout[0].timestamp)
        assert pwd_result.exit_code == 0
        assert pwd_result.complete is not None
        logger.info(f"✓ PWD command executed: {pwd_result}")

        logger.info("Step 3: Background command")
        start_time = time.time()
        background_result = await sandbox.commands.run(
            "sleep 30",
            opts=RunCommandOpts(background=True),
        )
        end_time = time.time()

        execution_time = (end_time - start_time) * 1000
        assert execution_time < 10000, \
            f"Background command should return quickly, but took {execution_time} ms"
        assert background_result.exit_code is None
        logger.info(f"✓ Background command returned in {execution_time:.2f} ms")

        logger.info("Step 4: Test failing command")
        # Clear event lists for fail test
        stdout_messages.clear()
        stderr_messages.clear()
        errors.clear()
        completed_events.clear()
        init_events.clear()

        fail_result = await sandbox.commands.run(
            "nonexistent-command-that-does-not-exist",
            handlers=handlers,
        )

        # Verify error result
        assert fail_result is not None
        assert fail_result.id is not None and fail_result.id.strip()
        assert fail_result.error is not None
        assert fail_result.error.name == "CommandExecError"
        assert len(fail_result.logs.stderr) > 0
        assert any(
            "nonexistent-command-that-does-not-exist" in m.text for m in fail_result.logs.stderr
        )
        assert all(m.is_error is True for m in fail_result.logs.stderr)
        _assert_recent_timestamp_ms(fail_result.logs.stderr[0].timestamp)
        assert fail_result.complete is None
        assert fail_result.exit_code == int(fail_result.error.value)

        # Verify handlers captured error events
        assert len(init_events) == 1, "Execution should have exactly one init event"
        assert init_events[0].id == fail_result.id
        _assert_recent_timestamp_ms(init_events[0].timestamp)
        # Contract: error and complete are mutually exclusive; failing command should emit error only.
        assert len(errors) >= 1, "Should have captured error events"
        assert len(completed_events) == 0, "Failing command should not emit completion event"

        assert errors[0].name == "CommandExecError", "Error name should match"
        assert len(stderr_messages) > 0, "Should have captured stderr messages"
        assert "nonexistent-command-that-does-not-exist" in stderr_messages[0].text, (
            "Stderr should contain command name"
        )

        logger.info(f"✓ Failed command result: {fail_result}")

        logger.info("TEST 2 PASSED: Basic command execution test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(2)
    async def test_02c_bash_session_api(self):
        """Test create_session / run_in_session / delete_session.

        Verifies working directory passing, session env persistence, and run_in_session exit_code behavior.
        """
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 2c: Bash session API — verify working directory is passed and applied")
        logger.info("=" * 80)

        logger.info("Step 1: Create session with working_directory=/tmp and verify session starts in that directory")
        sid = await sandbox.commands.create_session(working_directory="/tmp")
        assert sid is not None and isinstance(sid, str) and len(sid) > 0
        out_pwd = await sandbox.commands.run_in_session(sid, "pwd")
        assert out_pwd.error is None, f"pwd failed: {out_pwd.error}"
        assert out_pwd.exit_code == 0
        pwd_line = "".join(m.text for m in out_pwd.logs.stdout).strip()
        assert pwd_line == "/tmp", f"create_session(working_directory=/tmp) should run in /tmp, got: {pwd_line!r}"
        logger.info("✓ create_session(working_directory=/tmp) applied: pwd => %s", pwd_line)

        logger.info("Step 2: run_in_session with working_directory override — run in /var and verify")
        out_var = await sandbox.commands.run_in_session(sid, "pwd", working_directory="/var")
        assert out_var.error is None
        assert out_var.exit_code == 0
        var_line = "".join(m.text for m in out_var.logs.stdout).strip()
        assert var_line == "/var", f"run_in_session(..., working_directory=/var) should run in /var, got: {var_line!r}"
        logger.info("✓ run_in_session(..., working_directory=/var) applied: pwd => %s", var_line)

        logger.info("Step 3: run_in_session with working_directory=/tmp — verify override per run")
        out_tmp = await sandbox.commands.run_in_session(sid, "pwd", working_directory="/tmp")
        assert out_tmp.error is None
        assert out_tmp.exit_code == 0
        tmp_line = "".join(m.text for m in out_tmp.logs.stdout).strip()
        assert tmp_line == "/tmp", f"run_in_session(..., working_directory=/tmp) should run in /tmp, got: {tmp_line!r}"
        logger.info("✓ run_in_session(..., working_directory=/tmp) applied: pwd => %s", tmp_line)

        logger.info("Step 3b: Export env in one run, read in next run — verify session state (env) persists")
        await sandbox.commands.run_in_session(sid, "export E2E_SESSION_ENV=session-env-ok")
        out_env = await sandbox.commands.run_in_session(sid, "echo $E2E_SESSION_ENV")
        assert out_env.error is None
        assert out_env.exit_code == 0
        env_line = "".join(m.text for m in out_env.logs.stdout).strip()
        assert env_line == "session-env-ok", f"env set in previous run should be visible, got: {env_line!r}"
        logger.info("✓ session env persists across run_in_session: echo $E2E_SESSION_ENV => %s", env_line)

        logger.info("Step 3c: Failing subprocess in session should propagate non-zero exit_code")
        fail = await sandbox.commands.run_in_session(
            sid, "sh -c 'echo session-fail >&2; exit 7'"
        )
        assert fail.error is not None
        assert fail.error.name == "CommandExecError"
        assert fail.error.value == "7"
        assert fail.exit_code == 7
        assert fail.complete is None
        logger.info("✓ run_in_session failure propagated exit_code=7")

        logger.info("Step 4: New session with working_directory=/var — verify create_session working directory again")
        sid2 = await sandbox.commands.create_session(working_directory="/var")
        assert sid2 is not None
        out_var2 = await sandbox.commands.run_in_session(sid2, "pwd")
        assert out_var2.error is None
        assert out_var2.exit_code == 0
        var2_line = "".join(m.text for m in out_var2.logs.stdout).strip()
        assert var2_line == "/var", f"create_session(working_directory=/var) should run in /var, got: {var2_line!r}"
        logger.info("✓ create_session(working_directory=/var) applied: pwd => %s", var2_line)

        logger.info("Step 5: Delete both sessions")
        await sandbox.commands.delete_session(sid)
        await sandbox.commands.delete_session(sid2)
        logger.info("✓ Sessions deleted")

        logger.info("TEST 2c PASSED: working directory passing verified for create_session and run_in_session")

    @pytest.mark.timeout(120)
    @pytest.mark.order(3)
    async def test_02a_command_status_and_logs(self):
        """Test command status + background logs."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        exec_result = await sandbox.commands.run(
            "sh -c 'echo log-line-1; echo log-line-2; sleep 2'",
            opts=RunCommandOpts(background=True),
        )
        assert exec_result.id is not None
        command_id = exec_result.id

        status = await sandbox.commands.get_command_status(command_id)
        assert status.id == command_id
        assert isinstance(status.running, bool)

        logs_text = ""
        cursor = None
        for _ in range(20):
            logs = await sandbox.commands.get_background_command_logs(command_id, cursor=cursor)
            logs_text += logs.content
            cursor = logs.cursor if logs.cursor is not None else cursor
            if "log-line-2" in logs_text:
                break
            await asyncio.sleep(1.0)

        assert "log-line-1" in logs_text
        assert "log-line-2" in logs_text

    @pytest.mark.timeout(120)
    @pytest.mark.order(3)
    async def test_02b_run_command_with_envs(self):
        """Test run_command env injection via RunCommandOpts.envs."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        env_key = "OPEN_SANDBOX_E2E_CMD_ENV"
        env_value = f"env-ok-{int(time.time())}"
        probe_command = (
            f"sh -c 'if [ -z \"${{{env_key}:-}}\" ]; then echo \"__EMPTY__\"; "
            f"else echo \"${{{env_key}}}\"; fi'"
        )

        # Baseline: variable should be empty when not injected.
        baseline = await sandbox.commands.run(probe_command)
        assert baseline.error is None
        baseline_output = "\n".join(msg.text for msg in baseline.logs.stdout).strip()
        assert baseline_output == "__EMPTY__"

        # Inject environment variables for this command only.
        injected = await sandbox.commands.run(
            probe_command,
            opts=RunCommandOpts(
                envs={
                    env_key: env_value,
                    "OPEN_SANDBOX_E2E_SECOND_ENV": "second-ok",
                }
            ),
        )
        assert injected.error is None
        injected_output = "\n".join(msg.text for msg in injected.logs.stdout).strip()
        assert injected_output == env_value

    @pytest.mark.timeout(120)
    @pytest.mark.order(4)
    async def test_03_basic_filesystem_operations(self):
        """Test basic filesystem operations."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 3: Testing basic filesystem operations")
        logger.info("=" * 80)

        test_dir1 = f"/tmp/fs_test1_{int(time.time() * 1000)}"
        test_dir2 = f"/tmp/fs_test2_{int(time.time() * 1000)}"

        logger.info("Step 1: Create directories")
        dir_entry1 = WriteEntry(path=test_dir1, mode=755)
        dir_entry2 = WriteEntry(path=test_dir2, mode=644)
        await sandbox.files.create_directories([dir_entry1, dir_entry2])
        logger.info(f"✓ Created directories: {test_dir1}, {test_dir2}")

        dir_info_map = await sandbox.files.get_file_info([test_dir1, test_dir2])
        assert test_dir1 in dir_info_map
        assert test_dir2 in dir_info_map
        assert dir_info_map[test_dir1].path == test_dir1
        assert dir_info_map[test_dir2].path == test_dir2
        assert dir_info_map[test_dir1].mode == 755
        assert dir_info_map[test_dir2].mode == 644
        assert dir_info_map[test_dir1].owner
        assert dir_info_map[test_dir1].group
        _assert_times_close(dir_info_map[test_dir1].created_at, dir_info_map[test_dir1].modified_at)

        ls_result = await sandbox.commands.run(
            "ls -la | grep fs_test",
            opts=RunCommandOpts(working_directory="/tmp"),
        )
        assert len(ls_result.logs.stdout) == 2, "Should find exactly 2 directories"
        logger.info(f"✓ Directory verification: {ls_result}")

        logger.info("Step 2: Create and write files")
        test_file1 = f"{test_dir1}/test_file1.txt"
        test_file2 = f"{test_dir1}/test_file2.txt"
        test_file3 = f"{test_dir1}/test_file3.txt"
        test_content = "Hello Filesystem!\\nLine 2 with special chars: åäö\\nLine 3"

        write_entry1 = WriteEntry(path=test_file1, data=test_content, mode=644)
        write_entry2 = WriteEntry(path=test_file2, data=test_content.encode('utf-8'), mode=755)
        write_entry3 = WriteEntry(
            path=test_file3,
            data=BytesIO(test_content.encode('utf-8')),
            group="nogroup",
            owner="nobody",
            mode=755
        )
        await sandbox.files.write_files([write_entry1, write_entry2, write_entry3])
        logger.info("✓ Created 3 test files")

        logger.info("Step 3: Read and verify file content using different methods")
        read_content1 = await sandbox.files.read_file(test_file1, encoding='utf-8')
        read_content1_partial = await sandbox.files.read_file(
            test_file1, encoding='utf-8', range_header="bytes=0-9"
        )

        read_bytes2 = await sandbox.files.read_bytes(test_file2)
        read_content2 = read_bytes2.decode('utf-8')

        stream3 = await sandbox.files.read_bytes_stream(test_file3)
        read_content3_bytes = b""
        async for chunk in stream3:
            read_content3_bytes += chunk
        read_content3 = read_content3_bytes.decode("utf-8")

        expected_size = len(test_content.encode("utf-8"))
        assert read_content1 == test_content
        assert read_content2 == test_content
        assert read_content3 == test_content
        assert read_content1_partial == test_content[:10]
        logger.info("✓ All file reads successful and content verified")

        logger.info("Step 4: Get and verify file info")
        all_test_files = [test_file1, test_file2, test_file3]
        file_info_map = await sandbox.files.get_file_info(all_test_files)

        file_info1 = file_info_map[test_file1]
        assert file_info1 is not None
        assert file_info1.path == test_file1
        assert file_info1.size == expected_size
        assert file_info1.mode == 644
        assert file_info1.owner is not None
        assert file_info1.group is not None
        _assert_times_close(file_info1.created_at, file_info1.modified_at)

        file_info2 = file_info_map[test_file2]
        assert file_info2 is not None
        assert file_info2.path == test_file2
        assert file_info2.size == expected_size
        assert file_info2.mode == 755
        assert file_info2.owner is not None
        assert file_info2.group is not None
        _assert_times_close(file_info2.created_at, file_info2.modified_at)

        file_info3 = file_info_map[test_file3]
        assert file_info3 is not None
        assert file_info3.path == test_file3
        assert file_info3.size == expected_size
        assert file_info3.mode == 755
        assert file_info3.owner == "nobody"
        assert file_info3.group == "nogroup"
        _assert_times_close(file_info3.created_at, file_info3.modified_at)
        logger.info(f"✓ File info verified: size={file_info1.size}, mode={oct(file_info1.mode)}")

        logger.info("Step 5: Test search functionality")
        search_all_entry = SearchEntry(path=test_dir1, pattern="*")
        all_files_list = await sandbox.files.search(search_all_entry)
        all_files = {entry.path: entry for entry in all_files_list}

        assert len(all_files) == 3
        assert test_file1 in all_files
        assert test_file2 in all_files
        assert test_file3 in all_files
        assert all_files[test_file1].size == expected_size
        _assert_times_close(all_files[test_file1].created_at, all_files[test_file1].modified_at)
        logger.info("✓ Search found all 3 files")

        logger.info("Step 6: Test permission changes")
        perm_entry1 = SetPermissionEntry(
            path=test_file1,
            mode=755,
            owner="nobody",
            group="nogroup"
        )
        perm_entry2 = SetPermissionEntry(
            path=test_file2,
            mode=600,
            owner="nobody",
            group="nogroup"
        )
        await sandbox.files.set_permissions([perm_entry1, perm_entry2])

        updated_info_map = await sandbox.files.get_file_info([test_file1, test_file2])
        updated_info1 = updated_info_map[test_file1]
        updated_info2 = updated_info_map[test_file2]

        assert updated_info1.mode == 755
        assert updated_info1.owner == "nobody"
        assert updated_info1.group == "nogroup"

        assert updated_info2.mode == 600
        assert updated_info2.owner == "nobody"
        assert updated_info2.group == "nogroup"
        logger.info("✓ Permissions updated successfully")

        logger.info("Step 7: Update file content")
        before_update_info = (await sandbox.files.get_file_info([test_file1]))[test_file1]
        updated_content1 = test_content + "\\nAppended line to file1"
        updated_content2 = test_content + "\\nAppended line to file2"

        # Ensure server-visible mtime delta is measurable.
        await asyncio.sleep(0.05)

        update_entry1 = WriteEntry(path=test_file1, data=updated_content1, mode=644)
        update_entry2 = WriteEntry(path=test_file2, data=updated_content2, mode=755)
        await sandbox.files.write_files([update_entry1, update_entry2])

        new_content1 = await sandbox.files.read_file(test_file1, encoding="utf-8")
        new_content2 = await sandbox.files.read_file(test_file2, encoding="utf-8")

        assert new_content1 == updated_content1
        assert new_content2 == updated_content2
        logger.info("✓ File content updated successfully")

        after_update_info = (await sandbox.files.get_file_info([test_file1]))[test_file1]
        assert after_update_info.size == len(updated_content1.encode("utf-8"))
        _assert_modified_updated(before_update_info.modified_at, after_update_info.modified_at, min_delta_ms=1)

        logger.info("Step 8: Replace file contents via API (replace_contents)")
        before_replace_info = after_update_info
        await asyncio.sleep(0.05)
        replace_entry = ContentReplaceEntry(
            path=test_file1,
            old_content="Appended line to file1",
            new_content="Replaced line in file1",
        )
        await sandbox.files.replace_contents([replace_entry])
        replaced_content1 = await sandbox.files.read_file(test_file1, encoding="utf-8")
        assert "Replaced line in file1" in replaced_content1
        assert "Appended line to file1" not in replaced_content1

        after_replace_info = (await sandbox.files.get_file_info([test_file1]))[test_file1]
        _assert_modified_updated(before_replace_info.modified_at, after_replace_info.modified_at, min_delta_ms=1)

        logger.info("Step 9: Move/rename a file via API (move_files)")
        moved_path = f"{test_dir2}/moved_file3.txt"
        await sandbox.files.move_files([MoveEntry(src=test_file3, dest=moved_path)])
        moved_bytes = await sandbox.files.read_bytes(moved_path)
        assert moved_bytes.decode("utf-8") == test_content
        with pytest.raises(Exception):
            await sandbox.files.read_bytes(test_file3)

        logger.info("Step 10: Delete file via API (delete_files)")
        await sandbox.files.delete_files([test_file2])
        with pytest.raises(Exception):
            await sandbox.files.read_file(test_file2, encoding="utf-8")

        # After move+delete, search should reflect the updated view.
        files_after = await sandbox.files.search(SearchEntry(path=test_dir1, pattern="*"))
        assert {e.path for e in files_after} == {test_file1}

        logger.info("Step 11: Delete directories recursively (delete_directories)")
        await sandbox.files.delete_directories([test_dir1, test_dir2])
        verify_dirs_deleted = await sandbox.commands.run(
            f"test ! -d {test_dir1} && test ! -d {test_dir2} && echo OK",
            opts=RunCommandOpts(working_directory="/tmp"),
        )
        for _ in range(3):
            verified = (
                verify_dirs_deleted.error is None
                and len(verify_dirs_deleted.logs.stdout) == 1
                and verify_dirs_deleted.logs.stdout[0].text == "OK"
            )
            if verified:
                break
            await asyncio.sleep(1)
            verify_dirs_deleted = await sandbox.commands.run(
                f"test ! -d {test_dir1} && test ! -d {test_dir2} && echo OK",
                opts=RunCommandOpts(working_directory="/tmp"),
            )
        assert verify_dirs_deleted.error is None
        assert len(verify_dirs_deleted.logs.stdout) == 1
        assert verify_dirs_deleted.logs.stdout[0].text == "OK"

        logger.info("TEST 3 PASSED: Basic filesystem operations test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(5)
    async def test_04_interrupt_command(self):
        """Test interrupting a long-running command."""
        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 4: Testing command interrupt")
        logger.info("=" * 80)

        init_events: list[ExecutionInit] = []
        completed_events: list[ExecutionComplete] = []
        errors: list[ExecutionError] = []
        init_received = asyncio.Event()

        async def on_init(init: ExecutionInit):
            init_events.append(init)
            init_received.set()

        async def on_execution_complete(complete: ExecutionComplete):
            completed_events.append(complete)

        async def on_error(error: ExecutionError):
            errors.append(error)

        handlers = ExecutionHandlers(
            on_init=on_init,
            on_execution_complete=on_execution_complete,
            on_error=on_error,
        )

        start = time.time()
        task = asyncio.create_task(
            sandbox.commands.run(
                "sleep 30",
                handlers=handlers,
            )
        )

        await asyncio.wait_for(init_received.wait(), timeout=15)
        assert len(init_events) == 1
        assert init_events[0].id is not None and init_events[0].id.strip()
        _assert_recent_timestamp_ms(init_events[0].timestamp)

        await sandbox.commands.interrupt(init_events[0].id)

        execution = await asyncio.wait_for(task, timeout=30)
        elapsed = time.time() - start

        assert execution is not None
        assert execution.id == init_events[0].id
        assert elapsed < 20, f"Interrupted command took too long: {elapsed:.2f}s"
        # Contract: error and complete are mutually exclusive.
        assert (len(completed_events) > 0) or (len(errors) > 0), (
            f"expected exactly one of complete/error, got complete={len(completed_events)} "
            f"error={len(errors)}"
        )
        if len(completed_events) > 0:
            assert len(completed_events) == 1
            _assert_recent_timestamp_ms(completed_events[0].timestamp, tolerance_ms=180_000)

        # Interrupt should stop the process early; most implementations surface an error and/or stderr.
        assert execution.error is not None or len(execution.logs.stderr) > 0
        if execution.error is not None:
            assert execution.error.name
            assert execution.error.value
            _assert_recent_timestamp_ms(execution.error.timestamp, tolerance_ms=180_000)

    @pytest.mark.timeout(120)
    @pytest.mark.order(6)
    async def test_05_sandbox_pause(self):
        pytest.skip("skip pause/resume e2e test")
        """Test sandbox pause operation."""
        if is_kubernetes_runtime():
            pytest.skip("Pause is not supported by the Kubernetes runtime")

        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 5: Testing sandbox pause operation")
        logger.info("=" * 80)

        # Sandbox has been exercised through tests 01-04; a brief settle is sufficient.
        await asyncio.sleep(2)
        assert await sandbox.is_healthy(), "Sandbox should be healthy before pause"

        logger.info("Requesting sandbox pause...")
        await sandbox.pause()

        start_time = time.time()
        poll_count = 0
        final_status = None

        logger.info("Polling for status change (timeout: 30s)...")
        while poll_count < 30:
            await asyncio.sleep(1)
            poll_count += 1

            info = await sandbox.get_info()
            current_status = info.status
            logger.info(f"Poll {poll_count}: Status = {current_status.state}")

            if current_status.state == "Pausing":
                continue
            else:
                final_status = current_status
                break

        assert final_status is not None, "Failed to get final status after pause operation"
        assert final_status.state == "Paused", "Sandbox should be in Paused state"

        # Verify pause semantics: execd should be unreachable.
        # The global HTTP request_timeout is 3 min, so we wrap the single
        # is_healthy() call in a short asyncio timeout.  A paused container's
        # frozen process will never reply, causing either a timeout (good) or
        # an immediate connection refusal (also good).
        try:
            healthy = await asyncio.wait_for(sandbox.is_healthy(), timeout=15)
        except asyncio.TimeoutError:
            healthy = False
        assert healthy is False, "Sandbox should be unhealthy after pause"

        elapsed_time = (time.time() - start_time) * 1000
        logger.info(f"✓ Sandbox pause confirmed in {elapsed_time:.2f} ms")

    @pytest.mark.timeout(120)
    @pytest.mark.order(7)
    async def test_06_sandbox_resume(self):
        pytest.skip("skip pause/resume e2e test")
        """Test sandbox resume operation."""
        if is_kubernetes_runtime():
            pytest.skip("Resume is not supported by the Kubernetes runtime")

        await self._ensure_sandbox_created()
        sandbox = TestSandboxE2E.sandbox

        logger.info("=" * 80)
        logger.info("TEST 6: Testing sandbox resume operation")
        logger.info("=" * 80)

        logger.info("Requesting sandbox resume...")
        resumed = await Sandbox.resume(
            sandbox_id=sandbox.id,
            connection_config=TestSandboxE2E.connection_config,
        )
        # Replace the class-held instance so subsequent operations/teardown use the resumed instance.
        TestSandboxE2E.sandbox = resumed
        sandbox = resumed

        start_time = time.time()
        poll_count = 0
        final_status = None

        logger.info("Polling for status change (timeout: 1 minute)...")
        while poll_count < 60:
            await asyncio.sleep(1)
            poll_count += 1

            info = await sandbox.get_info()
            current_status = info.status
            logger.info(f"Poll {poll_count}: Status = {current_status.state}")

            if current_status.state == "Running":
                final_status = current_status
                break

        assert final_status is not None, "Failed to get final status after resume operation"
        assert final_status.state == "Running", "Sandbox should be in Running state after resume"

        logger.info("Verifying sandbox health after resume...")
        healthy = False
        for _ in range(30):
            healthy = await sandbox.is_healthy()
            if healthy:
                break
            await asyncio.sleep(1)
        assert healthy is True, "Sandbox should be healthy after resume"

        # Minimal smoke check: after resume, the existing Sandbox instance should still be usable.
        # This helps validate that SDK re-bound its execd adapters (endpoint may change across resume).
        echo = await sandbox.commands.run("echo resume-ok")
        assert echo.error is None
        assert len(echo.logs.stdout) == 1
        assert echo.logs.stdout[0].text == "resume-ok"

        elapsed_time = (time.time() - start_time) * 1000
        logger.info(f"✓ Sandbox resume completed in {elapsed_time:.2f} ms")
        logger.info("TEST 5 PASSED: Sandbox resume operation test completed successfully")

    @pytest.mark.timeout(120)
    @pytest.mark.order(8)
    async def test_07_x_request_id_passthrough_on_server_error(self):
        request_id = f"e2e-py-server-{int(time.time() * 1000)}"
        missing_sandbox_id = f"missing-{request_id}"
        cfg = ConnectionConfig(
            domain=TEST_DOMAIN,
            api_key=TEST_API_KEY,
            request_timeout=timedelta(minutes=3),
            protocol=TEST_PROTOCOL,
            headers={"X-Request-ID": request_id},
        )

        with pytest.raises(SandboxApiException) as ei:
            connected = await Sandbox.connect(sandbox_id=missing_sandbox_id, connection_config=cfg)
            await connected.get_info()
        assert ei.value.request_id == request_id
