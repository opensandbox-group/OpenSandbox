---
title: Gemini CLI
description: Call Google Gemini via the @google/gemini-cli npm package in OpenSandbox.
---

# Gemini CLI Example

Call Google Gemini via the `@google/gemini-cli` npm package in OpenSandbox.

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

## Create and Access the Gemini Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / GEMINI_API_KEY)
uv run python examples/gemini-cli/main.py
```

The script installs the Gemini CLI (`npm install -g @google/gemini-cli@latest`) at runtime (Node.js is already in the code-interpreter image), then sends a simple request `gemini "Compute 1+1=?."`. Auth is passed via `GEMINI_API_KEY`; you can override endpoint/model with `GEMINI_BASE_URL` / `GEMINI_MODEL`.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `GEMINI_API_KEY` | _(required)_ | Your Google Gemini API key |
| `GEMINI_BASE_URL` | _(optional)_ | Gemini API endpoint (e.g., proxy) |
| `GEMINI_MODEL` | `gemini-2.5-flash` | Model to use |

## References

- [@google/gemini-cli](https://www.npmjs.com/package/@google/gemini-cli) - Gemini CLI
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/gemini-cli)
