# OpenSandbox AGENTS

Use this file as the root router for the monorepo. Prefer the nearest `AGENTS.md` in the directory tree for task-specific instructions.

## Repository Map

- `server/`: FastAPI lifecycle control plane, Docker/Kubernetes runtime integration, snapshot metadata, and server tests
- `components/execd/`: in-sandbox execution daemon
- `components/egress/`: per-sandbox network egress policy sidecar
- `components/ingress/`: ingress gateway and endpoint routing
- `components/internal/`: shared Go helpers used by runtime components
- `sdks/`: sandbox, code-interpreter, and MCP SDKs plus generated clients
- `specs/`: public OpenAPI contracts and examples
- `kubernetes/`: Kubernetes operator, CRDs, task-executor, Helm charts, and Kind e2e tests
- `cli/`: `osb` command-line client and bundled CLI skills
- `tests/`: cross-language end-to-end SDK tests
- `docs/`, `examples/`, `sandboxes/`, `oseps/`: documentation, samples, images/environments, and proposals

## Routing

- For `server/**`, or lifecycle server behavior, sandbox creation flow, or user-visible server config, read `server/AGENTS.md`.
- For `sdks/**`, or SDK generation, handwritten adapters, or cross-language SDK alignment, read `sdks/AGENTS.md`.
- For `specs/**`, or API contract, schema, or example changes, read `specs/AGENTS.md`.
- For `kubernetes/**`, or CRDs, controller behavior, task execution, Helm/Kustomize deployment, pool scheduling, pause/resume snapshots, or Kind e2e tests, read `kubernetes/AGENTS.md`.
- For cross-cutting changes spanning spec, server, and SDKs, start with `specs/AGENTS.md` and then read affected consumer guides.
- For runtime component changes under `components/**`, read the nearest `README.md` or `DEVELOPMENT.md`; keep component APIs aligned with `specs/` and SDK consumers.
- For CLI changes under `cli/**`, read `cli/README.md` and verify command help/output behavior alongside unit tests.
- For cross-language e2e tests under `tests/**`, read the language-local README and keep test assumptions aligned with current server and SDK behavior.
- For areas without a local `AGENTS.md`, use the nearest `README.md`, `DEVELOPMENT.md`, and CI workflow as the next source of truth.

## Working Principles

- Think before coding: state assumptions, surface ambiguity, and ask or push back when the request has conflicting interpretations.
- Simplicity first: implement the smallest solution that satisfies the request; avoid speculative features, one-off abstractions, and unnecessary configurability.
- Surgical changes: touch only files and lines needed for the task, match local style, and do not refactor or delete unrelated pre-existing code.
- Goal-driven execution: translate non-trivial work into verifiable success criteria, add or update focused tests when behavior changes, and loop until checks pass or blockers are clear.

## Guardrails

Always:

- Keep changes focused on the user request.
- Treat `specs/*` as public contract sources.
- Keep spec, implementation, SDKs, docs, examples, config, and CLI behavior aligned when user-visible behavior changes.
- When changing `specs/*`, also update or verify affected server, SDK, docs, and release outputs when practical.
- When changing CRDs or Kubernetes public behavior, update or verify generated manifests, Helm/Kustomize deployment output, server Kubernetes integration, and docs when practical.
- Prefer additive, backward-compatible changes for public interfaces.
- Regenerate derived outputs when the source-of-truth file changes.
- Update tests when behavior changes or bugs are fixed.
- Mention unrun or blocked verification in the final handoff.
- Prefer file-scoped or package-scoped checks before full-suite validation.

Ask first:

- Breaking public API, SDK, config, protocol, or CLI changes
- Breaking CRD, annotation, label, Helm values, or Kubernetes deployment changes
- Intentional drift between a public contract and its implementation
- User-visible config or behavior changes without a clear migration story

Never:

- Edit generated output as the only fix.
- Mix unrelated component work into the same change.

## Documentation Rules

### Content ownership — single source of truth

| Content type | Source of truth | Rule |
|---|---|---|
| User and operations docs | `docs/` | Keep long-form docs here |
| Root README | repo root `README.md` | GitHub homepage |
| SDK, CLI, Helm, and other publishable package READMEs | package directory | Keep install, quick start, and package entry points |
| Non-publishable component/module READMEs | component/module directory | Keep minimal pointers to `docs/` when a docs page exists |
| Examples | `docs/examples/` + runnable code under `examples/` | Put docs in `docs/examples/`; keep example READMEs as thin pointers |
| OSEPs | `oseps/` | `docs/community/oseps.md` only indexes GitHub proposals |
| CONTRIBUTING, CODE_OF_CONDUCT, DEVELOPMENT | repo root / component directory | `docs/community/` links to them, does not duplicate them |

**When modifying user-visible or operations-visible behavior**: update `docs/` first.

**When editing READMEs**: avoid long-form docs outside root or publishable package READMEs.

**When adding examples**: keep runnable code under `examples/`, and document it under `docs/examples/`.

**When linking from README files to docs**: prefer repository-relative `docs/*.md` links so links match the checked-out branch or tag. Use `https://open-sandbox.ai` as a public site entry point, not as the only source link from versioned code directories.

**When handling localized READMEs**: do not add new localized copies for SDKs, examples, or non-special modules.

### Docs site structure

```
docs/
  getting-started/     # Quick start, installation, configuration
  architecture/        # Architecture overview, network design
  guides/              # Feature guides (credential vault, secure container, etc.)
  sdks/                # SDK reference (one page per language per SDK)
  components/          # Server, execd, ingress, egress
  kubernetes/          # Kubernetes operator and deployment
  api/                 # OpenAPI spec reference
  cli/                 # CLI reference
  examples/            # One page per example
  community/           # Contributing, code of conduct, OSEPs, releases
  reference/           # Migration guides
```

### Docs conventions

- Engine: VitePress. Config is a static `config.mts` with no build-time code generation.
- All images go in `docs/public/images/`, referenced as `/images/filename` in markdown.
- Every page must have YAML frontmatter with `title` and `description`.
- Internal links use VitePress absolute paths (e.g., `/sdks/python`, `/guides/credential-vault`).
- Links to source code or specs use full GitHub URLs.
- Use VitePress custom containers (`::: tip`, `::: warning`, `::: info`) and code groups where appropriate.
- Build and verify: `cd docs && pnpm docs:build` — must complete with zero errors.
- The README.md in `docs/` is for docs-site contributors only (how to run dev server), and must stay excluded from the published site.

## Review Focus

- Prioritize breaking changes in specs, SDK interfaces, config, CLI behavior, and protocols.
- Flag protocol changes that are unnecessary, inconsistent, or hard to implement.
- Flag changes that break source-of-truth boundaries or intended layering.
- Call out missing tests and compatibility risks explicitly.
