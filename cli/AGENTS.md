# CLI AGENTS

You are working on the OpenSandbox CLI. Keep commands as thin, predictable wrappers over the Python SDK, and keep help text, README examples, bundled skills, and tests aligned whenever user-visible CLI behavior changes.

## Scope

- `src/opensandbox_cli/**`
- `tests/**`
- `README.md`
- `pyproject.toml` and `uv.lock` when CLI dependencies or release metadata change
- `assets/**` only when screenshots or visual CLI documentation are intentionally refreshed

If the task changes SDK-facing behavior, also read `../sdks/AGENTS.md`. If the task is driven by public API contracts, also read `../specs/AGENTS.md`.

## Key Areas

- `src/opensandbox_cli/main.py`: root command registration, global options, version/banner behavior
- `src/opensandbox_cli/client.py`: resolved config, SDK manager/client construction, output formatter wiring
- `src/opensandbox_cli/commands/`: command groups and command-scoped options
- `src/opensandbox_cli/output.py`: table, JSON, YAML, and raw rendering behavior
- `src/opensandbox_cli/skills/`: bundled skills installed into external agent tools
- `src/opensandbox_cli/skill_registry.py`: skill metadata shown by `osb skills list/show`
- `tests/test_cli_help.py`: root and command help coverage
- `tests/test_commands.py`: SDK-backed command behavior with mocked SDK calls
- `tests/test_skills.py`: bundled skill quality and CLI alignment checks

## Command Design

Prefer clear command groups whose names match stable product concepts. Commands should call Python SDK facades such as `SandboxManager`, `Sandbox`, or service objects instead of rebuilding HTTP paths locally. Raw HTTP or private client access is acceptable only for explicitly experimental or legacy commands.

For new stable commands:

- expose concise flags with names that match SDK/API concepts
- support `-o/--output` consistently with nearby commands
- use `raw` only for payload text or streaming-style output
- use `json` / `yaml` for structured descriptors or SDK models
- register the command in `main.py`
- add help tests and mocked SDK command tests
- update README examples when the command is user-facing
- update bundled skills when agents should use the command

For deprecated commands:

- preserve old option meanings, especially short flags
- do not silently reuse an old flag for a new concept
- print or return explicit migration guidance
- keep compatibility wrappers small and route to the stable implementation when practical

## Skills

Bundled skills are operational guidance for real agents, not long-form documentation. Keep them concise, command-first, and aligned with actual CLI behavior.

When changing commands that appear in skills:

- update the relevant skill examples
- explain only the semantics agents need to make decisions
- keep examples executable and include explicit `-o` output formats
- avoid relying on deprecated CLI APIs unless the skill clearly marks them as fallback
- update `skill_registry.py` summaries when the skill's advertised behavior changes
- update `tests/test_skills.py` so command examples and option names stay aligned with the CLI

## Commands

Common CLI checks:

```bash
cd cli
uv run --frozen ruff check
uv run --frozen pyright
uv run --frozen pytest tests/ -q
```

Focused checks:

```bash
cd cli
uv run --frozen pytest tests/test_cli_help.py -q
uv run --frozen pytest tests/test_commands.py -q
uv run --frozen pytest tests/test_skills.py -q
```

Use `--frozen` for validation when you do not intend to update `uv.lock`. If `uv run` changes `uv.lock` unexpectedly, inspect the diff and keep it only when the dependency graph intentionally changed.

## Guardrails

Always:

- Keep CLI behavior aligned with the Python SDK surface it wraps.
- Keep help text accurate for supported options and output formats.
- Add or update tests for new commands, changed flags, changed rendering, and skill examples.
- Preserve command output compatibility unless the migration is explicit and documented.
- Treat bundled skills as part of the user-facing CLI surface.
- Keep command implementations small; put reusable rendering, validation, and error handling in local helpers when that matches existing style.
- Mention verification that was not run in the final handoff.

Ask first:

- Removing a command or flag.
- Changing the meaning of an existing option or short flag.
- Making a legacy or experimental command the preferred path without a migration story.
- Changing installed skill formats or target layouts.

Never:

- Reimplement stable SDK APIs with ad hoc HTTP calls in a new stable CLI command.
- Update generated or lock files as incidental noise.
- Leave README, help text, or bundled skills pointing at deprecated commands after adding a stable replacement.
- Mix unrelated CLI feature work into a command behavior change.

## Good Patterns

- Stable command wraps SDK manager/service method and shares rendering helpers.
- Legacy command delegates to the stable implementation and emits migration guidance.
- Tests mock `ClientContext` and assert exact SDK calls.
- Skill tests assert that examples use existing commands and explicit output formats.
