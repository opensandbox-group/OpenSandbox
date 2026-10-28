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
"""SDK contract e2e for the fast-sandbox (fsb) integration environment.

The deep stack behavior — the create wire-up, policy enforcement and update
convergence, checkpoint durability and route release, recorded-policy carry
across pause/resume and snapshots, vault transfer, idle-pause/wake, and
artifact-store hygiene — is covered by the shell verify suites under
``scripts/fast-sandbox-env/verify/``, which ``integration.sh up`` chains on
every run. This module stays deliberately thin: it exercises only the Python
SDK client contract against the live stack — template CRUD, create with a
network policy, the execd surface (commands/files/metrics), diagnostics, the
egress patch/delete methods, pause/resume state fidelity, snapshot
create/restore, and connect re-attach.

Requires the fsb integration stack to be up plus
``OPENSANDBOX_TEST_FSB_TEMPLATE_ID`` (the golden-image template built by
the env script's ``template_up`` stage):

    ./scripts/fast-sandbox-env/integration-env.sh up
    cd tests/python
    OPENSANDBOX_TEST_FSB_TEMPLATE_ID="$(cat "$WORK/template-id")" \
        uv run pytest tests/test_fsb_e2e.py

The whole module is skipped when ``OPENSANDBOX_TEST_FSB_TEMPLATE_ID`` is
unset and is excluded from the default ``make test`` run.
"""

import asyncio
import inspect
import logging
import os
import time
from datetime import timedelta

import pytest
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.manager import SandboxManager
from opensandbox.models.sandboxes import (
    NetworkPolicy,
    NetworkRule,
    SandboxOrigin,
)
from opensandbox.models.templates import (
    CreateTemplateRequest,
    TemplateFilter,
    TemplatePhase,
    TemplateReadiness,
)
from opensandbox.sandbox import Sandbox

logger = logging.getLogger(__name__)

FSB_TEMPLATE_ID = os.getenv("OPENSANDBOX_TEST_FSB_TEMPLATE_ID", "")
# Set per run by the env's sdk-e2e (native + overlaybd templates differ).
FSB_TEMPLATE_ORIGIN = os.getenv("OPENSANDBOX_TEST_FSB_TEMPLATE_ORIGIN", "fast-sandbox-env")
# alpine ships busybox wget: a real HTTPS request, so egress probes
# exercise both gates of the fsb dns+nft chain (DNS resolution and
# transport). Override with e.g. curlimages/curl if cert-verified curl
# semantics are needed (note: that image defaults to a non-root user).
FSB_TEMPLATE_IMAGE = os.getenv("OPENSANDBOX_TEST_FSB_TEMPLATE_IMAGE", "alpine:3.19")
FSB_PUBLISH_TARGET = os.getenv(
    "OPENSANDBOX_TEST_FSB_PUBLISH_TARGET", "s3://sandbox-images/publish"
)
FSB_DOMAIN = os.getenv("OPENSANDBOX_TEST_DOMAIN", "127.0.0.1:18080")
FSB_PROTOCOL = os.getenv("OPENSANDBOX_TEST_PROTOCOL", "http")
FSB_API_KEY = os.getenv("OPENSANDBOX_TEST_API_KEY", "fast-sandbox-env")
# Baked into the SDK-built template (test_01b) and verified inside a guest
# created from it: the golden image must export it at runtime.
FSB_TEMPLATE_ENV = {"FSB_E2E_MARKER": "env-baked-ok"}

pytestmark = pytest.mark.skipif(
    not FSB_TEMPLATE_ID,
    reason="OPENSANDBOX_TEST_FSB_TEMPLATE_ID is not set (requires the "
    "scripts/fast-sandbox-env integration environment)",
)

# The first create on a cold pool pulls the golden image through DART;
# the shell verify grants it a 600s ping budget.
COLD_READY_TIMEOUT = timedelta(minutes=15)
WARM_READY_TIMEOUT = timedelta(minutes=5)


def _connection_config() -> ConnectionConfig:
    return ConnectionConfig(
        domain=FSB_DOMAIN, protocol=FSB_PROTOCOL, api_key=FSB_API_KEY
    )


