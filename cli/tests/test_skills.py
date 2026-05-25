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

"""Tests for bundled skill install/list/show/uninstall flows."""

from __future__ import annotations

import importlib.resources
import json
from pathlib import Path
from types import SimpleNamespace

import pytest
from click import Command, Group, Option
from click.testing import CliRunner

from opensandbox_cli.commands.skills import _TARGETS, skills_group
from opensandbox_cli.main import cli
from opensandbox_cli.output import OutputFormatter
from opensandbox_cli.skill_registry import list_builtin_skills


@pytest.fixture()
def isolated_skill_targets(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    patched = {
        "claude": {
            **_TARGETS["claude"],
            "scopes": {
                "project": {
                    **_TARGETS["claude"]["scopes"]["project"],
                    "dest_dir": tmp_path / ".claude" / "skills",
                },
                "global": {
                    **_TARGETS["claude"]["scopes"]["global"],
                    "dest_dir": tmp_path / "home" / ".claude" / "skills",
                },
            },
        },
        "cursor": {
            **_TARGETS["cursor"],
            "scopes": {
                "project": {
                    **_TARGETS["cursor"]["scopes"]["project"],
                    "dest_dir": tmp_path / ".cursor" / "rules",
                },
                "global": {
                    **_TARGETS["cursor"]["scopes"]["global"],
                    "dest_dir": tmp_path / "home" / ".cursor" / "rules",
                },
            },
        },
        "codex": {
            **_TARGETS["codex"],
            "scopes": {
                "project": {
                    **_TARGETS["codex"]["scopes"]["project"],
                    "dest_dir": tmp_path / ".codex" / "skills",
                },
                "global": {
                    **_TARGETS["codex"]["scopes"]["global"],
                    "dest_dir": tmp_path / "home" / ".codex" / "skills",
                },
            },
        },
        "copilot": {
            **_TARGETS["copilot"],
            "scopes": {
                "project": {
                    **_TARGETS["copilot"]["scopes"]["project"],
                    "dest_file": tmp_path / ".github" / "copilot-instructions.md",
                },
                "global": {
                    **_TARGETS["copilot"]["scopes"]["global"],
                    "dest_file": tmp_path / "home" / ".github" / "copilot-instructions.md",
                },
            },
        },
        "windsurf": {
            **_TARGETS["windsurf"],
            "scopes": {
                "project": {
                    **_TARGETS["windsurf"]["scopes"]["project"],
                    "dest_file": tmp_path / ".windsurfrules",
                },
                "global": {
                    **_TARGETS["windsurf"]["scopes"]["global"],
                    "dest_file": tmp_path / "home" / ".windsurfrules",
                },
            },
        },
        "cline": {
            **_TARGETS["cline"],
            "scopes": {
                "project": {
                    **_TARGETS["cline"]["scopes"]["project"],
                    "dest_file": tmp_path / ".clinerules",
                },
                "global": {
                    **_TARGETS["cline"]["scopes"]["global"],
                    "dest_file": tmp_path / "home" / ".clinerules",
                },
            },
        },
        "opencode": {
            **_TARGETS["opencode"],
            "scopes": {
                "project": {
                    **_TARGETS["opencode"]["scopes"]["project"],
                    "dest_dir": tmp_path / ".agents" / "skills",
                },
                "global": {
                    **_TARGETS["opencode"]["scopes"]["global"],
                    "dest_dir": tmp_path / "home" / ".agents" / "skills",
                },
            },
        },
    }
    monkeypatch.setattr("opensandbox_cli.commands.skills._TARGETS", patched)


class TestSkillsCommands:
    def test_list_supports_json_output(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["list"],
            obj=SimpleNamespace(output=OutputFormatter("json", color=False)),
        )

        assert result.exit_code == 0
        data = json.loads(result.output)
        assert "skills" in data
        assert "targets" in data
        assert any(skill["slug"] == "sandbox-troubleshooting" for skill in data["skills"])
        assert any(skill["slug"] == "sandbox-lifecycle" for skill in data["skills"])

    def test_show_supports_json_output(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["show", "sandbox-lifecycle"],
            obj=SimpleNamespace(output=OutputFormatter("json", color=False)),
        )

        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["skill"] == "sandbox-lifecycle"
        assert data["title"] == "OpenSandbox Sandbox Lifecycle"
        assert '"defaultAction": "deny"' in data["json_shapes"]

    def test_install_without_args_prints_guidance_and_does_not_install(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(skills_group, ["install"])

        assert result.exit_code != 0
        assert "Install guidance:" in result.output
        assert "osb skills install <skill-name> --target <tool> --scope <scope>" in result.output
        assert not (tmp_path / ".claude" / "skills" / "sandbox-troubleshooting.md").exists()

    def test_install_with_skill_but_without_target_prints_guidance(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["install", "sandbox-troubleshooting"])

        assert result.exit_code != 0
        assert "Install guidance:" in result.output
        assert "Missing required option '--target'" in result.output

    def test_install_with_all_builtins_but_without_target_prints_guidance(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["install", "--all-builtins"])

        assert result.exit_code != 0
        assert "Install guidance:" in result.output
        assert "Missing required option '--target'" in result.output

    def test_install_copy_target_creates_named_skill_file(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "claude", "--scope", "project"],
        )

        assert result.exit_code == 0
        dest = tmp_path / ".claude" / "skills" / "sandbox-troubleshooting.md"
        assert dest.exists()
        content = dest.read_text(encoding="utf-8")
        assert content.startswith("---\nname: sandbox-troubleshooting")

    def test_install_codex_creates_skill_directory_with_frontmatter(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "codex", "--scope", "project"],
        )

        assert result.exit_code == 0
        dest = tmp_path / ".codex" / "skills" / "sandbox-troubleshooting" / "SKILL.md"
        content = dest.read_text(encoding="utf-8")
        assert content.startswith("---\nname: sandbox-troubleshooting")
        assert "# OpenSandbox Sandbox Troubleshooting" in content

    def test_install_reports_already_present_without_prompt(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        first = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "codex", "--scope", "project"],
        )
        assert first.exit_code == 0

        second = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "codex", "--scope", "project"],
        )

        assert second.exit_code == 0
        assert "already_present" in second.output
        dest = tmp_path / ".codex" / "skills" / "sandbox-troubleshooting" / "SKILL.md"
        assert dest.exists()

    def test_install_all_builtins_to_codex_creates_skill_directories(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "--all-builtins", "--target", "codex", "--scope", "project"],
        )

        assert result.exit_code == 0
        assert "Install plan:" in result.output
        assert "install one file per skill" in result.output
        for skill in list_builtin_skills():
            dest = tmp_path / ".codex" / "skills" / skill.slug / "SKILL.md"
            assert dest.exists()

    def test_install_supports_json_output(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "network-egress", "--target", "codex", "--scope", "project"],
            obj=SimpleNamespace(output=OutputFormatter("json", color=False)),
        )

        assert result.exit_code == 0
        data = json.loads(result.output)
        assert data["requires_restart"] is True
        assert data["operations"][0]["skill"] == "network-egress"
        assert data["operations"][0]["status"] == "installed"

    def test_install_rejects_skill_name_and_all_builtins_together(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--all-builtins"],
        )

        assert result.exit_code != 0
        assert "either a skill name or --all-builtins" in result.output

    def test_install_to_global_codex_uses_global_path(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "codex", "--scope", "global"],
        )

        assert result.exit_code == 0
        assert (tmp_path / "home" / ".codex" / "skills" / "sandbox-troubleshooting" / "SKILL.md").exists()

    def test_install_to_project_opencode_creates_skill_directory(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "network-egress", "--target", "opencode", "--scope", "project"],
        )

        assert result.exit_code == 0
        dest = tmp_path / ".agents" / "skills" / "network-egress" / "SKILL.md"
        assert dest.exists()
        assert dest.read_text(encoding="utf-8").startswith("---\nname: network-egress")

    def test_show_prints_skill_metadata_and_content(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["show", "file-operations"])

        assert result.exit_code == 0
        assert "Skill: file-operations" in result.output
        assert "Title: OpenSandbox File Operations" in result.output
        assert "When To Use:" in result.output
        assert "Quick Start:" in result.output
        assert "Minimal Closed Loops:" in result.output
        assert "Full Skill:" in result.output
        assert "osb file cat" in result.output

    def test_show_supports_new_network_egress_skill(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["show", "network-egress"])

        assert result.exit_code == 0
        assert "Skill: network-egress" in result.output
        assert "Quick Start:" in result.output
        assert "osb egress patch" in result.output

    def test_show_surfaces_json_shapes_for_lifecycle_skill(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["show", "sandbox-lifecycle"])

        assert result.exit_code == 0
        assert "JSON Shapes:" in result.output
        assert '"defaultAction": "deny"' in result.output
        assert '"mountPath": "/workspace/data"' in result.output

    def test_list_reports_all_builtins(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        result = runner.invoke(skills_group, ["list"])

        assert result.exit_code == 0
        assert "aggregate into one instructions file" in result.output
        assert "install one file per skill" in result.output
        for skill in list_builtin_skills():
            assert skill.slug in result.output

    def test_list_reports_not_installed_when_append_target_file_exists_without_marker(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        dest = tmp_path / ".github" / "copilot-instructions.md"
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text("user custom instructions\n", encoding="utf-8")

        result = runner.invoke(skills_group, ["list"])

        assert result.exit_code == 0
        assert "copilot" in result.output
        assert "not installed" in result.output

    def test_uninstall_append_target_preserves_non_skill_content(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        install_result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "copilot", "--scope", "project"],
        )
        assert install_result.exit_code == 0

        dest = tmp_path / ".github" / "copilot-instructions.md"
        original = dest.read_text(encoding="utf-8")
        dest.write_text("team rules\n\n" + original, encoding="utf-8")

        uninstall_result = runner.invoke(
            skills_group,
            ["uninstall", "sandbox-troubleshooting", "--target", "copilot", "--scope", "project"],
        )

        assert uninstall_result.exit_code == 0
        assert dest.read_text(encoding="utf-8") == "team rules\n"

    def test_uninstall_supports_json_output(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
    ) -> None:
        install_result = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "codex", "--scope", "project"],
        )
        assert install_result.exit_code == 0

        uninstall_result = runner.invoke(
            skills_group,
            ["uninstall", "sandbox-troubleshooting", "--target", "codex", "--scope", "project"],
            obj=SimpleNamespace(output=OutputFormatter("json", color=False)),
        )

        assert uninstall_result.exit_code == 0
        data = json.loads(uninstall_result.output)
        assert data["operations"][0]["status"] == "removed"

    def test_reinstall_append_target_does_not_duplicate_skill_block(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        first = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "copilot", "--scope", "project"],
        )
        assert first.exit_code == 0

        second = runner.invoke(
            skills_group,
            ["install", "sandbox-troubleshooting", "--target", "copilot", "--scope", "project", "--force"],
        )

        assert second.exit_code == 0
        dest = tmp_path / ".github" / "copilot-instructions.md"
        content = dest.read_text(encoding="utf-8")
        assert content.count("<!-- BEGIN opensandbox-sandbox-troubleshooting -->") == 1

    def test_install_all_builtins_to_copy_target_creates_new_skill_files(
        self,
        runner: CliRunner,
        isolated_skill_targets: None,
        tmp_path: Path,
    ) -> None:
        result = runner.invoke(
            skills_group,
            ["install", "--all-builtins", "--target", "claude", "--scope", "project"],
        )

        assert result.exit_code == 0
        assert "Install plan:" in result.output
        assert "install one file per skill" in result.output
        assert (tmp_path / ".claude" / "skills" / "network-egress.md").exists()
        assert (tmp_path / ".claude" / "skills" / "sandbox-troubleshooting.md").exists()


