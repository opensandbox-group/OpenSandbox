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

"""Install built-in OpenSandbox AI skills/rules for coding tools."""

from __future__ import annotations

import json
from dataclasses import asdict
from pathlib import Path
from typing import Literal, TypedDict, cast

import click

from opensandbox_cli.client import ClientContext
from opensandbox_cli.output import OutputFormatter
from opensandbox_cli.skill_registry import (
    BUILTIN_SKILLS,
    DEFAULT_SKILL,
    SkillSpec,
    extract_section,
    get_builtin_skill,
    get_builtin_skill_source,
    list_builtin_skills,
    read_skill_markdown,
    render_skill_for_target,
    split_frontmatter,
)
from opensandbox_cli.utils import handle_errors, output_option, prepare_output


class CopyScopeConfig(TypedDict):
    strategy: Literal["copy"]
    dest_dir: Path
    preserve_frontmatter: bool
    file_suffix: str | None
    dest_file_template: str | None


class AppendScopeConfig(TypedDict):
    strategy: Literal["append"]
    dest_file: Path
    preserve_frontmatter: bool


TargetScopeConfig = CopyScopeConfig | AppendScopeConfig


class TargetConfig(TypedDict):
    label: str
    scopes: dict[str, TargetScopeConfig]


_TARGETS = cast(dict[str, TargetConfig], {
    "claude": {
        "label": "Claude Code",
        "scopes": {
            "project": {
                "strategy": "copy",
                "dest_dir": Path(".claude") / "skills",
                "preserve_frontmatter": True,
            },
            "global": {
                "strategy": "copy",
                "dest_dir": Path.home() / ".claude" / "skills",
                "preserve_frontmatter": True,
            }
        },
    },
    "cursor": {
        "label": "Cursor",
        "scopes": {
            "project": {
                "strategy": "copy",
                "dest_dir": Path(".cursor") / "rules",
                "preserve_frontmatter": False,
                "file_suffix": ".mdc",
            },
            "global": {
                "strategy": "copy",
                "dest_dir": Path.home() / ".cursor" / "rules",
                "preserve_frontmatter": False,
                "file_suffix": ".mdc",
            }
        },
    },
    "codex": {
        "label": "Codex",
        "scopes": {
            "project": {
                "strategy": "copy",
                "dest_dir": Path(".codex") / "skills",
                "dest_file_template": "{slug}/SKILL.md",
                "preserve_frontmatter": True,
            },
            "global": {
                "strategy": "copy",
                "dest_dir": Path.home() / ".codex" / "skills",
                "dest_file_template": "{slug}/SKILL.md",
                "preserve_frontmatter": True,
            },
        },
    },
    "copilot": {
        "label": "GitHub Copilot",
        "scopes": {
            "project": {
                "strategy": "append",
                "dest_file": Path(".github") / "copilot-instructions.md",
                "preserve_frontmatter": False,
            },
            "global": {
                "strategy": "append",
                "dest_file": Path.home() / ".github" / "copilot-instructions.md",
                "preserve_frontmatter": False,
            }
        },
    },
    "windsurf": {
        "label": "Windsurf",
        "scopes": {
            "project": {
                "strategy": "append",
                "dest_file": Path(".windsurfrules"),
                "preserve_frontmatter": False,
            },
            "global": {
                "strategy": "append",
                "dest_file": Path.home() / ".windsurfrules",
                "preserve_frontmatter": False,
            }
        },
    },
    "cline": {
        "label": "Cline",
        "scopes": {
            "project": {
                "strategy": "append",
                "dest_file": Path(".clinerules"),
                "preserve_frontmatter": False,
            },
            "global": {
                "strategy": "append",
                "dest_file": Path.home() / ".clinerules",
                "preserve_frontmatter": False,
            }
        },
    },
    "opencode": {
        "label": "OpenCode",
        "scopes": {
            "project": {
                "strategy": "copy",
                "dest_dir": Path(".agents") / "skills",
                "dest_file_template": "{slug}/SKILL.md",
                "preserve_frontmatter": True,
            },
            "global": {
                "strategy": "copy",
                "dest_dir": Path.home() / ".agents" / "skills",
                "dest_file_template": "{slug}/SKILL.md",
                "preserve_frontmatter": True,
            },
        },
    },
})

_ALL_TARGET_NAMES = list(_TARGETS.keys())
_ALL_SKILL_NAMES = list(BUILTIN_SKILLS.keys())
_ALL_SCOPE_NAMES = ["project", "global"]
_SKILL_AREAS = {
    "sandbox-lifecycle": "Lifecycle",
    "command-execution": "Execution",
    "file-operations": "Files",
    "network-egress": "Network",
    "sandbox-troubleshooting": "Troubleshooting",
}


