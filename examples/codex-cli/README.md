# Codex/OpenAI CLI Example

Use the official `@openai/codex` npm package to call OpenAI/Codex-like models in OpenSandbox.

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

## Create and Access the Codex Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / OPENAI_API_KEY)
uv run python examples/codex-cli/main.py
```

The script installs the Codex CLI (`npm install -g @openai/codex@latest`) at runtime (Node.js is already in the code-interpreter image), then executes a simple request `codex exec "Compute 1+1 and return JSON with keys result and reasoning." --skip-git-repo-check`. Auth is passed via `OPENAI_API_KEY`; you can override endpoint/model with `OPENAI_BASE_URL` / `OPENAI_MODEL`.

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `OPENAI_API_KEY`: Your OpenAI API key (required)
- `OPENAI_BASE_URL`: OpenAI API endpoint (default: `https://api.openai.com/v1`)
- `OPENAI_MODEL`: Model to use (default: `gpt-4o-mini`)

## References
- [@openai/codex](https://www.npmjs.com/package/@openai/codex) - Official OpenAI Codex CLI
