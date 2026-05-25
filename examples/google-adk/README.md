# Google ADK + OpenSandbox Example

Integrate Google Agent Development Kit (ADK) with OpenSandbox. The ADK agent
drives tool calls that execute inside a sandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python):

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

## Run the example

```shell
# Install OpenSandbox + Google ADK deps
uv pip install opensandbox google-adk

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / GOOGLE_API_KEY)
uv run python examples/google-adk/main.py
```

The script uses ADK to create an agent with OpenSandbox tools (`write_file`,
`read_file`, `run_in_sandbox`). It runs a few prompts, prints tool events, and
cleans up the sandbox.

## Environment Variables

- `SANDBOX_DOMAIN`: Sandbox service address (default: `localhost:8080`)
- `SANDBOX_API_KEY`: API key if your server requires authentication (optional for local)
- `SANDBOX_IMAGE`: Sandbox image to use (default: `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2`)
- `GOOGLE_API_KEY`: Gemini API key (required)
- `GOOGLE_ADK_MODEL`: Gemini model name (default: `gemini-2.5-flash`)

## References
- [Google ADK](https://google.github.io/adk-docs/) - Agent Development Kit
