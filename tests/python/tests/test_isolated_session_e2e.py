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
E2E tests for isolated session (OSEP-0013 bwrap namespace isolation).
"""

import logging
import time

import pytest
from opensandbox import Sandbox
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.execd import ExecutionHandlers, OutputMessage
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    DirectoryListEntry,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)
from opensandbox.models.isolated import (
    BindMount,
    CreateIsolatedSessionRequest,
    IsolatedRunOpts,
    IsolatedWorkspaceSpec,
)

from tests.base_e2e_test import (
    create_connection_config,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)


@pytest.mark.asyncio
class TestIsolatedSessionE2E:
    """E2E tests for /v1/isolated/* via Python SDK."""

    sandbox: Sandbox | None = None
    overlay_supported: bool = False

    @pytest.fixture(scope="class", autouse=True)
    async def _sandbox_lifecycle(self, request):
        cls = request.cls
        config = create_connection_config()
        sandbox = await Sandbox.create(
            get_sandbox_image(),
            connection_config=config,
            resource=get_e2e_sandbox_resource(),
            extensions={"bootstrap.execd.isolation": "enable"},
        )
        cls.sandbox = sandbox

        caps = await sandbox.isolation.capabilities()
        logger.info(
            "Isolation capabilities: available=%s isolator=%s version=%s "
            "commit_supported=%s diff_supported=%s message=%s",
            caps.available, caps.isolator, caps.version,
            caps.commit_supported, caps.diff_supported, caps.message,
        )
        if not caps.available:
            pytest.fail(f"Isolation NOT available: {caps.message or 'unknown reason'}")

        cls.overlay_supported = caps.commit_supported or caps.diff_supported

        yield

        await sandbox.kill()
        await sandbox.close()

    # ── Helpers ────────────────────────────────────────────────────────

    async def _create_session(self, mode: str = "rw", path: str = "/tmp"):
        return await self.sandbox.isolation.create(
            CreateIsolatedSessionRequest(
                workspace=IsolatedWorkspaceSpec(path=path, mode=mode),
            )
        )

    def _require_overlay(self):
        if not self.overlay_supported:
            pytest.skip("overlay mode not available in this environment")

    # ── Core session tests ────────────────────────────────────────────

    async def test_capabilities(self):
        caps = await self.sandbox.isolation.capabilities()
        assert isinstance(caps.available, bool)

    async def test_session_lifecycle(self):
        session = await self._create_session()
        assert session.session_id
        assert session.info.created_at is not None

        state = await session.get()
        assert state.status == "active"

        await session.delete()

    async def test_list_sessions(self):
        session_a = await self._create_session()
        session_b = await self._create_session()
        session_b_deleted = False
        try:
            sessions = await self.sandbox.isolation.list()
            by_id = {s.session_id: s for s in sessions}

            assert session_a.session_id in by_id, "session_a should appear in list"
            assert session_b.session_id in by_id, "session_b should appear in list"
            assert by_id[session_a.session_id].status == "active"
            assert by_id[session_a.session_id].created_at is not None

            # After deleting a session it should no longer be listed.
            await session_b.delete()
            session_b_deleted = True
            remaining = await self.sandbox.isolation.list()
            remaining_ids = {s.session_id for s in remaining}
            assert session_b.session_id not in remaining_ids
        finally:
            await session_a.delete()
            if not session_b_deleted:
                await session_b.delete()

    async def test_run_echo(self):
        session = await self._create_session()
        try:
            result = await session.run("echo hello-isolation")
            assert "hello-isolation" in result.text
        finally:
            await session.delete()

    async def test_pid_isolation(self):
        session = await self._create_session()
        try:
            result = await session.run("echo $$")
            pid = int(result.text.strip())
            assert pid <= 2, f"expected PID 1 or 2 in namespace, got {pid}"
        finally:
            await session.delete()

    async def test_run_with_envs(self):
        session = await self._create_session()
        try:
            result = await session.run(
                "echo $MY_VAR",
                opts=IsolatedRunOpts(envs={"MY_VAR": "test-value-42"}),
            )
            assert "test-value-42" in result.text
        finally:
            await session.delete()

    async def test_session_state_persists(self):
        session = await self._create_session()
        try:
            await session.run("export PERSIST_VAR=abc123")
            result = await session.run("echo $PERSIST_VAR")
            assert "abc123" in result.text
        finally:
            await session.delete()

    async def test_tmp_isolation(self):
        await self.sandbox.commands.run("mkdir -p /workspace")
        session_a = await self._create_session(path="/workspace")
        session_b = await self._create_session(path="/workspace")
        try:
            await session_a.run("echo secret > /tmp/isolated_test_file.txt")
            result = await session_b.run(
                "cat /tmp/isolated_test_file.txt 2>&1 || echo NOT_FOUND"
            )
            assert "NOT_FOUND" in result.text or "No such file" in result.text
        finally:
            await session_a.delete()
            await session_b.delete()

    async def test_run_with_handlers(self):
        collected: list[str] = []

        async def on_stdout(msg: OutputMessage):
            collected.append(msg.text)

        session = await self._create_session()
        try:
            await session.run(
                "echo handler-test",
                handlers=ExecutionHandlers(on_stdout=on_stdout),
            )
            combined = "".join(collected)
            assert "handler-test" in combined
        finally:
            await session.delete()

    async def test_delete_nonexistent_session(self):
        with pytest.raises(SandboxApiException):
            fake_session = await self._create_session()
            await fake_session.delete()
            await fake_session.delete()  # second delete should fail

    # ── RW mode: run-based file tests ─────────────────────────────────

    async def test_rw_files_write_via_run(self):
        session = await self._create_session(mode="rw")
        try:
            await session.run("echo 'hello from sdk' > /tmp/hello.txt")
            result = await session.run("cat /tmp/hello.txt")
            assert "hello from sdk" in result.text
        finally:
            await session.delete()

    async def test_rw_files_persistence_across_runs(self):
        session = await self._create_session(mode="rw")
        try:
            await session.run("echo run1-data > /tmp/persist.txt")
            await session.run("mkdir -p /tmp/subdir && echo nested > /tmp/subdir/file.txt")
            result = await session.run("cat /tmp/persist.txt && cat /tmp/subdir/file.txt")
            assert "run1-data" in result.text
            assert "nested" in result.text
        finally:
            await session.delete()

    async def test_rw_host_visible(self):
        """RW mode: writes inside session are visible on host."""
        marker = f"rw_visible_{int(time.time() * 1000)}.txt"
        session = await self._create_session(mode="rw")
        try:
            await session.run(f"echo rw-data > /tmp/{marker}")
            host_check = await self.sandbox.commands.run(f"cat /tmp/{marker}")
            assert "rw-data" in host_check.text
        finally:
            await session.run(f"rm -f /tmp/{marker}")
            await session.delete()

    # ── RW mode: filesystem API tests ─────────────────────────────────

    async def test_rw_files_upload_download(self):
        session = await self._create_session(mode="rw")
        try:
            path = f"/tmp/upload_rw_{int(time.time() * 1000)}.txt"
            await session.files.write_files([WriteEntry(path=path, data="rw upload", mode=644)])
            content = await session.files.read_file(path, encoding="utf-8")
            assert content == "rw upload"
        finally:
            await session.delete()

    async def test_rw_files_read_bytes(self):
        session = await self._create_session(mode="rw")
        try:
            path = "/tmp/bytes_rw.bin"
            data = b"\x00\x01\x02\xff"
            await session.files.write_files([WriteEntry(path=path, data=data, mode=644)])
            assert await session.files.read_bytes(path) == data
        finally:
            await session.delete()

    async def test_rw_files_info(self):
        session = await self._create_session(mode="rw")
        try:
            path = "/tmp/info_rw.txt"
            await session.files.write_files([WriteEntry(path=path, data="info", mode=644)])
            info_map = await session.files.get_file_info([path])
            assert path in info_map
            assert info_map[path].size == 4
            assert info_map[path].mode == 644
        finally:
            await session.delete()

    async def test_rw_files_search(self):
        session = await self._create_session(mode="rw")
        try:
            prefix = f"/tmp/search_rw_{int(time.time() * 1000)}"
            await session.run(f"mkdir -p {prefix}")
            await session.files.write_files([
                WriteEntry(path=f"{prefix}/a.txt", data="a", mode=644),
                WriteEntry(path=f"{prefix}/b.txt", data="b", mode=644),
                WriteEntry(path=f"{prefix}/c.log", data="c", mode=644),
            ])
            results = await session.files.search(SearchEntry(path=prefix, pattern="*.txt"))
            assert len(results) == 2
            paths = [r.path for r in results]
            assert any("a.txt" in p for p in paths)
            assert any("b.txt" in p for p in paths)
        finally:
            await session.delete()

    async def test_rw_files_mkdir(self):
        session = await self._create_session(mode="rw")
        try:
            d = f"/tmp/mkdir_rw_{int(time.time() * 1000)}"
            await session.files.create_directories([WriteEntry(path=d, mode=755)])
            info_map = await session.files.get_file_info([d])
            assert d in info_map
        finally:
            await session.delete()

    async def test_rw_files_delete(self):
        session = await self._create_session(mode="rw")
        try:
            path = "/tmp/delete_rw.txt"
            await session.files.write_files([WriteEntry(path=path, data="del", mode=644)])
            await session.files.delete_files([path])
            with pytest.raises(SandboxApiException):
                await session.files.get_file_info([path])
        finally:
            await session.delete()

    async def test_rw_files_move(self):
        session = await self._create_session(mode="rw")
        try:
            src, dst = "/tmp/mv_rw_src.txt", "/tmp/mv_rw_dst.txt"
            await session.files.write_files([WriteEntry(path=src, data="move", mode=644)])
            await session.files.move_files([MoveEntry(source=src, destination=dst)])
            assert await session.files.read_file(dst, encoding="utf-8") == "move"
        finally:
            await session.delete()

    async def test_rw_files_chmod(self):
        session = await self._create_session(mode="rw")
        try:
            path = "/tmp/chmod_rw.txt"
            await session.files.write_files([WriteEntry(path=path, data="ch", mode=644)])
            await session.files.set_permissions([SetPermissionEntry(path=path, mode=755)])
            info_map = await session.files.get_file_info([path])
            assert info_map[path].mode == 755
        finally:
            await session.delete()

    async def test_rw_files_replace(self):
        session = await self._create_session(mode="rw")
        try:
            path = "/tmp/replace_rw.txt"
            await session.files.write_files([WriteEntry(path=path, data="hello old world", mode=644)])
            await session.files.replace_contents([
                ContentReplaceEntry(path=path, old_content="old", new_content="new")
            ])
            content = await session.files.read_file(path, encoding="utf-8")
            assert "new" in content and "old" not in content
        finally:
            await session.delete()

    async def test_rw_files_list_directory(self):
        session = await self._create_session(mode="rw")
        try:
            prefix = f"/tmp/listdir_rw_{int(time.time() * 1000)}"
            await session.run(f"mkdir -p {prefix}/sub")
            await session.files.write_files([
                WriteEntry(path=f"{prefix}/f1.txt", data="f1", mode=644),
                WriteEntry(path=f"{prefix}/sub/f2.txt", data="f2", mode=644),
            ])
            entries = await session.files.list_directory(DirectoryListEntry(path=prefix, depth=1))
            names = [e.path for e in entries]
            assert any("f1.txt" in n for n in names)
            assert any("sub" in n for n in names)
        finally:
            await session.delete()

    # ── RO mode tests ─────────────────────────────────────────────────

    async def test_ro_can_read_existing_files(self):
        """RO mode: session can read files that exist on host."""
        marker = f"ro_read_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo ro-data > /tmp/{marker}")
        session = await self._create_session(mode="ro")
        try:
            result = await session.run(f"cat /tmp/{marker}")
            assert "ro-data" in result.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_ro_cannot_write(self):
        """RO mode: writes to workspace are denied."""
        session = await self._create_session(mode="ro")
        try:
            result = await session.run(
                "echo fail > /tmp/ro_write_test.txt 2>&1; echo EXIT=$?"
            )
            assert "EXIT=1" in result.text or "Read-only" in result.text or "Permission denied" in result.text
        finally:
            await session.delete()

    async def test_ro_files_api_read(self):
        """RO mode: filesystem API can read existing files."""
        marker = f"ro_api_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo ro-api-data > /tmp/{marker}")
        session = await self._create_session(mode="ro")
        try:
            content = await session.files.read_file(f"/tmp/{marker}", encoding="utf-8")
            assert "ro-api-data" in content
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_ro_files_api_search(self):
        """RO mode: search works on read-only workspace."""
        prefix = f"/tmp/ro_search_{int(time.time() * 1000)}"
        await self.sandbox.commands.run(f"mkdir -p {prefix} && echo x > {prefix}/file.txt")
        session = await self._create_session(mode="ro")
        try:
            results = await session.files.search(SearchEntry(path=prefix, pattern="*.txt"))
            assert len(results) >= 1
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {prefix}")

    async def test_ro_files_api_list_directory(self):
        """RO mode: list directory works."""
        prefix = f"/tmp/ro_listdir_{int(time.time() * 1000)}"
        await self.sandbox.commands.run(f"mkdir -p {prefix} && echo x > {prefix}/f.txt")
        session = await self._create_session(mode="ro")
        try:
            entries = await session.files.list_directory(DirectoryListEntry(path=prefix, depth=1))
            assert len(entries) >= 1
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {prefix}")

    # ── Overlay mode tests ────────────────────────────────────────────

    async def test_overlay_writes_not_visible_on_host(self):
        """Overlay mode: writes inside session are NOT visible on host."""
        self._require_overlay()
        marker = f"overlay_invis_{int(time.time() * 1000)}.txt"
        session = await self._create_session(mode="overlay")
        try:
            await session.run(f"echo overlay-data > /tmp/{marker}")
            host_check = await self.sandbox.commands.run(
                f"cat /tmp/{marker} 2>&1 || echo NOT_FOUND"
            )
            assert "NOT_FOUND" in host_check.text or "No such file" in host_check.text
        finally:
            await session.delete()

    async def test_overlay_can_read_host_files(self):
        """Overlay mode: session can read pre-existing host files (lower layer)."""
        self._require_overlay()
        marker = f"overlay_lower_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo lower-data > /tmp/{marker}")
        session = await self._create_session(mode="overlay")
        try:
            result = await session.run(f"cat /tmp/{marker}")
            assert "lower-data" in result.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_overlay_cow_does_not_mutate_host(self):
        """Overlay mode: modifying a host file does not change the original."""
        self._require_overlay()
        marker = f"overlay_cow_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo original > /tmp/{marker}")
        session = await self._create_session(mode="overlay")
        try:
            await session.run(f"echo modified > /tmp/{marker}")
            in_session = await session.run(f"cat /tmp/{marker}")
            assert "modified" in in_session.text
            host_check = await self.sandbox.commands.run(f"cat /tmp/{marker}")
            assert "original" in host_check.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_overlay_files_api_upload_download(self):
        """Overlay mode: filesystem API write/read through upper layer."""
        self._require_overlay()
        session = await self._create_session(mode="overlay")
        try:
            path = f"/tmp/ov_upload_{int(time.time() * 1000)}.txt"
            await session.files.write_files([WriteEntry(path=path, data="overlay file", mode=644)])
            content = await session.files.read_file(path, encoding="utf-8")
            assert content == "overlay file"
            # Host should NOT see it
            host_check = await self.sandbox.commands.run(f"cat {path} 2>&1 || echo NOT_FOUND")
            assert "NOT_FOUND" in host_check.text or "No such file" in host_check.text
        finally:
            await session.delete()

    async def test_overlay_files_api_search(self):
        """Overlay mode: search merges upper and lower."""
        self._require_overlay()
        prefix = f"/tmp/ov_search_{int(time.time() * 1000)}"
        await self.sandbox.commands.run(f"mkdir -p {prefix} && echo lower > {prefix}/lower.txt")
        session = await self._create_session(mode="overlay")
        try:
            await session.files.write_files([
                WriteEntry(path=f"{prefix}/upper.txt", data="upper", mode=644),
            ])
            results = await session.files.search(SearchEntry(path=prefix, pattern="*.txt"))
            paths = [r.path for r in results]
            assert any("lower.txt" in p for p in paths)
            assert any("upper.txt" in p for p in paths)
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {prefix}")

    async def test_overlay_files_api_delete(self):
        """Overlay mode: deleting a file makes it invisible via API."""
        self._require_overlay()
        prefix = f"/tmp/ov_del_{int(time.time() * 1000)}"
        await self.sandbox.commands.run(f"mkdir -p {prefix} && echo x > {prefix}/target.txt")
        session = await self._create_session(mode="overlay")
        try:
            await session.files.delete_files([f"{prefix}/target.txt"])
            with pytest.raises(SandboxApiException):
                await session.files.get_file_info([f"{prefix}/target.txt"])
            # Host file should be untouched
            host_check = await self.sandbox.commands.run(f"cat {prefix}/target.txt")
            assert "x" in host_check.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {prefix}")

    async def test_overlay_files_api_move(self):
        """Overlay mode: move works and creates whiteout for source."""
        self._require_overlay()
        session = await self._create_session(mode="overlay")
        try:
            src, dst = "/tmp/ov_mv_src.txt", "/tmp/ov_mv_dst.txt"
            await session.files.write_files([WriteEntry(path=src, data="moveme", mode=644)])
            await session.files.move_files([MoveEntry(source=src, destination=dst)])
            content = await session.files.read_file(dst, encoding="utf-8")
            assert content == "moveme"
        finally:
            await session.delete()

    async def test_overlay_files_api_chmod(self):
        """Overlay mode: chmod copies up from lower before modifying."""
        self._require_overlay()
        marker = f"ov_chmod_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo ch > /tmp/{marker} && chmod 644 /tmp/{marker}")
        session = await self._create_session(mode="overlay")
        try:
            await session.files.set_permissions([
                SetPermissionEntry(path=f"/tmp/{marker}", mode=755)
            ])
            info_map = await session.files.get_file_info([f"/tmp/{marker}"])
            assert info_map[f"/tmp/{marker}"].mode == 755
            # Host should still be 644
            host_check = await self.sandbox.commands.run(f"stat -c %a /tmp/{marker}")
            assert "644" in host_check.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_overlay_files_api_replace(self):
        """Overlay mode: replace content copies up and modifies in upper."""
        self._require_overlay()
        marker = f"ov_repl_{int(time.time() * 1000)}.txt"
        await self.sandbox.commands.run(f"echo 'hello old world' > /tmp/{marker}")
        session = await self._create_session(mode="overlay")
        try:
            await session.files.replace_contents([
                ContentReplaceEntry(path=f"/tmp/{marker}", old_content="old", new_content="new")
            ])
            content = await session.files.read_file(f"/tmp/{marker}", encoding="utf-8")
            assert "new" in content and "old" not in content
            # Host unchanged
            host_check = await self.sandbox.commands.run(f"cat /tmp/{marker}")
            assert "old" in host_check.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -f /tmp/{marker}")

    async def test_overlay_files_api_list_directory(self):
        """Overlay mode: list directory merges upper and lower entries."""
        self._require_overlay()
        prefix = f"/tmp/ov_ls_{int(time.time() * 1000)}"
        await self.sandbox.commands.run(f"mkdir -p {prefix} && echo l > {prefix}/lower.txt")
        session = await self._create_session(mode="overlay")
        try:
            await session.files.write_files([
                WriteEntry(path=f"{prefix}/upper.txt", data="u", mode=644),
            ])
            entries = await session.files.list_directory(DirectoryListEntry(path=prefix, depth=1))
            names = [e.path for e in entries]
            assert any("lower.txt" in n for n in names)
            assert any("upper.txt" in n for n in names)
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {prefix}")

    # ── run_once / session convenience method tests ──────────────────

    async def test_run_once(self):
        result = await self.sandbox.isolation.run_once(
            "echo run-once-works",
            workspace="/tmp",
            workspace_mode="rw",
        )
        assert "run-once-works" in result.text

    async def test_run_once_with_envs(self):
        result = await self.sandbox.isolation.run_once(
            "echo $RUN_ONCE_VAR",
            workspace="/tmp",
            workspace_mode="rw",
            opts=IsolatedRunOpts(envs={"RUN_ONCE_VAR": "e2e-value"}),
        )
        assert "e2e-value" in result.text

    async def test_run_once_session_cleaned_up(self):
        result = await self.sandbox.isolation.run_once(
            "echo $$",
            workspace="/tmp",
            workspace_mode="rw",
        )
        assert result.text.strip().isdigit()

    async def test_session_context_manager(self):
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
        )
        async with self.sandbox.isolation.session(request) as session:
            r1 = await session.run("export CTX_VAR=hello123")
            r2 = await session.run("echo $CTX_VAR")
            assert "hello123" in r2.text

    async def test_session_context_manager_multi_run(self):
        request = CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
        )
        async with self.sandbox.isolation.session(request) as session:
            await session.run("echo step1 > /tmp/ctx_test.txt")
            result = await session.run("cat /tmp/ctx_test.txt")
            assert "step1" in result.text

    # ── Bind mount tests (explicit source->dest binds) ─────────────────

    async def test_bind_read_write_host_visible(self):
        """Legal RW bind: sandbox writes at dest are visible on host at source."""
        ts = int(time.time() * 1000)
        # Source must be within the execd writable allowlist (e.g. /data).
        src_dir = f"/data/bind_rw_{ts}"
        dest = "/mnt/bind_rw"
        file_name = "from_sandbox.txt"
        content = "bind-rw-visible-on-host"

        # Create the source dir and the destination mount point (bwrap binds
        # onto an existing dir; it cannot create one under the read-only root).
        await self.sandbox.commands.run(f"mkdir -p {src_dir} {dest}")

        session = await self.sandbox.isolation.create(
            CreateIsolatedSessionRequest(
                workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
                binds=[BindMount(source=src_dir, dest=dest)],
            )
        )
        try:
            result = await session.run(
                f"echo -n {content} > {dest}/{file_name} && cat {dest}/{file_name}"
            )
            assert content in result.text

            host_check = await self.sandbox.commands.run(f"cat {src_dir}/{file_name}")
            assert content in host_check.text
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {src_dir}")

    async def test_bind_illegal_rejected(self):
        """Bind whose source is outside the allowlist is rejected on create."""
        with pytest.raises(SandboxApiException):
            await self.sandbox.isolation.create(
                CreateIsolatedSessionRequest(
                    workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
                    # /etc is not in the writable allowlist.
                    binds=[BindMount(source="/etc", dest="/mnt/etc")],
                )
            )

    async def test_bind_read_only_readable(self):
        """Read-only bind: host-created file is readable inside the sandbox."""
        ts = int(time.time() * 1000)
        src_dir = f"/data/bind_ro_{ts}"
        dest = "/mnt/bind_ro"
        file_name = "host_created.txt"
        content = "bind-ro-host-content"

        await self.sandbox.commands.run(
            f"mkdir -p {src_dir} {dest} && echo -n {content} > {src_dir}/{file_name}"
        )

        session = await self.sandbox.isolation.create(
            CreateIsolatedSessionRequest(
                workspace=IsolatedWorkspaceSpec(path="/tmp", mode="rw"),
                binds=[BindMount(source=src_dir, dest=dest, readonly=True)],
            )
        )
        try:
            result = await session.run(f"cat {dest}/{file_name}")
            assert content in result.text

            write = await session.run(
                f"echo x > {dest}/newfile.txt 2>&1; echo EXIT=$?"
            )
            assert (
                "EXIT=1" in write.text
                or "Read-only" in write.text
                or "read-only" in write.text
                or "Permission denied" in write.text
            )
        finally:
            await session.delete()
            await self.sandbox.commands.run(f"rm -rf {src_dir}")
