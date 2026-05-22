# OpenSandbox CLI

`osb` is the command-line interface for OpenSandbox. It is built for the common day-to-day flows:

- create and manage sandboxes
- run commands inside a sandbox
- read and modify sandbox files
- inspect runtime egress policy
- collect low-level diagnostics
- install OpenSandbox-specific skills for coding agents

It uses the OpenSandbox Python SDK under the hood and is intended to be the shortest path from a terminal to a working sandbox workflow.

## Install

Choose one:

```bash
pip install opensandbox-cli
```

```bash
uv tool install opensandbox-cli
```

```bash
pipx install opensandbox-cli
```

Confirm the install:

```bash
osb --help
osb --version
```

## Before You Start

Make sure an OpenSandbox server is reachable. If you are running locally, start the server first and then point the CLI at it.

```bash
opensandbox-server
```

## Quick Start

### 1. Initialize config

```bash
osb config init
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb config set connection.api_key <your-api-key>
osb config show -o json
```

If you want a non-default config file, choose it at the root command level for the whole invocation:

```bash
osb --config /tmp/dev.toml config init
osb --config /tmp/dev.toml config set connection.domain localhost:8080
osb --config /tmp/dev.toml config show -o json
```

### 2. Create a sandbox

```bash
osb sandbox create --image python:3.12 --timeout 30m -o json
```

If you set defaults first, later create commands can be shorter:

```bash
osb config set defaults.image python:3.12
osb config set defaults.timeout 30m
osb sandbox create -o json
```

### 3. Verify it is usable

```bash
osb sandbox get <sandbox-id> -o json
osb sandbox health <sandbox-id> -o json
```

### 4. Run a command inside the sandbox

Use `--` before the sandbox command payload.

```bash
osb command run <sandbox-id> -o raw -- python -c "print(1 + 1)"
```

### 5. Read or write a file

```bash
osb file write <sandbox-id> /workspace/hello.txt -c "hello" -o json
osb file cat <sandbox-id> /workspace/hello.txt -o raw
```

### 6. Clean up

```bash
osb sandbox kill <sandbox-id> -o json
```

## Common Tasks

### Create sandboxes

Basic:

```bash
osb sandbox create --image python:3.12
```

Private image:

```bash
osb sandbox create \
  --image my-registry.example.com/team/app:latest \
  --image-auth-username alice \
  --image-auth-password <token>
```

Manual cleanup mode:

```bash
osb sandbox create --image python:3.12 --timeout none
```

Explicit entrypoint argv:

```bash
osb sandbox create \
  --image python:3.12 \
  --entrypoint python \
  --entrypoint -m \
  --entrypoint http.server
```

Create with network policy and volumes:

```bash
osb sandbox create \
  --image python:3.12 \
  --network-policy-file network-policy.json \
  --volumes-file volumes.json
```

### List and inspect sandboxes

```bash
osb sandbox list
osb sandbox list -o json
osb sandbox list --state running --state paused
osb sandbox get <sandbox-id> -o json
osb sandbox metrics <sandbox-id>
osb sandbox metrics <sandbox-id> --watch -o raw
```

### Expose a service

```bash
osb sandbox endpoint <sandbox-id> --port 8080 -o json
```

### Run commands

Foreground streaming:

```bash
osb command run <sandbox-id> -o raw -- sh -lc 'echo ready'
```

Tracked background execution:

```bash
osb command run <sandbox-id> --background -o json -- sh -c "sleep 10; echo done"
osb command status <sandbox-id> <execution-id> -o json
osb command logs <sandbox-id> <execution-id> -o json
```

Persistent shell session:

```bash
osb command session create <sandbox-id> --workdir /workspace -o json
osb command session run <sandbox-id> <session-id> -o raw -- pwd
osb command session run <sandbox-id> <session-id> -o raw -- export FOO=bar
osb command session run <sandbox-id> <session-id> -o raw -- sh -c 'echo $FOO'
osb command session delete <sandbox-id> <session-id> -o json
```

### Work with files

```bash
osb file upload <sandbox-id> ./local.txt /workspace/local.txt -o json
osb file download <sandbox-id> /workspace/result.json ./result.json -o json
osb file search <sandbox-id> /workspace --pattern "*.py" -o json
osb file info <sandbox-id> /workspace/main.py -o json
osb file replace <sandbox-id> /workspace/app.py --old old --new new -o json
osb file chmod <sandbox-id> /workspace/script.sh --mode 755 -o json
```

### Manage runtime egress policy

Inspect current policy:

```bash
osb egress get <sandbox-id> -o json
```

Patch specific rules:

```bash
osb egress patch <sandbox-id> --rule allow=pypi.org --rule deny=internal.example.com -o json
```

