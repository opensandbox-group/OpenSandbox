# Specs AGENTS

You are maintaining OpenSandbox public API contracts. Treat the spec files in this directory as the source of truth for public interfaces and prefer additive changes.

## Scope

- `sandbox-lifecycle.yml`
- `diagnostic-api.yml`
- `execd-api.yaml`
- `egress-api.yaml`
- `README*.md`

When a contract change affects downstream code, also read the nearest consumer guide:

- `../server/AGENTS.md` for lifecycle server impact
- `../sdks/AGENTS.md` for SDK-facing contract changes
- component README/DEVELOPMENT files under `../components/` for execd or egress impact
- `../cli/README.md` for CLI-visible diagnostics, lifecycle, or egress changes

## Contract Map

- `sandbox-lifecycle.yml`: lifecycle API used by `server/`, `cli/`, and sandbox SDKs
- `diagnostic-api.yml`: diagnostics API used by server diagnostics, CLI diagnostics, and troubleshooting flows
- `execd-api.yaml`: execution API used by `components/execd/` and code-interpreter SDKs
- `egress-api.yaml`: egress sidecar API and related docs

## Commands

Regenerate docs output:

```bash
node scripts/spec-doc/generate-spec.js
cd docs
pnpm docs:spec
```

Lifecycle consumer validation:

```bash
cd server
uv sync --all-groups
uv run ruff check
uv run pytest
```

SDK workspace setup for affected SDK regeneration:

```bash
cd sdks
pnpm install --frozen-lockfile
```

## Guardrails

Always:

- Keep operation IDs, schema names, examples, and descriptions consistent with existing naming.
- Regenerate derived outputs after spec edits.
- Update affected consumers in the same change when practical.
- Keep spec examples aligned with server schemas and generated SDK models.
- Call out downstream areas you did not verify.

Ask first:

- Breaking contract changes
- Renaming or removing public fields or operations
- Adding a new top-level API surface without implementation alignment

Never:

- Rename or remove public fields without approval.
- Hand-edit derived outputs without updating the source spec.
- Assume a spec-only edit is isolated from server, SDK, docs, or release impact.

## Good Patterns

- Concrete examples and descriptions when behavior changes
- Explicit backward-compatible field additions instead of field redefinition
- Contract and consumer updates landing together