async def _wait_until(operation, timeout: timedelta, description: str):
    """Poll ``operation`` until it returns truthy, mirroring the shell wait_for.

    ``operation`` must be a callable (a bare coroutine cannot be re-awaited
    across poll iterations); it may be sync or return an awaitable.
    """
    if not callable(operation):
        raise TypeError(
            "_wait_until expects a callable (e.g. a lambda), not a coroutine "
            "object - a coroutine cannot be re-awaited across iterations"
        )
    deadline = time.monotonic() + timeout.total_seconds()
    start = time.monotonic()
    polls = 0
    last = None
    logger.info("[wait] %s (budget %s)", description, timeout)
    while time.monotonic() < deadline:
        polls += 1
        result = operation()
        if inspect.isawaitable(result):
            result = await result
        last = result
        if result:
            logger.info(
                "[wait] %s: satisfied (polls=%d, %.1fs)",
                description,
                polls,
                time.monotonic() - start,
            )
            return result
        logger.debug("[wait] %s: not satisfied yet (polls=%d)", description, polls)
        await asyncio.sleep(2)
    raise AssertionError(
        f"timed out after {timeout} waiting for {description}: {last!r}"
    )


async def _get_sandbox_info(manager: SandboxManager, sandbox_id: str):
    """get_sandbox_info tolerating a transient 404.

    Right after a fsb create returns, the Sandbox CR may not have propagated
    through the server's informer yet; the GET then 404s even though the
    sandbox exists and serves traffic.
    """
    try:
        return await manager.get_sandbox_info(sandbox_id)
    except SandboxApiException as exc:
        if exc.status_code == 404:
            return None
        raise


async def _state_is(manager: SandboxManager, sandbox_id: str, state: str) -> bool:
    info = await _get_sandbox_info(manager, sandbox_id)
    return info is not None and info.status.state == state


async def _http_reachable_on(sandbox: Sandbox, target: str) -> bool:
    """HTTP-level probe: a real HTTPS GET from inside the guest must succeed."""
    result = await sandbox.commands.run(
        f"curl -fsS -m 8 -o /dev/null https://{target}"
    )
    return result.error is None


async def _http_blocked_on(sandbox: Sandbox, target: str) -> bool:
    result = await sandbox.commands.run(
        f"curl -fsS -m 8 -o /dev/null https://{target}"
    )
    return result.error is not None


async def _get_snapshot(manager: SandboxManager, snapshot_id: str):
    """get_snapshot tolerating a transient 404 (same informer lag as above)."""
    try:
        return await manager.get_snapshot(snapshot_id)
    except SandboxApiException as exc:
        if exc.status_code == 404:
            return None
        raise


async def _kill_and_wait_gone(manager: SandboxManager, sandbox_id: str) -> None:
    try:
        await manager.kill_sandbox(sandbox_id)
    except Exception as exc:  # noqa: BLE001 - cleanup must never mask test failures
        logger.warning("verify cleanup: kill %s failed: %s", sandbox_id, exc)
    finally:

        async def _gone() -> bool:
            try:
                await manager.get_sandbox_info(sandbox_id)
                return False
            except SandboxApiException as exc:
                return exc.status_code == 404

        await _wait_until(_gone, timedelta(minutes=2), f"sandbox {sandbox_id} deleted")


