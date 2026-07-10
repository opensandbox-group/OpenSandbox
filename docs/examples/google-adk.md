---
title: Google ADK
description: Integrate Google Agent Development Kit (ADK) with OpenSandbox for tool-calling agents.
---

# Google ADK + OpenSandbox Example

Integrate Google Agent Development Kit (ADK) with OpenSandbox. The ADK agent
drives tool calls that execute inside a sandbox.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python):

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

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `GOOGLE_API_KEY` | _(required)_ | Gemini API key |
| `GOOGLE_ADK_MODEL` | `gemini-2.5-flash` | Gemini model name |

## References

- [Google ADK](https://google.github.io/adk-docs/) - Agent Development Kit
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/google-adk)