class InstallResult(TypedDict):
    skill: str
    target: str
    target_label: str
    scope: str
    path: str
    status: Literal["installed", "updated", "already_present"]
    requires_restart: bool


class UninstallResult(TypedDict):
    skill: str
    target: str
    target_label: str
    scope: str
    path: str
    status: Literal["removed", "not_installed"]
    requires_restart: bool


def _marker_begin(skill: SkillSpec) -> str:
    return f"<!-- BEGIN {skill.marker_id} -->"


def _marker_end(skill: SkillSpec) -> str:
    return f"<!-- END {skill.marker_id} -->"


def _get_scope_cfg(name: str, scope: str) -> TargetScopeConfig:
    target_cfg = _TARGETS[name]
    scopes = target_cfg["scopes"]
    return scopes[scope]


def _target_layout_summary(name: str, scope: str) -> str:
    cfg = _get_scope_cfg(name, scope)
    if cfg["strategy"] == "append":
        return f"aggregate into one instructions file at {cfg['dest_file']}"

    dest_dir = cfg["dest_dir"]
    template = cfg.get("dest_file_template")
    if template:
        sample_path = dest_dir / template.format(slug="<skill-name>")
        return f"install one file per skill under {sample_path}"

    suffix = cfg.get("file_suffix") or ".md"
    sample_path = dest_dir / f"<skill-name>{suffix}"
    return f"install one file per skill under {sample_path}"


def _target_destination(name: str, scope: str, skill: SkillSpec) -> Path:
    cfg = _get_scope_cfg(name, scope)
    if cfg["strategy"] == "copy":
        dest_dir = cfg["dest_dir"]
        template = cfg.get("dest_file_template") or ""
        if template:
            return dest_dir / template.format(slug=skill.slug)
        suffix = cfg.get("file_suffix") or ".md"
        return dest_dir / f"{skill.slug}{suffix}"
    return cfg["dest_file"]


def _render_for_target(name: str, scope: str, skill: SkillSpec) -> str:
    cfg = _get_scope_cfg(name, scope)
    markdown = read_skill_markdown(skill)
    preserve_frontmatter = bool(cfg.get("preserve_frontmatter", False))
    return render_skill_for_target(
        skill,
        markdown,
        preserve_frontmatter=preserve_frontmatter,
    )


def _get_output_formatter() -> OutputFormatter | None:
    ctx = click.get_current_context(silent=True)
    obj = getattr(ctx, "obj", None) if ctx else None
    output = getattr(obj, "output", None) if obj else None
    return output if isinstance(output, OutputFormatter) else None


def _prepare_skills_output(output_format: str | None) -> None:
    ctx = click.get_current_context(silent=True)
    obj = getattr(ctx, "obj", None) if ctx else None
    if isinstance(obj, ClientContext):
        prepare_output(obj, output_format, allowed=("table", "json", "yaml"), fallback="table")
        return

    existing = getattr(obj, "output", None) if obj is not None else None
    if output_format is None and isinstance(existing, OutputFormatter):
        return

    fmt = output_format or "table"
    formatter = OutputFormatter(fmt, color=False)
    if obj is not None:
        obj.output = formatter


def _emit_output(
    *,
    table_renderer,
    data: object,
) -> None:
    output = _get_output_formatter()
    if output is None or output.fmt == "table":
        table_renderer()
        return

    if output.fmt == "json":
        click.echo(json.dumps(data, indent=2, default=str))
        return

    output._print_yaml(data)


def _remove_marked_block(existing: str, skill: SkillSpec) -> str:
    begin = _marker_begin(skill)
    end = _marker_end(skill)
    if begin not in existing or end not in existing:
        return existing

    start = existing.index(begin)
    finish = existing.index(end) + len(end)
    before = existing[:start].rstrip("\n")
    after = existing[finish:].lstrip("\n")

    if before and after:
        return before + "\n\n" + after
    return before or after


def _is_installed(name: str, scope: str, skill: SkillSpec) -> bool:
    dest = _target_destination(name, scope, skill)
    if not dest.exists():
        return False

    cfg = _get_scope_cfg(name, scope)
    if cfg["strategy"] == "copy":
        return True

    content = dest.read_text(encoding="utf-8")
    return _marker_begin(skill) in content and _marker_end(skill) in content


def _build_marked_block(skill: SkillSpec, content: str) -> str:
    return (
        f"{_marker_begin(skill)}\n"
        f"{content.strip()}\n"
        f"{_marker_end(skill)}\n"
    )


