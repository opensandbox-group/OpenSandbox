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

```shell
# Install OpenSandbox + Google ADK deps
uv pip install "google-adk[opensandbox]>=2.13.0"

# Set the Gemini API key and local server address
export GOOGLE_API_KEY="your-gemini-api-key"
export SANDBOX_DOMAIN="localhost:8080"
# Set SANDBOX_API_KEY too if your server requires authentication

# Run from the repository root
uv run python examples/google-adk/main.py
```

The script writes `/workspace/math.py`, executes it to produce `5754`, then writes and reads back `ADK + OpenSandbox` from `/workspace/notes.txt`. All prompts share one session and workspace. Tool events and model responses are printed to the terminal.

## Use DeepSeek instead of Gemini

Install ADK's LiteLLM connector with its MCP runtime dependency and select an OpenAI-compatible model with the `openai/` prefix:

```shell
uv pip install "google-adk[opensandbox,extensions]>=2.13.0" "mcp>=2.2,<3"
export GOOGLE_ADK_MODEL="openai/deepseek-flash"
export OPENAI_BASE_URL="https://api.deepseek.com/v1"
export OPENAI_API_KEY="your-deepseek-api-key"
uv run python examples/google-adk/main.py
```

This path uses ADK's `LiteLlm` adapter and does not require `GOOGLE_API_KEY`. The sandbox tools and lifecycle are unchanged. The selected endpoint must support tool calling.

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

Earlier versions of this example wrapped the OpenSandbox SDK in three custom functions. The example now uses ADK's native environment integration while retaining the same entry point and environment variables. It explicitly passes `ConnectionConfig`, so the existing `SANDBOX_*` names continue to work.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `GOOGLE_API_KEY` | _(required for Gemini)_ | Gemini API key |
| `GOOGLE_ADK_MODEL` | `gemini-2.5-flash` | Gemini model name, or an `openai/`-prefixed model for an OpenAI-compatible endpoint |
| `OPENAI_API_KEY` | _(required for `openai/` models)_ | API key for the selected endpoint |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Base URL for `openai/` models |

## References

- [Google ADK](https://google.github.io/adk-docs/) - Agent Development Kit
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/google-adk)

- [ADK 2.13.0 release](https://github.com/google/adk-python/releases/tag/v2.13.0)
- [ADK native OpenSandbox sample](https://github.com/google/adk-python/tree/v2.13.0/contributing/samples/environment_and_skills/opensandbox_environment)
