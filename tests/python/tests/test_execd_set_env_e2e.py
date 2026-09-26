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
E2E tests for the SDK commands.set_env surface (issue #1837).

Creates a sandbox whose execd reads a *custom* env file (EXECD_ENVS points at
/workspace/setenv-e2e-env.list, so the test also proves the SDK resolves the
sandbox's EXECD_ENVS variable instead of assuming the default path) and
verifies through the sync SDK that:

- commands.set_env persists a variable that later commands see
- values round-trip verbatim for every serialization form the SDK emits:
  single-quoted (plain, `$`, newlines, `=`), double-quoted (values containing
  a single quote but no `$`), and the empty value
- the last write for a key wins (append-only file)
- variables set before session creation are visible inside bash sessions
  (the env file is read when the session snapshots its environment)

Values combining a single quote with shell-style `$NAME` sequences are
intentionally not asserted byte-exact: the runtime env-file format cannot
represent them losslessly (documented SDK behavior), as the loader expands
`$NAME` in double-quoted values (components/execd/pkg/runtime/env.go).
"""

import logging
from datetime import timedelta

import pytest
from opensandbox import SandboxSync
from opensandbox.exceptions import InvalidArgumentException
from opensandbox.models.sandboxes import SandboxImageSpec

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_e2e_sandbox_resource,
    get_sandbox_image,
)

logger = logging.getLogger(__name__)

ENV_FILE = "/workspace/setenv-e2e-env.list"

# (key, value) pairs whose values must round-trip byte-exact. Each value
# exercises exactly one serialization form (see module docstring).
ROUND_TRIP_CASES = [
    ("E2E_SETENV_SIMPLE", "bar-1"),
    ("E2E_SETENV_DOLLAR", "cost $5 ${HOME} tail"),
    ("E2E_SETENV_NEWLINE", "line1\nline2"),
    ("E2E_SETENV_EQUALS", "a=b=c"),
    ("E2E_SETENV_QUOTE", "it's fine"),
    ("E2E_SETENV_EMPTY", ""),
]


def _stdout(result) -> str:
    """Joined stdout text.

    execd emits one stdout event per line and strips the line terminator, so
    multi-line output arrives as several events; re-join them with newlines
    (same semantics as Execution.text).
    """
    return "\n".join(m.text for m in result.logs.stdout)


def _read_env_var(sandbox, key: str) -> str:
    """Echo a variable inside the sandbox, bracket-delimited."""
    result = sandbox.commands.run(f'printf "[%s]" "${{{key}}}"')
    assert result.error is None, result.error
    assert result.exit_code == 0
    out = _stdout(result)
    assert out.startswith("[") and out.endswith("]"), f"unexpected output: {out!r}"
    return out[1:-1]


class TestExecdSetEnvE2E:
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
            metadata={"tag": "execd-set-env-e2e"},
            env={"EXECD_ENVS": ENV_FILE},
        )
        cls._setup_done = True

    @pytest.mark.timeout(120)
    def test_set_env_persists_value_for_subsequent_commands(self) -> None:
        """A variable set via commands.set_env must be visible to later commands."""
        sandbox = self.sandbox
        sandbox.commands.set_env("E2E_SETENV_TOKEN", "bar-1")
        assert _read_env_var(sandbox, "E2E_SETENV_TOKEN") == "bar-1"

    @pytest.mark.timeout(240)
    @pytest.mark.parametrize("key,value", ROUND_TRIP_CASES)
    def test_set_env_values_round_trip_verbatim(self, key: str, value: str) -> None:
        """Every serialization form must round-trip the value byte-exact."""
        sandbox = self.sandbox
        sandbox.commands.set_env(key, value)
        assert _read_env_var(sandbox, key) == value, (
            f"value for {key} did not survive the env-file round trip: "
            f"expected {value!r}"
        )

    @pytest.mark.timeout(120)
    def test_set_env_last_write_wins(self) -> None:
        """The env file is append-only: the last write for a key wins."""
        sandbox = self.sandbox
        sandbox.commands.set_env("E2E_SETENV_OVERWRITE", "v1")
        assert _read_env_var(sandbox, "E2E_SETENV_OVERWRITE") == "v1"
        sandbox.commands.set_env("E2E_SETENV_OVERWRITE", "v2 with spaces")
        assert _read_env_var(sandbox, "E2E_SETENV_OVERWRITE") == "v2 with spaces"

    @pytest.mark.timeout(120)
    def test_new_session_sees_set_env_value(self) -> None:
        """Sessions snapshot their env at creation: one created after set_env
        must include the persisted variable."""
        sandbox = self.sandbox
        sandbox.commands.set_env("E2E_SETENV_SESSION", "from-set-env")
        sid = sandbox.commands.create_session()
        try:
            result = sandbox.commands.run_in_session(
                sid, 'printf "[%s]" "$E2E_SETENV_SESSION"'
            )
            assert result.error is None, result.error
            assert result.exit_code == 0
            out = _stdout(result)
            assert out == "[from-set-env]", (
                "set_env variable missing from a session created after the write, "
                f"got: {out!r}"
            )
        finally:
            sandbox.commands.delete_session(sid)

    @pytest.mark.timeout(120)
    def test_set_env_rejects_invalid_key_without_sandbox_round_trip(self) -> None:
        """Invalid keys must fail locally with InvalidArgumentException."""
        with pytest.raises(InvalidArgumentException, match="set_env key must match"):
            self.sandbox.commands.set_env("NOT-A-VALID-KEY", "value")