def _read_builtin_skill(package_file: str) -> str:
    resource = importlib.resources.files("opensandbox_cli") / "skills" / package_file
    return Path(str(resource)).read_text(encoding="utf-8")


def _command(path: list[str]) -> Command:
    current: Command = cli
    for part in path:
        assert isinstance(current, Group), f"{current.name} is not a command group"
        current = current.commands[part]
    return current


def _option_names(command: Command) -> set[str]:
    names: set[str] = set()
    for param in command.params:
        if isinstance(param, Option):
            names.update(param.opts)
            names.update(param.secondary_opts)
    return names


class TestSkillContentQuality:
    def test_sandbox_troubleshooting_keeps_triage_and_diagnostics_contract(self) -> None:
        content = _read_builtin_skill("opensandbox-sandbox-troubleshooting.md")

        assert "## Triage Order" in content
        assert "osb sandbox get <sandbox-id> -o json" in content
        assert "osb diagnostics events <sandbox-id> --scope lifecycle -o raw" in content
        assert "osb diagnostics events <sandbox-id> --scope runtime -o raw" in content
        assert "osb diagnostics logs <sandbox-id> --scope container -o raw" in content
        assert "## Diagnostics Streams" in content
        assert "## Evidence Semantics" in content
        assert "## URL Delivery" in content
        assert "truncated: true" in content
        assert "osb devops" not in content
        assert "## Symptom To Command Mapping" in content

    def test_builtin_skills_do_not_reference_legacy_devops_diagnostics(self) -> None:
        skills_dir = Path("src/opensandbox_cli/skills")
        forbidden = (
            "osb devops inspect",
            "osb devops summary",
            "devops inspect",
            "devops summary",
        )

        violations: list[str] = []
        for skill_path in sorted(skills_dir.glob("*.md")):
            content = skill_path.read_text(encoding="utf-8")
            for term in forbidden:
                if term in content:
                    violations.append(f"{skill_path.name}: {term}")

        assert violations == []

    def test_lifecycle_skill_keeps_json_shapes_and_health_guidance(self) -> None:
        content = _read_builtin_skill("opensandbox-sandbox-lifecycle.md")

        assert "## JSON Shapes" in content
        assert '"defaultAction": "deny"' in content
        assert '"mountPath": "/workspace/data"' in content
        assert "Prefer `health` over assuming readiness from `create` output alone" in content

    def test_sandbox_troubleshooting_keeps_cli_first_and_http_fallback_guidance(self) -> None:
        content = _read_builtin_skill("opensandbox-sandbox-troubleshooting.md")

        assert "## Operating Rules" in content
        assert "use CLI commands when `osb` is available" in content
        assert "use HTTP only when the CLI is unavailable" in content
        assert "Use raw HTTP only after domain, protocol, and API key expectations are explicit." in content
        assert "## Symptom To Command Mapping" in content


