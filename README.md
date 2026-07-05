<div align="center">
  <img src="docs/public/images/logo.svg" alt="OpenSandbox logo" width="150" />

  <h1>OpenSandbox</h1>

  <p align="center">
    <a href="https://trendshift.io/repositories/21828" target="_blank">
      <img src="https://trendshift.io/api/badge/repositories/21828" alt="alibaba%2FOpenSandbox | Trendshift" style="width: 320px; height: 70px;" width="320" height="70" />
    </a>
  </p>

<p align="center">
  <a href="https://github.com/alibaba/OpenSandbox">
    <img src="https://img.shields.io/badge/Stars-11.6k-181717?style=flat-square&logo=github&logoColor=white" alt="Stars" />
  </a>
  <a href="https://www.bestpractices.dev/projects/12588">
    <img src="https://img.shields.io/badge/OpenSSF-Best-4C566A?style=flat-square" alt="OpenSSF Best Practices" />
  </a>
  <a href="https://landscape.cncf.io/?item=orchestration-management--scheduling-orchestration--opensandbox">
    <img src="https://img.shields.io/badge/CNCF-Landscape-0C66E4?style=flat-square" alt="CNCF Landscape" />
  </a>
  <a href="https://discord.gg/g7FuPs8YeD">
    <img src="https://img.shields.io/badge/Discord-Join-5865F2?style=flat-square&logo=discord&logoColor=white" alt="Discord" />
  </a>
  <a href="https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11">
    <img src="https://img.shields.io/badge/DingTalk-Join-0089FF?style=flat-square" alt="DingTalk" />
  </a>
  <a href="https://github.com/alibaba/OpenSandbox/actions">
    <img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/real-e2e.yml?branch=main&label=TEST&style=flat-square&logo=github&logoColor=white" alt="E2E Status" />
  </a>
  <a href="https://github.com/alibaba/OpenSandbox/actions">
    <img src="https://img.shields.io/github/actions/workflow/status/opensandbox-group/OpenSandbox/kubernetes-nightly-build.yml?branch=main&label=K8S&style=flat-square&logo=kubernetes&logoColor=white" alt="Kubernetes nightly build status" />
  </a>
</p>

  <hr />
</div>

OpenSandbox is a **general-purpose sandbox platform** for AI applications, offering multi-language SDKs, unified sandbox APIs, and Docker/Kubernetes runtimes for scenarios like Coding Agents, GUI Agents, Agent Evaluation, AI Code Execution, and RL Training.

## Features

