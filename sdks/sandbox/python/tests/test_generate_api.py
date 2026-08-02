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
from __future__ import annotations

import importlib.util
import subprocess
import sys
from pathlib import Path

import pytest


SCRIPT_PATH = Path(__file__).parents[1] / "scripts" / "generate_api.py"


@pytest.fixture
def generate_api_module():
    spec = importlib.util.spec_from_file_location("generate_api", SCRIPT_PATH)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_target_execd_only_runs_isolated_generation(
    monkeypatch: pytest.MonkeyPatch, generate_api_module
) -> None:
    generated: list[str] = []
    monkeypatch.setattr(sys, "argv", [str(SCRIPT_PATH), "--target", "execd"])
    monkeypatch.setattr(
        generate_api_module.subprocess,
        "run",
        lambda *args, **kwargs: subprocess.CompletedProcess(args[0], 0, stdout=b"0.28"),
    )
    monkeypatch.setattr(
        generate_api_module,
        "generate_execd_target",
        lambda: generated.append("execd"),
    )
    monkeypatch.setattr(
        generate_api_module,
        "generate_execd_api_client",
        lambda: pytest.fail("full generation must not run for --target execd"),
    )

    generate_api_module.main()

    assert generated == ["execd"]


def test_without_target_preserves_full_generation(
    monkeypatch: pytest.MonkeyPatch, generate_api_module
) -> None:
    generated: list[str] = []
    monkeypatch.setattr(sys, "argv", [str(SCRIPT_PATH)])
    monkeypatch.setattr(
        generate_api_module.subprocess,
        "run",
        lambda *args, **kwargs: subprocess.CompletedProcess(args[0], 0, stdout=b"0.28"),
    )
    for target in ("execd", "egress", "diagnostic", "lifecycle"):
        monkeypatch.setattr(
            generate_api_module,
            f"generate_{'sandbox_lifecycle' if target == 'lifecycle' else target}_api"
            f"{'_client' if target != 'lifecycle' else ''}",
            lambda target=target: generated.append(target),
        )
    monkeypatch.setattr(
        generate_api_module,
        "post_process_generated_code",
        lambda: generated.append("post-process"),
    )

    generate_api_module.main()

    assert generated == ["execd", "egress", "diagnostic", "lifecycle", "post-process"]


def test_replace_execd_directory_restores_existing_output_when_promotion_fails(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, generate_api_module
) -> None:
    output_path = tmp_path / "execd"
    output_path.mkdir()
    (output_path / "old.py").write_text("old", encoding="utf-8")
    staged_package = tmp_path / "staged"
    staged_package.mkdir()
    (staged_package / "new.py").write_text("new", encoding="utf-8")

    path_rename = Path.rename

    def fail_staged_promotion(path: Path, target: Path) -> Path:
        if path == staged_package:
            raise OSError("promotion failed")
        return path_rename(path, target)

    monkeypatch.setattr(Path, "rename", fail_staged_promotion)

    with pytest.raises(OSError, match="promotion failed"):
        generate_api_module.replace_execd_directory(staged_package, output_path)

    assert (output_path / "old.py").read_text(encoding="utf-8") == "old"


def _prepare_execd_generation_workspace(tmp_path: Path) -> Path:
    python_dir = tmp_path / "repo" / "sdks" / "sandbox" / "python"
    (tmp_path / "repo" / "specs").mkdir(parents=True)
    (tmp_path / "repo" / "specs" / "execd-api.yaml").write_text(
        "openapi: 3.0.0", encoding="utf-8"
    )
    (python_dir / "scripts").mkdir(parents=True)
    (python_dir / "scripts" / "openapi_execd_config.yaml").write_text(
        "", encoding="utf-8"
    )
    output_path = python_dir / "src" / "opensandbox" / "api" / "execd"
    output_path.mkdir(parents=True)
    (output_path / "sentinel.py").write_text("preserve me", encoding="utf-8")
    return python_dir


def test_generate_execd_target_preserves_output_when_generation_fails(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, generate_api_module
) -> None:
    python_dir = _prepare_execd_generation_workspace(tmp_path)
    output_parent = python_dir / "src" / "opensandbox" / "api"
    monkeypatch.chdir(python_dir)

    def fail_generation(*args: object, **kwargs: object) -> None:
        raise subprocess.CalledProcessError(1, "openapi-python-client")

    monkeypatch.setattr(generate_api_module, "run_command", fail_generation)

    with pytest.raises(subprocess.CalledProcessError):
        generate_api_module.generate_execd_target()

    assert (output_parent / "execd" / "sentinel.py").read_text(encoding="utf-8") == (
        "preserve me"
    )
    assert not list(output_parent.glob(".execd-generation-*"))
    assert not list(output_parent.glob(".execd-backup-*"))


def test_generate_execd_target_preserves_output_when_package_is_missing(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, generate_api_module
) -> None:
    python_dir = _prepare_execd_generation_workspace(tmp_path)
    output_parent = python_dir / "src" / "opensandbox" / "api"
    monkeypatch.chdir(python_dir)
    monkeypatch.setattr(
        generate_api_module,
        "run_command",
        lambda *args, **kwargs: subprocess.CompletedProcess(args[0], 0),
    )

    with pytest.raises(
        RuntimeError, match="execd generator did not produce opensandbox_api_execd"
    ):
        generate_api_module.generate_execd_target()

    assert (output_parent / "execd" / "sentinel.py").read_text(encoding="utf-8") == (
        "preserve me"
    )
    assert not list(output_parent.glob(".execd-generation-*"))
    assert not list(output_parent.glob(".execd-backup-*"))