def _install_copy(name: str, scope: str, skill: SkillSpec, content: str) -> tuple[str, Path]:
    dest = _target_destination(name, scope, skill)
    if not dest.exists():
        status = "installed"
    else:
        existing = dest.read_text(encoding="utf-8")
        status = "already_present" if existing == content else "updated"
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(content, encoding="utf-8")
    return status, dest


def _install_append(name: str, scope: str, skill: SkillSpec, content: str) -> tuple[str, Path]:
    dest = _target_destination(name, scope, skill)
    dest.parent.mkdir(parents=True, exist_ok=True)

    existing = dest.read_text(encoding="utf-8") if dest.exists() else ""
    cleaned = _remove_marked_block(existing, skill).rstrip("\n")
    marked_block = _build_marked_block(skill, content)
    new_content = f"{cleaned}\n\n{marked_block}" if cleaned else marked_block
    if not existing:
        status = "installed"
    elif new_content == existing:
        status = "already_present"
    else:
        status = "updated" if _is_installed(name, scope, skill) else "installed"
    dest.write_text(new_content, encoding="utf-8")
    return status, dest


def _install_target(name: str, scope: str, skill: SkillSpec) -> tuple[str, Path]:
    content = _render_for_target(name, scope, skill)
    cfg = _get_scope_cfg(name, scope)
    if cfg["strategy"] == "copy":
        return _install_copy(name, scope, skill, content)
    return _install_append(name, scope, skill, content)


def _uninstall_target(name: str, scope: str, skill: SkillSpec) -> tuple[bool, Path]:
    dest = _target_destination(name, scope, skill)
    if not dest.exists():
        return False, dest

    cfg = _get_scope_cfg(name, scope)
    if cfg["strategy"] == "copy":
        dest.unlink()
        if dest.parent.exists() and not any(dest.parent.iterdir()):
            dest.parent.rmdir()
        return True, dest

    existing = dest.read_text(encoding="utf-8")
    cleaned = _remove_marked_block(existing, skill)
    if cleaned == existing:
        return False, dest
    if cleaned.strip():
        dest.write_text(cleaned.rstrip("\n") + "\n", encoding="utf-8")
    else:
        dest.unlink()
    return True, dest


def _resolve_skills(skill_name: str | None, install_all_builtins: bool) -> list[SkillSpec]:
    if install_all_builtins:
        return list_builtin_skills()
    if not skill_name:
        raise click.UsageError("A skill name is required unless --all-builtins is used.")
    return [get_builtin_skill(skill_name)]


def _install_guidance_text() -> str:
    return (
        "Install guidance:\n\n"
        "  Discover bundled skills first:\n"
        "    osb skills list\n"
        "    osb skills show <skill-name>\n\n"
        "  Install one skill for one tool:\n"
        "    osb skills install <skill-name> --target <tool> --scope <scope>\n\n"
        "  Install all bundled skills for one tool:\n"
        "    osb skills install --all-builtins --target <tool> --scope <scope>\n\n"
        "  Discover skills and targets:\n"
        "    osb skills list\n"
        "    osb skills show <skill-name>\n\n"
        f"  Available skills: {', '.join(_ALL_SKILL_NAMES)}\n"
        f"  Available targets: {', '.join(_ALL_TARGET_NAMES)}\n"
        f"  Available scopes: {', '.join(_ALL_SCOPE_NAMES)}"
    )


@click.group("skills", invoke_without_command=True)
@click.pass_context
def skills_group(ctx: click.Context) -> None:
    """Manage bundled OpenSandbox skills for AI coding tools.

    Discover with `osb skills list`, inspect with `osb skills show <skill>`,
    then install non-interactively with
    `osb skills install <skill> --target codex --scope project`.
    """
    if ctx.invoked_subcommand is None:
        click.echo(ctx.get_help())


