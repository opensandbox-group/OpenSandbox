---
title: Claude Code
description: Access Claude via the claude-cli npm package in an OpenSandbox container.
---

# Claude Code Example

Access Claude via the `claude-cli` npm package in OpenSandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Node.js):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

# use docker hub
# docker pull opensandbox/code-interpreter:v1.1.0
```

Then start the local OpenSandbox server, stdout logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the Claude Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / ANTHROPIC_AUTH_TOKEN)
uv run python examples/claude-code/main.py
```

The script installs the Claude CLI (`npm i -g @anthropic-ai/claude-code@latest`) at runtime (Node.js is already in the code-interpreter image), then sends a simple request `claude "Compute 1+1=?."`. Auth is passed via `ANTHROPIC_AUTH_TOKEN`, and you can override endpoint/model with `ANTHROPIC_BASE_URL` / `ANTHROPIC_MODEL`.

![Claude Code screenshot](../public/images/claude-code-screenshot.jpg)

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `ANTHROPIC_AUTH_TOKEN` | _(required)_ | Your Anthropic auth token |
| `ANTHROPIC_BASE_URL` | _(optional)_ | Anthropic API endpoint (e.g., self-hosted proxy) |
| `ANTHROPIC_MODEL` | `claude_sonnet4` | Model name |

## References

- [claude-code](https://www.npmjs.com/package/claude-code) - NPM package for Claude Code CLI
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/claude-code)
