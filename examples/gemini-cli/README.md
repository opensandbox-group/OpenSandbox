# Gemini CLI Example

Call Google Gemini via the `@google/gemini-cli` npm package in OpenSandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Node.js):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2

# use docker hub
# docker pull opensandbox/code-interpreter:v1.0.2
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

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `GEMINI_API_KEY`: Your Google Gemini API key (required)
- `GEMINI_BASE_URL`: Gemini API endpoint (optional; e.g., proxy)
- `GEMINI_MODEL`: Model to use (default: `gemini-2.5-flash`)

## References
- [@google/gemini-cli](https://www.npmjs.com/package/@google/gemini-cli) - Gemini CLI
