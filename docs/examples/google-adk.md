---
title: Google ADK
description: Integrate Google Agent Development Kit (ADK) with OpenSandbox for tool-calling agents.
---

# Google ADK + OpenSandbox Example

Integrate Google Agent Development Kit (ADK) with OpenSandbox. The agent uses ADK's native `OpenSandboxEnvironment` and `EnvironmentToolset` to execute commands and edit files inside a sandbox. Requires Python 3.11+ and Google ADK 2.13.0 or newer.

::: warning Experimental API
ADK currently marks the OpenSandbox environment and environment toolset as experimental.
:::

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

The agent uses DeepSeek through ADK's `LiteLlm` adapter. Install the connector and its MCP runtime dependency:

```shell
uv pip install "google-adk[opensandbox,extensions]>=2.13.0" "mcp>=2.2,<3"

export OPENAI_API_KEY="your-deepseek-api-key"
export SANDBOX_DOMAIN="localhost:8080"
# Set SANDBOX_API_KEY too if your server requires authentication

# Run from the repository root
uv run python examples/google-adk/main.py
```

The default model is `openai/deepseek-flash` at `https://api.deepseek.com/v1`. To use another OpenAI-compatible endpoint, set `GOOGLE_ADK_MODEL` with the `openai/` prefix and set `OPENAI_BASE_URL`. The endpoint must support tool calling.

The script writes `/workspace/math.py`, executes it to produce `5754`, then writes and reads back `ADK + OpenSandbox` from `/workspace/notes.txt`. All prompts share one session and workspace. Tool events and model responses are printed to the terminal.

## Native ADK integration

```python
from google.adk.integrations.opensandbox import OpenSandboxEnvironment
from google.adk.tools.environment import EnvironmentToolset
from opensandbox.config import ConnectionConfig

toolset = EnvironmentToolset(
    environment=OpenSandboxEnvironment(
        image="sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0",
        timeout=300,
        connection_config=ConnectionConfig(domain="localhost:8080"),
    )
)
# Pass tools=[toolset] to the ADK Agent.
# In your async application's finally block: await toolset.close()
```

ADK provides `Execute`, `WriteFile`, `ReadFile`, and `EditFile`. The toolset initializes the environment lazily. Relative paths resolve under `/workspace`; commands also run there. The environment creates a sandbox with a 300-second lifetime and renews it before command execution. The example closes the toolset in `finally`, destroying the sandbox it created even when a request fails.

Earlier versions of this example wrapped the OpenSandbox SDK in three custom functions. The example now uses ADK's native environment integration while retaining the same entry point and sandbox environment variables. It explicitly passes `ConnectionConfig`, so the existing `SANDBOX_*` names continue to work.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `GOOGLE_ADK_MODEL` | `openai/deepseek-flash` | Model name with the `openai/` prefix |
| `OPENAI_API_KEY` | _(required)_ | API key for the selected endpoint |
| `OPENAI_BASE_URL` | `https://api.deepseek.com/v1` | OpenAI-compatible endpoint base URL |

## References

- [Google ADK](https://google.github.io/adk-docs/) - Agent Development Kit
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/google-adk)

- [ADK 2.13.0 release](https://github.com/google/adk-python/releases/tag/v2.13.0)
- [ADK native OpenSandbox sample](https://github.com/google/adk-python/tree/v2.13.0/contributing/samples/environment_and_skills/opensandbox_environment)
