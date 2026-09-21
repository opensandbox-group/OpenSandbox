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
E2E tests for EXECD_ENVS file injection into bash sessions.

Creates a sandbox whose execd reads an env file (EXECD_ENVS points at
/workspace/e2e-env.list, written by the test itself through /command) and
verifies through the sync SDK that:

- variables from the file are visible to commands run inside bash sessions
  (POST /session/{id}/run) — the session env snapshot must include them
  (regression: the file was only read by the command path)
- session cwd values (create_session and run_in_session) expand $NAME against
  the session environment, which includes the file variables
- the command path (POST /command) still reads the file (pre-existing)

The execd config env blacklist (EXECD_ENVS itself, tokens, ...) is pinned by
unit tests (TestNewBashSessionEnvOverlaysFileAndKeepsBlacklist); whether the
name is visible through process inheritance depends on the runtime mode
(hardening strip vs classic), so it is not asserted here.
"""

import logging
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.models.sandboxes import SandboxImageSpec

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)

ENV_FILE = "/workspace/e2e-env.list"
ENV_DIR = "/workspace/e2e-envs"
ENV_FILE_CONTENT = f"E2E_FOO=bar-e2e\nE2E_DIR={ENV_DIR}\n"


def _stdout(result) -> str:
    return "".join(m.text for m in result.logs.stdout).strip()


class TestExecdEnvsSessionE2E:
    sandbox = None
    connection_config = None
    _setup_done = False

    @pytest.fixture(scope="class", autouse=True)
    def _sandbox(self, request):
        request.cls._ensure_sandbox_created()
        yield
        sandbox = request.cls.sandbox
        if sandbox is not None:
            try:
                sandbox.kill()
            except Exception as e:
                logger.warning("Teardown: sandbox.kill() failed: %s", e, exc_info=True)
            try:
                sandbox.close()
            except Exception as e:
                logger.warning("Teardown: sandbox.close() failed: %s", e, exc_info=True)
        cfg = request.cls.connection_config
        if cfg is not None:
            try:
                cfg.transport.close()
            except Exception:
                pass

    @classmethod
    def _ensure_sandbox_created(cls) -> None:
        if cls._setup_done:
            return

        logger.info("=" * 80)
        logger.info("SETUP: Creating sandbox with EXECD_ENVS=%s", ENV_FILE)
        logger.info("=" * 80)

        cls.connection_config = create_connection_config_sync()
        cls.sandbox = SandboxSync.create(
            image=SandboxImageSpec(get_sandbox_image()),
            resource=get_e2e_sandbox_resource(),
            connection_config=cls.connection_config,
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=30),
            metadata={"tag": "execd-envs-session-e2e"},
            env={"EXECD_ENVS": ENV_FILE},
        )

        # The env file must exist before any session is created: sessions
        # snapshot their environment at creation time.
        setup = cls.sandbox.commands.run(
            f"mkdir -p {ENV_DIR} && printf '{ENV_FILE_CONTENT}' > {ENV_FILE} && cat {ENV_FILE}"
        )
        assert setup.exit_code == 0, f"env file setup failed: {setup.error}"
        assert "E2E_FOO=bar-e2e" in _stdout(setup), _stdout(setup)

        cls._setup_done = True

    @pytest.mark.timeout(120)
    def test_session_sees_execd_envs_variable(self) -> None:
        """A file variable must be visible inside a bash session run."""
        sandbox = self.sandbox
        sid = sandbox.commands.create_session()
        try:
            result = sandbox.commands.run_in_session(sid, "printf '%s' \"$E2E_FOO\"")
            assert result.error is None, result.error
            assert result.exit_code == 0
            value = _stdout(result)
            assert value == "bar-e2e", (
                "EXECD_ENVS variable missing from session environment "
                f"(regression of the command-only env-file path), got: {value!r}"
            )
        finally:
            sandbox.commands.delete_session(sid)

    @pytest.mark.timeout(120)
    def test_session_create_cwd_expands_execd_envs_variable(self) -> None:
        """create_session(cwd=$VAR) must expand against the session env."""
        sandbox = self.sandbox
        sid = sandbox.commands.create_session(working_directory="$E2E_DIR")
        try:
            result = sandbox.commands.run_in_session(sid, "pwd")
            assert result.error is None, result.error
            assert result.exit_code == 0
            pwd_line = _stdout(result)
            assert pwd_line == ENV_DIR, (
                f"create_session cwd should expand to {ENV_DIR}, got: {pwd_line!r}"
            )
        finally:
            sandbox.commands.delete_session(sid)

    @pytest.mark.timeout(120)
    def test_session_run_cwd_expands_execd_envs_variable(self) -> None:
        """run_in_session(working_directory=$VAR) must expand against the session env."""
        sandbox = self.sandbox
        sid = sandbox.commands.create_session()
        try:
            result = sandbox.commands.run_in_session(
                sid, "pwd", working_directory="$E2E_DIR"
            )
            assert result.error is None, result.error
            assert result.exit_code == 0
            pwd_line = _stdout(result)
            assert pwd_line == ENV_DIR, (
                f"run_in_session cwd should expand to {ENV_DIR}, got: {pwd_line!r}"
            )
        finally:
            sandbox.commands.delete_session(sid)

    @pytest.mark.timeout(120)
    def test_command_path_still_reads_env_file(self) -> None:
        """The command path must keep reading the env file (pre-existing)."""
        result = self.sandbox.commands.run("printf '%s' \"$E2E_FOO\"")
        assert result.error is None, result.error
        assert result.exit_code == 0
        assert _stdout(result) == "bar-e2e", _stdout(result)
