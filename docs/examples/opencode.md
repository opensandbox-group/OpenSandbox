---
title: OpenCode
description: Run the OpenCode coding agent inside an OpenSandbox container.
---

# OpenCode Example

Run [OpenCode](https://opencode.ai/) inside an OpenSandbox container. The example installs the official CLI and executes a prompt non-interactively with `opencode run`.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Node.js):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

# use Docker Hub
# docker pull opensandbox/code-interpreter:v1.1.0
```

Then start the local OpenSandbox server. Logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the OpenCode Sandbox

```shell
# Install the OpenSandbox package
uv pip install opensandbox

# Run the example. The default free model does not require an API key.
uv run python examples/opencode/main.py
```

The script installs OpenCode (`npm install -g opencode-ai@latest`) at runtime, creates an isolated working directory, and runs `opencode run` with a simple prompt. The default model is currently available without authentication. Set `OPENCODE_API_KEY` and `OPENCODE_MODEL` to use an authenticated model from the OpenCode provider instead.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `OPENCODE_MODEL` | `opencode/deepseek-v4-flash-free` | Model ID in `provider/model` format |
| `OPENCODE_API_KEY` | _(optional)_ | API key for authenticated models from the OpenCode provider |

::: info
The default free model is offered by OpenCode for a limited time. Do not send private code, credentials, or other confidential data to a free model; review the provider's data-use terms before using it. Override `OPENCODE_MODEL` when you need a different model. Other providers may require additional environment variables or configuration.
:::

## References

- [OpenCode CLI](https://opencode.ai/docs/cli/) - Installation and non-interactive `run` command
- [OpenCode Providers](https://opencode.ai/docs/providers) - Provider authentication and configuration
- [OpenCode Zen](https://opencode.ai/docs/zen/) - Model availability and data-use notes
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/opencode)
