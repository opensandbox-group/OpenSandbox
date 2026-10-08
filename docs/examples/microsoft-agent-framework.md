---
title: Microsoft Agent Framework
description: Run a general-purpose Microsoft Agent Framework agent with OpenSandbox.
---

# Microsoft Agent Framework Example

Run a general-purpose Microsoft Agent Framework agent alongside an
OpenSandbox workflow. The example uses Agent Framework's functional workflow
API to create a sandbox, prepare and execute a Python job, retry with a
fallback command when needed, ask the agent an English question, and destroy
the sandbox.

The Python client uses the OpenSandbox SDK. The local OpenSandbox Server
creates and manages the container through its Docker runtime.

The sandbox uses the Docker Hub image
`opensandbox/code-interpreter:v1.1.0`; no Alibaba Cloud container registry is
required.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python 3.12+):

```shell
docker pull opensandbox/code-interpreter:v1.1.0
```

Create a Python 3.10+ environment and install the packages declared by the
example project:

```shell
uv venv
source .venv/bin/activate

uv pip install \
  --default-index https://packagefeedproxy.microsoft.io/pypi/simple \
  "agent-framework-core==1.19.0" \
  "agent-framework-openai==1.14.4" \
  "opensandbox==1.1.0" \
  "opensandbox-server==0.2.3" \
  "python-dotenv>=1.0,<2"
```

The selective Agent Framework packages provide the workflow, agent, and
OpenAI/Azure OpenAI client APIs used by this example without installing
unrelated integrations. The Microsoft package proxy matches the package
source configured by the example project's `pyproject.toml`.

Initialize and start the local Docker runtime:

```shell
uv run opensandbox-server init-config ~/.sandbox.toml --example docker
OPENSANDBOX_INSECURE_SERVER=YES uv run opensandbox-server
```

`OPENSANDBOX_INSECURE_SERVER=YES` explicitly acknowledges that this local
server has no API key. Keep it bound to localhost. For a shared deployment,
configure `server.api_key` instead.

The project pins `opensandbox-server==0.2.3`. The upstream `1.1.0` server
wheel is missing its generated FastPath gRPC modules and fails during startup.

## Configure the Agent

Copy the environment template:

```shell
cp examples/microsoft-agent-framework/.env.example .env
```

Configure either OpenAI:

```shell
OPENAI_API_KEY=replace_me
OPENAI_MODEL=gpt-4o-mini
```

Or Azure OpenAI:

```shell
AZURE_OPENAI_API_KEY=replace_me
AZURE_OPENAI_ENDPOINT=https://<your-resource>.openai.azure.com/
AZURE_OPENAI_MODEL=<your-deployment-name>
```

When both configurations exist, `OPENAI_API_KEY` takes precedence. Remove it
to use the Azure OpenAI environment fallback.

## Run the Example

Run from the repository root:

```shell
uv run python examples/microsoft-agent-framework/main.py
```

The default English prompt is:

```text
What model are you using?
```

Set another question in `.env`, or override it for one invocation:

```shell
AGENT_QUESTION="Explain what OpenSandbox does." \
  uv run python examples/microsoft-agent-framework/main.py
```

The workflow writes and executes a small Python job in the sandbox, then
passes the question, configured model name, and sandbox result to a
general-purpose Agent Framework agent. The sandbox is stopped and closed even
when execution or the agent call fails.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Local sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if configured |
| `SANDBOX_IMAGE` | `opensandbox/code-interpreter:v1.1.0` | Docker Hub sandbox image |
| `SANDBOX_COMMAND` | `python3 /tmp/math.py` | Initial sandbox command |
| `SANDBOX_FALLBACK_COMMAND` | `python /tmp/math.py` | Retry command |
| `AGENT_QUESTION` | `What model are you using?` | English question sent to the agent |
| `OPENAI_API_KEY` | _(required for OpenAI)_ | OpenAI API key |
| `OPENAI_MODEL` | `gpt-4o-mini` in `.env.example` | OpenAI model |
| `AZURE_OPENAI_API_KEY` | _(required for Azure API-key auth)_ | Azure OpenAI API key |
| `AZURE_OPENAI_ENDPOINT` | _(required for Azure OpenAI)_ | Azure OpenAI endpoint |
| `AZURE_OPENAI_MODEL` | _(required for Azure OpenAI)_ | Azure deployment name |

## References

- [Microsoft Agent Framework Python samples](https://github.com/microsoft/agent-framework/tree/main/python/samples)
- [Functional workflow with agents](https://github.com/microsoft/agent-framework/blob/main/python/samples/01-get-started/05_functional_workflow_with_agents.py)
- [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/microsoft-agent-framework)
