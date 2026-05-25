# Qwen Code Example

Run [Qwen Code](https://github.com/QwenLM/qwen-code) inside an OpenSandbox container through an OpenAI-compatible endpoint.

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

## Create and Access the Qwen Sandbox

```shell
# Install OpenSandbox package
uv pip install opensandbox

# Export provider settings
export API_KEY=your-api-key
export BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
export MODEL_NAME=qwen3-coder-plus

# Run the example
uv run python examples/qwen-code/main.py
```

The script installs Qwen Code (`npm install -g @qwen-code/qwen-code@latest`) at runtime, writes a project-local `.qwen/settings.json` inside the sandbox, and runs `qwen -p "Compute 1+1 and reply with only the final number."` in headless mode. The API key is injected only through the `API_KEY` environment variable and is not written into the repository.

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `API_KEY`: API key for the OpenAI-compatible provider used by Qwen Code (required)
- `BASE_URL`: OpenAI-compatible base URL (default: `https://dashscope.aliyuncs.com/compatible-mode/v1`)
- `MODEL_NAME`: Model name for Qwen Code (default: `qwen3-coder-plus`)

## References

- [Qwen Code](https://github.com/QwenLM/qwen-code) - Official repository
- [Qwen Code Authentication](https://qwenlm.github.io/qwen-code-docs/en/users/configuration/auth/) - Provider configuration reference
