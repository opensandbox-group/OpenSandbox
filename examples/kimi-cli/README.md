# Kimi CLI Example

Run [Kimi Code CLI](https://github.com/MoonshotAI/kimi-cli) (Moonshot AI) inside an OpenSandbox container.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python 3.12+):

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

## Create and Access the Kimi Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / KIMI_API_KEY)
uv run python examples/kimi-cli/main.py
```

The script installs Kimi Code CLI (`pip install kimi-cli`) at runtime (Python 3.12+ is already in the code-interpreter image), then sends a simple request `kimi -p "Compute 1+1=?."`. Auth is passed via `KIMI_API_KEY`, and you can override endpoint/model with `KIMI_BASE_URL` / `KIMI_MODEL_NAME`.

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `KIMI_API_KEY`: Your Moonshot AI / Kimi API key (required)
- `KIMI_BASE_URL`: Kimi API endpoint (optional; defaults to Kimi's official endpoint)
- `KIMI_MODEL_NAME`: Model to use (default: `kimi-k2.5`)

## References
- [Kimi Code CLI](https://github.com/MoonshotAI/kimi-cli) - Official Kimi Code CLI repository
- [Moonshot AI Platform](https://platform.moonshot.ai/) - API key management and documentation
- [Kimi CLI Documentation](https://moonshotai.github.io/kimi-cli/en/) - Full CLI documentation