class TestFsbE2E:
    """SDK client contract against the live fsb stack (kept deliberately
    thin — the deep stack behavior is the shell verify suites' job)."""

    @pytest.fixture(scope="class")
    async def connection_config(self):
        # Shared by the manager and every classmethod entry point
        # (create/resume), so all calls hit the integration-env server
        # instead of the SDK default localhost:8080.
        config = _connection_config().with_transport_if_missing()
        yield config
        await config.close_transport_if_owned()

    @pytest.fixture(scope="class")
    async def manager(self, connection_config) -> SandboxManager:
        async with await SandboxManager.create(connection_config) as mgr:
            yield mgr

    @pytest.mark.timeout(600)
    async def test_01_template_catalog_visible(self, manager: SandboxManager) -> None:
        info = await manager.get_template(FSB_TEMPLATE_ID)
        assert info.status.phase == TemplatePhase.SUCCEEDED
        assert info.status.manifest_ref, "template manifestRef is empty"
        assert info.metadata.get("origin") == FSB_TEMPLATE_ORIGIN

        paged = await manager.list_templates(
            TemplateFilter(metadata={"origin": FSB_TEMPLATE_ORIGIN})
        )
        assert FSB_TEMPLATE_ID in [t.template_id for t in paged.template_infos]

    @pytest.mark.timeout(1800)
    async def test_01b_template_crud(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """Full template lifecycle via the SDK: async build -> Succeeded ->
        listed under its metadata filter -> a guest created from it sees the
        baked env -> deleted (404)."""
        created = await manager.create_template(
            CreateTemplateRequest(
                image=FSB_TEMPLATE_IMAGE,
                publish=FSB_PUBLISH_TARGET,
                format="native",
                resource_limits={"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
                env=FSB_TEMPLATE_ENV,
                readiness=TemplateReadiness(warmup_seconds=15),
                metadata={"origin": "fast-sandbox-env-sdk"},
            )
        )
        try:
            assert created.status.phase == TemplatePhase.PENDING

            async def _terminal() -> bool:
                info = await manager.get_template(created.template_id)
                return info.status.phase in {TemplatePhase.SUCCEEDED, TemplatePhase.FAILED}

            await _wait_until(_terminal, timedelta(minutes=10), "build terminal")

            info = await manager.get_template(created.template_id)
            assert info.status.phase == TemplatePhase.SUCCEEDED, (
                f"template build failed: {info.status.message}"
            )
            assert info.status.manifest_ref, "template manifestRef is empty"
            assert info.image == FSB_TEMPLATE_IMAGE
            assert info.env == FSB_TEMPLATE_ENV, (
                f"template env not persisted: {info.env}"
            )

            paged = await manager.list_templates(
                TemplateFilter(metadata={"origin": "fast-sandbox-env-sdk"})
            )
            assert created.template_id in [t.template_id for t in paged.template_infos]

            # Runtime env verification: a sandbox created from this template
            # must see the baked env in the guest process environment.
            sandbox = await Sandbox.create_from_template(
                created.template_id,
                timeout=timedelta(hours=1),
                ready_timeout=COLD_READY_TIMEOUT,
                connection_config=connection_config,
            )
            try:
                result = await sandbox.commands.run("echo $FSB_E2E_MARKER")
                assert result.error is None
                marker = result.logs.stdout[0].text.strip()
                assert marker == FSB_TEMPLATE_ENV["FSB_E2E_MARKER"], (
                    f"template env not visible in the guest: "
                    f"expected {FSB_TEMPLATE_ENV['FSB_E2E_MARKER']!r}, got {marker!r}"
                )
            finally:
                await _kill_and_wait_gone(manager, sandbox.id)
        finally:
            await manager.delete_template(created.template_id)

        async def _gone() -> bool:
            try:
                await manager.get_template(created.template_id)
                return False
            except SandboxApiException as exc:
                return exc.status_code == 404

        assert await _gone()

    @pytest.mark.timeout(1200)
    async def test_02_sandbox_surface_and_egress_methods(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """SDK create-with-policy, the execd surface, diagnostics, and the
        patch/delete egress methods converging with real traffic. The
        enforcement wire-up itself (networkPolicy -> binding -> nft) is the
        shell verify's job — one reachable probe per method is enough here."""
        sandbox = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-verify"},
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[
                    NetworkRule(action="allow", target="example.com"),
                ],
            ),
        )
        logger.info("sandbox ready: id=%s origin=%s", sandbox.id, sandbox.origin)
        try:
            assert sandbox.origin == SandboxOrigin.TEMPLATE

            # Execd surface: command execution, filesystem round trip, metrics.
            result = await sandbox.commands.run("echo hello-fsb")
            assert result.error is None
            assert "hello-fsb" in result.logs.stdout[0].text
            await sandbox.files.write_file("/tmp/fsb-e2e.txt", "hello-fsb")
            assert await sandbox.files.read_file("/tmp/fsb-e2e.txt") == "hello-fsb"
            metrics = await sandbox.get_metrics()
            assert metrics.cpu_count > 0
            assert metrics.memory_total_in_mib > 0

            # Diagnostics surface: fsb serves context + inline events; the
            # docker-style container logs endpoint is unsupported (501).
            context = await manager.get_diagnostic_context(sandbox.id)
            current = await sandbox.get_diagnostic_context()
            assert context.sandbox_id == current.sandbox_id == sandbox.id
            assert context.backend_type == current.backend_type == "fast-sandbox"
            assert context.state
            for scope in ("runtime", "all"):
                for events in (
                    await manager.get_diagnostic_events(sandbox.id, scope),
                    await sandbox.get_diagnostic_events(scope),
                ):
                    assert events.sandbox_id == sandbox.id
                    assert events.kind == "events"
                    assert events.scope == scope
                    assert events.delivery == "inline"
                    assert isinstance(events.content, str)
            for get_logs in (
                lambda: manager.get_diagnostic_logs(sandbox.id, "container"),
                lambda: sandbox.get_diagnostic_logs("container"),
            ):
                with pytest.raises(SandboxApiException) as error:
                    await get_logs()
                assert error.value.status_code == 501

            # Egress methods converge with real traffic: PATCH adds an allow
            # rule (pypi.org starts serving), DELETE removes the create-carried
            # one (example.com stops serving).
            await sandbox.patch_egress_rules(
                [NetworkRule(action="allow", target="pypi.org")]
            )
            await _wait_until(
                lambda: _http_reachable_on(sandbox, "pypi.org"),
                timedelta(minutes=2),
                "PATCH enforced: pypi.org reachable",
            )
            await sandbox.delete_egress_rules(["example.com"])
            await _wait_until(
                lambda: _http_blocked_on(sandbox, "example.com"),
                timedelta(minutes=2),
                "DELETE enforced: example.com blocked",
            )
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)

    @pytest.mark.timeout(600)
    async def test_02b_network_policy_rejects_port_scoped_rules(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """Fast-sandbox enforcement does not cover port-scoped rules yet, so
        the server must fail closed with 400 on both the create-carried
        policy and the PATCH route instead of silently installing rules the
        fastlet nft path would ignore (a dropped deny rule fails open)."""
        # Create-carried policy: a rule that pairs ports with an IP target is
        # rejected the same as a port-only rule.
        with pytest.raises(SandboxApiException) as create_exc:
            await Sandbox.create_from_template(
                FSB_TEMPLATE_ID,
                timeout=timedelta(hours=1),
                ready_timeout=COLD_READY_TIMEOUT,
                connection_config=connection_config,
                metadata={"origin": "fsb-port-scope-reject"},
                network_policy=NetworkPolicy(
                    defaultAction="deny",
                    egress=[NetworkRule(action="allow", target="1.1.1.1", ports=[443])],
                ),
            )
        assert create_exc.value.status_code == 400, (
            "fsb create with a port-scoped rule must be rejected with 400"
        )

        # A policy-only sandbox so the PATCH rejection can be asserted
        # against a live fsb sandbox; killed in the finally block.
        sandbox = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fsb-port-scope-reject"},
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[NetworkRule(action="allow", target="example.com")],
            ),
        )
        try:
            for rules in (
                [NetworkRule(action="deny", ports=[25])],
                [NetworkRule(action="allow", target="1.1.1.1", ports=[443])],
            ):
                with pytest.raises(SandboxApiException) as patch_exc:
                    await sandbox.patch_egress_rules(rules)
                assert patch_exc.value.status_code == 400, (
                    "fsb PATCH with a port-scoped rule must be rejected with 400"
                )
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)

    @pytest.mark.timeout(1200)
    async def test_03_pause_resume_state_fidelity(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """Pause/resume through the SDK preserves guest state: a regular
        file, a RAM-backed tmpfs file, and continuous uptime survive the
        checkpoint round trip (a reboot would reset uptime to ~0). The
        durability semantics and recorded-policy carry are the shell
        pause/resume verify's job."""
        sandbox = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-pause"},
        )
        try:
            # Pause a RUNNING sandbox: on a warm pool the guest serves execd
            # almost immediately, and a pause fired before the Sandbox CR
            # settles loses its intent (the controller never sees it — the
            # CI run on 2026-10-21 swallowed a pause fired ~500ms after
            # create). Wait for Running first.
            await _wait_until(
                lambda: _state_is(manager, sandbox.id, "Running"),
                timedelta(minutes=3),
                "Running",
            )

            # State markers written before the checkpoint.
            await sandbox.files.write_file("/tmp/fsb-state.txt", "pause-state-check")
            await sandbox.commands.run("echo ram-state-check > /dev/shm/fsb-mem.txt")
            uptime_before = float(
                (
                    await sandbox.commands.run("cut -d' ' -f1 /proc/uptime")
                ).logs.stdout[0].text
            )

            await sandbox.pause()
            await _wait_until(
                lambda: _state_is(manager, sandbox.id, "Paused"),
                timedelta(minutes=4),
                "Paused",
            )

            # Resume advances the route generation; Sandbox.resume re-resolves
            # endpoints and reads the sandbox origin from the server header.
            resumed = await Sandbox.resume(
                sandbox.id,
                connection_config=connection_config,
                resume_timeout=WARM_READY_TIMEOUT,
            )
            assert resumed.origin == SandboxOrigin.TEMPLATE
            assert await resumed.is_healthy()

            # Checkpoint fidelity.
            assert (
                await resumed.files.read_file("/tmp/fsb-state.txt")
                == "pause-state-check"
            )
            assert (
                await resumed.commands.run("cat /dev/shm/fsb-mem.txt")
            ).logs.stdout[0].text.strip() == "ram-state-check"
            uptime_after = float(
                (
                    await resumed.commands.run("cut -d' ' -f1 /proc/uptime")
                ).logs.stdout[0].text
            )
            assert uptime_after >= uptime_before, (
                f"uptime regressed {uptime_before} -> {uptime_after}: "
                "the sandbox rebooted instead of resuming the checkpoint"
            )
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)

    @pytest.mark.timeout(1800)
    async def test_04_snapshot_restore_minimal(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """Minimal SDK snapshot round trip: create -> Ready -> restore boots
        the published artifact set (the pre-snapshot file is present in the
        restored guest). The artifact/policy carry detail is the shell
        snapshot verify's job."""
        source = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-snapshot"},
        )
        snapshot_ids: list[str] = []
        try:
            await _wait_until(
                lambda: _state_is(manager, source.id, "Running"),
                timedelta(minutes=5),
                "Running",
            )
            assert await source.is_healthy()

            # State marker written before the snapshot: a restored sandbox
            # boots the artifact set published at dump time, so the file must
            # be present in the restored guest.
            await source.files.write_file(
                "/tmp/fsb-snap-state.txt", "snapshot-state-check"
            )

            snapshot = await manager.create_snapshot(source.id, name="env-verify")
            snapshot_ids.append(snapshot.id)
            assert snapshot.status.state == "Creating"

            async def _snapshot_ready(snapshot_id: str) -> bool:
                info = await _get_snapshot(manager, snapshot_id)
                return info is not None and info.status.state == "Ready"

            await _wait_until(
                lambda: _snapshot_ready(snapshot.id),
                timedelta(minutes=5),
                f"snapshot {snapshot.id} Ready",
            )

            restore = await Sandbox.create(
                snapshot_id=snapshot.id,
                timeout=timedelta(hours=1),
                resource={"cpu": "1", "memory": "512Mi", "pids": "128"},
                connection_config=connection_config,
                ready_timeout=timedelta(minutes=10),
            )
            snapshot_ids.append(snapshot.id)
            try:
                assert await restore.is_healthy()
                assert (
                    await restore.files.read_file("/tmp/fsb-snap-state.txt")
                    == "snapshot-state-check"
                )
            finally:
                await _kill_and_wait_gone(manager, restore.id)
        finally:
            await _kill_and_wait_gone(manager, source.id)
            for snapshot_id in set(snapshot_ids):
                try:
                    await manager.delete_snapshot(snapshot_id)
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    logger.warning(
                        "verify cleanup: delete snapshot %s failed: %s",
                        snapshot_id,
                        exc,
                    )

    @pytest.mark.timeout(1200)
    async def test_05_connect_reattaches_template_sandbox(
        self, manager: SandboxManager, connection_config
    ) -> None:
        """Sandbox.connect re-attaches to a running template sandbox: the
        server header drives origin auto-detection (control-plane egress),
        and the re-attached instance fully serves execd + reads the
        persisted policy."""
        sandbox = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-connect"},
            network_policy=NetworkPolicy(
                defaultAction="deny",
                egress=[NetworkRule(action="allow", target="example.com")],
            ),
        )
        try:
            attached = await Sandbox.connect(
                sandbox.id, connection_config=connection_config
            )
            assert attached.id == sandbox.id
            assert attached.origin == SandboxOrigin.TEMPLATE
            assert await attached.is_healthy()

            # execd serves through the re-attached instance.
            result = await attached.commands.run("echo connect-ok")
            assert result.error is None
            assert "connect-ok" in result.logs.stdout[0].text

            # The policy persisted across attach.
            policy = await attached.get_egress_policy()
            assert any(r.target == "example.com" for r in policy.egress or [])
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)
