---
title: Codex CLI
description: Use the official @openai/codex npm package to call OpenAI/Codex-like models in OpenSandbox.
---

# Codex/OpenAI CLI Example

Use the official `@openai/codex` npm package to call OpenAI/Codex-like models in OpenSandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Node.js):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

# use docker hub
# docker pull opensandbox/code-interpreter:v1.1.0
```

Start the local OpenSandbox server, logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the Codex Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / OPENAI_API_KEY)
uv run python examples/codex-cli/main.py
```

The script installs the Codex CLI (`npm install -g @openai/codex@latest`) at runtime (Node.js is already in the code-interpreter image), then executes a simple request `codex exec "Compute 1+1 and return JSON with keys result and reasoning." --skip-git-repo-check`. `codex exec` ignores the `OPENAI_*` variables, so the script passes `OPENAI_API_KEY` into the sandbox as `CODEX_API_KEY`, and `OPENAI_MODEL` / `OPENAI_BASE_URL` as `-m` / `-c openai_base_url="..."`.

## Multi-Turn Sessions: Resume a Previous Run

Each `codex exec` run is a session. For pipelines that need a follow-up turn (review → fix, plan → implement), resume the session instead of starting over with a fresh context.

Turn 1 — run with `--json` and capture the session id. `--json` turns stdout into a JSON Lines (JSONL) event stream; the first event, `thread.started`, carries the `thread_id`, which is the session id that `codex exec resume` accepts:

```shell
codex exec --json "Remember this for later: my favorite sandbox number is 42." --skip-git-repo-check
```

```json
{"type": "thread.started", "thread_id": "0199a213-81c0-7800-8aa1-bbab2a035a53"}
```

Turn 2 — resume that session with a follow-up prompt; the model recalls the context of the previous turns, so the reply is `42`:

```shell
codex exec resume "0199a213-81c0-7800-8aa1-bbab2a035a53" "What is my favorite sandbox number? Reply with just the number." --skip-git-repo-check
```

::: tip
`codex exec resume --last "..."` continues the most recent session from the current working directory — convenient for quick two-stage pipelines. The example script captures the explicit id instead, which stays correct when several sessions interleave.
:::

If you only need the final message for a downstream step, `-o <path>` / `--output-last-message <path>` writes it to a file, and `--output-schema <file>` validates the final response against a JSON Schema.

## Sandbox and Approval Modes

Codex applies its own sandbox policy to model-generated shell commands; these flags control it inside the OpenSandbox container:

- `--sandbox workspace-write` (or `-s workspace-write`) lets model-generated commands write inside the workspace — the usual choice for autonomous runs.
- `--sandbox read-only` keeps the run strictly read-only.
- `-a never` / `--ask-for-approval never` never pauses for human approval, so a non-interactive run cannot block on a person.
- `--dangerously-bypass-approvals-and-sandbox` (alias `--yolo`) runs every command without approvals or sandboxing. The Codex CLI documents it as safe only "inside an externally hardened environment" — in OpenSandbox terms, that means a sandbox running a hardened runtime (gVisor, Kata), not the default runc; see [Secure Container Runtimes](/guides/secure-container).

::: warning
`--full-auto` is deprecated — prefer `--sandbox workspace-write`; Codex prints a warning when the old flag is used.
:::

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `OPENAI_API_KEY` | _(required)_ | Your OpenAI API key |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI API endpoint |
| `OPENAI_MODEL` | `gpt-4o-mini` | Model to use |

## References

- [@openai/codex](https://www.npmjs.com/package/@openai/codex) - Official OpenAI Codex CLI
- [Codex non-interactive mode](https://developers.openai.com/codex/noninteractive) - `codex exec`, JSONL output, and session resume
- [Codex CLI reference](https://developers.openai.com/codex/cli/reference) - Sandbox and approval flags
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/codex-cli)
