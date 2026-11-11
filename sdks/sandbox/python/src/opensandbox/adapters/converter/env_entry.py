#
# Copyright 2025 The OpenSandbox Authors
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
Helpers for persisting environment variables via the sandbox env file.

The sandbox runtime loads ``KEY=VALUE`` entries from the file named by the
sandbox's ``EXECD_ENVS`` environment variable for every command and session.
These helpers build the sandbox-side command that appends an entry to that
file, and interpret the resulting execution.
"""

import re

from opensandbox.exceptions import InvalidArgumentException, SandboxException
from opensandbox.models.execd import Execution

ENV_KEY_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\Z")


def _shell_quote(value: str) -> str:
    """Quote a string as a single POSIX shell word."""
    return "'" + value.replace("'", "'\\''") + "'"


def _escape_double_quoted(value: str) -> str:
    """Escape a value for the env file's double-quoted form."""
    return (
        value.replace("\\", "\\\\")
        .replace('"', '\\"')
        .replace("\n", "\\n")
        .replace("\r", "\\r")
        .replace("\t", "\\t")
    )


def build_set_env_command(key: str, value: str) -> str:
    """Build the sandbox-side command appending ``KEY=VALUE`` to the env file.

    Values without a single quote use the env file's lossless single-quoted
    form; otherwise the double-quoted form is used (shell-style ``$NAME``
    sequences in such values may be expanded when the runtime loads the file).
    """
    if not ENV_KEY_PATTERN.match(key):
        raise InvalidArgumentException(
            f"set_env key must match {ENV_KEY_PATTERN.pattern}, got '{key}'"
        )
    if "\0" in value:
        raise InvalidArgumentException("set_env value cannot contain NUL bytes")
    if "'" in value:
        entry = f'{key}="{_escape_double_quoted(value)}"'
    else:
        entry = f"{key}='{value}'"
    return "\n".join(
        [
            (
                "if [ -z \"${EXECD_ENVS:-}\" ]; then "
                "printf '%s\\n' "
                f"'EXECD_ENVS is not set; cannot persist environment variable {key}' "
                ">&2; exit 1; fi"
            ),
            'mkdir -p "$(dirname "$EXECD_ENVS")"',
            f"printf '%s\\n' {_shell_quote(entry)} >> \"$EXECD_ENVS\"",
        ]
    )


def raise_for_set_env_failure(key: str, execution: Execution) -> None:
    """Raise :class:`SandboxException` when the append command failed.

    A foreground command only reports ``exit_code == 0`` after a confirmed
    ``execution_complete``; a missing exit code (e.g. a dropped stream) is
    treated as failure because the append was never confirmed.
    """
    failed = execution.error is not None or execution.exit_code != 0
    if not failed:
        return
    stderr_text = "".join(msg.text for msg in execution.logs.stderr).strip()
    detail = stderr_text or (execution.error.value if execution.error else "")
    message = f"commands.set_env failed for '{key}'"
    if detail:
        message = f"{message}: {detail}"
    raise SandboxException(message)
