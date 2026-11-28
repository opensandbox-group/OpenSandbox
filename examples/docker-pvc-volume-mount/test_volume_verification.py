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

import runpy
import subprocess
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock

import pytest
from opensandbox import Sandbox


def execution(stdout="", *, stderr="", exit_code=0, error=None):
    return SimpleNamespace(
        logs=SimpleNamespace(
            stdout=[SimpleNamespace(text=stdout)],
            stderr=[SimpleNamespace(text=stderr)],
        ),
        exit_code=exit_code,
        error=error,
    )


class ExampleSandbox:
    def __init__(self, fault):
        self.fault = fault
        self.commands = SimpleNamespace(run=self.run)
        self.kill = AsyncMock()
        self.closed = False

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        self.closed = True

    async def run(self, command):
        error = SimpleNamespace(name="CommandExecError", value="1")
        if command == "cat /mnt/data/marker.txt":
            if self.fault == "missing-marker":
                return execution(exit_code=1, error=error)
            if self.fault == "exit-only":
                return execution(exit_code=1)
            return execution("hello-from-named-volume\n", exit_code=None)
        if "touch /mnt/readonly/should-fail.txt" in command:
            if self.fault == "write-accepted":
                return execution()
            reason = (
                "Permission denied"
                if self.fault == "other-write-error"
                else "Read-only file system"
            )
            if "|| echo" in command:
                return execution(f"touch: {reason}\nWrite denied (expected)")
            return execution(stderr=f"touch: {reason}", exit_code=1, error=error)
        if command == "cat /mnt/shared/cross-sandbox.txt":
            return execution(
                "unexpected content"
                if self.fault == "wrong-shared-data"
                else "message-from-sandbox-a\n"
            )
        if command.startswith("test -f /mnt/training-data/marker.txt"):
            return execution(
                "FOUND" if self.fault == "visible-volume-root" else "NOT-FOUND"
            )
        return execution()


@pytest.mark.parametrize(
    ("fault", "expected_error", "count"),
    [
        (None, None, 5),
        ("missing-marker", "Required sandbox command failed", 1),
        ("exit-only", "Required sandbox command failed", 1),
        ("write-accepted", "Read-only write", 2),
        ("other-write-error", "Read-only write", 2),
        ("wrong-shared-data", "Unexpected cross-sandbox content", 4),
        ("visible-volume-root", "Unexpected subPath check output", 5),
    ],
)
def test_docker_pvc_example(fault, expected_error, count, monkeypatch, capsys):
    sandboxes = []

    async def create(**kwargs):
        sandbox = ExampleSandbox(fault)
        sandboxes.append(sandbox)
        return sandbox

    monkeypatch.setattr(Sandbox, "create", create)
    monkeypatch.setattr(subprocess, "run", Mock())
    outcome = (
        pytest.raises(RuntimeError, match=expected_error)
        if expected_error
        else nullcontext()
    )
    with outcome:
        runpy.run_path(str(Path(__file__).with_name("main.py")), run_name="__main__")

    output = capsys.readouterr().out
    assert ("All scenarios completed successfully!" in output) == (
        expected_error is None
    )
    if expected_error is None:
        assert "Write denied (expected)" in output
        assert "Cross-sandbox data sharing verified!" in output
        assert "Scenario 4 completed." in output
    assert len(sandboxes) == count
    for sandbox in sandboxes:
        sandbox.kill.assert_awaited_once()
        assert sandbox.closed
