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
"""E2E coverage for the fast-sandbox (fsb) integration environment.

Mirrors the HTTP verify stages of
``scripts/fast-sandbox-env/integration-env.sh`` using the Python SDK
instead of raw curl: template catalog visibility, template-based sandbox
creation through the signed gateway route, networkpolicy convergence
(PATCH merge + DELETE), lifecycle ops (get/list/metadata/renew),
pause/resume round trip, and the public snapshot round trip (snapshot,
re-entry fence, restore, cleanup).

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
    SandboxFilter,
    SandboxOrigin,
    SnapshotFilter,
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


async def _get_policy_tolerant(sandbox: Sandbox):
    """get_egress_policy tolerating transient 404/503 during CR propagation."""
    try:
        return await sandbox.get_egress_policy()
    except SandboxApiException as exc:
        if exc.status_code in {404, 503}:
            return None
        raise


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
    """SDK-driven conversion of the fsb integration-env verify stages."""

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
        assert info.metadata.get("origin") == "fast-sandbox-env"
        logger.info(
            "env template %s: phase=%s manifestRef=%s",
            FSB_TEMPLATE_ID,
            info.status.phase,
            info.status.manifest_ref,
        )

        paged = await manager.list_templates(
            TemplateFilter(metadata={"origin": "fast-sandbox-env"})
        )
        assert FSB_TEMPLATE_ID in [t.template_id for t in paged.template_infos]
        logger.info(
            "metadata filter hit: %d template(s), env template listed",
            len(paged.template_infos),
        )

    @pytest.mark.timeout(900)
    async def test_01b_template_crud(self, manager: SandboxManager) -> None:
        """Full template lifecycle via the SDK: async build -> Succeeded ->
        listed under its metadata filter -> deleted (404)."""
        created = await manager.create_template(
            CreateTemplateRequest(
                image=FSB_TEMPLATE_IMAGE,
                publish=FSB_PUBLISH_TARGET,
                format="native",
                resource_limits={"cpu": "1", "memory": "512Mi", "disk": "2Gi"},
                readiness=TemplateReadiness(warmup_seconds=15),
                metadata={"origin": "fast-sandbox-env-sdk"},
            )
        )
        logger.info(
            "template created: id=%s phase=%s (build runs asynchronously)",
            created.template_id,
            created.status.phase,
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
            logger.info(
                "template build Succeeded: manifestRef=%s image=%s",
                info.status.manifest_ref,
                info.image,
            )

            paged = await manager.list_templates(
                TemplateFilter(metadata={"origin": "fast-sandbox-env-sdk"})
            )
            assert created.template_id in [t.template_id for t in paged.template_infos]
            logger.info(
                "template listed under metadata filter (%d hit(s))",
                len(paged.template_infos),
            )
        finally:
            await manager.delete_template(created.template_id)
            logger.info("template deleted: %s", created.template_id)

        async def _gone() -> bool:
            try:
                await manager.get_template(created.template_id)
                return False
            except SandboxApiException as exc:
                return exc.status_code == 404

        assert await _gone()
        logger.info("template 404 confirmed after delete")

    @pytest.mark.timeout(1200)
    async def test_02_template_sandbox_lifecycle_and_policy(
        self, manager: SandboxManager, connection_config
    ) -> None:
        # verify_one_sandbox: create carries the default egress policy so the
        # networkPolicy -> egress action binding -> nft chain is exercised on
        # every create; SDK readiness is the execd /ping through the signed
        # gateway route.
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
                    NetworkRule(action="allow", target="*.opensandbox.ai"),
                ],
            ),
        )
        logger.info("sandbox ready: id=%s origin=%s", sandbox.id, sandbox.origin)
        try:
            assert sandbox.origin == SandboxOrigin.TEMPLATE

            # HTTP-level enforcement probe: busybox wget performs a real
            # HTTPS request, covering both gates of the fsb dns+nft chain
            # (DNS resolution and transport).
            async def _http_reachable(target: str) -> bool:
                result = await sandbox.commands.run(
                    f"wget -T 8 -q -O /dev/null https://{target}"
                )
                return result.error is None

            async def _http_blocked(target: str) -> bool:
                result = await sandbox.commands.run(
                    f"wget -T 8 -q -O /dev/null https://{target}"
                )
                return result.error is not None

            async def _example_com_enforced() -> bool:
                policy = await _get_policy_tolerant(sandbox)
                return policy is not None and any(
                    rule.target == "example.com" for rule in policy.egress or []
                )

            await _wait_until(
                _example_com_enforced,
                timedelta(minutes=2),
                "egress policy enforcing (networkPolicy -> action binding -> nft)",
            )
            await _wait_until(
                lambda: _http_reachable("example.com"),
                timedelta(seconds=30),
                "allowed target serves HTTPS",
            )
            await _wait_until(
                lambda: _http_blocked("pypi.org"),
                timedelta(seconds=30),
                "denied target must not serve HTTPS",
            )

            # execd surface: command execution, filesystem round trip, metrics.
            result = await sandbox.commands.run("echo hello-fsb")
            assert result.error is None
            assert "hello-fsb" in result.logs.stdout[0].text
            logger.info("execd command ok: stdout=%r", result.logs.stdout[0].text)
            await sandbox.files.write_file("/tmp/fsb-e2e.txt", "hello-fsb")
            assert await sandbox.files.read_file("/tmp/fsb-e2e.txt") == "hello-fsb"
            logger.info("execd file round trip ok: /tmp/fsb-e2e.txt")
            metrics = await sandbox.get_metrics()
            assert metrics.cpu_count > 0
            assert metrics.memory_total_in_mib > 0
            logger.info(
                "execd metrics ok: cpu=%s memory=%.1fMiB",
                metrics.cpu_count,
                metrics.memory_total_in_mib,
            )

            # verify_policy_updated, converted to the SDK merge/delete paths:
            # PATCH merges pypi.org into the binding and the egress
            # sidecar hot-swaps the nft rules; DELETE removes example.com.
            # A fresh FQDN never probed before (no stale negative DNS
            # cache) and a flat A record; the shell verify allows up to 120s
            # for the binding hot-swap to converge.
            await sandbox.patch_egress_rules(
                [NetworkRule(action="allow", target="pypi.org")]
            )

            async def _pypi_org_enforced() -> bool:
                policy = await _get_policy_tolerant(sandbox)
                return policy is not None and any(
                    rule.target == "pypi.org" for rule in policy.egress or []
                )

            await _wait_until(
                _pypi_org_enforced,
                timedelta(minutes=2),
                "policy update converged (PATCH -> ReplaceActionBindings -> egress)",
            )
            await _wait_until(
                lambda: _http_reachable("pypi.org"),
                timedelta(minutes=2),
                "newly allowed target serves HTTPS",
            )
            logger.info(
                "PATCH enforced: pypi.org reachable after allow rule merge"
            )
            await sandbox.delete_egress_rules(["example.com"])

            async def _example_com_gone() -> bool:
                policy = await _get_policy_tolerant(sandbox)
                return policy is not None and all(
                    rule.target != "example.com" for rule in policy.egress or []
                )

            await _wait_until(
                _example_com_gone,
                timedelta(minutes=2),
                "deleted rule no longer served",
            )
            await _wait_until(
                lambda: _http_blocked("example.com"),
                timedelta(minutes=2),
                "deleted target must stop serving HTTPS",
            )
            logger.info("DELETE enforced: example.com blocked after rule removal")

            # verify_lifecycle_ops: get, list, metadata merge-patch
            # (upsert + delete via null), renew-expiration.
            info = await manager.get_sandbox_info(sandbox.id)
            assert info.id == sandbox.id

            listed = await manager.list_sandbox_infos(
                SandboxFilter(page=1, page_size=50)
            )
            assert sandbox.id in [item.id for item in listed.sandbox_infos]
            logger.info(
                "list ok: %d sandbox(es), verify sandbox present",
                len(listed.sandbox_infos),
            )

            await manager.patch_sandbox_metadata(
                sandbox.id, {"env": "verify", "stage": "lifecycle-ops"}
            )

            async def _metadata_upserted() -> bool:
                current = await manager.get_sandbox_info(sandbox.id)
                return (
                    current.metadata.get("env") == "verify"
                    and current.metadata.get("stage") == "lifecycle-ops"
                )

            await _wait_until(_metadata_upserted, timedelta(minutes=1), "metadata upsert")
            await manager.patch_sandbox_metadata(sandbox.id, {"stage": None})

            async def _metadata_deleted() -> bool:
                current = await manager.get_sandbox_info(sandbox.id)
                return (
                    current.metadata.get("stage") is None
                    and current.metadata.get("env") == "verify"
                )

            await _wait_until(_metadata_deleted, timedelta(minutes=1), "metadata delete")

            before = await manager.get_sandbox_info(sandbox.id)
            renewed = await manager.renew_sandbox(sandbox.id, timedelta(hours=2))
            after = await manager.get_sandbox_info(sandbox.id)
            assert renewed.expires_at > before.expires_at
            logger.info(
                "renew ok: expiresAt %s -> %s",
                before.expires_at,
                renewed.expires_at,
            )
            # The server persists expiresAt truncated to whole seconds while
            # the renew response carries microseconds.
            assert abs(
                (after.expires_at - renewed.expires_at).total_seconds()
            ) < 1
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)

    @pytest.mark.timeout(900)
    async def test_03_pause_resume_round_trip(
        self, manager: SandboxManager, connection_config
    ) -> None:
        started = time.monotonic()
        sandbox = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-pause"},
        )
        logger.info(
            "sandbox ready: id=%s (create+ready %.1fs)", sandbox.id, time.monotonic() - started
        )
        try:
            async def _state_is(state: str) -> bool:
                info = await _get_sandbox_info(manager, sandbox.id)
                return info is not None and info.status.state == state

            await _wait_until(lambda: _state_is("Running"), timedelta(minutes=3), "Running")

            # Paused is durable-first: checkpoint complete + capacity released;
            # the signed gateway route stops serving.
            pause_started = time.monotonic()
            await sandbox.pause()
            logger.info("pause initiated: %s", sandbox.id)
            await _wait_until(lambda: _state_is("Paused"), timedelta(minutes=4), "Paused")
            logger.info(
                "Paused observed (durable-first, %.1fs after pause)",
                time.monotonic() - pause_started,
            )

            # Resume advances the route generation; Sandbox.resume re-resolves
            # endpoints and reads the sandbox origin from the server header.
            resume_started = time.monotonic()
            resumed = await Sandbox.resume(
                sandbox.id,
                connection_config=connection_config,
                resume_timeout=WARM_READY_TIMEOUT,
            )
            logger.info(
                "resumed: origin=%s healthy (restore window %.1fs incl. 503 retries)",
                resumed.origin,
                time.monotonic() - resume_started,
            )
            assert resumed.origin == SandboxOrigin.TEMPLATE
            assert await resumed.is_healthy()
        finally:
            await _kill_and_wait_gone(manager, sandbox.id)

    @pytest.mark.timeout(1500)
    async def test_04_snapshot_round_trip(
        self, manager: SandboxManager, connection_config
    ) -> None:
        source = await Sandbox.create_from_template(
            FSB_TEMPLATE_ID,
            timeout=timedelta(hours=1),
            ready_timeout=COLD_READY_TIMEOUT,
            connection_config=connection_config,
            metadata={"origin": "fast-sandbox-env-snapshot"},
        )
        snapshot_ids: list[str] = []
        try:
            async def _state_is(state: str) -> bool:
                info = await _get_sandbox_info(manager, source.id)
                return info is not None and info.status.state == state

            await _wait_until(lambda: _state_is("Running"), timedelta(minutes=5), "Running")
            assert await source.is_healthy()

            # 1. Snapshot create returns Creating; the server row converges
            # from the fast-sandbox SandboxSnapshot CR via its watcher.
            snapshot = await manager.create_snapshot(source.id, name="env-verify")
            snapshot_ids.append(snapshot.id)
            assert snapshot.status.state == "Creating"
            logger.info(
                "snapshot created: id=%s state=%s", snapshot.id, snapshot.status.state
            )

            # 2. Re-entry while the dump window holds the sandbox: fenced as
            # 409 once the CR is cache-visible, or accepted (202) during
            # fence cache lag and resolved to a terminal phase afterwards.
            extra_snapshot_id: str | None = None
            try:
                reentry = await manager.create_snapshot(
                    source.id, name="env-verify-reentry"
                )
                extra_snapshot_id = reentry.id
                snapshot_ids.append(reentry.id)
                logger.info(
                    "re-entry snapshot accepted during fence cache lag: id=%s",
                    extra_snapshot_id,
                )
            except SandboxApiException as exc:
                assert exc.status_code == 409, f"unexpected re-entry error: {exc}"
                logger.info("re-entry snapshot fenced by the pause window (409)")

            async def _snapshot_ready(snapshot_id: str) -> bool:
                info = await _get_snapshot(manager, snapshot_id)
                return info is not None and info.status.state == "Ready"

            snapshot_started = time.monotonic()
            await _wait_until(
                lambda: _snapshot_ready(snapshot.id),
                timedelta(minutes=5),
                f"snapshot {snapshot.id} Ready",
            )
            logger.info(
                "snapshot Ready (%.1fs after POST)",
                time.monotonic() - snapshot_started,
            )

            # 3. Source survival: the pause window must be released and the
            # sandbox back to serving after the snapshot went terminal.
            await _wait_until(lambda: _state_is("Running"), timedelta(minutes=2), "source Running")
            assert await source.is_healthy()
            logger.info("source sandbox survived: Running + /ping 200 after snapshot")

            if extra_snapshot_id is not None:
                async def _extra_terminal() -> bool:
                    info = await _get_snapshot(manager, extra_snapshot_id)
                    return info is not None and info.status.state in {"Ready", "Failed"}

                await _wait_until(
                    _extra_terminal,
                    timedelta(minutes=3),
                    "accepted re-entry snapshot reached a terminal phase",
                )

            # 4. Second snapshot after the first is terminal: fresh id.
            second = await manager.create_snapshot(source.id, name="env-verify-2")
            snapshot_ids.append(second.id)
            assert second.id != snapshot.id
            await _wait_until(
                lambda: _snapshot_ready(second.id),
                timedelta(minutes=5),
                f"snapshot {second.id} Ready",
            )
            logger.info("second snapshot Ready: id=%s (distinct id)", second.id)

            # 5. Listing scoped by sandboxId contains the snapshots as Ready.
            listed = await manager.list_snapshots(
                SnapshotFilter(sandbox_id=source.id, page=1, page_size=50)
            )
            ready_ids = {
                item.id
                for item in listed.snapshot_infos
                if item.status.state == "Ready"
            }
            assert snapshot.id in ready_ids
            assert second.id in ready_ids
            logger.info(
                "snapshot list ok: %d row(s), both required snapshots Ready",
                len(listed.snapshot_infos),
            )

            # 6. Restore: the restored sandbox boots the published artifact
            # set; resourceLimits must restate the pool profile.
            restore_started = time.monotonic()
            logger.info("restoring sandbox from snapshot %s ...", snapshot.id)
            restore = await Sandbox.create(
                snapshot_id=snapshot.id,
                timeout=timedelta(hours=1),
                resource={"cpu": "1", "memory": "512Mi", "pids": "128"},
                connection_config=connection_config,
                ready_timeout=timedelta(minutes=10),
            )
            try:
                assert await restore.is_healthy()
                logger.info(
                    "restore ok: %s boots the published artifact set (ready %.1fs)",
                    restore.id,
                    time.monotonic() - restore_started,
                )
            finally:
                await _kill_and_wait_gone(manager, restore.id)
        finally:
            await _kill_and_wait_gone(manager, source.id)
            logger.info(
                "cleanup: source sandbox deleted, %d snapshot row(s) removed",
                len(snapshot_ids),
            )
            for snapshot_id in snapshot_ids:
                try:
                    await manager.delete_snapshot(snapshot_id)
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    logger.warning(
                        "verify cleanup: delete snapshot %s failed: %s",
                        snapshot_id,
                        exc,
                    )
