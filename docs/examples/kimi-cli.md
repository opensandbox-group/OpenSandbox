---
title: Kimi CLI
description: Run Kimi Code CLI (Moonshot AI) inside an OpenSandbox container.
---

# Kimi CLI Example

Run [Kimi Code CLI](https://github.com/MoonshotAI/kimi-cli) (Moonshot AI) inside an OpenSandbox container.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python 3.12+):

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

## Create and Access the Kimi Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / KIMI_API_KEY)
uv run python examples/kimi-cli/main.py
```

The script installs Kimi Code CLI (`pip install kimi-cli`) at runtime (Python 3.12+ is already in the code-interpreter image), then sends a simple request `kimi -p "Compute 1+1=?."`. Auth is passed via `KIMI_API_KEY`, and you can override endpoint/model with `KIMI_BASE_URL` / `KIMI_MODEL_NAME`.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `KIMI_API_KEY` | _(required)_ | Your Moonshot AI / Kimi API key |
| `KIMI_BASE_URL` | _(optional)_ | Kimi API endpoint (defaults to Kimi's official endpoint) |
| `KIMI_MODEL_NAME` | `kimi-k2.5` | Model to use |

## References

- [Kimi Code CLI](https://github.com/MoonshotAI/kimi-cli) - Official Kimi Code CLI repository
- [Moonshot AI Platform](https://platform.moonshot.ai/) - API key management and documentation
- [Kimi CLI Documentation](https://moonshotai.github.io/kimi-cli/en/) - Full CLI documentation
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/kimi-cli)