If you are debugging connectivity, verify behavior with an actual command:

```bash
osb command run <sandbox-id> -o raw -- curl -I https://pypi.org
```

### Collect diagnostics

Use the stable diagnostics commands for API-backed log and event descriptors.

```bash
osb diagnostics events <sandbox-id> --scope lifecycle -o raw
osb diagnostics events <sandbox-id> --scope runtime -o raw
osb diagnostics logs <sandbox-id> --scope container -o raw
osb diagnostics logs <sandbox-id> --scope lifecycle -o json
osb diagnostics events <sandbox-id> --scope runtime -o json
osb diagnostics logs <sandbox-id> --scope container -o yaml
```

`--scope` is required for stable diagnostics. Common scopes are `lifecycle` and
`container` for logs, and `lifecycle` and `runtime` for events. Raw output
prints inline diagnostic text, or the content URL when diagnostics are
delivered as a temporary URL. Structured CLI output follows the SDK/Python field
style, for example `content_url`, `content_length`, and `expires_at`.
Some server builds may return `DIAGNOSTICS_NOT_IMPLEMENTED` for scoped
diagnostics until the stable backend implementation is enabled.

Legacy DevOps diagnostics remain experimental. Prefer `osb diagnostics logs/events`
for stable API-backed log and event collection.

```bash
osb devops inspect <sandbox-id> -o raw
osb devops summary <sandbox-id> -o raw
```

## Output Formats

Output selection is command-scoped, not global.

- `table`: human-readable tables and panels
- `json`: machine-readable JSON
- `yaml`: machine-readable YAML
- `raw`: unformatted text or streaming output

Examples:

```bash
osb sandbox list -o json
osb sandbox list -o yaml
osb file cat <sandbox-id> /workspace/hello.txt -o raw
```

Not every command supports every format. Use `--help` on the specific command when in doubt.

## Command Groups

The main command groups are:

- `osb sandbox`: lifecycle management
- `osb command`: command execution and persistent sessions
- `osb file`: file and directory operations
- `osb egress`: runtime egress policy
- `osb diagnostics`: stable diagnostics logs and events
- `osb devops`: experimental legacy diagnostics
- `osb config`: local CLI configuration
- `osb skills`: bundled skills for AI tools

Explore them directly:

```bash
osb sandbox --help
osb command --help
osb file --help
osb skills --help
```

## Agent Skills

The CLI ships with built-in OpenSandbox skills for coding agents and agent-oriented tools.

Bundled skills:

- `sandbox-lifecycle`
- `command-execution`
- `file-operations`
- `network-egress`
- `sandbox-troubleshooting`

Supported targets:

| Target | Install location |
| --- | --- |
| `claude` | `./.claude/skills/` or `~/.claude/skills/` |
| `cursor` | `./.cursor/rules/` or `~/.cursor/rules/` |
| `codex` | `./.codex/skills/<name>/SKILL.md` or `~/.codex/skills/<name>/SKILL.md` |
| `copilot` | `./.github/copilot-instructions.md` or `~/.github/copilot-instructions.md` |
| `windsurf` | `./.windsurfrules` or `~/.windsurfrules` |
| `cline` | `./.clinerules` or `~/.clinerules` |
| `opencode` | `./.agents/skills/<name>/SKILL.md` or `~/.agents/skills/<name>/SKILL.md` |

Common flows:

```bash
osb skills list
osb skills show sandbox-lifecycle
osb skills install sandbox-lifecycle --target codex --scope project
osb skills install --all-builtins --target codex --scope global
osb skills uninstall sandbox-troubleshooting --target claude --scope global
```

For scripts or agents, use structured output:

```bash
osb skills install sandbox-lifecycle --target codex --scope project -o json
```

## Configuration Model

The CLI resolves configuration in this order:

1. root CLI flags such as `--api-key`, `--domain`, `--protocol`, `--request-timeout`, `--config`
2. environment variables such as `OPEN_SANDBOX_API_KEY` and `OPEN_SANDBOX_DOMAIN`
3. config file, defaulting to `~/.opensandbox/config.toml`
4. SDK defaults

Config commands:

```bash
osb config init
osb config show
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb config set defaults.image python:3.12
osb config set defaults.timeout 30m
```

Example config file:

```toml
[connection]
api_key = "your-api-key"
domain = "localhost:8080"
protocol = "http"
request_timeout = 30
use_server_proxy = false

[output]
color = true

[defaults]
image = "python:3.12"
timeout = "30m"
```

## Development

For local development in this monorepo:

```bash
cd cli
uv sync
uv run osb --help
uv run pytest
```

This repository uses a local `uv` source override for the OpenSandbox Python SDK, so running from `cli/` will resolve against the checked-out SDK in the monorepo.
