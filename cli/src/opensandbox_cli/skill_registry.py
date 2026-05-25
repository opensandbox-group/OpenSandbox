"""Built-in OpenSandbox skill metadata and rendering helpers."""

from __future__ import annotations

import importlib.resources
from dataclasses import dataclass
from pathlib import Path


@dataclass(frozen=True)
class SkillSpec:
    """A bundled skill shipped with the CLI."""

    slug: str
    package_file: str
    title: str
    summary: str
    trigger_hint: str
    marker_id: str


BUILTIN_SKILLS: dict[str, SkillSpec] = {
    "sandbox-troubleshooting": SkillSpec(
        slug="sandbox-troubleshooting",
        package_file="opensandbox-sandbox-troubleshooting.md",
        title="OpenSandbox Sandbox Troubleshooting",
        summary=(
            "Triage failed or unhealthy sandboxes with state, health, stable "
            "diagnostic logs/events, and concrete remediation steps."
        ),
        trigger_hint=(
            "Use when the user reports sandbox startup failures, crashes, OOM, "
            "image pull problems, pending sandboxes, or unreachable services."
        ),
        marker_id="opensandbox-sandbox-troubleshooting",
    ),
    "sandbox-lifecycle": SkillSpec(
        slug="sandbox-lifecycle",
        package_file="opensandbox-sandbox-lifecycle.md",
        title="OpenSandbox Sandbox Lifecycle",
        summary=(
            "Create, inspect, renew, pause, resume, and terminate sandboxes "
            "with the right defaults and follow-up checks."
        ),
        trigger_hint=(
            "Use when the user wants to create or manage a sandbox and needs "
            "the exact OpenSandbox CLI/API flow."
        ),
        marker_id="opensandbox-sandbox-lifecycle",
    ),
    "command-execution": SkillSpec(
        slug="command-execution",
        package_file="opensandbox-command-execution.md",
        title="OpenSandbox Command Execution",
        summary=(
            "Run foreground and background commands, inspect status/logs, and "
            "use persistent sessions inside a sandbox."
        ),
        trigger_hint=(
            "Use when the user wants to execute commands in a sandbox, collect "
            "logs, interrupt work, or reuse a persistent shell session."
        ),
        marker_id="opensandbox-command-execution",
    ),
    "file-operations": SkillSpec(
        slug="file-operations",
        package_file="opensandbox-file-operations.md",
        title="OpenSandbox File Operations",
        summary=(
            "Read, write, upload, download, search, replace, and manage files "
            "inside a sandbox without hand-wavy shell advice."
        ),
        trigger_hint=(
            "Use when the user needs file or directory manipulation inside an "
            "OpenSandbox sandbox."
        ),
        marker_id="opensandbox-file-operations",
    ),
    "network-egress": SkillSpec(
        slug="network-egress",
        package_file="opensandbox-network-egress.md",
        title="OpenSandbox Network Egress",
        summary=(
            "Inspect and patch sandbox runtime egress rules when the user "
            "needs to allow or deny outbound domains."
        ),
        trigger_hint=(
            "Use when the user needs to view or modify outbound network access "
            "for a sandbox, or debug domain allow and deny behavior."
        ),
        marker_id="opensandbox-network-egress",
    ),
}

DEFAULT_SKILL = "sandbox-troubleshooting"


def list_builtin_skills() -> list[SkillSpec]:
    """Return bundled skills in stable declaration order."""
    return list(BUILTIN_SKILLS.values())


def get_builtin_skill(slug: str) -> SkillSpec:
    """Return a bundled skill definition."""
    return BUILTIN_SKILLS[slug]


def get_builtin_skill_source(skill: SkillSpec) -> Path:
    """Locate the bundled skill file shipped with the CLI package."""
    pkg = importlib.resources.files("opensandbox_cli") / "skills" / skill.package_file
    if hasattr(pkg, "__fspath__"):
        return Path(str(pkg))
    with importlib.resources.as_file(pkg) as resolved:
        return Path(resolved)


def read_skill_markdown(skill: SkillSpec) -> str:
    """Load bundled skill markdown."""
    return get_builtin_skill_source(skill).read_text(encoding="utf-8")


def split_frontmatter(markdown: str) -> tuple[str | None, str]:
    """Split markdown into YAML frontmatter and body."""
    if not markdown.startswith("---\n"):
        return None, markdown

    closing = markdown.find("\n---\n", 4)
    if closing == -1:
        return None, markdown

    frontmatter = markdown[4:closing]
    body = markdown[closing + 5 :]
    return frontmatter, body


def extract_section(body: str, heading: str) -> str | None:
    """Extract a markdown section by exact heading text."""
    lines = body.splitlines()
    capture = False
    capture_level = 0
    captured: list[str] = []

    for line in lines:
        if line.startswith("## ") or line.startswith("### "):
            if capture:
                if capture_level == 2 and line.startswith("## "):
                    break
                if capture_level == 3 and (line.startswith("## ") or line.startswith("### ")):
                    break
            if line == f"## {heading}":
                capture = True
                capture_level = 2
                continue
            if line == f"### {heading}":
                capture = True
                capture_level = 3
                continue
        if capture:
            captured.append(line)

    if not captured:
        return None

    return "\n".join(captured).strip() or None


def render_skill_for_target(
    skill: SkillSpec,
    markdown: str,
    *,
    preserve_frontmatter: bool,
) -> str:
    """Render skill content for the target tool."""
    frontmatter, body = split_frontmatter(markdown)
    body = body.strip()

    if preserve_frontmatter or not frontmatter:
        return markdown.strip() + "\n"

    return (
        f"# {skill.title}\n\n"
        f"{skill.summary}\n\n"
        f"{body}\n"
    )
