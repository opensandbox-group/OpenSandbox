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

import errno
import importlib.util
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock

import pytest


@pytest.fixture(scope="module")
def example():
    spec = importlib.util.spec_from_file_location(
        "rclone_volume_example", Path(__file__).with_name("main.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


@pytest.fixture
def run_check(example, tmp_path):
    def run(read_only):
        script = example.verification_script("marker.txt", "expected content\n", read_only)
        script = script.replace(repr("/mnt/remote/marker.txt"), repr(str(tmp_path / "marker.txt")))
        exec(compile(script, "<volume-check>", "exec"), {})

    return run


def test_write_verifies_content_without_overwriting_existing_files(run_check, tmp_path):
    run_check(False)
    marker = tmp_path / "marker.txt"
    assert marker.read_text(encoding="utf-8") == "expected content\n"
    with pytest.raises(FileExistsError):
        run_check(False)
    assert marker.read_text(encoding="utf-8") == "expected content\n"


def test_read_only_check_rejects_content_mismatch(run_check, tmp_path):
    (tmp_path / "marker.txt").write_text("wrong content", encoding="utf-8")
    with pytest.raises(RuntimeError, match="content does not match"):
        run_check(True)


def test_read_only_check_rejects_writable_mount(run_check, tmp_path):
    run_check(False)
    with pytest.raises(RuntimeError, match="unexpectedly allowed a write"):
        run_check(True)
    assert not (tmp_path / "marker.readonly-check").exists()


@pytest.mark.parametrize("error_number", [errno.EROFS, errno.EACCES])
def test_read_only_check_requires_read_only_filesystem_error(run_check, monkeypatch, error_number):
    run_check(False)
    original_open = Path.open

    def open_file(path, mode="r", *args, **kwargs):
        if mode == "x":
            raise OSError(error_number, "write rejected")
        return original_open(path, mode, *args, **kwargs)

    monkeypatch.setattr(Path, "open", open_file)
    if error_number == errno.EROFS:
        run_check(True)
    else:
        with pytest.raises(OSError) as exc_info:
            run_check(True)
        assert exc_info.value.errno == errno.EACCES


@pytest.fixture
def sandbox(example, monkeypatch):
    instance = MagicMock()
    instance.kill = AsyncMock()
    instance.commands.run = AsyncMock()
    monkeypatch.setattr(example.Sandbox, "create", AsyncMock(return_value=instance))
    return instance


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "error,exit_code,output",
    [
        ("execution failed", 0, "volume check passed"),
        (None, 1, "volume check passed"),
        (None, 0, ""),
    ],
)
async def test_failed_command_cleans_up_and_does_not_report_success(
    example, sandbox, error, exit_code, output
):
    sandbox.commands.run.return_value = SimpleNamespace(
        error=error,
        exit_code=exit_code,
        logs=SimpleNamespace(stdout=[SimpleNamespace(text=output)], stderr=[]),
    )
    with pytest.raises(RuntimeError, match="Volume check"):
        await example.verify_mount(
            MagicMock(),
            "python:3.11",
            "rclone-data",
            "marker.txt",
            "hello",
            read_only=False,
        )
    sandbox.kill.assert_awaited_once()
    sandbox.__aexit__.assert_awaited_once()


@pytest.mark.asyncio
async def test_transport_failure_still_cleans_up(example, sandbox):
    sandbox.commands.run.side_effect = RuntimeError("connection lost")
    with pytest.raises(RuntimeError, match="connection lost"):
        await example.verify_mount(
            MagicMock(),
            "python:3.11",
            "rclone-data",
            "marker.txt",
            "hello",
            read_only=False,
        )
    sandbox.kill.assert_awaited_once()
    sandbox.__aexit__.assert_awaited_once()


@pytest.mark.asyncio
async def test_success_uses_external_read_only_volume(example, sandbox):
    sandbox.commands.run.return_value = SimpleNamespace(
        error=None,
        exit_code=0,
        logs=SimpleNamespace(stdout=[SimpleNamespace(text="volume check passed\n")], stderr=[]),
    )
    await example.verify_mount(
        MagicMock(),
        "python:3.11",
        "rclone-data",
        "marker.txt",
        "hello",
        read_only=True,
    )
    volume = example.Sandbox.create.call_args.kwargs["volumes"][0].model_dump(by_alias=True)
    assert volume["pvc"]["claimName"] == "rclone-data"
    assert volume["pvc"]["createIfNotExists"] is False
    assert volume["pvc"]["deleteOnSandboxTermination"] is False
    assert volume["readOnly"] is True
    assert volume.get("subPath") is None
    sandbox.kill.assert_awaited_once()


@pytest.mark.asyncio
async def test_missing_volume_name_fails_before_creating_sandbox(example, sandbox, monkeypatch):
    monkeypatch.delenv("SANDBOX_VOLUME_NAME", raising=False)
    with pytest.raises(ValueError, match="SANDBOX_VOLUME_NAME"):
        await example.main()
    example.Sandbox.create.assert_not_awaited()