- 🧩 **SDKs, CLI, and MCP**: Provides multi-language SDKs, the osb CLI, and MCP server integration for sandbox creation, command execution, and file operations. See [SDKs](#sdks), [CLI](#cli), and [MCP](#mcp).
- 📜 **Sandbox Protocol**: Defines sandbox lifecycle management APIs and sandbox execution APIs so you can extend custom sandbox runtimes. See [API specs](specs/README.md).
- 🚀 **Sandbox Runtime**: Built-in lifecycle management supporting Docker and high-performance Kubernetes runtime, enabling both local runs and large-scale distributed scheduling. See [Kubernetes runtime](./kubernetes).
- 🖥️ **Sandbox Environments**: Built-in Command, Filesystem, and Code Interpreter implementations. Examples cover Coding Agents (e.g., Claude Code), browser automation (Chrome, Playwright), and desktop environments (VNC, VS Code).
- 🚦 **Network Policy**: Unified ingress gateway with multiple routing strategies plus per-sandbox egress controls. See [Ingress Gateway](components/ingress) and [egress controls](components/egress).
- 🔑 **Credential Vault**: Secure credential injection for sandbox outbound requests without exposing real secrets to workloads. See [Credential Vault](docs/guides/credential-vault.md).
- 🏰 **Strong Isolation**: Supports secure container runtimes like gVisor, Kata Containers, and Firecracker microVM for enhanced isolation between sandbox workloads and the host. See [Secure Container Runtime Guide](docs/guides/secure-container.md) for details.

## SDKs

Python:

```bash
pip install opensandbox
```

Java/Kotlin (Gradle Kotlin DSL):

```kotlin
dependencies {
    implementation("com.alibaba.opensandbox:sandbox:{latest_version}")
}
```

Java/Kotlin (Maven):

```xml
<dependency>
    <groupId>com.alibaba.opensandbox</groupId>
    <artifactId>sandbox</artifactId>
    <version>{latest_version}</version>
</dependency>
```

JavaScript/TypeScript:

```bash
npm install @alibaba-group/opensandbox
```

C#/.NET:

```bash
dotnet add package Alibaba.OpenSandbox
```

Go:

```bash
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

## CLI

OpenSandbox also provides `osb`, a terminal CLI for the common sandbox workflow: create sandboxes, run commands, move files, inspect diagnostics, and manage runtime egress policy.

Install:

```bash
pip install opensandbox-cli
# or
uv tool install opensandbox-cli
```

Quick start:

```bash
osb config init
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb config set connection.api_key <your-api-key>
osb sandbox create --image python:3.12 --timeout 30m -o json
osb command run <sandbox-id> -o raw -- python -c "print(1 + 1)"
```

See the [CLI README](cli/README.md) for the full command reference.

## MCP

The OpenSandbox MCP server exposes sandbox creation, command execution, and text file operations to MCP-capable clients such as Claude Code and Cursor.

Install and run:

```bash
pip install opensandbox-mcp
opensandbox-mcp --domain localhost:8080 --protocol http
```

Minimal stdio config:

```json
{
  "mcpServers": {
    "opensandbox": {
      "command": "opensandbox-mcp",
      "args": ["--domain", "localhost:8080", "--protocol", "http"]
    }
  }
}
```

See the [MCP README](sdks/mcp/sandbox/python/README.md) for client-specific setup.

## Getting Started

Requirements:

- Docker (required for local execution)
- Python 3.10+ (required for examples and local runtime)

### Install and Configure the Sandbox Server

```bash
uvx opensandbox-server init-config ~/.sandbox.toml --example docker

uvx opensandbox-server

# Show help
# uvx opensandbox-server -h
```

### Create a Code Interpreter and Execute Commands/Codes

Install the Code Interpreter SDK

```bash
uv pip install opensandbox-code-interpreter
```

Create a sandbox and execute commands and codes.

```python
import asyncio
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import Sandbox
from opensandbox.models import WriteEntry

async def main() -> None:
    # 1. Create a sandbox
    sandbox = await Sandbox.create(
        "opensandbox/code-interpreter:v1.1.0",
        entrypoint=["/opt/code-interpreter/code-interpreter.sh"],
        env={"PYTHON_VERSION": "3.11"},
        timeout=timedelta(minutes=10),
    )

    async with sandbox:

        # 2. Execute a shell command
        execution = await sandbox.commands.run("echo 'Hello OpenSandbox!'")
        print(execution.logs.stdout[0].text)

        # 3. Write a file
        await sandbox.files.write_files([
            WriteEntry(path="/tmp/hello.txt", data="Hello World", mode=644)
        ])

        # 4. Read a file
        content = await sandbox.files.read_file("/tmp/hello.txt")
        print(f"Content: {content}") # Content: Hello World

        # 5. Create a code interpreter
        interpreter = await CodeInterpreter.create(sandbox)

        # 6. Execute Python code (single-run, pass language directly)
        result = await interpreter.codes.run(
              """
                  import sys
                  print(sys.version)
                  result = 2 + 2
                  result
              """,
              language=SupportedLanguage.PYTHON,
        )

        print(result.result[0].text) # 4
        print(result.logs.stdout[0].text) # 3.11.14

        # 7. Cleanup the sandbox
        await sandbox.kill()

if __name__ == "__main__":
    asyncio.run(main())
```

### More Examples

OpenSandbox provides examples covering SDK usage, agent integrations, browser automation, and training workloads. All example code is located in the `examples/` directory.

#### 🎯 Basic Examples

- **[code-interpreter](docs/examples/code-interpreter.md)** - End-to-end Code Interpreter SDK workflow in a sandbox.
- **[aio-sandbox](docs/examples/aio-sandbox.md)** - All-in-One sandbox setup using the OpenSandbox SDK.
- **[agent-sandbox](docs/examples/agent-sandbox.md)** - Example integration for running OpenSandbox workloads on Kubernetes with [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox).
- **Volumes** — [Docker PVC / named volumes](docs/examples/docker-pvc-volume-mount.md), [Docker OSSFS](docs/examples/docker-ossfs-volume-mount.md), [Kubernetes PVC](docs/examples/kubernetes-pvc-volume-mount.md): persistent and shared storage patterns.

#### 🤖 Coding Agent Integrations

- **Coding CLIs** — [Claude Code](docs/examples/claude-code.md), [Gemini CLI](docs/examples/gemini-cli.md), [OpenAI Codex CLI](docs/examples/codex-cli.md), [Qwen Code](docs/examples/qwen-code.md), [Kimi CLI](docs/examples/kimi-cli.md): run each vendor CLI inside OpenSandbox.
- **[langgraph](docs/examples/langgraph.md)** - LangGraph state-machine workflow that creates/runs a sandbox job with fallback retry.
- **[google-adk](docs/examples/google-adk.md)** - Google ADK agent using OpenSandbox tools to write/read files and run commands.
- **[openclaw](docs/examples/openclaw.md)** - Launch an OpenClaw Gateway inside a sandbox.

#### 🌐 Browser and Desktop Environments

- **[chrome](docs/examples/chrome.md)** - Chromium sandbox with VNC and DevTools access for automation and debugging.
- **[playwright](docs/examples/playwright.md)** - Playwright + Chromium headless scraping and testing example.
- **[desktop](docs/examples/desktop.md)** - Full desktop environment in a sandbox with VNC access.
- **[vscode](docs/examples/vscode.md)** - code-server (VS Code Web) running inside a sandbox for remote dev.

#### 🧠 Training and Evaluation

- **[rl-training](docs/examples/rl-training.md)** - DQN CartPole training in a sandbox with checkpoints and summary output.
- **[harbor-evaluation](docs/examples/harbor-evaluation.md)** - Run a [Harbor](https://github.com/harbor-framework/harbor) agent evaluation on OpenSandbox, one sandbox per trial.

For more details, please refer to the [examples documentation](docs/examples/index.md).

## Project Structure

| Directory | Description                                                      |
|-----------|------------------------------------------------------------------|
| [`sdks/`](sdks/) | Multi-language SDKs (Python, Java/Kotlin, TypeScript/JavaScript, C#/.NET) |
| [`specs/`](specs/README.md) | OpenAPI specs and lifecycle specifications                      |
| [`server/`](server/README.md) | Python FastAPI sandbox lifecycle server                          |
| [`cli/`](cli/README.md) | OpenSandbox command-line interface                               |
| [`kubernetes/`](kubernetes/README.md) | Kubernetes deployment and examples                               |
| [`components/execd/`](components/execd/README.md) | Sandbox execution daemon (commands and file operations)          |
| [`components/ingress/`](components/ingress/README.md) | Sandbox traffic ingress proxy                                    |
| [`components/egress/`](components/egress/README.md) | Sandbox network egress control                                   |
| [`sandboxes/`](sandboxes/) | Runtime sandbox implementations                                   |
| [`examples/`](examples/) | Runnable example code                                            |
| [`docs/examples/`](docs/examples/index.md) | Example documentation and use cases                              |
| [`oseps/`](oseps/README.md) | OpenSandbox Enhancement Proposals                                |
| [`docs/`](docs/) | Architecture and design documentation                            |
| [`tests/`](tests/) | Cross-component E2E tests                                        |
| [`scripts/`](scripts/) | Development and maintenance scripts                              |

For detailed architecture, see [Architecture](docs/architecture/).

## Documentation

- [Architecture](docs/architecture/) – Overall architecture & design philosophy
- [Credential Vault](docs/guides/credential-vault.md) - Credential Vault credential injection guide
- [Release Verification](docs/community/release-verification.md) - Release signing and artifact verification
- [oseps/README.md](oseps/README.md) – OpenSandbox Enhancement Proposals
- SDK
  - Sandbox base SDK ([Java/Kotlin SDK](sdks/sandbox/kotlin/README.md), [Python SDK](sdks/sandbox/python/README.md), [JavaScript/TypeScript SDK](sdks/sandbox/javascript/README.md), [C#/.NET SDK](sdks/sandbox/csharp/README.md)), [Go SDK](sdks/sandbox/go/README.md) - includes sandbox lifecycle, command execution, file operations
  - Code Interpreter SDK ([Java/Kotlin SDK](sdks/code-interpreter/kotlin/README.md), [Python SDK](sdks/code-interpreter/python/README.md), [JavaScript/TypeScript SDK](sdks/code-interpreter/javascript/README.md), [C#/.NET SDK](sdks/code-interpreter/csharp/README.md)) - code interpreter
- [cli/README.md](cli/README.md) - OpenSandbox CLI installation and command reference
- [sdks/mcp/sandbox/python/README.md](sdks/mcp/sandbox/python/README.md) - MCP server installation and client setup
- [specs/README.md](specs/README.md) - OpenAPI definitions for sandbox lifecycle API and sandbox execution API
- [server/README.md](server/README.md) - Sandbox server startup and configuration; supports Docker and Kubernetes runtimes
- [ROADMAP.md](ROADMAP.md) - Lightweight project roadmap and planning process

## License

This project is open source under the [Apache 2.0 License](LICENSE).

## Roadmap

See [ROADMAP.md](ROADMAP.md) for the current project roadmap, planning scope,
and how roadmap items are managed.

## Contact and Discussion

- Issues: Submit bugs, feature requests, or design discussions through GitHub Issues
- Discord: Join the [OpenSandbox Discord community](https://discord.gg/g7FuPs8YeD)
- DingTalk: Join the [OpenSandbox technical discussion group](https://qr.dingtalk.com/action/joingroup?code=v1,k1,A4Bgl5q1I1eNU/r33D18YFNrMY108aFF38V+r19RJOM=&_dt_no_comment=1&origin=11)

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=alibaba/OpenSandbox&type=date&legend=top-left)](https://www.star-history.com/#alibaba/OpenSandbox&type=date&legend=top-left)