@skills_group.command("install")
@click.argument(
    "skill_name",
    required=False,
    type=click.Choice(_ALL_SKILL_NAMES, case_sensitive=False),
)
@click.option(
    "--all-builtins",
    is_flag=True,
    default=False,
    help="Install all bundled skills instead of a single skill.",
)
@click.option(
    "--target",
    "-t",
    type=click.Choice(_ALL_TARGET_NAMES + ["all"], case_sensitive=False),
    default=None,
    help="Target AI tool to install the skill for.",
)
@click.option(
    "--scope",
    type=click.Choice(_ALL_SCOPE_NAMES, case_sensitive=False),
    default=None,
    help="Install scope for targets that support multiple locations.",
)
@click.option(
    "--force",
    "-f",
    is_flag=True,
    default=False,
    help="Accepted for compatibility. Installs are already non-interactive and idempotent.",
)
@output_option("table", "json", "yaml")
@handle_errors
def skills_install(
    skill_name: str | None,
    all_builtins: bool,
    target: str | None,
    scope: str | None,
    force: bool,
    output_format: str | None,
) -> None:
    """Install one or more bundled OpenSandbox skills.

    This command is non-interactive and idempotent. Re-running an install will
    report `already_present` or `updated` instead of prompting.
    """
    _prepare_skills_output(output_format)
    if all_builtins and skill_name:
        raise click.UsageError("Pass either a skill name or --all-builtins, not both.")
    if target is None:
        raise click.UsageError(
            "Missing required option '--target'.\n\n" + _install_guidance_text()
        )
    if scope is None:
        raise click.UsageError(
            "Missing required option '--scope'.\n\n" + _install_guidance_text()
        )
    if not all_builtins and skill_name is None:
        raise click.UsageError(
            "A skill name is required unless --all-builtins is used.\n\n" + _install_guidance_text()
        )
    _ = force

    skills = _resolve_skills(skill_name, all_builtins)
    targets = _ALL_TARGET_NAMES if target == "all" else [target]
    results: list[InstallResult] = []

    for skill in skills:
        for target_name in targets:
            label = str(_TARGETS[target_name]["label"])
            status, installed_path = _install_target(target_name, scope, skill)
            results.append(
                {
                    "skill": skill.slug,
                    "target": target_name,
                    "target_label": label,
                    "scope": scope,
                    "path": str(installed_path),
                    "status": cast(Literal["installed", "updated", "already_present"], status),
                    "requires_restart": True,
                }
            )

    def _render_table() -> None:
        click.echo("Install plan:\n")
        for target_name in targets:
            label = str(_TARGETS[target_name]["label"])
            click.echo(f"  {label} [{scope}]: {_target_layout_summary(target_name, scope)}")
        click.echo()

        for result in results:
            click.echo(
                f"  {result['status']:<15} "
                f"{result['target_label']} [{result['scope']}]: "
                f"{result['skill']} -> {result['path']}"
            )

        click.echo()
        click.echo("Done! Restart your AI coding tool to pick up the updated skill set.")

    _emit_output(
        table_renderer=_render_table,
        data={
            "operations": results,
            "requires_restart": True,
        },
    )


@skills_group.command("show")
@click.argument(
    "skill_name",
    type=click.Choice(_ALL_SKILL_NAMES, case_sensitive=False),
)
@output_option("table", "json", "yaml")
@handle_errors
def skills_show(skill_name: str, output_format: str | None) -> None:
    """Show details for a bundled skill."""
    _prepare_skills_output(output_format)
    skill = get_builtin_skill(skill_name)
    markdown = read_skill_markdown(skill)
    _, body = split_frontmatter(markdown)
    when_to_use = extract_section(body, "When To Use")
    quick_start = None
    for heading in (
        "Triage Order",
        "Golden Paths",
        "Core Workflow",
        "Command Map",
        "Common Commands",
        "Fast Path",
        "Inspect Current Policy",
        "Preferred Workflow",
    ):
        quick_start = extract_section(body, heading)
        if quick_start:
            break

    json_shapes = None
    if "```json" in body:
        start = body.find("```json")
        end = body.find("```", start + 7)
        if start != -1 and end != -1:
            json_shapes = body[start + 7 : end].strip()

    payload = {
        "skill": skill.slug,
        "title": skill.title,
        "area": _SKILL_AREAS.get(skill.slug, "General"),
        "summary": skill.summary,
        "trigger_hint": skill.trigger_hint,
        "when_to_use": when_to_use,
        "quick_start": quick_start,
        "json_shapes": json_shapes,
        "content": markdown.strip(),
    }

    def _render_table() -> None:
        click.echo(f"Skill: {skill.slug}")
        click.echo(f"Title: {skill.title}")
        click.echo(f"Area: {_SKILL_AREAS.get(skill.slug, 'General')}")
        click.echo(f"Summary: {skill.summary}")
        click.echo(f"Trigger: {skill.trigger_hint}")
        click.echo()

        if when_to_use:
            click.echo("When To Use:")
            click.echo(when_to_use)
            click.echo()

        if quick_start:
            click.echo("Quick Start:")
            click.echo(quick_start)
            click.echo()

        for heading in ("Minimal Closed Loops", "Response Pattern", "Guidance"):
            section = extract_section(body, heading)
            if section:
                click.echo(f"{heading}:")
                click.echo(section)
                click.echo()

        if json_shapes:
            click.echo("JSON Shapes:")
            click.echo(json_shapes)
            click.echo()

        click.echo("Full Skill:")
        click.echo(markdown.strip())

    _emit_output(table_renderer=_render_table, data=payload)


