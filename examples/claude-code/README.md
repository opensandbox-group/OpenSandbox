# Claude Code Example

Access Claude via the `claude-cli` npm package in OpenSandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Node.js):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2

# use docker hub
# docker pull opensandbox/code-interpreter:v1.0.2
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

![Claude Code screenshot](./screenshot.jpg)

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `ANTHROPIC_AUTH_TOKEN`: Your Anthropic auth token (required)
- `ANTHROPIC_BASE_URL`: Anthropic API endpoint (optional; e.g., self-hosted proxy)
- `ANTHROPIC_MODEL`: Model name (default: `claude_sonnet4`)

## References
- [claude-code](https://www.npmjs.com/package/claude-code) - NPM package for Claude Code CLI
