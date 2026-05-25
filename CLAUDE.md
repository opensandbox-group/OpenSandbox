# OpenSandbox Claude Guide

Use this file as the Claude Code entry point for the OpenSandbox monorepo. Treat `AGENTS.md` as the canonical router and prefer the nearest local `AGENTS.md` for task-specific rules.

## Read First

- Root rules: `AGENTS.md`
- Server changes: `server/AGENTS.md`
- SDK changes: `sdks/AGENTS.md`
- Spec changes: `specs/AGENTS.md`
- Kubernetes changes: `kubernetes/AGENTS.md`
- Areas without a local `AGENTS.md`: read the nearest `README.md`, `DEVELOPMENT.md`, and relevant CI workflow.

## Repository Map

- `server/`: FastAPI lifecycle control plane, Docker/Kubernetes runtime integration, snapshot metadata, and server tests
- `components/execd/`: in-sandbox execution daemon
- `components/egress/`: per-sandbox network egress policy sidecar
- `components/ingress/`: ingress gateway and endpoint routing
- `sdks/`: sandbox, code-interpreter, and MCP SDKs plus generated clients
- `specs/`: public OpenAPI contracts and examples
- `kubernetes/`: Kubernetes operator, CRDs, task-executor, Helm charts, and Kind e2e tests
- `cli/`: `osb` command-line client and bundled CLI skills
- `tests/`: cross-language end-to-end SDK tests
- `docs/`, `examples/`, `sandboxes/`, `oseps/`: documentation, samples, images/environments, and proposals

## Working Principles

- Think before coding: state assumptions, surface ambiguity, and ask or push back when the request has conflicting interpretations.
- Simplicity first: implement the smallest solution that satisfies the request; avoid speculative features, one-off abstractions, and unnecessary configurability.
- Surgical changes: touch only files and lines needed for the task, match local style, and do not refactor or delete unrelated pre-existing code.
- Goal-driven execution: translate non-trivial work into verifiable success criteria, add or update focused tests when behavior changes, and loop until checks pass or blockers are clear.

## Guardrails

Always:

- Keep changes focused on the user request.
- Keep spec, implementation, SDKs, docs, examples, config, CLI, and Kubernetes behavior aligned when user-visible behavior changes.
- Prefer additive, backward-compatible changes for public interfaces.
- Regenerate derived outputs when source-of-truth files change.
- Update tests when behavior changes or bugs are fixed.
- Prefer focused package/file checks before full-suite validation.
- Mention unrun or blocked verification in the final handoff.

Ask first:

- Breaking public API, SDK, config, protocol, CLI, CRD, annotation, label, Helm value, or deployment changes
- Intentional drift between a public contract and its implementation
- User-visible config or behavior changes without a clear migration story

Never:

- Edit generated output as the only fix.
- Mix unrelated component work into the same change.
- Refactor adjacent code just because it is nearby.

## Review Focus

- Prioritize breaking changes in specs, SDK interfaces, config, CLI behavior, CRDs, annotations, labels, and protocols.
- Flag protocol changes that are unnecessary, inconsistent, or hard to implement.
- Flag source-of-truth boundary violations and missing downstream updates.
- Call out missing tests and compatibility risks explicitly.