class TestSkillCliAlignment:
    def test_command_execution_skill_matches_command_cli(self) -> None:
        run_cmd = _command(["command", "run"])
        session_run_cmd = _command(["command", "session", "run"])
        logs_cmd = _command(["command", "logs"])
        session_delete_cmd = _command(["command", "session", "delete"])

        assert {"-d", "--background", "-w", "--workdir", "-t", "--timeout", "-o", "--output"} <= _option_names(run_cmd)
        assert {"-w", "--workdir", "-t", "--timeout", "-o", "--output"} <= _option_names(session_run_cmd)
        assert {"--cursor", "-o", "--output"} <= _option_names(logs_cmd)
        assert {"-o", "--output"} <= _option_names(session_delete_cmd)

    def test_sandbox_lifecycle_skill_matches_sandbox_cli(self) -> None:
        create_cmd = _command(["sandbox", "create"])
        resume_cmd = _command(["sandbox", "resume"])
        endpoint_cmd = _command(["sandbox", "endpoint"])
        metrics_cmd = _command(["sandbox", "metrics"])

        assert {
            "-i", "--image", "-t", "--timeout", "--entrypoint", "--network-policy-file",
            "--volumes-file", "--skip-health-check", "--ready-timeout", "-o", "--output",
        } <= _option_names(create_cmd)
        assert {"--skip-health-check", "--resume-timeout", "-o", "--output"} <= _option_names(resume_cmd)
        assert {"-p", "--port", "-o", "--output"} <= _option_names(endpoint_cmd)
        assert {"--watch", "-o", "--output"} <= _option_names(metrics_cmd)

    def test_file_operations_skill_matches_file_cli(self) -> None:
        expected_subcommands = {
            "cat", "write", "upload", "download", "rm", "mv", "mkdir",
            "rmdir", "search", "info", "chmod", "replace",
        }
        file_group = _command(["file"])
        assert isinstance(file_group, Group)
        assert expected_subcommands <= set(file_group.commands)

        assert {"-c", "--content", "--encoding", "--mode", "--owner", "--group", "-o", "--output"} <= _option_names(
            _command(["file", "write"])
        )
        assert {"-p", "--pattern", "-o", "--output"} <= _option_names(_command(["file", "search"]))
        assert {"--mode", "--owner", "--group", "-o", "--output"} <= _option_names(_command(["file", "chmod"]))
        assert {"--old", "--new", "-o", "--output"} <= _option_names(_command(["file", "replace"]))

    def test_network_egress_diagnostics_and_devops_skills_match_cli(self) -> None:
        patch_cmd = _command(["egress", "patch"])
        diag_logs_cmd = _command(["diagnostics", "logs"])
        diag_events_cmd = _command(["diagnostics", "events"])
        logs_cmd = _command(["devops", "logs"])
        events_cmd = _command(["devops", "events"])
        summary_cmd = _command(["devops", "summary"])

        assert {"--rule", "-o", "--output"} <= _option_names(patch_cmd)
        assert {"--scope", "-s", "-o", "--output"} <= _option_names(diag_logs_cmd)
        assert {"--scope", "-s", "-o", "--output"} <= _option_names(diag_events_cmd)
        assert {"--tail", "-n", "--since", "-s", "-o", "--output"} <= _option_names(logs_cmd)
        assert {"--limit", "-l", "-o", "--output"} <= _option_names(events_cmd)
        assert {"--tail", "-n", "--event-limit", "-o", "--output"} <= _option_names(summary_cmd)

    def test_skill_osb_examples_use_explicit_output_formats(self) -> None:
        allowed_without_output = {
            "osb --version",
        }
        skills_dir = Path("src/opensandbox_cli/skills")

        missing_output: list[str] = []
        for skill_path in sorted(skills_dir.glob("*.md")):
            in_block = False
            for line in skill_path.read_text(encoding="utf-8").splitlines():
                stripped = line.strip()
                if stripped.startswith("```bash"):
                    in_block = True
                    continue
                if in_block and stripped == "```":
                    in_block = False
                    continue
                if not in_block or not stripped.startswith("osb "):
                    continue
                if stripped in allowed_without_output:
                    continue
                if " -o " not in stripped:
                    missing_output.append(f"{skill_path.name}: {stripped}")

        assert missing_output == []