@skills_group.command("list")
@output_option("table", "json", "yaml")
@handle_errors
def skills_list(output_format: str | None) -> None:
    """List bundled skills, supported targets, and install status."""
    _prepare_skills_output(output_format)
    skill_rows = [
        {
            **asdict(skill),
            "area": _SKILL_AREAS.get(skill.slug, "General"),
            "source_path": str(get_builtin_skill_source(skill)),
        }
        for skill in list_builtin_skills()
    ]
    target_rows: list[dict[str, object]] = []
    for target_name, cfg in _TARGETS.items():
        label = str(cfg["label"])
        for scope_name in cfg["scopes"]:
            installed_skills = []
            for skill in list_builtin_skills():
                dest = _target_destination(target_name, scope_name, skill)
                status = "installed" if _is_installed(target_name, scope_name, skill) else "not installed"
                installed_skills.append(
                    {
                        "skill": skill.slug,
                        "status": status,
                        "path": str(dest),
                    }
                )
            target_rows.append(
                {
                    "target": target_name,
                    "scope": scope_name,
                    "label": label,
                    "layout": _target_layout_summary(target_name, scope_name),
                    "skills": installed_skills,
                }
            )

    def _render_table() -> None:
        click.echo("Bundled skills:\n")
        for skill in list_builtin_skills():
            area = _SKILL_AREAS.get(skill.slug, "General")
            click.echo(f"  {skill.slug:<24}  [{area}] {skill.summary}")
            click.echo(f"  {'':<24}  Trigger: {skill.trigger_hint}")

        click.echo("\nSupported targets:\n")
        for target_row in target_rows:
            click.echo(
                f"  {target_row['target']:<10}  {target_row['scope']:<8}  "
                f"{target_row['label']:<18}  {target_row['layout']}"
            )
            for skill_row in cast(list[dict[str, str]], target_row["skills"]):
                click.echo(
                    f"  {'':<10}  {'':<8}  {'':<18}  {skill_row['skill']:<24}  "
                    f"{skill_row['status']:<13}  ({skill_row['path']})"
                )

    _emit_output(
        table_renderer=_render_table,
        data={
            "skills": skill_rows,
            "targets": target_rows,
        },
    )


@skills_group.command("uninstall")
@click.argument(
    "skill_name",
    required=False,
    default=DEFAULT_SKILL,
    type=click.Choice(_ALL_SKILL_NAMES, case_sensitive=False),
)
@click.option(
    "--target",
    "-t",
    type=click.Choice(_ALL_TARGET_NAMES + ["all"], case_sensitive=False),
    default=None,
    help="Target AI tool to remove the skill from.",
)
@click.option(
    "--scope",
    type=click.Choice(_ALL_SCOPE_NAMES, case_sensitive=False),
    default=None,
    help="Install scope to remove from.",
)
@output_option("table", "json", "yaml")
@handle_errors
def skills_uninstall(
    skill_name: str,
    target: str | None,
    scope: str | None,
    output_format: str | None,
) -> None:
    """Remove an installed OpenSandbox skill from one or more AI tools."""
    _prepare_skills_output(output_format)
    if target is None:
        raise click.UsageError(
            "Missing required option '--target'.\n\n" + _install_guidance_text()
        )
    if scope is None:
        raise click.UsageError(
            "Missing required option '--scope'.\n\n" + _install_guidance_text()
        )
    skill = get_builtin_skill(skill_name)
    targets = _ALL_TARGET_NAMES if target == "all" else [target]
    results: list[UninstallResult] = []

    for target_name in targets:
        label = str(_TARGETS[target_name]["label"])
        removed, dest = _uninstall_target(target_name, scope, skill)
        results.append(
            {
                "skill": skill.slug,
                "target": target_name,
                "target_label": label,
                "scope": scope,
                "path": str(dest),
                "status": "removed" if removed else "not_installed",
                "requires_restart": True,
            }
        )

    def _render_table() -> None:
        for result in results:
            click.echo(
                f"  {result['status']:<15} "
                f"{result['target_label']} [{result['scope']}]: "
                f"{result['skill']} -> {result['path']}"
            )

    _emit_output(
        table_renderer=_render_table,
        data={
            "operations": results,
            "requires_restart": True,
        },
    )
