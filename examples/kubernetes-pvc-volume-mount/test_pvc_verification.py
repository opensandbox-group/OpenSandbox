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
from contextlib import nullcontext
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from opensandbox import Sandbox


@pytest.mark.parametrize(
    "output, exit_code, error, expected_error",
    [
        ("Hello from OpenSandbox!", 0, None, None),
        ("Hello from OpenSandbox!", None, None, None),
        ("unexpected content", 0, None, "Expected"),
        ("Hello from OpenSandbox!\n", 0, None, "Expected"),
        ("", 1, None, "Failed to read"),
        ("Hello from OpenSandbox!", 0, "command failed", "Failed to read"),
    ],
)
def test_pvc_verification(output, exit_code, error, expected_error, monkeypatch, capsys):
    def sandbox(text, code=0, command_error=None):
        result = SimpleNamespace(
            logs=SimpleNamespace(stdout=[SimpleNamespace(text=text)], stderr=[]),
            exit_code=code,
            error=command_error,
        )
        return SimpleNamespace(
            id="sandbox",
            commands=SimpleNamespace(run=AsyncMock(return_value=result)),
            kill=AsyncMock(),
        )

    writer = sandbox("Hello from OpenSandbox!")
    reader = sandbox(output, exit_code, error)
    monkeypatch.setattr(Sandbox, "create", AsyncMock(side_effect=[writer, reader]))

    outcome = (
        pytest.raises(RuntimeError, match=expected_error)
        if expected_error
        else nullcontext()
    )
    with outcome:
        runpy.run_path(str(Path(__file__).with_name("main.py")), run_name="__main__")

    reported_success = "All steps completed successfully!" in capsys.readouterr().out
    assert reported_success == (expected_error is None)
    writer.kill.assert_awaited_once()
    reader.kill.assert_awaited_once()
